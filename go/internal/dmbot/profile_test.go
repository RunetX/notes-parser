package dmbot

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lovegw/internal/kbd"
	"lovegw/internal/love"
	"lovegw/internal/store"
)

const profileUID = 55501

// fakeSiteProfile — сайт, умеющий управлять своей анкетой. Кнопку отдаёт такой
// же, как настоящий сайт: подпись приходит со страницы, а не из кода бота.
type fakeSiteProfile struct {
	fakeSite
	blocked   bool
	noButton  bool  // сайт кнопки не предлагает
	readErr   error // чем отвечает чтение состояния
	submitErr error
	reads     int
	submits   int
	// stuck — сайт принял запрос (200), но состояние не изменил.
	stuck bool
	// dieAfterSubmit — сайт закрыл сессию сразу после действия.
	dieAfterSubmit bool
}

func (f *fakeSiteProfile) ProfileControl(context.Context, []*http.Cookie) (love.ProfileControl, error) {
	f.reads++
	if f.readErr != nil {
		return love.ProfileControl{}, f.readErr
	}
	ctrl := love.ProfileControl{Blocked: f.blocked, Available: !f.noButton}
	if ctrl.Available {
		ctrl.Label = "Заблокировать профиль"
		if f.blocked {
			ctrl.Label = "Разблокировать профиль"
		}
	}
	return ctrl, nil
}

func (f *fakeSiteProfile) SubmitProfileControl(context.Context, []*http.Cookie, love.ProfileControl) error {
	f.submits++
	if f.submitErr != nil {
		return f.submitErr
	}
	if !f.stuck {
		f.blocked = !f.blocked
	}
	if f.dieAfterSubmit {
		f.readErr = love.ErrUnauthorized
	}
	return nil
}

func newProfileLogic(t *testing.T) (*Logic, *fakeTransport, *fakeSiteProfile, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tr := &fakeTransport{}
	site := &fakeSiteProfile{}
	return NewLogic(st, site, tr, store.MessengerTelegram, slog.Default()), tr, site, st
}

