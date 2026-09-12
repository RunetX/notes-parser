package web

// Проверки писем со стороны морды (эпик L, Ш2).
//
// Хранилище поддельное, как и везде в этом пакете, но поддельное НЕ пустое: оно
// вправду хранит переписку двоих, потому что главный вопрос этапа — доходит ли
// написанное в форме до собеседника и возвращается ли ему на страницу. Правила
// же «кому можно писать» живут в SQL и проверяются против настоящего Postgres
// (platform/mail_pg_test.go); здесь речь о том, что видит человек.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

const peerID = 1372959 // «Полынь-Трава» — второй участник в этих проверках

// fakeMail — переписка в памяти. Пара нормализуется, как в ядре, иначе два
// встречных письма завели бы две разные переписки и тест зеленел бы на том, чего
// в бою не бывает.
type fakeMail struct {
	letters map[int64][]platform.MessageView // переписка → письма в порядке разговора
	pair    map[[2]int64]int64               // нормализованная пара → номер переписки
	peers   map[int64]platform.Author
	next    int64
	// deny — чем ответит CanWriteTo и SendMessage. Пусто — можно писать.
	deny error
	// markedUpto — с какой границей пришла отметка прочитанного.
	markedUpto int64
	hidden     []int64
	unread     int
	// senders — кто написал какое письмо. Отдельно от самих писем, потому что
	// FromMe у вида это ответ КОНКРЕТНОМУ читателю, а не свойство письма.
	senders map[int64]int64
	// blocks — чёрный список, ключ НЕнормализованный: запрет односторонний, и
	// «я закрыл его» с «он закрыл меня» это разные строки. Нормализуй пару — и
	// тест зеленел бы на том, чего в ядре нет.
	blocks map[[2]int64]bool
	// reports — поданные жалобы: проверяется, что нажатие дошло до ядра, а не
	// что страница нарисовала кнопку.
	reports []fakeReport
}

type fakeReport struct {
	message  int64
	reporter int64
	reason   string
}

func newFakeMail() *fakeMail {
	return &fakeMail{
		letters: map[int64][]platform.MessageView{},
		pair:    map[[2]int64]int64{},
		senders: map[int64]int64{},
		blocks:  map[[2]int64]bool{},
		peers: map[int64]platform.Author{
			testProfileID: {ID: testProfileID, Nick: testNick},
			peerID:        {ID: peerID, Nick: "Полынь-Трава"},
		},
	}
}

func key(a, b int64) [2]int64 {
	if a > b {
		a, b = b, a
	}
	return [2]int64{a, b}
}

func (f *fakeMail) CanWriteTo(_ context.Context, me, peer int64) (int64, error) {
	if f.deny != nil {
		return 0, f.deny
	}
	if me == peer {
		return 0, platform.ErrSelfMessage
	}
	if err := f.blockedBetween(me, peer); err != nil {
		return 0, err
	}
	return f.pair[key(me, peer)], nil
}

// blockedBetween — обе стороны разом, и ответы РАЗНЫЕ: «вы закрыли» снимается
// своей же кнопкой, «вас закрыли» не снимается ничем, и говорить об этих двух
// одинаково значило бы обещать человеку кнопку, которой нет.
func (f *fakeMail) blockedBetween(me, peer int64) error {
	switch {
	case f.blocks[[2]int64{me, peer}]:
		return platform.ErrBlockedByYou
	case f.blocks[[2]int64{peer, me}]:
		return platform.ErrBlockedByPeer
	}
	return nil
}

func (f *fakeMail) SendMessage(_ context.Context, from, to int64, body string) (platform.MessageSent, error) {
	if f.deny != nil {
		return platform.MessageSent{}, f.deny
	}
	if strings.TrimSpace(body) == "" {
		return platform.MessageSent{}, platform.ErrEmptyBody
	}
	if err := f.blockedBetween(from, to); err != nil {
		return platform.MessageSent{}, err
	}
	k := key(from, to)
	id, first := f.pair[k], false
	if id == 0 {
		f.next++
		id, first = f.next, true
		f.pair[k] = id
	}
	f.next++
	f.letters[id] = append(f.letters[id], platform.MessageView{
		ID: f.next, FromMe: false, Body: body, SentAt: time.Now(),
	})
	f.senders[f.next] = from
	return platform.MessageSent{DialogID: id, MessageID: f.next, First: first}, nil
}

func (f *fakeMail) Dialogs(_ context.Context, me int64, offset, limit int) ([]platform.DialogView, error) {
	var out []platform.DialogView
	for k, id := range f.pair {
		peer := k[0]
		if peer == me {
			peer = k[1]
		}
		ls := f.letters[id]
		if len(ls) == 0 {
			continue
		}
		last := ls[len(ls)-1]
		out = append(out, platform.DialogView{
			DialogID: id, Peer: f.peers[peer], Unread: f.unread,
			LastAt: last.SentAt, Excerpt: last.Body, LastFromMe: f.senders[last.ID] == me,
		})
	}
	if offset >= len(out) {
		return nil, nil
	}
	return out[offset:min(offset+limit, len(out))], nil
}

func (f *fakeMail) CountDialogs(context.Context, int64) (int, error) { return len(f.pair), nil }

func (f *fakeMail) Dialog(_ context.Context, me, id int64, offset, limit int) (platform.DialogHead, []platform.MessageView, error) {
	ls, ok := f.letters[id]
	if !ok {
		return platform.DialogHead{}, nil, platform.ErrNotFound
	}
	var peer int64
	for k, did := range f.pair {
		if did != id {
			continue
		}
		if k[0] != me && k[1] != me {
			// Посторонний: ядро отвечает ему тем же, чем и на несуществующую
			// переписку, — существование чужой само по себе сведения.
			return platform.DialogHead{}, nil, platform.ErrNotFound
		}
		peer = k[0]
		if peer == me {
			peer = k[1]
		}
	}
	head := platform.DialogHead{DialogID: id, Peer: f.peers[peer], Total: len(ls), Unread: f.unread}
	if offset >= len(ls) {
		return head, nil, nil
	}
	page := make([]platform.MessageView, 0, limit)
	for _, l := range ls[offset:min(offset+limit, len(ls))] {
		l.FromMe = f.senders[l.ID] == me
		page = append(page, l)
	}
	return head, page, nil
}

func (f *fakeMail) MarkDialogRead(_ context.Context, _, _, upto int64) error {
	f.markedUpto = upto
	return nil
}

func (f *fakeMail) UnreadMail(context.Context, int64) (int, error) { return f.unread, nil }

func (f *fakeMail) HideDialog(_ context.Context, _, id int64) error {
	f.hidden = append(f.hidden, id)
	return nil
}

// party — сторона ли этот человек в переписке. У ядра это строка mail_sides, и
// подделка обязана спрашивать то же: жалоба на чужое письмо не должна проходить
// и в тесте.
func (f *fakeMail) party(dialogID, userID int64) bool {
	for k, id := range f.pair {
		if id == dialogID {
			return k[0] == userID || k[1] == userID
		}
	}
	return false
}

func (f *fakeMail) MailMessage(_ context.Context, me, id int64) (platform.MessageView, int64, error) {
	for did, ls := range f.letters {
		if !f.party(did, me) {
			continue
		}
		for _, l := range ls {
			if l.ID == id {
				l.FromMe = f.senders[l.ID] == me
				return l, did, nil
			}
		}
	}
	return platform.MessageView{}, 0, platform.ErrNotFound
}

