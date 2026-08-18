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
	fail     error
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

// modServer — площадка с вошедшим человеком заданной роли и живой модерацией.
func modServer(t *testing.T, role platform.Role) (http.Handler, *fakeMod, string) {
	t.Helper()
	auth, token := signedInAs(t, platform.User{
		ID: modUserID, Nick: "Хатуль мадан", Kind: platform.KindMember, Role: role,
	})
	mod := newFakeMod()
	return newFullServer(t, noteStore(), auth, &fakeWriter{}, mod, nil, Config{}), mod, token
}

// signedInAs — вошедший человек с ОБОИМИ подписанными согласиями. Без них любая
// страница уводит на /consent: вход не доводится до конца, пока документы не
// подписаны, и это правило Ш4, а не мелочь теста.
func signedInAs(t *testing.T, u platform.User) (*fakeAuth, string) {
	t.Helper()
	ctx := context.Background()
	auth := newFakeAuth()
	auth.users[u.ID] = u
	for _, k := range []string{platform.ConsentProcessing, platform.ConsentDistribution} {
		if err := auth.GrantConsent(ctx, u.ID, k, 1, ""); err != nil {
			t.Fatal(err)
		}
	}
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
