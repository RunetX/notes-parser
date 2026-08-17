package love

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// HTML повторяет реальную разметку talks (снято в Ф0), но с вымышленными данными.

const buddiesHTML = `
<li class="lv-talks__user js-talks__buddy_777" data-user-passport-id="777" data-user-id="555" data-user-nick="Мария" data-user-sex="female">
  <div class="lv-talks__userpic"></div>
  <div class="lv-talks__unread-inbox">3</div>
</li>
<li class="lv-talks__user js-talks__buddy_888" data-user-passport-id="888" data-user-id="666" data-user-nick="Иван">
  <div class="lv-talks__unread-inbox"></div>
</li>`

const messagesHTML = `
<li class='js_msgr-item lv-talks__message-item lv-talks__message-item_in'>
  <div id="msg-aaa" class="lv-msg__message-box" data-msg-id='aaa111'>
    <time class="lv-msg__time">18:10</time>
    <div class="lv-msg__inmsg">
      <div class="lv-talks__message-author js_msgr-author">Мария</div>
      Привет!
      <div class="message-image"><ul></ul></div>
    </div>
  </div>
</li>
<li class='js_msgr-item lv-talks__message-item lv-talks__message-item_out'>
  <div id="msg-bbb" class="lv-msg__message-box" data-msg-id='bbb222'>
    <time class="lv-msg__time">18:13</time>
    <div class="lv-msg__outmsg">
      <div class="lv-talks__message-author js_msgr-author">Рантье</div>
      Здравствуйте
    </div>
  </div>
</li>`

func TestParseBuddies(t *testing.T) {
	dialogs, err := parseBuddies(buddiesHTML)
	if err != nil {
		t.Fatal(err)
	}
	if len(dialogs) != 2 {
		t.Fatalf("ожидалось 2 диалога, got %d", len(dialogs))
	}
	if d := dialogs[0]; d.PassportID != "777" || d.ProfileID != "555" || d.Nick != "Мария" || d.Unread != 3 {
		t.Errorf("диалог 0: %+v", d)
	}
	if d := dialogs[1]; d.PassportID != "888" || d.Nick != "Иван" || d.Unread != 0 {
		t.Errorf("диалог 1: %+v", d)
	}
}

func TestParseMessages(t *testing.T) {
	msgs, err := parseMessages(messagesHTML)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("ожидалось 2 сообщения, got %d", len(msgs))
	}
	if m := msgs[0]; m.SiteMsgID != "aaa111" || m.FromSelf || m.Text != "Привет!" {
		t.Errorf("входящее: %+v", m)
	}
	if m := msgs[1]; m.SiteMsgID != "bbb222" || !m.FromSelf || m.Text != "Здравствуйте" {
		t.Errorf("исходящее: %+v", m)
	}
}

func TestResultHTML(t *testing.T) {
	// sendMessage отдаёт строку, getMessagesHistory — {html}.
	if h, err := resultHTML(json.RawMessage(`"<li>привет</li>"`)); err != nil || h != "<li>привет</li>" {
		t.Errorf("строка: %q %v", h, err)
	}
	if h, err := resultHTML(json.RawMessage(`{"html":"<li>x</li>"}`)); err != nil || h != "<li>x</li>" {
		t.Errorf("объект: %q %v", h, err)
	}
}

// talksServer — httptest сайта talks: GET /ajax (loadBuddiesList) и POST /ajax/ (RPC).
func talksServer(t *testing.T, guest bool) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ajax", func(w http.ResponseWriter, r *http.Request) {
		if guest {
			_, _ = io.WriteString(w, `{"loadBuddiesList":{"data":[],"html":"","error":"Ошибка авторизации"}}`)
			return
		}
		// Реальная форма (снята в Ф0-прогоне): разметка в loadBuddiesList.html,
		// а data несёт лишь user_ids (data.html пустой).
		resp := map[string]any{"loadBuddiesList": map[string]any{
			"html": buddiesHTML,
			"data": map[string]any{"html": "", "user_ids": []int64{777, 888}},
		}}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/ajax/", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "getMessagesHistory":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]string{"html": messagesHTML}})
		case "sendMessage":
			out := `<li class='lv-talks__message-item lv-talks__message-item_out'><div class="lv-msg__message-box" data-msg-id='new999'><div class="lv-msg__outmsg"><div class="lv-talks__message-author">Рантье</div>текст</div></div></li>`
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": out})
		default:
			http.Error(w, "unknown", 400)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL, "test-ua", 0, nil)
}

func TestTalksDialogsLive(t *testing.T) {
	c := talksServer(t, false)
	dialogs, err := c.TalksDialogs(context.Background(), nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(dialogs) != 2 || dialogs[0].PassportID != "777" {
		t.Fatalf("диалоги: %+v", dialogs)
	}
}

func TestTalksGuestIsUnauthorized(t *testing.T) {
	c := talksServer(t, true)
	_, err := c.TalksDialogs(context.Background(), nil, 10)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("гостевой ответ должен давать ErrUnauthorized, got %v", err)
	}
}

// 5xx фронта — временный отказ, а не дрейф API: поллер talks по этому признаку
// отличает «моргнул гейтвей» от «сломался формат» и не уходит в kill-switch.
func TestTalks5xxIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "test-ua", 0, nil)
	_, err := c.TalksDialogs(context.Background(), nil, 10)
	if !errors.Is(err, ErrSiteUnavailable) {
		t.Fatalf("502 должен давать ErrSiteUnavailable, got %v", err)
	}
	var se *SchemaError
	if errors.As(err, &se) {
		t.Fatalf("502 — не дрейф API: %v", err)
	}
}

func TestTalksHistoryAndSend(t *testing.T) {
	c := talksServer(t, false)
	msgs, err := c.TalksHistory(context.Background(), nil, "777", "", 20)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("история: %+v %v", msgs, err)
	}
	sent, err := c.TalksSend(context.Background(), nil, "777", "текст")
	if err != nil || sent.SiteMsgID != "new999" || !sent.FromSelf {
		t.Fatalf("отправка: %+v %v", sent, err)
	}
}

func TestSiteIdentityRegex(t *testing.T) {
	page := []byte(`... Love.user = "1472546"; ... "passport_id":280703879, ... "nick":"Рантье" ...`)
	if got := firstSubmatch(reLoveUser, page); got != "1472546" {
		t.Errorf("Love.user: %q", got)
	}
	if got := firstSubmatch(rePassportID, page); got != "280703879" {
		t.Errorf("passport_id: %q", got)
	}
	if got := firstSubmatch(reLayoutNick, page); got != "Рантье" {
		t.Errorf("nick: %q", got)
	}
}

// Живая страница печатает кириллицу в нике escape-последовательностями — так
// в accounts.db и оседало «Па...» вместо «Паноптикум».
func TestSiteIdentityNickEscaped(t *testing.T) {
	cases := []struct{ in, want string }{
		{`Паноптикум`, "Паноптикум"},
		{"Рантье", "Рантье"},
		{`Отвали`, "Отвали"},
		{`\"кавычки\"`, `"кавычки"`},
		{`битый \u04`, `битый \u04`}, // не разобралось — отдаём как есть
	}
	for _, c := range cases {
		if got := unescapeJS(c.in); got != c.want {
			t.Errorf("unescapeJS(%q) = %q, ожидалось %q", c.in, got, c.want)
		}
	}
}