func (f *fakeMail) ReportMessage(ctx context.Context, reporter, id int64, reason string) error {
	l, _, err := f.MailMessage(ctx, reporter, id)
	if err != nil {
		return err
	}
	if l.FromMe {
		return platform.ErrSelfReport
	}
	if l.Purged {
		return platform.ErrMessagePurged
	}
	f.reports = append(f.reports, fakeReport{message: id, reporter: reporter, reason: reason})
	return nil
}

func (f *fakeMail) MessagesSince(_ context.Context, me, dialogID, after int64, limit int) ([]platform.MessageView, error) {
	if !f.party(dialogID, me) {
		return nil, nil
	}
	var out []platform.MessageView
	for _, l := range f.letters[dialogID] {
		if l.ID <= after || len(out) >= limit {
			continue
		}
		l.FromMe = f.senders[l.ID] == me
		out = append(out, l)
	}
	return out, nil
}

func (f *fakeMail) BlockUser(_ context.Context, me, peer int64) error {
	if me == peer {
		return platform.ErrSelfMessage
	}
	f.blocks[[2]int64{me, peer}] = true
	return nil
}

func (f *fakeMail) UnblockUser(_ context.Context, me, peer int64) error {
	delete(f.blocks, [2]int64{me, peer})
	return nil
}

func (f *fakeMail) BlockedList(_ context.Context, me int64) ([]platform.Author, error) {
	var out []platform.Author
	for k := range f.blocks {
		if k[0] == me {
			out = append(out, f.peers[k[1]])
		}
	}
	return out, nil
}

// mailServer — сервер с перепиской и ДВУМЯ вошедшими: писать в одиночку
// бессмысленно, а вторая сессия нужна, чтобы прочесть письмо глазами адресата.
func mailServer(t *testing.T, m Mail) (http.Handler, string, string) {
	t.Helper()
	h, mine, theirs, _ := mailModServer(t, m, nil)
	return h, mine, theirs
}

// mailModServer — та же сборка, но с модерацией. Отдельным помощником, потому
// что БЕЗ модерации жалоб на письма не существует вовсе: читать их некому, и
// кнопка не рисуется — это проверяется своим тестом.
func mailModServer(t *testing.T, m Mail, mod Moderator) (http.Handler, string, string, *fakeAuth) {
	t.Helper()
	auth := newFakeAuth()
	auth.users[testProfileID] = platform.User{ID: testProfileID, Nick: testNick, Kind: platform.KindMember}
	auth.users[peerID] = platform.User{ID: peerID, Nick: "Полынь-Трава", Kind: platform.KindMember}
	mine, _, err := auth.CreateSession(context.Background(), testProfileID, "")
	if err != nil {
		t.Fatal(err)
	}
	theirs, _, err := auth.CreateSession(context.Background(), peerID, "")
	if err != nil {
		t.Fatal(err)
	}
	// Обязательные согласия подписаны у обоих: без них «Моя страница» уводит на
	// экран подписи, и раздел про письма на ней не увидеть вовсе.
	grantConsents(t, auth, testProfileID)
	grantConsents(t, auth, peerID)
	st := &fakeStore{profile: platform.Profile{
		ID: peerID, Nick: "Полынь-Трава", Kind: platform.KindMember, CreatedAt: time.Now()}}
	srv := New(Config{BaseURL: "http://127.0.0.1", Log: quietLog()}, st, auth, nil, mod, nil)
	t.Cleanup(func() { _ = srv.Close() })
	if m != nil {
		srv.SetMail(m)
	}
	return srv.routes(), mine, theirs, auth
}

// ГЛАВНАЯ проверка гейта: пока переписка выключена, писем нет ни у кого и ничем.
// Не «страница есть, но пустая» и не «войдите» — страницы нет вовсе, потому что
// принимать чужие письма площадка до Ш0 не вправе.
func TestПисемНетПокаГейтВыключен(t *testing.T) {
	h, mine, _ := mailServer(t, nil)
	for _, target := range []string{"/mail", "/mail/new?to=1372959", "/mail/consent", "/mail/7"} {
		if w := do(h, as(guest(t, "GET", target), mine)); w.Code != http.StatusNotFound {
			t.Errorf("%s при выключенной переписке ответил %d, ожидалось 404", target, w.Code)
		}
	}
	// Гостю — ТОТ ЖЕ ответ, а не «войдите»: за дверью страницы всё равно нет.
	if w := do(h, guest(t, "GET", "/mail")); w.Code != http.StatusNotFound {
		t.Errorf("гостю ответили %d, ожидалось 404", w.Code)
	}
	if body := do(h, as(guest(t, "GET", "/"), mine)).Body.String(); strings.Contains(body, `href="/mail"`) {
		t.Error("пункт меню «Письма» стои́т при выключенной переписке")
	}
	if body := do(h, as(guest(t, "GET", "/u/1372959"), mine)).Body.String(); strings.Contains(body, "Написать") {
		t.Error("кнопка «Написать» стои́т при выключенной переписке")
	}
}

// Двое переписываются: первое письмо заводит переписку, ответ приходит в неё же,
// и каждый видит у себя оба письма. Проверка НА ПУТИ ДАННЫХ — текст ищется в
// разметке, а не в подделке.
func TestДвоеПереписываются(t *testing.T) {
	h, mine, theirs := mailServer(t, newFakeMail())

	w := do(h, postAs(t, "/mail/new", url.Values{
		"to": {"1372959"}, "body": {"здравствуйте, помните тот тред про гараж"}}, mine))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("отправка ответила %d: %s", w.Code, w.Body.String())
	}
	where := w.Header().Get("Location")
	if !strings.HasPrefix(where, "/mail/") {
		t.Fatalf("после отправки ведут на %q, ожидалась переписка", where)
	}

	// Адресат видит письмо у себя.
	body := do(h, as(guest(t, "GET", where), theirs)).Body.String()
	if !strings.Contains(body, "помните тот тред про гараж") {
		t.Fatal("письма нет на странице адресата")
	}
	if strings.Contains(body, `class="ltwho">Вы`) {
		t.Error("чужое письмо подписано «Вы» у адресата")
	}

	// Ответ в ту же переписку.
	if w := do(h, postAs(t, where, url.Values{"body": {"помню, и полку помню"}}, theirs)); w.Code != http.StatusSeeOther {
		t.Fatalf("ответ отвергнут: %d %s", w.Code, w.Body.String())
	}
	body = do(h, as(guest(t, "GET", where), mine)).Body.String()
	for _, want := range []string{"помните тот тред про гараж", "помню, и полку помню"} {
		if !strings.Contains(body, want) {
			t.Errorf("в переписке нет %q", want)
		}
	}
	// Своё письмо подписано, чужое — нет: собеседник назван в шапке, и подпись
	// над каждой строкой повторяла бы его полсотни раз.
	if !strings.Contains(body, `class="ltwho">Вы`) {
		t.Error("своё письмо не подписано «Вы»")
	}
	// И переписка видна в списке.
	if list := do(h, as(guest(t, "GET", "/mail"), mine)).Body.String(); !strings.Contains(list, "Полынь-Трава") {
		t.Error("переписки нет в списке писем")
	}
}

