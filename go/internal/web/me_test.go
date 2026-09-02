package web

// «Моя страница»: настройки, которые человек меняет о себе.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// Галочка выноса на НГС: показывается только тому, у кого есть анкета НГС, и
// переключается формой с CSRF. Это не настройка экрана, а согласие на
// публикацию своих слов на чужом сайте, поэтому подделанное нажатие обязано
// отлетать — в отличие от соседней «проматывать к новым».
func TestГалочкаОтправкиНаНГС(t *testing.T) {
	auth := newFakeAuth()
	const ngsID = testProfileID
	auth.users[ngsID] = platform.User{ID: ngsID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), ngsID, "")
	if err != nil {
		t.Fatal(err)
	}
	grantConsents(t, auth, ngsID)
	wr := &fakeWriter{}
	h := newFullServer(t, &fakeStore{}, auth, wr, nil, nil, Config{})

	body := do(h, as(guest(t, "GET", "/me"), token)).Body.String()
	if !strings.Contains(body, "Отправлять мои записи на НГС") {
		t.Fatal("галочки нет у участника с анкетой НГС")
	}
	if !strings.Contains(body, "сейчас выключено") {
		t.Error("умолчание должно быть выключено: публикуя здесь, человек соглашался на публикацию здесь")
	}

	// Включаем.
	if w := do(h, postAs(t, "/me/ngssend", url.Values{"on": {"1"}}, token)); w.Code != http.StatusSeeOther {
		t.Fatalf("включение: код %d", w.Code)
	}
	if !wr.ngsSend[ngsID] {
		t.Fatal("ядро не узнало о включении")
	}

	// Без CSRF — отлуп: это согласие, а не прокрутка.
	r2 := as(post(t, "/me/ngssend", url.Values{"on": {"0"}}), token)
	if w := do(h, r2); w.Code == http.StatusSeeOther {
		t.Error("нажатие без CSRF прошло")
	}
	if !wr.ngsSend[ngsID] {
		t.Error("подделанное нажатие всё-таки выключило отправку")
	}
}

// ГАЛОЧКА, КОТОРАЯ МОЛЧА НИЧЕГО НЕ ДЕЛАЕТ, ХУЖЕ ОТСУТСТВУЮЩЕЙ (02.09.2026).
//
// Живой случай: у участницы семь ответов подряд легли в skipped «нет живой
// сессии сайта» — она включила отправку, а входа в РюмкинЪ у нас для неё нет, —
// и узнать об этом ей было неоткуда. Сессия живёт в SQLite демона, морда её не
// видит вовсе; зато видит СЛЕД, оставленный им в очереди.
//
// Проверяется и обратное: при нуле застрявших предупреждения нет. Строка обязана
// гаснуть сама, иначе однажды не ушедшая заметка попрекала бы человека год
// спустя, когда всё давно наладилось.
func TestОстановленнаяОтправкаНаНГСОбъясняетсяНаСтранице(t *testing.T) {
	auth := newFakeAuth()
	const ngsID = testProfileID
	auth.users[ngsID] = platform.User{ID: ngsID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), ngsID, "")
	if err != nil {
		t.Fatal(err)
	}
	grantConsents(t, auth, ngsID)
	wr := &fakeWriter{
		ngsSend:    map[int64]bool{ngsID: true},
		ngsStuck:   map[int64]int{ngsID: 7},
		ngsStuckAt: time.Date(2026, 9, 2, 7, 55, 0, 0, time.UTC),
	}
	h := newFullServer(t, &fakeStore{}, auth, wr, nil, nil, Config{})

	body := do(h, as(guest(t, "GET", "/me"), token)).Body.String()
	if !strings.Contains(body, "На НГС не ушло 7 записей") {
		t.Error("страница молчит о том, что записи не уходят")
	}
	if !strings.Contains(body, "/login") {
		t.Error("сказано о беде и не сказано, что делать")
	}

	wr.ngsStuck = map[int64]int{ngsID: 0}
	if body := do(h, as(guest(t, "GET", "/me"), token)).Body.String(); strings.Contains(body, "На НГС не ушло") {
		t.Error("предупреждение осталось после того, как отправка наладилась")
	}
}

