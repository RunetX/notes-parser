package web

// Страницы модерации. Проверяются здесь не SQL и не политика (это ядро и
// platmod), а ровно то, что живёт в морде: кому что видно и что происходит по
// нажатию.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

const modUserID = 606064 // «Хатуль мадан»: в архиве это и правда модератор

// testInviteCode — код, который отдаёт дубль. Настоящий выдаёт ядро, морда его
// только показывает — и вот это показывание тесты и проверяют.
const testInviteCode = "T3H-TEST-CODE"

// fakeMod — модерация в памяти.
type fakeMod struct {
	queue    []platform.ReviewItem
	auto     []platform.ReviewItem
	stats    platform.ModerationStats
	audit    []platform.AuditEntry
	mine     []platform.MyCheck
	users    map[int64]platform.User
	acts     []string // что позвали, по порядку
	reported []platform.Subject
	appealed []platform.Subject
	invites  []platform.Invite
	// edited — что дошло до ядра при правке чужой заметки: текст и «зачем».
	edited     string
	editReason string
	// pinnedFull — закреплённых уже столько, сколько лента выдерживает: ядро в
	// этом случае отказывает, и морда обязана сказать об этом человеком, а не
	// пятисоткой.
	pinnedFull bool
	fail       error
}

func newFakeMod() *fakeMod {
	return &fakeMod{users: map[int64]platform.User{}}
}

func (f *fakeMod) note(what string) error {
	f.acts = append(f.acts, what)
	return f.fail
}

func (f *fakeMod) ReviewQueue(context.Context, int) ([]platform.ReviewItem, error) {
	return f.queue, f.fail
}
func (f *fakeMod) AutoHidden(context.Context, int) ([]platform.ReviewItem, error) {
	return f.auto, f.fail
}
func (f *fakeMod) ModerationStats(context.Context) (platform.ModerationStats, error) {
	return f.stats, f.fail
}
func (f *fakeMod) AuditTail(context.Context, int) ([]platform.AuditEntry, error) {
	return f.audit, f.fail
}
func (f *fakeMod) HideSubject(_ context.Context, _ platform.Viewer, s platform.Subject, _, _ string) error {
	return f.note("hide " + s.String())
}
func (f *fakeMod) RestoreSubject(_ context.Context, _ platform.Viewer, s platform.Subject, _ string) error {
	return f.note("restore " + s.String())
}
func (f *fakeMod) Decide(_ context.Context, _ platform.Viewer, s platform.Subject, d platform.Decision, _ string) error {
	if d == platform.DecisionHide {
		return f.note("drop " + s.String())
	}
	return f.note("keep " + s.String())
}
func (f *fakeMod) SetNotePinned(_ context.Context, _ platform.Viewer, id int64, pinned bool, _ string) error {
	if pinned {
		if f.pinnedFull {
			return platform.ErrTooManyPinned
		}
		return f.note("pin")
	}
	return f.note("unpin")
}

func (f *fakeMod) SetThreadLocked(_ context.Context, _ platform.Viewer, id int64, locked bool, _ string) error {
	if locked {
		return f.note("lock")
	}
	return f.note("unlock")
}
func (f *fakeMod) BanUser(_ context.Context, _ platform.Viewer, _ int64, _ time.Time, _ string) error {
	return f.note("ban")
}
func (f *fakeMod) UnbanUser(_ context.Context, _ platform.Viewer, _ int64, _ string) error {
	return f.note("unban")
}
func (f *fakeMod) SetRole(_ context.Context, _ platform.Viewer, _ int64, r platform.Role) error {
	return f.note("role " + string(rune('0'+int(r))))
}

// Правка чужой заметки. Дубль повторяет два правила ядра, на которые опирается
// морда: право администратора и «только нативная». Остальное (журнал, очередь,
// «текст и так такой») проверяется в platform на живом Postgres.
func (f *fakeMod) EditNoteAsAdmin(_ context.Context, actor platform.Viewer,
	id int64, body, reason string) error {
	if !actor.CanAdmin() {
		return platform.ErrNotAdmin
	}
	if !platform.IsNative(id) {
		return platform.ErrNotNative
	}
	f.edited, f.editReason = body, reason
	return f.note("edit " + strconv.FormatInt(id, 10))
}