// РАЗМЕТКА И СМАЙЛЫ В ПИСЬМЕ РАЗБИРАЮТСЯ. Тест не про красоту: письма лежат в
// СВОЕЙ последовательности с единицы, то есть по номеру попадают в полосу НГС, и
// общий eraOf счёл бы их чужим текстом — человек нажал бы кнопку смайла под
// формой и получил «:::popcorn:::». Снятие mailEra роняет эту проверку.
func TestПисьмоРазбираетРазметкуИСмайлы(t *testing.T) {
	m := newFakeMail()
	h, mine, _ := mailServer(t, m)
	if w := do(h, postAs(t, "/mail/new", url.Values{
		"to": {"1372959"}, "body": {"[b]точно[/b] :::popcorn:::"}}, mine)); w.Code != http.StatusSeeOther {
		t.Fatalf("отправка ответила %d", w.Code)
	}
	body := do(h, as(guest(t, "GET", "/mail/1"), mine)).Body.String()
	if !strings.Contains(body, "<b>точно</b>") {
		t.Error("разметка в письме не разобрана — письмо сочли чужим текстом по полосе id")
	}
	// Искать надо КАРТИНКУ, а не отсутствие кода: код остаётся в alt (там он и
	// нужен — читалке и тому, у кого картинки выключены), и «нет подстроки
	// :::popcorn:::» провалилось бы на верном коде.
	if !strings.Contains(body, `src="/assets/smile/popcorn.`) {
		t.Error("смайл в письме остался кодом — письмо сочли чужим текстом по полосе id")
	}
}

// Чужая переписка и несуществующая отвечаются ОДИНАКОВО: существование чужой —
// само по себе сведения.
func TestЧужаяПерепискаНеОтдаётся(t *testing.T) {
	m := newFakeMail()
	h, mine, _ := mailServer(t, m)
	if _, err := m.SendMessage(context.Background(), 777, 888, "не для вас"); err != nil {
		t.Fatal(err)
	}
	got := do(h, as(guest(t, "GET", "/mail/1"), mine)).Code
	missing := do(h, as(guest(t, "GET", "/mail/99"), mine)).Code
	if got != http.StatusNotFound || missing != got {
		t.Errorf("чужая переписка %d, несуществующая %d — ответы обязаны совпадать и быть 404", got, missing)
	}
}

// Не подписан документ — ведём К НЕМУ, а не прячем кнопку: спрятанная кнопка
// ничего не объясняет.
func TestБезПодписиВедутКДокументу(t *testing.T) {
	m := newFakeMail()
	m.deny = platform.ErrNoTalkConsent
	h, mine, _ := mailServer(t, m)

	w := do(h, as(guest(t, "GET", "/mail/new?to=1372959"), mine))
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/mail/consent") {
		t.Errorf("без подписи ответили %d → %q", w.Code, w.Header().Get("Location"))
	}
	// И кнопка на странице участника ведёт туда же, а не пропадает.
	body := do(h, as(guest(t, "GET", "/u/1372959"), mine)).Body.String()
	if !strings.Contains(body, `href="/mail/consent?to=1372959"`) {
		t.Error("кнопка «Написать» не ведёт к документу")
	}
}

// Отказ не теряет набранного: письмо, пропавшее вместе со страницей отказа, —
// худшее, что можно сделать с тем, кто его только что написал.
func TestОтказНеТеряетНабранного(t *testing.T) {
	m := newFakeMail()
	m.deny = platform.ErrBlockedByPeer
	h, mine, _ := mailServer(t, m)
	w := do(h, postAs(t, "/mail/new", url.Values{
		"to": {"1372959"}, "body": {"длинное письмо, которое жалко потерять"}}, mine))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("отказ ответил %d, ожидалось 422", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "длинное письмо, которое жалко потерять") {
		t.Error("набранный текст не вернулся в форму")
	}
	// Закрытому говорят ПРЯМО (решение владельца 11.09.2026), а не делают вид,
	// что письмо ушло.
	if !strings.Contains(body, "закрыл от вас переписку") {
		t.Error("причина отказа не названа")
	}
}

// Кнопка ведёт в уже заведённую переписку, а не на второй экран того же
// разговора.
func TestКнопкаВедётВСуществующуюПереписку(t *testing.T) {
	m := newFakeMail()
	h, mine, _ := mailServer(t, m)
	if _, err := m.SendMessage(context.Background(), testProfileID, peerID, "первое"); err != nil {
		t.Fatal(err)
	}
	body := do(h, as(guest(t, "GET", "/u/1372959"), mine)).Body.String()
	if !strings.Contains(body, `href="/mail/1"`) {
		t.Error("кнопка «Написать» не ведёт в заведённую переписку")
	}
	if w := do(h, as(guest(t, "GET", "/mail/new?to=1372959"), mine)); w.Header().Get("Location") != "/mail/1" {
		t.Errorf("форма первого письма не увела в переписку: %q", w.Header().Get("Location"))
	}
}

// Отметка прочитанного гасит то, что человек ВИДЕЛ, а не всё подряд.
func TestОтметкаПрочитанногоБерётГраницуСтраницы(t *testing.T) {
	m := newFakeMail()
	m.unread = 2
	h, mine, _ := mailServer(t, m)
	if _, err := m.SendMessage(context.Background(), peerID, testProfileID, "письмо"); err != nil {
		t.Fatal(err)
	}
	body := do(h, as(guest(t, "GET", "/mail/1"), mine)).Body.String()
	if !strings.Contains(body, `name="upto" value="2"`) {
		t.Fatal("граница отметки прочитанного не равна самому свежему письму страницы")
	}
	if w := do(h, postAs(t, "/mail/1/read", url.Values{"upto": {"2"}, "back": {"/mail/1"}}, mine)); w.Code != http.StatusSeeOther {
		t.Fatalf("отметка ответила %d", w.Code)
	}
	if m.markedUpto != 2 {
		t.Errorf("ядру велено гасить до %d, ожидалось 2", m.markedUpto)
	}
}