// ЗАМЕТКА ЧЕЛОВЕКА С ГАЛОЧКОЙ ЗДЕСЬ НЕ ЗАВОДИТСЯ ВОВСЕ (решение владельца
// 02.09.2026: «если галка отправки стоит, то свою не создаём, отправляем на НГС,
// а с НГС забираем как обычно»).
//
// Проверяется НЕ «черновик завёлся», а то, что ядро НЕ звали публиковать здесь:
// прежде заметка выходила ДВАЖДЫ, и весь смысл правки в том, что второй копии
// больше нет. Плюс адрес перехода — в ЛЕНТУ, а не на заметку: заметки ещё нет, и
// её номер станет известен, только когда зеркало принесёт её с сайта.
func TestЗаметкаСГалочкойУходитНаНГСВместоПлощадки(t *testing.T) {
	auth := newFakeAuth()
	const ngsID = testProfileID
	auth.users[ngsID] = platform.User{ID: ngsID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), ngsID, "")
	if err != nil {
		t.Fatal(err)
	}
	grantConsents(t, auth, ngsID)
	wr := &fakeWriter{ngsSend: map[int64]bool{ngsID: true}}
	h := newFullServer(t, &fakeStore{}, auth, wr, nil, nil, Config{})

	w := do(h, postAs(t, "/new", url.Values{"body": {"пойдёт на сайт"}}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("публикация: код %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/?ngs=1" {
		t.Errorf("после отправки повели на %q, а заметки здесь ещё нет", got)
	}
	if wr.note.Body != "" {
		t.Error("заметка всё-таки заведена здесь — она выйдет дважды")
	}
	if wr.ngsDraft == nil || wr.ngsDraft.Body != "пойдёт на сайт" {
		t.Fatalf("на сайт не отдали ничего: %+v", wr.ngsDraft)
	}
	// Лента говорит, что заметка в пути: полторы минуты тишины читаются как
	// пропажа текста.
	if body := do(h, as(guest(t, "GET", "/?ngs=1"), token)).Body.String(); !strings.Contains(body, "появится здесь сама") {
		t.Error("лента молчит о том, что заметка ушла на сайт")
	}
}

// БЕЗ ГАЛОЧКИ ВСЁ КАК БЫЛО: заметка заводится здесь, и переход ведёт на неё.
// Тест парный к предыдущему — порознь они не значат ничего: первый один прошёл
// бы и на сломанном условии, отправляющем на сайт вообще всё.
func TestБезГалочкиЗаметкаОстаётсяЗдесь(t *testing.T) {
	auth := newFakeAuth()
	const ngsID = testProfileID
	auth.users[ngsID] = platform.User{ID: ngsID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), ngsID, "")
	if err != nil {
		t.Fatal(err)
	}
	grantConsents(t, auth, ngsID)
	wr := &fakeWriter{nextID: 100000000500}
	h := newFullServer(t, &fakeStore{}, auth, wr, nil, nil, Config{})

	w := do(h, postAs(t, "/new", url.Values{"body": {"останется тут"}}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("публикация: код %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/n/100000000500" {
		t.Errorf("после публикации повели на %q, а заметка здесь", got)
	}
	if wr.ngsDraft != nil {
		t.Error("заметка ушла на сайт без галочки")
	}
	if wr.note.Body != "останется тут" {
		t.Errorf("ядро не завело заметку: %q", wr.note.Body)
	}
}

// ЗАМЕТКА В ПУТИ НАЗЫВАЕТСЯ НА «МОЕЙ СТРАНИЦЕ». Её нет ни в ленте, ни у автора —
// и без этой строки минута ожидания выглядит как потерянный текст.
func TestЗаметкаВПутиВиднаНаСвоейСтранице(t *testing.T) {
	auth := newFakeAuth()
	const ngsID = testProfileID
	auth.users[ngsID] = platform.User{ID: ngsID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), ngsID, "")
	if err != nil {
		t.Fatal(err)
	}
	grantConsents(t, auth, ngsID)
	wr := &fakeWriter{
		ngsSend:    map[int64]bool{ngsID: true},
		ngsPending: map[int64]int{ngsID: 1},
	}
	h := newFullServer(t, &fakeStore{}, auth, wr, nil, nil, Config{})

	body := do(h, as(guest(t, "GET", "/me"), token)).Body.String()
	if !strings.Contains(body, "ещё не вернулась сюда") {
		t.Error("страница молчит о заметке, которая в пути")
	}
	wr.ngsPending = map[int64]int{ngsID: 0}
	if body := do(h, as(guest(t, "GET", "/me"), token)).Body.String(); strings.Contains(body, "ещё не вернулась сюда") {
		t.Error("строка осталась после того, как заметка доехала")
	}
}
