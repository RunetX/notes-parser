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
	"net/http"
	"net/url"
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
}

func newFakeMail() *fakeMail {
	return &fakeMail{
		letters: map[int64][]platform.MessageView{},
		pair:    map[[2]int64]int64{},
		senders: map[int64]int64{},
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
	return f.pair[key(me, peer)], nil
}

func (f *fakeMail) SendMessage(_ context.Context, from, to int64, body string) (platform.MessageSent, error) {
	if f.deny != nil {
		return platform.MessageSent{}, f.deny
	}
	if strings.TrimSpace(body) == "" {
		return platform.MessageSent{}, platform.ErrEmptyBody
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

// mailServer — сервер с перепиской и ДВУМЯ вошедшими: писать в одиночку
// бессмысленно, а вторая сессия нужна, чтобы прочесть письмо глазами адресата.
func mailServer(t *testing.T, m Mail) (http.Handler, string, string) {
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
	st := &fakeStore{profile: platform.Profile{
		ID: peerID, Nick: "Полынь-Трава", Kind: platform.KindMember, CreatedAt: time.Now()}}
	srv := New(Config{BaseURL: "http://127.0.0.1", Log: quietLog()}, st, auth, nil, nil, nil)
	t.Cleanup(func() { _ = srv.Close() })
	if m != nil {
		srv.SetMail(m)
	}
	return srv.routes(), mine, theirs
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