// У КАЖДОЙ КНОПКИ НАД РАЗГОВОРОМ ТРИ ЧАСТИ: значок, подпись и title.
//
// Тест держит их ВМЕСТЕ, потому что порознь каждая часть бессмысленна и
// по-своему вредна. Без значка на телефоне подпись не спрятать — прятать станет
// нечего, и три слова снова уедут под аватар отдельной кучкой (просьба владельца
// 12.09.2026). Без подписи в .lbl прятать её пришлось бы display:none, то есть
// у кнопки пропало бы имя для читалки: она объявила бы «кнопка». Без title
// значок остаётся неопознанным знаком — ровно то, что запрещает правило Ш5з про
// собственные метки площадки.
//
// Проверяется РАЗМЕТКА, а не вид: какая из двух частей видна, решает ширина
// экрана, которой у сервера нет. Что показ переключается на 700px — дело CSS и
// закрыто своими тестами в fidelity_test.
func TestКнопкиНадРазговоромНесутЗначокПодписьИЗаголовок(t *testing.T) {
	m := newFakeMail()
	m.unread = 1
	h, mine, _ := mailServer(t, m)
	if _, err := m.SendMessage(context.Background(), peerID, testProfileID, "письмо"); err != nil {
		t.Fatal(err)
	}
	body := do(h, as(guest(t, "GET", "/mail/1"), mine)).Body.String()

	for _, want := range []string{"Отметить прочитанным", "Убрать у себя", "Закрыть переписку"} {
		if !strings.Contains(body, `<span class="lbl">`+want+`</span>`) {
			t.Errorf("у кнопки %q нет подписи в .lbl — на телефоне её нечем спрятать от глаза и незачем показывать читалке", want)
		}
	}
	// Значков ровно столько же, сколько кнопок: полоска не бывает наполовину
	// знаковой — смешанный ряд «слово рядом со значком» читается как поломка.
	//
	// Считаем ВНУТРИ полоски, а не по всей странице: значки стоят ещё в шапке и у
	// каждого письма, и проверка «их не меньше трёх» зеленела бы на полоске вовсе
	// без значков. Вырезать ровно её позволяет то, что внутри .dlgacts лежат одни
	// формы и ни одного вложенного div.
	strip := body[strings.Index(body, `class="dlgacts"`):]
	strip = strip[:strings.Index(strip, "</div>")]
	if n := strings.Count(strip, `<svg class="ico"`); n != 3 {
		t.Errorf("значков в полоске %d, кнопок 3", n)
	}
	for _, want := range []string{
		`title="Отметить прочитанным по последнее письмо этой страницы"`,
		`title="Убрать переписку из своего списка`,
		`title="Закрыть переписку:`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("нет заголовка %s — на телефоне значок остаётся неопознанным", want)
		}
	}

	// Закрытая переписка меняет ОДНУ кнопку из трёх, и меняет целиком: замок на
	// открытый замок, «Закрыть» на «Снять запрет». Мёртвой кнопки «Закрыть» у
	// закрытой переписки не бывает.
	if w := do(h, postAs(t, "/mail/block", url.Values{"to": {"1372959"}, "back": {"/mail/1"}}, mine)); w.Code != http.StatusSeeOther {
		t.Fatalf("закрытие ответило %d", w.Code)
	}
	body = do(h, as(guest(t, "GET", "/mail/1"), mine)).Body.String()
	if !strings.Contains(body, `<span class="lbl">Снять запрет</span>`) {
		t.Error("у закрытой переписки нет кнопки «Снять запрет» с подписью")
	}
	if strings.Contains(body, `<span class="lbl">Закрыть переписку</span>`) {
		t.Error("у закрытой переписки осталась мёртвая кнопка «Закрыть переписку»")
	}
}

// Формы переписки — это согласие, а не настройка экрана: без CSRF они не
// работают.
func TestФормыПисемТребуютCSRF(t *testing.T) {
	h, mine, _ := mailServer(t, newFakeMail())
	r := as(post(t, "/mail/new", url.Values{"to": {"1372959"}, "body": {"без токена"}}), mine)
	if w := do(h, r); w.Code == http.StatusSeeOther {
		t.Error("письмо ушло без CSRF")
	}
}

// Письма закрыты от поисковиков — и заголовком, и robots.txt: разъехавшись, эти
// двое дали бы раздел, закрытый в одном месте и открытый в другом.
func TestПисьмаЗакрытыОтРоботов(t *testing.T) {
	h, mine, _ := mailServer(t, newFakeMail())
	if got := do(h, as(guest(t, "GET", "/mail"), mine)).Header().Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Errorf("X-Robots-Tag у писем: %q", got)
	}
	if body := do(h, guest(t, "GET", "/robots.txt")).Body.String(); !strings.Contains(body, "Disallow: /mail") {
		t.Errorf("robots.txt не закрывает письма:\n%s", body)
	}
}

// Повод «вам написали» ведёт В ПЕРЕПИСКУ. Ветка про письмо обязана стоять
// ПЕРВОЙ: заметки у него нет вовсе, и следующая строка увела бы человека на
// «Мою страницу», причём молча.
func TestПоводОПисьмеВедётВПереписку(t *testing.T) {
	n := platform.NotificationView{Reason: platform.ReasonMessage, Kind: platform.EventMessage, DialogID: 42}
	if got := eventURL(n); got != "/mail/42" {
		t.Errorf("повод о письме ведёт на %q", got)
	}
	if got := eventTitle(n); got != "Вам написали" {
		t.Errorf("заголовок повода: %q", got)
	}
}

// Пункт меню несёт число непрочитанных писем, а пятого значка в шапке не
// заводится: он уже не помещался в строку на телефоне.
func TestПунктМенюСчитаетПисьма(t *testing.T) {
	m := newFakeMail()
	m.unread = 3
	h, mine, _ := mailServer(t, m)
	body := do(h, as(guest(t, "GET", "/"), mine)).Body.String()
	if !strings.Contains(body, `href="/mail"`) {
		t.Fatal("пункта меню «Письма» нет")
	}
	if !strings.Contains(body, `class="mn">3</span>`) {
		t.Error("число писем не показано у пункта меню")
	}
}

// Экран согласия показывает САМ ДОКУМЕНТ до кнопки, а подпись уводит туда, куда
// человек шёл. Проверка заодно стережёт, что четвёртый текст вообще есть в
// бинарнике: пропади он — переписка молча осталась бы без основания.
func TestЭкранСогласияПоказываетДокумент(t *testing.T) {
	m := newFakeMail()
	m.deny = platform.ErrNoTalkConsent
	h, mine, _ := mailServer(t, m)

	w := do(h, as(guest(t, "GET", "/mail/consent?to=1372959"), mine))
	if w.Code != http.StatusOK {
		t.Fatalf("экран согласия ответил %d", w.Code)
	}
	body := w.Body.String()
	// Ищем то, ради чего документ и написан: срок хранения и статью, которая
	// его устанавливает. Пропади текст из бинарника — здесь и станет видно.
	if !strings.Contains(body, "шесть месяцев") || !strings.Contains(body, "10.1") {
		t.Error("на экране нет самого документа: ни срока хранения, ни статьи")
	}
	if !strings.Contains(body, `name="to" value="1372959"`) {
		t.Error("после подписи человека не вернут к тому, кому он шёл писать")
	}

	w = do(h, postAs(t, "/mail/consent", url.Values{"to": {"1372959"}}, mine))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/mail/new?to=1372959" {
		t.Errorf("после подписи ответили %d → %q", w.Code, w.Header().Get("Location"))
	}
}

// Атрибуты картинки в списке переписок обязаны совпадать с показом: место под
// фотографию браузер держит по ним, и разойдись они с CSS — список прыгал бы на
// каждой загруженной фотографии. Ровно это правило уже стережёт полосу лиц.
func TestЛицоВСпискеПисемНеПрыгает(t *testing.T) {
	m := newFakeMail()
	h, mine, _ := mailServer(t, m)
	if _, err := m.SendMessage(context.Background(), peerID, testProfileID, "письмо"); err != nil {
		t.Fatal(err)
	}
	body := do(h, as(guest(t, "GET", "/mail"), mine)).Body.String()
	if !strings.Contains(body, `width="48" height="48"`) {
		t.Error("у лица в списке писем нет атрибутов размера или они не 48")
	}
	rule := cssRule(t, cssText(t), ".dlgava .ava {")
	if !strings.Contains(rule, "width: 48px") || !strings.Contains(rule, "height: 48px") {
		t.Errorf("показ разошёлся с атрибутами: %s", rule)
	}
}

