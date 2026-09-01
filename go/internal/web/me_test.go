package web

// «Моя страница»: настройки, которые человек меняет о себе.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

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