// Приглашения. Дубль повторяет ровно два правила ядра, на которые опирается
// морда: право администратора и «такого участника нет» — остальное (хеши,
// журнал) проверяется в platform на живом Postgres.
func (f *fakeMod) IssueInvite(_ context.Context, actor platform.Viewer, bind int64,
	note string, ttl time.Duration) (string, error) {
	if !actor.CanAdmin() {
		return "", platform.ErrNotAdmin
	}
	if f.fail != nil {
		return "", f.fail
	}
	u, known := f.users[bind]
	if bind != 0 && !known {
		return "", platform.ErrNotFound
	}
	if err := f.note("invite " + strconv.FormatInt(bind, 10)); err != nil {
		return "", err
	}
	now := time.Now()
	in := platform.Invite{
		CreatedAt: now, ExpiresAt: now.Add(ttl), Note: note,
		BindUser: bind, BindNick: u.Nick, BindKind: u.Kind,
	}
	f.invites = append([]platform.Invite{in}, f.invites...)
	return testInviteCode, nil
}

func (f *fakeMod) Invites(context.Context, int) ([]platform.Invite, error) {
	return f.invites, f.fail
}

func (f *fakeMod) RevokeInvite(_ context.Context, actor platform.Viewer, at time.Time) error {
	if !actor.CanAdmin() {
		return platform.ErrNotAdmin
	}
	for i := range f.invites {
		if f.invites[i].CreatedAt.Equal(at) && f.invites[i].Live(time.Now()) {
			f.invites[i].ExpiresAt = time.Now().Add(-time.Second)
			return f.note("revoke")
		}
	}
	return platform.ErrNothingToDo
}

func (f *fakeMod) UserByID(_ context.Context, id int64) (platform.User, error) {
	u, ok := f.users[id]
	if !ok {
		return platform.User{}, platform.ErrNotFound
	}
	return u, nil
}
func (f *fakeMod) AddReport(_ context.Context, _ int64, s platform.Subject, _ string) error {
	f.reported = append(f.reported, s)
	return f.fail
}
func (f *fakeMod) Appeal(_ context.Context, _ int64, s platform.Subject) error {
	f.appealed = append(f.appealed, s)
	return f.fail
}
func (f *fakeMod) MyHidden(context.Context, int64, int) ([]platform.MyCheck, error) {
	return f.mine, f.fail
}

// ------------------------------------------------ модерация из ленты

// hiddenFeed — лента, в которой одна заметка скрыта модерацией. Ядро отдаёт её
// только модератору (platform.feedModQuery), дубль это лишь повторяет: здесь
// проверяется, ЧТО МОРДА С НЕЙ ДЕЛАЕТ.
func hiddenFeed() *fakeStore {
	live := sampleNote()
	gone := sampleNote()
	gone.ID, gone.Status, gone.Body = 312812, platform.StatusHiddenMod, "за это скрыли"
	return &fakeStore{total: 2, hidden: 1, notes: []platform.NoteView{live, gone}, note: live}
}

// Модератор работает ТАМ, ГДЕ ЧИТАЕТ, а читает он ленту: до 26.08.2026 за каждым
// решением надо было открывать страницу заметки.
func TestКнопкиМодерацииВЛенте(t *testing.T) {
	h, _, token := modServerOn(t, hiddenFeed(), platform.RoleModerator)

	body := do(h, as(guest(t, "GET", "/"), token)).Body.String()
	for _, want := range []string{
		`value="hide"`,    // скрыть — под живой
		`value="restore"`, // вернуть — под скрытой
		`value="pin"`,     // закрепление это решение о МЕСТЕ В ЛЕНТЕ
		`value="lock"`,
		"/mod/u/", // и дорога к автору
	} {
		if !strings.Contains(body, want) {
			t.Errorf("в ленте модератора нет %q", want)
		}
	}
	// Скрытая заметка не пропала, а затенена — и видно, почему.
	if !strings.Contains(body, "nhid") || !strings.Contains(body, "скрыто модерацией") {
		t.Error("скрытая заметка в ленте не помечена скрытой")
	}
	if !strings.Contains(body, "за это скрыли") {
		t.Error("скрытая заметка пропала из ленты у модератора")
	}
}