// Выдержка в списке — ПЛОСКИЙ текст: она приезжает обрезанной, и разбирать в ней
// разметку значит рисовать то, чего в письме нет. Знаки снимаются, как в
// заголовке вкладки.
func TestВыдержкаВСпискеБезЗнаковРазметки(t *testing.T) {
	m := newFakeMail()
	h, mine, _ := mailServer(t, m)
	if _, err := m.SendMessage(context.Background(), peerID, testProfileID, "помню [b]отлично[/b] :::agree:::"); err != nil {
		t.Fatal(err)
	}
	body := do(h, as(guest(t, "GET", "/mail"), mine)).Body.String()
	i := strings.Index(body, `class="dlgx"`)
	if i < 0 {
		t.Fatal("выдержки нет вовсе")
	}
	line := body[i:min(i+200, len(body))]
	if strings.Contains(line, "[b]") || strings.Contains(line, ":::agree:::") {
		t.Errorf("в выдержке остались знаки разметки: %s", line)
	}
	if !strings.Contains(line, "помню") || !strings.Contains(line, "отлично") {
		t.Errorf("выдержка потеряла сам текст: %s", line)
	}
}

// ------------------------------------------------------------------ Ш3

// ЗАКРЫТОМУ ГОВОРЯТ ПРЯМО — главная проверка этапа (решение владельца
// 11.09.2026). Молчаливая блокировка копит у отправителя переписку, которой
// никто не читает; здесь проверяется, что причина названа в обоих местах, где
// человек её ищет: на странице участника (вместо кнопки) и в отказе отправки.
func TestЗакрытыйПолучаетНазваннуюПричину(t *testing.T) {
	m := newFakeMail()
	h, mine, theirs := mailServer(t, m)

	// Переписка заводится ДО запрета: закрывают обычно того, кто уже написал.
	if _, err := m.SendMessage(context.Background(), testProfileID, peerID, "письмо"); err != nil {
		t.Fatal(err)
	}
	// Собеседник закрывает переписку кнопкой со своей стороны.
	w := do(h, postAs(t, "/mail/block",
		url.Values{"to": {strconv.FormatInt(testProfileID, 10)}, "back": {"/mail"}}, theirs))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/mail" {
		t.Fatalf("закрытие ответило %d → %q", w.Code, w.Header().Get("Location"))
	}
	if !m.blocks[[2]int64{peerID, testProfileID}] {
		t.Fatal("запрет не дошёл до ядра")
	}

	// Закрытый видит ПРИЧИНУ на странице собеседника, а не пустое место и не
	// кнопку, которая ответит отказом.
	body := do(h, as(guest(t, "GET", "/u/1372959"), mine)).Body.String()
	if !strings.Contains(body, "закрыл от вас переписку") {
		t.Error("закрытому не названа причина на странице участника")
	}
	if strings.Contains(body, `href="/mail/new?to=`) {
		t.Error("закрытому нарисована кнопка «Написать»")
	}
	// И НИКАКОЙ кнопки снятия: чужой запрет не снимается ничем, а кнопка,
	// обещающая обратное, — это ложь, за которую нажавший заплатит надеждой.
	if strings.Contains(body, `action="/mail/unblock"`) {
		t.Error("закрытому предложено снять ЧУЖОЙ запрет")
	}
	// И в отказе отправки — той же причиной, а не «не получилось».
	w = do(h, postAs(t, "/mail/new", url.Values{"to": {"1372959"}, "body": {"пусти"}}, mine))
	if !strings.Contains(w.Body.String(), "закрыл от вас переписку") {
		t.Errorf("отказ отправки не называет причину: %s", w.Body.String())
	}

	// А ТОТ, КТО ЗАКРЫЛ, видит у себя кнопку снятия — и не видит второй кнопки
	// «Закрыть», которая ничего бы не сделала.
	body = do(h, as(guest(t, "GET", "/mail/1"), theirs)).Body.String()
	if !strings.Contains(body, `action="/mail/unblock"`) {
		t.Error("у закрывшего нет кнопки «Снять запрет»")
	}
	if strings.Contains(body, `action="/mail/block"`) {
		t.Error("у закрытой переписки осталась кнопка «Закрыть»: она мёртвая")
	}
}

// Снять запрет можно со своей страницы — и это единственное место, куда за этим
// идти: закрытая переписка ушла из списка писем, а строка на странице закрытого
// отсылает сюда же.
func TestЗакрытыеПеречисленыНаСвоейСтранице(t *testing.T) {
	m := newFakeMail()
	h, mine, _ := mailServer(t, m)
	if err := m.BlockUser(context.Background(), testProfileID, peerID); err != nil {
		t.Fatal(err)
	}
	body := do(h, as(guest(t, "GET", "/me/settings"), mine)).Body.String()
	if !strings.Contains(body, "Закрытая переписка") || !strings.Contains(body, "Полынь-Трава") {
		t.Fatal("на своей странице нет закрытых")
	}
	if !strings.Contains(body, `value="1372959"`) {
		t.Error("в форме снятия нет номера закрытого")
	}
	w := do(h, postAs(t, "/mail/unblock", url.Values{"to": {"1372959"}, "back": {"/me/settings"}}, mine))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/me/settings" {
		t.Fatalf("снятие ответило %d → %q", w.Code, w.Header().Get("Location"))
	}
	if m.blocks[[2]int64{testProfileID, peerID}] {
		t.Fatal("запрет остался после снятия")
	}
	if body = do(h, as(guest(t, "GET", "/me/settings"), mine)).Body.String(); strings.Contains(body, "Закрытая переписка") {
		t.Error("раздел остался при пустом списке")
	}
}

// Пустой чёрный список раздела не рисует, а выключенная переписка не спрашивает
// его вовсе: заголовок над пустотой отвечает на вопрос, которого не задавали.
func TestБезПерепискиЧёрногоСпискаНет(t *testing.T) {
	h, mine, _ := mailServer(t, nil)
	body := do(h, as(guest(t, "GET", "/me/settings"), mine)).Body.String()
	if strings.Contains(body, "Закрытая переписка") {
		t.Error("раздел чёрного списка стои́т при выключенной переписке")
	}
}

// ОТКАЗ ПО ЧАСТОТЕ НАЗЫВАЕТ СРОК И ГОВОРИТ О ПИСЬМАХ.
//
// Две половины, и обе оплачены: «подождите немного» при часовом правиле
// означало одиннадцать минут (жалоба 06.09.2026), а общий текст отказа называл
// письмо ПУБЛИКАЦИЕЙ — то есть говорил про заметки там, где считались письма.
func TestОтказПоЧастотеПисемНазываетМинутыИПисьма(t *testing.T) {
	m := newFakeMail()
	m.deny = &platform.RateLimited{
		Window: time.Hour, Max: platform.MessagesPerHour,
		RetryAt: time.Now().Add(11 * time.Minute),
	}
	h, mine, _ := mailServer(t, m)
	body := do(h, postAs(t, "/mail/new", url.Values{"to": {"1372959"}, "body": {"письмо"}}, mine)).Body.String()
	switch {
	case strings.Contains(body, "Подождите немного"):
		t.Error("отказ по частоте остался расплывчатым")
	case !strings.Contains(body, "11 минут"):
		t.Errorf("отказ не называет срок: %s", body)
	case !strings.Contains(body, "писем"):
		t.Errorf("отказ не говорит о письмах: %s", body)
	case strings.Contains(body, "публикаци"):
		t.Errorf("отказ называет письмо публикацией: %s", body)
	}
	// Набранное при этом остаётся в форме — иначе отказ означает пропажу
	// письма, которое человек только что написал.
	if !strings.Contains(body, "письмо</textarea>") {
		t.Error("отказ по частоте потерял набранное")
	}
}

