package web

// Проверки живого добора (эпик F).
//
// Главная мысль набора: строка, ДОПИСАННАЯ в страницу, обязана быть той же
// самой, что нарисовало бы обновление. Если эти два пути разойдутся, площадка
// получит вторую разметку — а вместе с ней второе место, где однажды забудут
// про маскирование анонима, обращение из ребра и права читателя.

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

func freshComment(id int64, parent int64, depth int) platform.CommentView {
	c := platform.CommentView{
		ID: id, NoteID: 312811, Depth: depth,
		Author:      platform.Author{ID: 1044551, Nick: "Линда"},
		Body:        "только что написанное",
		PublishedAt: time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC),
	}
	if parent != 0 {
		c.ReplyTo = &platform.ReplyRef{CommentID: parent, Nick: "Мавр"}
	}
	return c
}

// Кусок разметки — это именно кусок: ни «базы», ни шапки, ни колокольчика.
// Клиент вставляет его как есть, поэтому лишнее здесь не мусор, а поломка.
func TestFreshReturnsBareItems(t *testing.T) {
	st := &fakeStore{note: sampleNote(), fresh: []platform.CommentView{freshComment(9, 2, 2)}}
	h := openServer(t, st)

	w := do(h, guest(t, "GET", "/n/312811/fresh?after=3,0"))
	if w.Code != http.StatusOK {
		t.Fatalf("добор ответил %d", w.Code)
	}
	body := w.Body.String()
	for _, bad := range []string{"<!doctype", "<html", "class=\"top\"", "<title"} {
		if strings.Contains(strings.ToLower(body), bad) {
			t.Errorf("в куске разметки оказалась страница целиком (%q):\n%s", bad, body)
		}
	}
	if !strings.HasPrefix(strings.TrimSpace(body), "<li") {
		t.Errorf("кусок начинается не со строки списка:\n%s", body)
	}
	// Границу добора клиент не разбирает, а возвращает как есть — но сдвинуть её
	// обязан сервер, иначе следующий добор принесёт то же самое ещё раз.
	if got := w.Header().Get("X-Fresh-After"); got != "9,0" {
		t.Errorf("граница после добора: %q, ожидалась 9,0", got)
	}
	if (st.freshAfter != platform.FreshAfter{NGS: 3}) || st.freshNoteID != 312811 {
		t.Errorf("в хранилище ушло note=%d after=%+v", st.freshNoteID, st.freshAfter)
	}
}

// Куда встанет строка, решает КЛИЕНТ, и решает по этим двум атрибутам. Без них
// ответ на давнюю реплику уехал бы в конец треда.
func TestFreshItemCarriesItsPlaceInTree(t *testing.T) {
	st := &fakeStore{note: sampleNote(), fresh: []platform.CommentView{freshComment(9, 2, 2)}}
	body := do(openServer(t, st), guest(t, "GET", "/n/312811/fresh?after=3,0")).Body.String()

	for _, want := range []string{`id="c9"`, `data-depth="2"`, `data-parent="2"`} {
		if !strings.Contains(body, want) {
			t.Errorf("в строке нет %s:\n%s", want, body)
		}
	}
}

// Та же реплика, нарисованная страницей и добором, обязана дать ОДНУ И ТУ ЖЕ
// разметку. Это и есть весь смысл общей части: разойдись они — и у площадки
// стало бы два способа превратить текст в HTML.
func TestFreshItemMatchesThePageItem(t *testing.T) {
	c := freshComment(9, 0, 1)
	st := &fakeStore{note: sampleNote(), thread: []platform.CommentView{c}, fresh: []platform.CommentView{c}}
	h := openServer(t, st)

	page := do(h, guest(t, "GET", "/n/312811")).Body.String()
	frag := strings.TrimSpace(do(h, guest(t, "GET", "/n/312811/fresh?after=0,0")).Body.String())

	item := regexp.MustCompile(`(?s)<li class="c .*?</li>`).FindString(page)
	if item == "" {
		t.Fatal("на странице не нашлось строки комментария")
	}
	if squeeze(item) != squeeze(frag) {
		t.Errorf("страница и добор рисуют по-разному:\n-- страница --\n%s\n-- добор --\n%s", item, frag)
	}
}

// squeeze — сравниваем разметку, а не расстановку переносов: шаблон один, но
// вокруг вызова у страницы и у куска отступы разные.
func squeeze(s string) string { return strings.Join(strings.Fields(s), " ") }