// У читателя лента прежняя: ни кнопок, ни жалобы под каждой из двадцати строк.
// Скрытого ядро ему и не отдаёт, но проверяется здесь другое — что морда не
// зовёт его туда, где ему откажут.
func TestЛентаЧитателяБезКнопок(t *testing.T) {
	h, _, token := modServerOn(t, hiddenFeed(), platform.RoleUser)

	body := do(h, as(guest(t, "GET", "/"), token)).Body.String()
	for _, no := range []string{`value="hide"`, `value="pin"`, `value="lock"`, "/mod/u/", "Пожаловаться"} {
		if strings.Contains(body, no) {
			t.Errorf("в ленте участника есть %q", no)
		}
	}
	if body := do(h, guest(t, "GET", "/")).Body.String(); strings.Contains(body, "modbar") {
		t.Error("гостю в ленте показана полоска модерации")
	}
}

// «Поправить» под чужой заметкой — право администратора, и в ленте оно тоже
// администраторское: модератору его быть не должно.
func TestПравкаВЛентеТолькоАдминистратору(t *testing.T) {
	native := sampleNote()
	native.ID = platform.NativeIDBase + 7
	store := func() *fakeStore {
		return &fakeStore{total: 1, notes: []platform.NoteView{native}, note: native}
	}
	link := "/n/" + itoa64(native.ID) + "/edit"

	h, _, token := modServerOn(t, store(), platform.RoleModerator)
	if strings.Contains(do(h, as(guest(t, "GET", "/"), token)).Body.String(), link) {
		t.Error("модератору в ленте предложена правка чужого текста")
	}
	h, _, token = modServerOn(t, store(), platform.RoleAdmin)
	if !strings.Contains(do(h, as(guest(t, "GET", "/"), token)).Body.String(), link) {
		t.Error("администратору в ленте не предложена правка")
	}
	// А под ЗЕРКАЛЬНОЙ её нет ни у кого: здесь копия страницы НГС.
	h, _, token = modServerOn(t, hiddenFeed(), platform.RoleAdmin)
	if strings.Contains(do(h, as(guest(t, "GET", "/"), token)).Body.String(), "/n/312811/edit") {
		t.Error("в ленте предложена правка зеркальной заметки")
	}
}

// modServer — площадка с вошедшим человеком заданной роли и живой модерацией.
func modServer(t *testing.T, role platform.Role) (http.Handler, *fakeMod, string) {
	t.Helper()
	return modServerOn(t, noteStore(), role)
}

// modServerOn — то же самое, но на своей заготовке базы: правка администратора
// смотрит на саму заметку (нативная или зеркальная, чья, в каком статусе), и
// одной общей заготовкой её не проверить.
func modServerOn(t *testing.T, st *fakeStore, role platform.Role) (http.Handler, *fakeMod, string) {
	t.Helper()
	auth, token := signedInAs(t, platform.User{
		ID: modUserID, Nick: "Хатуль мадан", Kind: platform.KindMember, Role: role,
	})
	mod := newFakeMod()
	return newFullServer(t, st, auth, &fakeWriter{}, mod, nil, Config{}), mod, token
}