// Справка о письмах гаснет ВМЕСТЕ с перепиской: тема, зовущая в /mail, который
// отвечает «нет такой страницы», отправляет человека в никуда. Тот же гейт, что
// у тем про мессенджеры и про сбор пожертвований.
func TestСправкаОПисьмахГаснетСГейтом(t *testing.T) {
	h, _, _ := mailServer(t, nil)
	if w := do(h, guest(t, "GET", "/help/mail")); w.Code != http.StatusNotFound {
		t.Errorf("/help/mail при выключенной переписке ответил %d", w.Code)
	}
	if body := do(h, guest(t, "GET", "/help")).Body.String(); strings.Contains(body, "/help/mail") {
		t.Error("оглавление ведёт на тему, которой нет")
	}
	h, _, _ = mailServer(t, newFakeMail())
	if w := do(h, guest(t, "GET", "/help/mail")); w.Code != http.StatusOK {
		t.Errorf("/help/mail при включённой переписке ответил %d", w.Code)
	}
}

// СВОЙ запрет снимается там же, где о нём сказано. Кнопка стои́т на странице
// участника рядом с причиной — иначе строка отсылала бы на «Мою страницу» за
// кнопкой, которую можно поставить сюда.
func TestСвойЗапретСнимаетсяСоСтраницыУчастника(t *testing.T) {
	m := newFakeMail()
	h, mine, _ := mailServer(t, m)
	if err := m.BlockUser(context.Background(), testProfileID, peerID); err != nil {
		t.Fatal(err)
	}
	body := do(h, as(guest(t, "GET", "/u/1372959"), mine)).Body.String()
	if !strings.Contains(body, `action="/mail/unblock"`) {
		t.Fatal("на странице закрытого нет кнопки снятия")
	}
	if !strings.Contains(body, `value="/u/1372959"`) {
		t.Error("после снятия человека не вернут туда, где он нажал")
	}
	// Место снятия в самом тексте НЕ называется: кнопка рядом, и назови текст
	// другую страницу — он спорил бы с ней.
	if strings.Contains(body, "на своей странице") {
		t.Error("причина отсылает на другую страницу, хотя кнопка стои́т рядом")
	}
}

// ------------------------------------------------------------------ Ш4

// Жалоба на письмо: кнопка стои́т под ЧУЖИМ письмом, форма показывает ровно то,
// что уйдёт модератору, и нажатие доходит до ядра.
func TestЖалобаНаПисьмоНесётОдноПисьмо(t *testing.T) {
	m := newFakeMail()
	h, mine, _, _ := mailModServer(t, m, newFakeMod())
	ctx := context.Background()
	// Номер письма берётся У ЯДРА, а не выдумывается: у писем своя
	// последовательность, и «первое письмо — это единица» однажды окажется
	// неправдой (номера начинаются с переписки).
	bad, err := m.SendMessage(ctx, peerID, testProfileID, "отдай телефон, иначе")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.SendMessage(ctx, testProfileID, peerID, "мой ответ"); err != nil {
		t.Fatal(err)
	}
	letter := strconv.FormatInt(bad.MessageID, 10)
	body := do(h, as(guest(t, "GET", "/mail/1"), mine)).Body.String()
	// Кнопка ровно одна: под чужим письмом. На своё жаловаться незачем, и ядро
	// ответило бы отказом, а площадка мёртвых кнопок не печатает.
	if n := strings.Count(body, `href="/mail/report?m=`); n != 1 {
		t.Fatalf("кнопок «Пожаловаться» %d, ожидалась одна (под чужим письмом)", n)
	}

	form := do(h, as(guest(t, "GET", "/mail/report?m="+letter), mine))
	if form.Code != http.StatusOK {
		t.Fatalf("форма жалобы ответила %d", form.Code)
	}
	fb := form.Body.String()
	switch {
	case !strings.Contains(fb, "отдай телефон"):
		t.Error("на форме не показано письмо, на которое жалуются")
	case strings.Contains(fb, "мой ответ"):
		t.Error("на форме показано СОСЕДНЕЕ письмо: уходить должно одно")
	case !strings.Contains(fb, "только это письмо"):
		t.Error("форма не говорит, что уйдёт одно письмо")
	}

	w := do(h, postAs(t, "/mail/report",
		url.Values{"m": {letter}, "reason": {"угрожает"}, "back": {"/mail/1"}}, mine))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/mail/1" {
		t.Fatalf("жалоба ответила %d → %q", w.Code, w.Header().Get("Location"))
	}
	if len(m.reports) != 1 || m.reports[0].message != bad.MessageID || m.reports[0].reason != "угрожает" {
		t.Fatalf("до ядра дошло: %+v", m.reports)
	}
}

// На СВОЁ письмо жаловаться нечем и незачем: ни кнопки, ни прохода по прямому
// адресу — ядро ответило бы ErrSelfReport, и морда говорит то же словами.
func TestНаСвоёПисьмоЖалобыНет(t *testing.T) {
	m := newFakeMail()
	h, mine, _, _ := mailModServer(t, m, newFakeMod())
	sent, err := m.SendMessage(context.Background(), testProfileID, peerID, "моё письмо")
	if err != nil {
		t.Fatal(err)
	}
	mine2 := strconv.FormatInt(sent.MessageID, 10)
	if w := do(h, as(guest(t, "GET", "/mail/report?m="+mine2), mine)); w.Code != http.StatusForbidden {
		t.Errorf("форма жалобы на своё письмо ответила %d", w.Code)
	}
	if w := do(h, postAs(t, "/mail/report", url.Values{"m": {mine2}}, mine)); w.Code == http.StatusSeeOther {
		t.Error("жалоба на своё письмо прошла")
	}
}

// ЧУЖОЕ письмо не показывается и не обжалуется: посторонний получает тот же
// ответ, что и на несуществующее, — существование чужого письма само по себе
// сведения.
func TestНаЧужоеПисьмоПожаловатьсяНельзя(t *testing.T) {
	m := newFakeMail()
	ctx := context.Background()
	sent, err := m.SendMessage(ctx, peerID, testProfileID, "письмо двоих")
	if err != nil {
		t.Fatal(err)
	}
	// Третий: сессии у него в этой сборке нет, поэтому проверяется сам вызов
	// ядра-подделки — она повторяет правило mail_sides.
	if _, _, err := m.MailMessage(ctx, 606064, sent.MessageID); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("постороннему отдали письмо: %v", err)
	}
	if err := m.ReportMessage(ctx, 606064, sent.MessageID, "почитал чужое"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("посторонний пожаловался на чужое письмо: %v", err)
	}
}