// seedProfileSession кладёт живую сессию сайта — иначе команда зовёт к /login.
func seedProfileSession(t *testing.T, st *store.Store) {
	t.Helper()
	ck, err := love.CookiesToJSON([]*http.Cookie{{Name: "sid", Value: "x"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSession(context.Background(), store.MessengerTelegram, profileUID, ck, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestProfileNeedsLogin(t *testing.T) {
	ctx := context.Background()
	l, tr, site, _ := newProfileLogic(t)
	l.HandleText(ctx, profileUID, "1", "/profile")
	if !strings.Contains(tr.lastSent(), "/login") {
		t.Errorf("без сессии зовём ко входу, получили %q", tr.lastSent())
	}
	if site.reads != 0 {
		t.Error("без сессии сайт трогать незачем")
	}
}

// Активная анкета: ровно одна кнопка, и это блокировка с подписью сайта.
func TestProfileShowsSingleButton(t *testing.T) {
	ctx := context.Background()
	l, tr, site, st := newProfileLogic(t)
	seedProfileSession(t, st)
	l.HandleText(ctx, profileUID, "1", "/profile")
	if !strings.Contains(tr.lastSent(), "активна") {
		t.Errorf("состояние анкеты: %q", tr.lastSent())
	}
	btns := buttonTexts(tr.lastKB())
	if len(btns) != 1 || !strings.Contains(btns[0], "Заблокировать профиль") {
		t.Fatalf("кнопки: %v", btns)
	}
	if site.submits != 0 {
		t.Error("показ состояния ничего не нажимает")
	}
}

// Блокировка идёт через подтверждение, и до него сайт не трогаем.
func TestProfileBlockAsksConfirmation(t *testing.T) {
	ctx := context.Background()
	l, tr, site, st := newProfileLogic(t)
	seedProfileSession(t, st)
	l.HandleCallback(ctx, profileUID, kbd.Callback{
		MessageID: "1", Payload: kbd.Pack(verbProfileAsk, argProfileBlock)})

	edit := tr.lastEdit()
	if !strings.Contains(edit.text, "Заблокировать анкету?") {
		t.Errorf("вопрос о блокировке: %q", edit.text)
	}
	if btns := buttonTexts(edit.kb); len(btns) != 2 {
		t.Errorf("ждём подтверждение и отмену, получили %v", btns)
	}
	if site.submits != 0 {
		t.Fatal("до подтверждения анкета блокироваться не должна")
	}
}

func TestProfileBlockAndUnblock(t *testing.T) {
	ctx := context.Background()
	l, tr, site, st := newProfileLogic(t)
	seedProfileSession(t, st)

	l.HandleCallback(ctx, profileUID, kbd.Callback{
		MessageID: "1", Payload: kbd.Pack(verbProfileSet, argProfileBlock)})
	if site.submits != 1 || !site.blocked {
		t.Fatalf("после подтверждения анкета должна быть заблокирована: submits=%d blocked=%v",
			site.submits, site.blocked)
	}
	edit := tr.lastEdit()
	if !strings.Contains(edit.text, "заблокирована") {
		t.Errorf("итог блокировки: %q", edit.text)
	}
	if btns := buttonTexts(edit.kb); len(btns) != 1 || !strings.Contains(btns[0], "Разблокировать") {
		t.Fatalf("после блокировки кнопка должна вернуть анкету, получили %v", btns)
	}

	// Разблокировка — сразу, без промежуточного подтверждения.
	l.HandleCallback(ctx, profileUID, kbd.Callback{
		MessageID: "2", Payload: kbd.Pack(verbProfileSet, argProfileUnblock)})
	if site.submits != 2 || site.blocked {
		t.Fatalf("анкета должна вернуться: submits=%d blocked=%v", site.submits, site.blocked)
	}
	if !strings.Contains(tr.lastEdit().text, "активна") {
		t.Errorf("итог разблокировки: %q", tr.lastEdit().text)
	}
}

// Кнопка провисела, а анкету заблокировали на самом сайте: делать надо ровно
// то, что написано на кнопке, — то есть уже ничего.
func TestProfileSetSkipsWhenAlreadyInTargetState(t *testing.T) {
	ctx := context.Background()
	l, tr, site, st := newProfileLogic(t)
	seedProfileSession(t, st)
	site.blocked = true

	l.HandleCallback(ctx, profileUID, kbd.Callback{
		MessageID: "1", Payload: kbd.Pack(verbProfileSet, argProfileBlock)})
	if site.submits != 0 {
		t.Fatal("повторно блокировать заблокированную анкету незачем")
	}
	if !strings.Contains(tr.lastEdit().text, "заблокирована") {
		t.Errorf("показываем актуальное состояние: %q", tr.lastEdit().text)
	}
}

// Сайт ответил 200, а состояние не поменялось — это отказ, и врать о нём нельзя.
func TestProfileReportsSiteRefusal(t *testing.T) {
	ctx := context.Background()
	l, tr, site, st := newProfileLogic(t)
	seedProfileSession(t, st)
	site.stuck = true

	l.HandleCallback(ctx, profileUID, kbd.Callback{
		MessageID: "1", Payload: kbd.Pack(verbProfileSet, argProfileBlock)})
	if !strings.Contains(tr.lastEdit().text, "не принял") {
		t.Errorf("ждём честный отказ, получили %q", tr.lastEdit().text)
	}
}

// Блокировка оборвала сессию: проверить результат нечем, и об этом надо
// сказать прямо, а не отделаться «сессия истекла».
func TestProfileSessionDiesAfterBlock(t *testing.T) {
	ctx := context.Background()
	l, tr, site, st := newProfileLogic(t)
	seedProfileSession(t, st)
	site.dieAfterSubmit = true

	l.HandleCallback(ctx, profileUID, kbd.Callback{
		MessageID: "1", Payload: kbd.Pack(verbProfileSet, argProfileBlock)})
	if site.submits != 1 {
		t.Fatalf("действие должно было уйти: submits=%d", site.submits)
	}
	got := tr.lastSent()
	if !strings.Contains(got, "love.ngs.ru") || !strings.Contains(got, "/login") {
		t.Errorf("ждём «отправил, но сессия закрыта», получили %q", got)
	}
	if _, valid, err := st.SessionCookies(ctx, store.MessengerTelegram, profileUID); err != nil || valid {
		t.Errorf("сессию надо погасить: valid=%v err=%v", valid, err)
	}
}

// Отказ на самой отправке: анкета осталась как была, и об этом сказано.
func TestProfileSubmitFailureKeepsSession(t *testing.T) {
	ctx := context.Background()
	l, tr, site, st := newProfileLogic(t)
	seedProfileSession(t, st)
	site.submitErr = errors.New("статус 502")

	l.HandleCallback(ctx, profileUID, kbd.Callback{
		MessageID: "1", Payload: kbd.Pack(verbProfileSet, argProfileBlock)})
	if site.blocked {
		t.Error("отказ сайта не должен менять состояние")
	}
	if !strings.Contains(tr.lastSent(), "не отвечает") {
		t.Errorf("ждём «сайт не отвечает», получили %q", tr.lastSent())
	}
	if _, valid, err := st.SessionCookies(ctx, store.MessengerTelegram, profileUID); err != nil || !valid {
		t.Errorf("сессия должна остаться валидной: valid=%v err=%v", valid, err)
	}
}

// Заблокированной анкете сайт может не предложить кнопки — говорим прямо.
func TestProfileWithoutButton(t *testing.T) {
	ctx := context.Background()
	l, tr, _, st := newProfileLogic(t)
	seedProfileSession(t, st)
	l.profile.(*fakeSiteProfile).blocked = true
	l.profile.(*fakeSiteProfile).noButton = true

	l.HandleText(ctx, profileUID, "1", "/profile")
	if !strings.Contains(tr.lastSent(), "love.ngs.ru") {
		t.Errorf("без кнопки отправляем на сайт, получили %q", tr.lastSent())
	}
	if btns := buttonTexts(tr.lastKB()); len(btns) != 0 {
		t.Errorf("кнопок быть не должно, получили %v", btns)
	}
}

func TestProfileUnauthorizedInvalidatesSession(t *testing.T) {
	ctx := context.Background()
	l, tr, site, st := newProfileLogic(t)
	seedProfileSession(t, st)
	site.readErr = love.ErrUnauthorized

	l.HandleText(ctx, profileUID, "1", "/profile")
	if !strings.Contains(tr.lastSent(), "/login") {
		t.Errorf("гостевой ответ зовёт ко входу, получили %q", tr.lastSent())
	}
	if _, valid, err := st.SessionCookies(ctx, store.MessengerTelegram, profileUID); err != nil || valid {
		t.Errorf("сессию надо погасить: valid=%v err=%v", valid, err)
	}
}

// Сайт моргнул 5xx или упёрся в DDoS-Guard — сессия тут ни при чём.
func TestProfileSiteErrorKeepsSession(t *testing.T) {
	ctx := context.Background()
	l, tr, site, st := newProfileLogic(t)
	seedProfileSession(t, st)
	site.readErr = errors.New("статус 502")

	l.HandleText(ctx, profileUID, "1", "/profile")
	if !strings.Contains(tr.lastSent(), "не отвечает") {
		t.Errorf("ждём «сайт не отвечает», получили %q", tr.lastSent())
	}
	if _, valid, err := st.SessionCookies(ctx, store.MessengerTelegram, profileUID); err != nil || !valid {
		t.Errorf("сессия должна остаться валидной: valid=%v err=%v", valid, err)
	}
}

// Клиент сайта без этой способности — команды просто нет.
func TestProfileAbsentWithoutCapableSite(t *testing.T) {
	ctx := context.Background()
	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)
	if l.profile != nil {
		t.Fatal("fakeSite управлять анкетой не умеет")
	}
	l.HandleText(ctx, profileUID, "1", "/profile")
	if !strings.Contains(tr.lastSent(), "недоступно") {
		t.Errorf("ждём отказ, получили %q", tr.lastSent())
	}
	for _, c := range botCommands(false, true, false) {
		if c.Name == "profile" {
			t.Error("в меню команды быть не должно")
		}
	}
}

// Бот переписки к анкете отношения не имеет: у него нет ни сайта, ни /login.
func TestProfileRejectedByTalksBot(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tr := &fakeTransport{}
	l := NewTalksLogic(st, tr, store.MessengerTelegram, slog.Default())

	l.HandleText(ctx, profileUID, "1", "/profile")
	if !strings.Contains(tr.lastSent(), "только личная переписка") {
		t.Errorf("бот переписки должен отфутболить, получили %q", tr.lastSent())
	}
	l.HandleCallback(ctx, profileUID, kbd.Callback{Payload: kbd.Pack(verbProfileSet, argProfileBlock)})
	if got := tr.answers[len(tr.answers)-1]; !strings.Contains(got, "только личная переписка") {
		t.Errorf("нажатие тоже не для него: %q", got)
	}
	for _, c := range botCommands(true, true, true) {
		if c.Name == "profile" {
			t.Error("в меню бота переписки команды быть не должно")
		}
	}
}

// Команда есть в меню и в приветствии ровно тогда, когда сайт её поддерживает.
func TestProfileInMenus(t *testing.T) {
	var found bool
	for _, c := range botCommands(false, true, true) {
		found = found || c.Name == "profile"
	}
	if !found {
		t.Error("в меню бота команд команда должна быть")
	}
	if !strings.Contains(startMessage(false, true, true), "/profile") {
		t.Error("в приветствии команда должна быть")
	}
	if strings.Contains(startMessage(false, true, false), "/profile") {
		t.Error("без способности сайта команды в приветствии быть не должно")
	}
	var inMenu bool
	for _, b := range buttonTexts(mainMenu(false, true, true)) {
		inMenu = inMenu || b == btnProfile
	}
	if !inMenu {
		t.Error("кнопка анкеты должна быть в главном меню")
	}
}