// signedInAs — вошедший человек с ОБОИМИ подписанными согласиями. Без них любая
// страница уводит на /consent: вход не доводится до конца, пока документы не
// подписаны, и это правило Ш4, а не мелочь теста.
func signedInAs(t *testing.T, u platform.User) (*fakeAuth, string) {
	t.Helper()
	ctx := context.Background()
	auth := newFakeAuth()
	auth.users[u.ID] = u
	grantConsents(t, auth, u.ID)
	token, _, err := auth.CreateSession(ctx, u.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	return auth, token
}

// Существование закрытой двери — само по себе сведения: постороннему про
// страницу модерации знать незачем, поэтому ответ «нет такой страницы», а не
// «нужны права».
func TestОчередьЗакрытаОтПосторонних(t *testing.T) {
	h, _, token := modServer(t, platform.RoleUser)

	for _, target := range []string{"/mod", "/mod/log", "/mod/u/" + itoa64(modUserID)} {
		if w := do(h, guest(t, "GET", target)); w.Code != http.StatusNotFound {
			t.Errorf("гость на %s: код %d, ожидался 404", target, w.Code)
		}
		if w := do(h, as(guest(t, "GET", target), token)); w.Code != http.StatusNotFound {
			t.Errorf("участник на %s: код %d, ожидался 404", target, w.Code)
		}
	}
}

func TestОчередьВидитМодератор(t *testing.T) {
	h, mod, token := modServer(t, platform.RoleModerator)
	mod.stats = platform.ModerationStats{Review: 2, AutoHidden: 1, Appeals: 1, Reports: 3}
	mod.queue = []platform.ReviewItem{{
		Subject: platform.CommentSubject(700), NoteID: 312811, AuthorID: 1, AuthorNick: "Пух",
		Body: "купите слона", Category: platform.CatSpam, Reason: "реклама", Quote: "купите",
		Model: "claude-haiku-4-5", QueuedAt: time.Now(),
		Reports: []platform.Report{{Nick: "Мавр", Reason: "спам"}},
	}}

	w := do(h, as(guest(t, "GET", "/mod"), token))
	if w.Code != http.StatusOK {
		t.Fatalf("страница ответила %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"купите слона",                           // сам текст, целиком
		platform.CategoryTitle(platform.CatSpam), // причина словами
		"claude-haiku-4-5",                       // чьё мнение
		"Жалоба",                                 // и что на это жаловались
		`href="/n/312811#c700"`,                  // ссылка в место разговора
		`value="keep"`, `value="drop"`,           // два решения
	} {
		if !strings.Contains(body, want) {
			t.Errorf("на странице очереди нет %q", want)
		}
	}
}

// Модератор работает ТАМ, ГДЕ ЧИТАЕТ: кнопки стоят под каждой репликой страницы
// заметки, а не только в очереди.
func TestКнопкиПодРепликами(t *testing.T) {
	h, _, token := modServer(t, platform.RoleModerator)

	body := do(h, as(guest(t, "GET", "/n/312811"), token)).Body.String()
	if n := strings.Count(body, `value="hide"`); n < 4 {
		t.Fatalf("кнопок «скрыть» %d — ожидались под заметкой и каждой из трёх реплик", n)
	}
	if !strings.Contains(body, `value="lock"`) {
		t.Error("нет кнопки замка треда")
	}
	if !strings.Contains(body, "/mod/u/") {
		t.Error("нет ссылки на карточку автора")
	}
}

// У обычного участника вместо этого «Пожаловаться», и на СВОЁ он жаловаться не
// может.
func TestУчастникуЖалобаАНеКнопки(t *testing.T) {
	h, _, token := modServer(t, platform.RoleUser)

	body := do(h, as(guest(t, "GET", "/n/312811"), token)).Body.String()
	if strings.Contains(body, `value="hide"`) {
		t.Error("участнику видны кнопки модератора")
	}
	if !strings.Contains(body, "/report?kind=comment") {
		t.Error("нет ссылки «Пожаловаться»")
	}
}

func TestГостюНиКнопокНиЖалобы(t *testing.T) {
	h, _, _ := modServer(t, platform.RoleUser)

	body := do(h, guest(t, "GET", "/n/312811")).Body.String()
	if strings.Contains(body, "/report?") || strings.Contains(body, `value="hide"`) {
		t.Error("гостю показали инструменты, которых у него нет")
	}
}

func TestДействиеМодератора(t *testing.T) {
	h, mod, token := modServer(t, platform.RoleModerator)

	form := url.Values{
		"kind": {"comment"}, "id": {"700"}, "do": {"hide"},
		"reason": {"реклама"}, "back": {"/n/312811"},
	}
	w := do(h, postAs(t, "/mod/act", form, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/n/312811" {
		t.Errorf("вернуло на %q, а не туда, откуда нажали", got)
	}
	if len(mod.acts) != 1 || mod.acts[0] != "hide comment 700" {
		t.Fatalf("позвано %v", mod.acts)
	}
}

// Форма без токена — отказ: то же правило, что у всех пишущих форм.
func TestДействиеТребуетТокена(t *testing.T) {
	h, mod, token := modServer(t, platform.RoleModerator)

	form := url.Values{"kind": {"comment"}, "id": {"700"}, "do": {"hide"}}
	w := do(h, as(post(t, "/mod/act", form), token))
	if w.Code != http.StatusForbidden {
		t.Fatalf("код %d, ожидался отказ", w.Code)
	}
	if len(mod.acts) != 0 {
		t.Fatal("действие выполнено без токена")
	}
}

// Участник не должен уметь скрывать чужое, даже зная адрес ручки.
func TestУчастникНеСкрывает(t *testing.T) {
	h, mod, token := modServer(t, platform.RoleUser)

	form := url.Values{"kind": {"comment"}, "id": {"700"}, "do": {"hide"}}
	if w := do(h, postAs(t, "/mod/act", form, token)); w.Code != http.StatusNotFound {
		t.Fatalf("код %d", w.Code)
	}
	if len(mod.acts) != 0 {
		t.Fatalf("выполнено %v", mod.acts)
	}
}

// «Состояние уже такое» отказом не считается: два модератора, нажавших одно и
// то же, не должны видеть ошибку.
func TestПовторноеДействиеНеОшибка(t *testing.T) {
	h, mod, token := modServer(t, platform.RoleModerator)
	mod.fail = platform.ErrNothingToDo

	form := url.Values{"kind": {"comment"}, "id": {"700"}, "do": {"restore"}, "back": {"/mod"}}
	if w := do(h, postAs(t, "/mod/act", form, token)); w.Code != http.StatusSeeOther {
		t.Fatalf("код %d", w.Code)
	}
}

// Роли раздаёт только администратор: право скрывать чужие слова не должно
// размножаться само.
func TestРолиТолькоАдмин(t *testing.T) {
	h, mod, token := modServer(t, platform.RoleModerator)
	mod.users[modUserID] = platform.User{ID: modUserID, Nick: "Хатуль мадан"}

	form := url.Values{"do": {"role"}, "role": {"moderator"}}
	if w := do(h, postAs(t, "/mod/u/"+itoa64(modUserID), form, token)); w.Code != http.StatusForbidden {
		t.Fatalf("код %d, ожидался отказ", w.Code)
	}
	if len(mod.acts) != 0 {
		t.Fatalf("выполнено %v", mod.acts)
	}
}

func TestЗапретПишетСрок(t *testing.T) {
	h, mod, token := modServer(t, platform.RoleModerator)
	mod.users[1] = platform.User{ID: 1, Nick: "Пух"}

	form := url.Values{"do": {"ban"}, "days": {"7"}, "reason": {"реклама"}, "back": {"/mod"}}
	if w := do(h, postAs(t, "/mod/u/1", form, token)); w.Code != http.StatusSeeOther {
		t.Fatalf("код %d", w.Code)
	}
	if len(mod.acts) != 1 || mod.acts[0] != "ban" {
		t.Fatalf("позвано %v", mod.acts)
	}
}

// ---------------------------------------------------------------- жалоба

func TestЖалобаУчастника(t *testing.T) {
	h, mod, token := modServer(t, platform.RoleUser)

	page := do(h, as(guest(t, "GET", "/report?kind=comment&id=700&note=312811"), token))
	if page.Code != http.StatusOK {
		t.Fatalf("форма жалобы ответила %d", page.Code)
	}
	if !strings.Contains(page.Body.String(), "не вмешивается") {
		t.Error("форма не объясняет, что модерация не судит ссоры")
	}

	form := url.Values{"kind": {"comment"}, "id": {"700"}, "note": {"312811"}, "reason": {"реклама"}}
	w := do(h, postAs(t, "/report", form, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d", w.Code)
	}
	if len(mod.reported) != 1 || mod.reported[0].ID != 700 {
		t.Fatalf("жалоб %v", mod.reported)
	}
}

func TestГостьНеЖалуется(t *testing.T) {
	h, mod, _ := modServer(t, platform.RoleUser)

	if w := do(h, guest(t, "GET", "/report?kind=comment&id=700")); w.Code != http.StatusSeeOther {
		t.Fatalf("гостя с формы жалобы ведёт кодом %d", w.Code)
	}
	form := url.Values{"kind": {"comment"}, "id": {"700"}}
	if w := do(h, post(t, "/report", form)); w.Code == http.StatusSeeOther {
		t.Fatal("жалоба от гостя принята")
	}
	if len(mod.reported) != 0 {
		t.Fatal("жалоба от гостя записана")
	}
}

// Повторная жалоба на то же — не ошибка: человек просто не помнит, что уже
// жаловался, и отказ ему ничего не объясняет.
func TestПовторнаяЖалобаМолчит(t *testing.T) {
	h, mod, token := modServer(t, platform.RoleUser)
	mod.fail = platform.ErrNothingToDo

	form := url.Values{"kind": {"comment"}, "id": {"700"}, "note": {"312811"}}
	if w := do(h, postAs(t, "/report", form, token)); w.Code != http.StatusSeeOther {
		t.Fatalf("код %d", w.Code)
	}
}

// ---------------------------------------------------------------- пересмотр

// Скрытое своё показывается АВТОРУ с причиной и кнопкой: молча исчезнувшая
// реплика — худшее, что можно сделать с только что переехавшим сообществом.
func TestСвоёСкрытоеСПричиной(t *testing.T) {
	h, mod, token := modServer(t, platform.RoleUser)
	mod.mine = []platform.MyCheck{{
		Subject: platform.CommentSubject(700), NoteID: 312811, Body: "мой текст",
		Category: platform.CatSpam, Reason: "похоже на рекламу",
		HiddenAt: time.Now(), ByMachine: true,
	}}

	body := do(h, as(guest(t, "GET", "/me"), token)).Body.String()
	for _, want := range []string{"мой текст", "похоже на рекламу", "решение автомата", "/appeal"} {
		if !strings.Contains(body, want) {
			t.Errorf("на своей странице нет %q", want)
		}
	}

	form := url.Values{"kind": {"comment"}, "id": {"700"}}
	if w := do(h, postAs(t, "/appeal", form, token)); w.Code != http.StatusSeeOther {
		t.Fatalf("код %d", w.Code)
	}
	if len(mod.appealed) != 1 {
		t.Fatalf("просьб о пересмотре %d", len(mod.appealed))
	}
}

// Запрет писать показывается на своей же странице: выкинув человека из учётной
// записи, мы отняли бы у него ровно ту строку, где написано, за что и до когда.
func TestЗапретВиденСамомуЧеловеку(t *testing.T) {
	until := time.Now().Add(48 * time.Hour)
	auth, token := signedInAs(t, platform.User{
		ID: modUserID, Nick: "Пух", Kind: platform.KindMember,
		BannedUntil: &until, BanReason: "реклама",
	})
	h := newFullServer(t, noteStore(), auth, &fakeWriter{}, newFakeMod(), nil, Config{})

	body := do(h, as(guest(t, "GET", "/me"), token)).Body.String()
	if !strings.Contains(body, "Публикации вам запрещены") || !strings.Contains(body, "реклама") {
		t.Error("на своей странице не сказано ни про запрет, ни про причину")
	}
}

// Без модерации (mod == nil) морда обязана подниматься: страниц нет, кнопок
// нет, всё остальное работает.
func TestБезМодерацииСтраницНет(t *testing.T) {
	auth, token := signedInAs(t, platform.User{
		ID: modUserID, Nick: "Хатуль мадан", Kind: platform.KindMember, Role: platform.RoleAdmin,
	})
	h := newFullServer(t, noteStore(), auth, &fakeWriter{}, nil, nil, Config{})

	if w := do(h, as(guest(t, "GET", "/mod"), token)); w.Code != http.StatusNotFound {
		t.Fatalf("код %d", w.Code)
	}
	body := do(h, as(guest(t, "GET", "/n/312811"), token)).Body.String()
	if strings.Contains(body, `value="hide"`) {
		t.Error("кнопки модератора без модерации")
	}
	if w := do(h, as(guest(t, "GET", "/me"), token)); w.Code != http.StatusOK {
		t.Fatalf("своя страница ответила %d", w.Code)
	}
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