// Без модерации кнопки нет вовсе: жалобу некому читать, а кнопка, ведущая на
// страницу «жалобы сейчас не принимаются», хуже отсутствующей.
func TestБезМодерацииЖалобыНаПисьмаНет(t *testing.T) {
	m := newFakeMail()
	h, mine, _ := mailServer(t, m) // mailServer поднимает сервер БЕЗ модерации
	sent, err := m.SendMessage(context.Background(), peerID, testProfileID, "письмо")
	if err != nil {
		t.Fatal(err)
	}
	body := do(h, as(guest(t, "GET", "/mail/1"), mine)).Body.String()
	if strings.Contains(body, "/mail/report") {
		t.Error("кнопка жалобы стои́т без модерации")
	}
	if w := do(h, as(guest(t, "GET", "/mail/report?m="+strconv.FormatInt(sent.MessageID, 10)), mine)); w.Code != http.StatusServiceUnavailable {
		t.Errorf("форма жалобы без модерации ответила %d", w.Code)
	}
}

// ------------------------------------------------------------------ Ш5

// Живой добор переписки: страница отдаёт ГОТОВУЮ разметку письма тем же
// шаблоном, что и сама переписка, и двигает границу.
func TestДоборПриноситПисьмоРазметкой(t *testing.T) {
	m := newFakeMail()
	h, mine, _ := mailServer(t, m)
	ctx := context.Background()
	first, err := m.SendMessage(ctx, peerID, testProfileID, "первое")
	if err != nil {
		t.Fatal(err)
	}
	// Страница переписки несёт границу добора — номер самого свежего письма.
	page := do(h, as(guest(t, "GET", "/mail/1"), mine)).Body.String()
	want := `data-fresh="` + strconv.FormatInt(first.MessageID, 10) + `"`
	if !strings.Contains(page, want) {
		t.Fatalf("на странице нет границы добора (%s)", want)
	}
	if !strings.Contains(page, `data-fresh-url="/mail/1/fresh"`) {
		t.Error("на странице нет адреса добора")
	}

	second, err := m.SendMessage(ctx, peerID, testProfileID, "второе, [b]жирным[/b]")
	if err != nil {
		t.Fatal(err)
	}
	w := do(h, as(guest(t, "GET",
		"/mail/1/fresh?after="+strconv.FormatInt(first.MessageID, 10)), mine))
	if w.Code != http.StatusOK {
		t.Fatalf("добор ответил %d", w.Code)
	}
	body := w.Body.String()
	switch {
	case !strings.Contains(body, "<b>жирным</b>"):
		t.Errorf("письмо приехало без разбора разметки: %s", body)
	case strings.Contains(body, "первое"):
		t.Error("добор принёс то, что на странице уже стои́т")
	case !strings.Contains(body, `id="l`+strconv.FormatInt(second.MessageID, 10)+`"`):
		t.Errorf("у письма нет номера в разметке: %s", body)
	}
	if got := w.Header().Get("X-Fresh-After"); got != strconv.FormatInt(second.MessageID, 10) {
		t.Errorf("граница после добора %q", got)
	}
	// Счётчика у переписки нет вовсе, и заголовка с числом быть не должно:
	// страница применила бы его к чужому числу в шапке.
	if got := w.Header().Get("X-Fresh-Count"); got != "" {
		t.Errorf("добор переписки отдал число %q", got)
	}
}

