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