// Права у дописанной строки те же, что у страницы: гостю не предлагают
// «Ответить» ни там, ни там.
func TestFreshItemRespectsWhoIsAsking(t *testing.T) {
	st := &fakeStore{note: sampleNote(), fresh: []platform.CommentView{freshComment(9, 0, 1)}}
	body := do(openServer(t, st), guest(t, "GET", "/n/312811/fresh?after=0,0")).Body.String()
	if strings.Contains(body, "Ответить") {
		t.Errorf("гостю предложено отвечать:\n%s", body)
	}
}

// Число над тредом ставит сервер: порция добора ограничена потолком, и считать
// его по числу вставленных строк значило бы врать при первом же шторме.
func TestFreshCarriesCommentCount(t *testing.T) {
	note := sampleNote()
	note.CommentCount = 158
	st := &fakeStore{note: note, fresh: []platform.CommentView{freshComment(9, 0, 1)}}
	w := do(openServer(t, st), guest(t, "GET", "/n/312811/fresh?after=0,0"))
	if got := w.Header().Get("X-Fresh-Count"); got != "158" {
		t.Errorf("число комментариев: %q, ожидалось 158", got)
	}
}

// Пустой добор — рабочий случай, а не отказ: сигнал мог прийти о реплике,
// которую тут же скрыли. Граница при этом остаётся прежней.
func TestFreshEmptyKeepsCursor(t *testing.T) {
	st := &fakeStore{note: sampleNote()}
	w := do(openServer(t, st), guest(t, "GET", "/n/312811/fresh?after=42,7"))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "" {
		t.Fatalf("пустой добор ответил %d, тело %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Fresh-After"); got != "42,7" {
		t.Errorf("граница после пустого добора: %q, ожидалась прежняя 42,7", got)
	}
}

// Тред у площадки СМЕШАННЫЙ: своё и зеркальное живут в одном разговоре, а
// нативный id больше любого ngs'ного. Граница поэтому обязана помнить обе полосы
// — иначе первая же реплика, написанная здесь, уводит её в нативную полосу, и
// приходящие следом комментарии НГС не догоняют страницу никогда. Ровно это и
// было видно на боевой заметке 313056: в мессенджере реплики идут, на странице
// их нет до обновления.
func TestFreshCursorRemembersBothBands(t *testing.T) {
	mixed := sampleThread()
	mixed[0].ID = 63238683                    // с НГС
	mixed[2].ID = platform.NativeIDBase + 719 // написана здесь
	st := &fakeStore{note: sampleNote(), thread: mixed}
	h := openServer(t, st)

	page := do(h, guest(t, "GET", "/n/312811")).Body.String()
	want := `data-fresh="63238683,100000000719"`
	if !strings.Contains(page, want) {
		t.Fatalf("граница страницы потеряла полосу, ожидалось %s:\n%s", want, page)
	}

	// И обратно: та же граница обязана доехать до хранилища обеими половинами.
	do(h, guest(t, "GET", "/n/312811/fresh?after=63238683,100000000719"))
	if want := (platform.FreshAfter{NGS: 63238683, Native: platform.NativeIDBase + 719}); st.freshAfter != want {
		t.Errorf("в хранилище ушло %+v, ожидалось %+v", st.freshAfter, want)
	}
}

// Границу страница берёт У ЯДРА, а не считает по показанным строкам. В линейном
// виде на странице ОКНО из тридцати самых свежих реплик, и полоса, в него не
// попавшая, получила бы границу «с начала» — первый же сигнал притащил бы наверх
// страницы самые старые реплики этой полосы.
func TestFreshCursorComesFromTheCoreNotFromTheWindow(t *testing.T) {
	note := sampleNote()
	note.CommentCount = 100
	window := sampleThread() // в окне только своё
	for i := range window {
		window[i].ID = platform.NativeIDBase + int64(i) + 1
	}
	bound := platform.FreshAfter{NGS: 63238683, Native: platform.NativeIDBase + 3}
	st := &fakeStore{note: note, flat: window, freshBound: &bound}

	page := do(openServer(t, st), guest(t, "GET", "/n/312811?view=linear")).Body.String()
	if want := `data-fresh="63238683,100000000003"`; !strings.Contains(page, want) {
		t.Errorf("страница посчитала границу по окну, ожидалось %s:\n%s", want, page)
	}
}

// Отказ на границе выключает добор, а не страницу: дописываться она перестанет,
// читаться — нет.
func TestFreshCursorFailureLeavesThePageReadable(t *testing.T) {
	st := &fakeStore{note: sampleNote(), thread: sampleThread(), freshErr: errors.New("база молчит")}
	w := do(openServer(t, st), guest(t, "GET", "/n/312811"))
	if w.Code != http.StatusOK {
		t.Fatalf("страница ответила %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "data-fresh=") {
		t.Error("добор включён при неизвестной границе")
	}
}

// Границу прежнего вида — одно число — страницы, открытые до выкатки, ещё
// возвращают. Отвечать им отказом незачем: число кладётся в свою полосу.
func TestFreshAcceptsCursorOfPreviousRelease(t *testing.T) {
	st := &fakeStore{note: sampleNote()}
	h := openServer(t, st)

	if code := do(h, guest(t, "GET", "/n/312811/fresh?after=63238683")).Code; code != http.StatusOK {
		t.Fatalf("прежняя граница отвергнута с кодом %d", code)
	}
	// Максимум оказался ngs'ным — значит своих реплик страница не показывала
	// вовсе, и нативная полоса честно начинается с нуля.
	if want := (platform.FreshAfter{NGS: 63238683}); st.freshAfter != want {
		t.Errorf("прежняя граница разобрана как %+v, ожидалось %+v", st.freshAfter, want)
	}
}

func TestFreshRejectsBadCursor(t *testing.T) {
	h := openServer(t, &fakeStore{note: sampleNote()})
	for _, q := range []string{"", "?after=", "?after=абв", "?after=-1"} {
		if got := do(h, guest(t, "GET", "/n/312811/fresh"+q)).Code; got != http.StatusBadRequest {
			t.Errorf("граница %q принята с кодом %d", q, got)
		}
	}
}

// Лента добирается парой «время, id»: одного id мало — зеркальная заметка НГС
// имеет номер МЕНЬШЕ любой нативной, будучи новее её.
func TestFreshFeedUsesTimeCursor(t *testing.T) {
	at := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	fresh := sampleNote()
	fresh.ID = 100000000042
	fresh.PublishedAt = at.Add(time.Hour)
	st := &fakeStore{freshNotes: []platform.NoteView{fresh}}

	w := do(openServer(t, st), guest(t, "GET", "/fresh?after="+feedCursor(at, 312811)))
	if w.Code != http.StatusOK {
		t.Fatalf("добор ленты ответил %d", w.Code)
	}
	if !st.freshSince.Equal(at) {
		t.Errorf("в хранилище ушло время %v, ожидалось %v", st.freshSince, at)
	}
	if !strings.Contains(w.Body.String(), `id="n100000000042"`) {
		t.Errorf("заметки нет в ответе:\n%s", w.Body.String())
	}
	if got := w.Header().Get("X-Fresh-After"); got != feedCursor(fresh.PublishedAt, fresh.ID) {
		t.Errorf("граница ленты после добора: %q", got)
	}
}

// Запрос отдаёт от новых к старым, а клиент вставляет сверху — значит в теле
// они обязаны идти НАОБОРОТ, иначе три пришедшие разом заметки встанут в ленту
// задом наперёд.
func TestFreshFeedReversesForTopInsertion(t *testing.T) {
	at := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	older, newer := sampleNote(), sampleNote()
	older.ID, older.PublishedAt = 111, at.Add(time.Minute)
	newer.ID, newer.PublishedAt = 222, at.Add(2*time.Minute)
	st := &fakeStore{freshNotes: []platform.NoteView{newer, older}} // как отдаёт лента

	body := do(openServer(t, st), guest(t, "GET", "/fresh?after="+feedCursor(at, 0))).Body.String()
	if strings.Index(body, `id="n111"`) > strings.Index(body, `id="n222"`) {
		t.Errorf("порядок для вставки сверху перевёрнут не в ту сторону:\n%s", body)
	}
}

func TestFreshFeedRejectsBadCursor(t *testing.T) {
	h := openServer(t, &fakeStore{})
	for _, q := range []string{"", "?after=42", "?after=абв,1", "?after=1,абв"} {
		if got := do(h, guest(t, "GET", "/fresh"+q)).Code; got != http.StatusBadRequest {
			t.Errorf("граница %q принята с кодом %d", q, got)
		}
	}
}

// Атрибут data-fresh и есть выключатель добора. Он стоит у дерева и у первой
// страницы линейного вида — и не стоит у остальных: там срез истории, и хвост
// разговора, дописанный сверху, врал бы о том, что человек читает.
func TestFreshSwitchIsTheAttribute(t *testing.T) {
	note := sampleNote()
	note.CommentCount = 100 // хватит на несколько страниц линейного вида
	st := &fakeStore{note: note, thread: sampleThread(), flat: sampleThread()}
	h := openServer(t, st)

	tree := do(h, guest(t, "GET", "/n/312811")).Body.String()
	if !strings.Contains(tree, `data-fresh="3,0"`) || !strings.Contains(tree, `data-fresh-url="/n/312811/fresh"`) {
		t.Errorf("у дерева нет границы добора:\n%s", tree)
	}
	if first := do(h, guest(t, "GET", "/n/312811?view=linear")).Body.String(); !strings.Contains(first, "data-fresh=") {
		t.Error("первая страница линейного вида не дописывается")
	}
	if second := do(h, guest(t, "GET", "/n/312811?view=linear&page=2")).Body.String(); strings.Contains(second, "data-fresh=") {
		t.Error("вторая страница линейного вида дописывается, хотя это срез истории")
	}
}

func TestFreshSwitchOnFeedIsFirstPageOnly(t *testing.T) {
	st := &fakeStore{total: 100, notes: []platform.NoteView{sampleNote()}}
	h := openServer(t, st)

	if first := do(h, guest(t, "GET", "/")).Body.String(); !strings.Contains(first, `data-fresh-url="/fresh"`) {
		t.Errorf("лента не дописывается:\n%s", first)
	}
	if second := do(h, guest(t, "GET", "/?page=2")).Body.String(); strings.Contains(second, "data-fresh=") {
		t.Error("вторая страница ленты дописывается, хотя это срез истории")
	}
}

// Живой добор дешевле страницы треда, и корзину частоты он тратит как обычная
// страница: платить за него как за дерево значило бы наказывать читателя за то,
// что он держит вкладку открытой.
func TestFreshCostsLessThanThread(t *testing.T) {
	if costOf(guest(t, "GET", "/n/312811/fresh?after=1")) != costPage {
		t.Error("добор треда стоит не как страница")
	}
	if costOf(guest(t, "GET", "/fresh?after=1,1")) != costPage {
		t.Error("добор ленты стоит не как страница")
	}
	if costOf(guest(t, "GET", "/n/312811")) != costThread {
		t.Error("страница треда перестала стоить как тред")
	}
}

// ПЕРЕЕЗДЫ. Дерево перестраивается под открытой страницей: зеркало ставит ребро
// по обращению «Ник, …» и угадывает его примерно с половинной точностью, а обход
// мобильной версии заменяет догадку настоящим ребром. По границе id такая строка
// не приезжает никогда — id у неё прежний, — и до этой правки читатель видел
// ветку, выросшую не там, до самого обновления страницы.
//
// Замер 23.08.2026, боевая заметка 313058: Kowalski 63238879 нарисован страницей
// на глубине 4, обход переставил его на 2, и следующая строка треда — его
// собственный ответ 63238886 — приехала с глубиной 3, то есть МЕНЬШЕ, чем у
// родителя строкой выше.
func TestFreshCarriesMovedRows(t *testing.T) {
	at := time.Date(2026, 8, 23, 21, 50, 0, 0, time.UTC)
	next := platform.MovedAfter{At: at.Add(time.Minute), ID: 63238879}
	st := &fakeStore{
		note:      sampleNote(),
		moved:     []platform.CommentView{freshComment(63238879, 63238869, 2)},
		movedNext: next,
	}
	h := openServer(t, st)

	from := platform.FreshAfter{NGS: 3, Moved: platform.MovedAfter{At: at, ID: 5}}
	w := do(h, guest(t, "GET", "/n/312811/fresh?after="+threadCursor(from)))
	if w.Code != http.StatusOK {
		t.Fatalf("добор ответил %d", w.Code)
	}
	// Сравниваем МОМЕНТ, а не структуру: с провода отметка приезжает наносекундами
	// и оживает в местном поясе, а один и тот же миг — это один и тот же миг.
	if !st.movedAfter.At.Equal(from.Moved.At) || st.movedAfter.ID != from.Moved.ID {
		t.Errorf("в хранилище ушла граница переездов %+v, ожидалась %+v", st.movedAfter, from.Moved)
	}
	if body := w.Body.String(); !strings.Contains(body, `id="c63238879"`) ||
		!strings.Contains(body, `data-parent="63238869"`) {
		t.Errorf("переехавшая строка не приехала или приехала без нового места:\n%s", body)
	}
	// Какие строки — переезды, страница узнаёт заголовком: всё остальное, что у
	// неё уже стоит, она по-прежнему пропускает (этим гасится эхо своей реплики).
	if got := w.Header().Get("X-Fresh-Moved"); got != "63238879" {
		t.Errorf("список переездов: %q, ожидался 63238879", got)
	}
	if got, want := w.Header().Get("X-Fresh-After"), threadCursor(platform.FreshAfter{NGS: 3, Moved: next}); got != want {
		t.Errorf("граница после добора: %q, ожидалась %q", got, want)
	}
}

// Страница, открытая до выкатки, возвращает границу без переездов — и переездов
// ей не носят. Нулевая граница означала бы «неси всё, что когда-либо
// переезжало», то есть переставить читателю пол-треда разом.
func TestFreshMovedNotOfferedWithoutBoundary(t *testing.T) {
	st := &fakeStore{note: sampleNote(), moved: []platform.CommentView{freshComment(9, 2, 2)}}
	w := do(openServer(t, st), guest(t, "GET", "/n/312811/fresh?after=3,0"))

	if st.movedAfter.On() {
		t.Errorf("в хранилище ушла граница переездов %+v, ожидалась пустая", st.movedAfter)
	}
	if got := w.Header().Get("X-Fresh-Moved"); got != "" {
		t.Errorf("переезды предложены странице, которая их не просила: %q", got)
	}
	if strings.Contains(w.Body.String(), `id="c9"`) {
		t.Error("переехавшая строка уехала на страницу, которая её никогда не видела")
	}
}

// Строка, которая и появилась впервые, и успела переехать, годится в обоих
// качествах — берём её как новую: на странице у неё ещё нет места, которое надо
// исправлять, а прислать её дважды значило бы вставить и тут же переставить.
func TestFreshMovedYieldsToWhatCameAsNew(t *testing.T) {
	at := time.Date(2026, 8, 23, 21, 50, 0, 0, time.UTC)
	c := freshComment(9, 2, 2)
	st := &fakeStore{
		note:      sampleNote(),
		fresh:     []platform.CommentView{c},
		moved:     []platform.CommentView{c},
		movedNext: platform.MovedAfter{At: at.Add(time.Minute), ID: 9},
	}
	w := do(openServer(t, st), guest(t, "GET",
		"/n/312811/fresh?after="+threadCursor(platform.FreshAfter{Moved: platform.MovedAfter{At: at}})))

	if n := strings.Count(w.Body.String(), `id="c9"`); n != 1 {
		t.Errorf("строка приехала %d раз(а), ожидался один:\n%s", n, w.Body.String())
	}
	if got := w.Header().Get("X-Fresh-Moved"); got != "" {
		t.Errorf("новая строка объявлена переездом: %q", got)
	}
}

// Отказ на переездах — не отказ добора: место строки хуже, чем её отсутствие, и
// терять из-за него новые реплики незачем.
func TestFreshMovedFailureKeepsNewRows(t *testing.T) {
	at := time.Date(2026, 8, 23, 21, 50, 0, 0, time.UTC)
	from := platform.FreshAfter{NGS: 3, Moved: platform.MovedAfter{At: at, ID: 5}}
	st := &fakeStore{
		note:     sampleNote(),
		fresh:    []platform.CommentView{freshComment(9, 2, 2)},
		movedErr: errors.New("база молчит"),
	}
	w := do(openServer(t, st), guest(t, "GET", "/n/312811/fresh?after="+threadCursor(from)))

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="c9"`) {
		t.Fatalf("новые реплики потеряны вместе с переездами: %d\n%s", w.Code, w.Body.String())
	}
	// Граница переездов при отказе остаётся ПРЕЖНЕЙ: следующий такт попробует
	// снова, а сдвинув её, мы потеряли бы переезд навсегда.
	if got, want := w.Header().Get("X-Fresh-After"), threadCursor(platform.FreshAfter{NGS: 9, Moved: from.Moved}); got != want {
		t.Errorf("граница после отказа: %q, ожидалась %q", got, want)
	}
}

// Граница на проводе: четыре составляющие, и обратный разбор обязан вернуть ту
// же самую. Клиент её не читает вовсе — но возвращает как есть, поэтому
// расхождение туда-обратно означало бы тихо застрявший добор.
func TestThreadCursorRoundTrip(t *testing.T) {
	want := platform.FreshAfter{
		NGS: 63238683, Native: platform.NativeIDBase + 719,
		Moved: platform.MovedAfter{At: time.Date(2026, 8, 23, 21, 50, 4, 123456000, time.UTC), ID: 63238879},
	}
	got, ok := parseThreadCursor(threadCursor(want))
	if !ok {
		t.Fatalf("своя же граница %q не разобрана", threadCursor(want))
	}
	if got.NGS != want.NGS || got.Native != want.Native ||
		!got.Moved.At.Equal(want.Moved.At) || got.Moved.ID != want.Moved.ID {
		t.Errorf("туда-обратно: %+v, ожидалось %+v", got, want)
	}
}

func TestThreadCursorRejectsBadMovedPart(t *testing.T) {
	h := openServer(t, &fakeStore{note: sampleNote()})
	for _, q := range []string{"3,0,абв,5", "3,0,-1,5", "3,0,17", "3,0,17,-1", "3,0,17,5,9"} {
		if got := do(h, guest(t, "GET", "/n/312811/fresh?after="+q)).Code; got != http.StatusBadRequest {
			t.Errorf("граница %q принята с кодом %d", q, got)
		}
	}
}

// Переезд не должен уносить строку из-под глаз читателя. Проверяется здесь то
// же, что и у прыжка к новому (jump_test.go): скрипт живёт в bundle'е, и
// единственный способ уберечь его правила — назвать их по именам.
//
// Правил три, и каждое отвечает за свою беду:
//   - место чтения: строку замеряют ДО перестановки и возвращают на ту же
//     высоту ПОСЛЕ, иначе текст уезжает вместе с веткой;
//   - целиком или никак: порция переездов не применяется наполовину, иначе
//     потомок нарисуется глубиной меньше родительской — ровно та картинка,
//     из-за которой переезды и заведены;
//   - занятую строку не трогают: выделение и курсор живут в узле и умирают
//     вместе с ним, поэтому переезд ждёт, пока её отпустят.
func TestLiveScriptKeepsReadingPlaceOnMove(t *testing.T) {
	js := jsText(t)
	for _, want := range []string{
		"getBoundingClientRect", // где строка стояла на экране
		"scrollBy",              // и куда её вернуть после перестановки
		"held",                  // занятую строку откладываем
		"selectionchange",       // отпустили — переезжаем
		" moved",                // метка переезда, она же в style.css
	} {
		if !strings.Contains(js, want) {
			t.Errorf("в живом доборе нет %q — переезд унесёт строку из-под глаз читателя", want)
		}
	}
}

// МЕТКА «УШЛО НА НГС» ДОЕЗЖАЕТ ЖИВЫМ ДОБОРОМ.
//
// Написан по жалобе владельца 03.09.2026: значок у своей реплики меняется через
// секунды после публикации (очередь выноса ходит раз в пятнадцать секунд), а
// увидеть смену можно было только обновлением страницы.
//
// Проверяется здесь ровно то, что разошлось: путь добора не спрашивал про
// унесённое вовсе, поэтому строка приезжала со значком «написано здесь» и
// спорила с той же строкой после F5. Тест падает на коде до этой правки.
func TestFreshCarriesTheAwayMark(t *testing.T) {
	sent := freshComment(9, 2, 2)
	sent.ID = 100000000009 // своя полоса: чужую реплику на НГС мы не уносим
	st := &fakeStore{
		note:    sampleNote(),
		fresh:   []platform.CommentView{sent},
		ngsSent: map[string]bool{platform.NGSComment + ":100000000009": true},
	}
	body := do(openServer(t, st), guest(t, "GET", "/n/312811/fresh?after=0,0")).Body.String()
	if !strings.Contains(body, "копия ушла на НГС") {
		t.Errorf("добранная реплика приехала без метки выноса:\n%s", body)
	}
}

// А не унесённая — со своей обычной меткой: третье состояние не должно
// доставаться каждому только оттого, что путь научился про него спрашивать.
func TestFreshKeepsPlainMarkWhenNotSent(t *testing.T) {
	own := freshComment(9, 2, 2)
	own.ID = 100000000009
	st := &fakeStore{note: sampleNote(), fresh: []platform.CommentView{own}}
	body := do(openServer(t, st), guest(t, "GET", "/n/312811/fresh?after=0,0")).Body.String()
	if strings.Contains(body, "копия ушла на НГС") {
		t.Errorf("метка выноса встала у реплики, которая никуда не уезжала:\n%s", body)
	}
}