// Добор ЧУЖОЙ переписки молчит: не отказ, а пусто — право проверяет ядро тем же
// условием, что и чтение, а живой добор молчалив по устройству.
func TestДоборЧужойПерепискиМолчит(t *testing.T) {
	m := newFakeMail()
	h, _, theirs := mailServer(t, m)
	ctx := context.Background()
	// Переписка ДВОИХ других: сессия «theirs» к ней отношения не имеет.
	if _, err := m.SendMessage(ctx, 606064, 1038894, "не для вас"); err != nil {
		t.Fatal(err)
	}
	w := do(h, as(guest(t, "GET", "/mail/1/fresh?after=0"), theirs))
	if w.Code != http.StatusOK {
		t.Fatalf("добор чужой переписки ответил %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "не для вас") {
		t.Error("добор отдал чужое письмо")
	}
}

// Прежние страницы переписки живым добором НЕ дописываются: там середина
// разговора, и дописывать в её конец свежее письмо значило бы соврать о том,
// что человек читает. Тот же довод, что у второй страницы ленты.
func TestПрежниеСтраницыПерепискиНеДописываются(t *testing.T) {
	m := newFakeMail()
	h, mine, _ := mailServer(t, m)
	ctx := context.Background()
	for i := 0; i < letterPageSize+5; i++ {
		if _, err := m.SendMessage(ctx, peerID, testProfileID, "письмо"); err != nil {
			t.Fatal(err)
		}
	}
	if body := do(h, as(guest(t, "GET", "/mail/1?page=1"), mine)).Body.String(); strings.Contains(body, "data-fresh") {
		t.Error("первая страница длинной переписки дописывается живым добором")
	}
	if body := do(h, as(guest(t, "GET", "/mail/1?page=2"), mine)).Body.String(); !strings.Contains(body, "data-fresh") {
		t.Error("последняя страница переписки не дописывается")
	}
}

// Выключенная переписка добора не имеет вовсе — как и всего остального.
func TestДоборПисемЗаГейтом(t *testing.T) {
	h, mine, _ := mailServer(t, nil)
	if w := do(h, as(guest(t, "GET", "/mail/1/fresh?after=0"), mine)); w.Code != http.StatusNotFound {
		t.Errorf("добор при выключенной переписке ответил %d", w.Code)
	}
}

// Меню каркаса печатает ЧИСЛО, а не сами письма.
//
// Оплачено боем 12.09.2026: dialogPage.Letters ([]MessageView) перекрыл
// одноимённое поле каркаса — счётчик, — и на месте числа в пункте меню встал
// дамп среза вместе с текстами писем. Соседний page_shadow_test.go запрещает
// такое соседство имён вовсе, а этот смотрит на страницу: дефект увидели
// глазами, и проверять его надо там же, где он был виден.
func TestМенюПечатаетЧислоПисемАНеСамиПисьма(t *testing.T) {
	m := newFakeMail()
	m.unread = 2
	h, mine, _ := mailServer(t, m)
	if _, err := m.SendMessage(context.Background(), peerID, testProfileID, "тайное письмо"); err != nil {
		t.Fatal(err)
	}
	body := do(h, as(guest(t, "GET", "/mail/1"), mine)).Body.String()
	i := strings.Index(body, "acctmenu")
	if i < 0 {
		t.Fatal("меню участника не нарисовано вовсе")
	}
	menu := body[i:min(i+900, len(body))]
	if !strings.Contains(menu, `class="mn">2</span>`) {
		t.Error("в пункте меню нет числа непрочитанных писем")
	}
	if strings.Contains(menu, "тайное письмо") || strings.Contains(menu, "[{") {
		t.Errorf("в пункте меню оказались сами письма: %s", menu)
	}
}

// Форма отметки прочитанного стои́т ВСЕГДА и лишь прячется, пока отмечать
// нечего: письмо, дописанное живым добором на открытой странице, человек видит,
// и кнопка обязана его накрыть, — а завести её скриптом из ничего нельзя.
// Оплачено тем же днём: отметил прочитанным, колокольчик погас, а счётчик писем
// остался гореть, потому что граница стояла на том письме, что нарисовал
// сервер.
func TestФормаОтметкиСтоитДажеКогдаОтмечатьНечего(t *testing.T) {
	quiet := newFakeMail()
	h, mine, _ := mailServer(t, quiet)
	// Переписка есть, а непрочитанного нет: письмо своё.
	if _, err := quiet.SendMessage(context.Background(), testProfileID, peerID, "моё письмо"); err != nil {
		t.Fatal(err)
	}
	body := do(h, as(guest(t, "GET", "/mail/1"), mine)).Body.String()
	if !strings.Contains(body, `class="mkread"`) {
		t.Fatal("формы отметки нет вовсе — живому добору нечего будет открыть")
	}
	if !strings.Contains(body, `action="/mail/1/read" hidden`) {
		t.Error("форма отметки не спрятана, хотя отмечать нечего")
	}

	loud := newFakeMail()
	loud.unread = 1
	h2, mine2, _ := mailServer(t, loud)
	body2 := do(h2, as(guest(t, "GET", "/mail/1"), mine2)).Body.String()
	if strings.Contains(body2, `action="/mail/1/read" hidden`) {
		t.Error("форма отметки спрятана, хотя непрочитанное есть")
	}
}

// РАЗГОВОР ПРОКРУЧИВАЕТСЯ ВНУТРИ СЕБЯ (12.09.2026, жалоба владельца: «когда в
// личной переписке много сообщений, страница становится простынёй»). Проверяется
// ДВОЕ, и порознь: класс на списке писем и само правило в стилях, — потому что
// рамка с прокруткой обязана работать БЕЗ скрипта, а рисует её CSS. Класс без
// правила и правило без класса дали бы ровно прежнюю простыню, и ни один тест на
// поведение этого не увидел бы.
func TestРазговорПрокручиваетсяВнутриСебя(t *testing.T) {
	m := newFakeMail()
	h, mine, _ := mailServer(t, m)
	if _, err := m.SendMessage(context.Background(), peerID, testProfileID, "и ещё одно"); err != nil {
		t.Fatal(err)
	}
	body := do(h, as(guest(t, "GET", "/mail/1"), mine)).Body.String()
	if !strings.Contains(body, `class="lts talk"`) {
		t.Fatalf("у списка писем нет рамки с прокруткой:\n%s", body)
	}

	rule := cssRule(t, cssText(t), ".lts.talk {")
	for _, want := range []string{"max-height", "overflow-y: auto"} {
		if !strings.Contains(rule, want) {
			t.Errorf("в правиле разговора нет %q — без скрипта прокрутки не будет вовсе", want)
		}
	}
	// Именно max-height: у разговора в три письма пустой высоты быть не должно.
	if strings.Contains(rule, "height: 60vh") && !strings.Contains(rule, "max-height: 60vh") {
		t.Error("высота задана жёстко: короткий разговор получит пустую рамку")
	}
	// А СЦЕПКА ПРОКРУТОК ОСТАЁТСЯ УМОЛЧАНИЕМ. Стоял здесь overscroll-behavior:
	// contain — «страница не едет, когда разговор дочитан и палец тянет
	// дальше», — и он же запер человека в рамке: скрипт открывает её сразу на
	// последнем письме, тянуть внутри некуда с первого жеста, а наружу гнать
	// нечем, кроме узких полей страницы по краям (жалоба владельца 12.09.2026).
	if strings.Contains(rule, "overscroll-behavior") {
		t.Error("рамка снова не отдаёт прокрутку странице: из разговора не выбраться к форме ответа")
	}
}

// А НА ТЕЛЕФОНЕ РАМКИ НЕТ ВОВСЕ, и это не вкус, а арифметика (жалоба владельца
// 12.09.2026, тот же вечер: «кнопка отправки уезжает вниз и её не видно, по полю
// с сообщениями вниз не проскроллить»). У аппарата с viewport 384×719 карточка
// собеседника занимает 240 точек, рамка в 60vh — ещё 430, и форма ответа
// начинается за нижним краем экрана: единственное, ради чего рамка заведена, там
// недостижимо, а плата — две поверхности прокрутки под одним пальцем — остаётся.
func TestНаТелефонеРазговорПрокручиваетсяСоСтраницей(t *testing.T) {
	mobile := cssRule(t, cssText(t), "@media (max-width: 700px)")
	if !strings.Contains(mobile, ".lts.talk { max-height: none; overflow-y: visible; }") {
		t.Error("на телефоне у разговора осталась своя прокрутка: форма ответа за краем экрана, а выбраться к ней нечем")
	}
}

// А ЦИТАТА ЖАЛОБЫ прокрутки не получает: письмо там одно, прокручиваться ему
// некуда, и рамка с потолком высоты сказала бы человеку, что показано не всё.
// Это и есть довод, по которому рамка сделана модификатором, а не свойством
// самого `.lts`.
func TestЦитатаЖалобыПрокруткиНеПолучает(t *testing.T) {
	m := newFakeMail()
	h, mine, _, _ := mailModServer(t, m, newFakeMod())
	bad, err := m.SendMessage(context.Background(), peerID, testProfileID, "отдай телефон, иначе")
	if err != nil {
		t.Fatal(err)
	}
	body := do(h, as(guest(t, "GET", "/mail/report?m="+strconv.FormatInt(bad.MessageID, 10)), mine)).Body.String()
	if !strings.Contains(body, `class="lts"`) {
		t.Fatalf("на форме жалобы нет самого письма:\n%s", body)
	}
	if strings.Contains(body, "lts talk") {
		t.Error("цитата жалобы получила рамку с прокруткой")
	}
}

// [Ж] Скрипт ставит разговор на ПОСЛЕДНЕЕ письмо. Проверить это исполнением
// нечем — браузера в тестах нет, — но правила обязаны быть НАЗВАНЫ в коде, а не
// жить договорённостью. Их три, и каждое отменяет отдельную беду: без прокрутки
// в конец переписка открывается письмами полугодовой давности; без уступки
// анкору страница отменяет чужое указание и уводит от письма, за которым
// пришли; без замера «стои́т ли человек в конце» живой добор становится
// невидимым — письмо ложится ниже нижнего края рамки.
func TestСкриптСтавитРазговорНаПоследнееПисьмо(t *testing.T) {
	js := jsText(t)
	for _, want := range []string{
		".lts.talk",       // рамку скрипт ищет по тому же классу, что рисует CSS
		"scrollHeight",    // и ставит её в конец
		"location.hash",   // кроме тех, кто пришёл по ссылке на письмо
		"talkEnded()",     // дочитанный до конца разговор остаётся дочитанным
		"selected()",      // но выделенный текст из вида не уводим
		"talkScroller",    // а конец разговора — это рамка ЛИБО страница
		"scrollIntoView(", // на телефоне рамки нет, и к концу подводят страницу
	} {
		if !strings.Contains(js, want) {
			t.Errorf("в скрипте нет %q — переписка открывалась бы началом страницы", want)
		}
	}
	// РАМКА ЛИБО СТРАНИЦА решается ПО САМОЙ РАМКЕ, а не по ширине экрана: порог
	// написан в стилях, и второе его написание в скрипте разошлось бы с первым
	// молча — ровно так же, как разошлись бы две константы окна у народа.
	if strings.Contains(js, "max-width") {
		t.Error("скрипт завёл свой порог ширины: он разъедется с тем, что стоит в стилях")
	}
	// И к форме ответа подводит именно ФОРМА, а не последнее письмо: письмо,
	// прижатое к нижнему краю, оставляет форму ровно там же, где она была, — за
	// краем экрана.
	if !strings.Contains(js, "'.wbox'") {
		t.Error("страницу подводят не к форме ответа — с неё и началась жалоба")
	}
}
