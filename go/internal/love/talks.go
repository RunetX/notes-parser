package love

// Личная переписка сайта (talks): доменные типы + клиентские методы поверх
// AJAX/JSON-RPC на `/ajax/` (см. json.go). Эндпоинты и формат ответов сняты в
// Ф0 живьём — см. briefs/love-talks-telegram.md §2:
//   - список диалогов: GET /ajax?request=loadBuddiesList → JSON {loadBuddiesList:{html, data:{user_ids}}};
//   - история: JSON-RPC getMessagesHistory(passportId, page) → result.html;
//   - отправка: JSON-RPC sendMessage(passportId, text, []) → result (HTML сообщения).
// Ответы отдаются rendered HTML, поэтому парсим goquery (селекторы — ниже).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// Регулярки site-идентичности с авторизованной главной (dataFromBlade.layout).
var (
	reLoveUser   = regexp.MustCompile(`Love\.user\s*=\s*"?(\d+)"?`)
	rePassportID = regexp.MustCompile(`"passport_id"\s*:\s*"?(\d+)"?`)
	reLayoutNick = regexp.MustCompile(`"nick"\s*:\s*"([^"]*)"`)
)

func firstSubmatch(re *regexp.Regexp, body []byte) string {
	if m := re.FindSubmatch(body); m != nil {
		return string(m[1])
	}
	return ""
}

// Селекторы разметки talks (единый блок, дисциплина как у selNote* в parse.go).
const (
	// Список диалогов (buddy list). Элемент собеседника несёт data-атрибуты.
	selBuddyItem       = "[data-user-passport-id]"
	attrBuddyPassport  = "data-user-passport-id"
	attrBuddyNick      = "data-user-nick"
	attrBuddyProfileID = "data-user-id"
	selBuddyUnread     = ".lv-talks__unread-inbox"

	// Сообщение диалога (история и ответ sendMessage — одинаковые <li>).
	selMsgItem   = "li.lv-talks__message-item"
	classMsgOut  = "lv-talks__message-item_out" // исходящее; иначе входящее
	selMsgBox    = ".lv-msg__message-box"       // data-msg-id
	attrMsgID    = "data-msg-id"
	selMsgOut    = ".lv-msg__outmsg"
	selMsgIn     = ".lv-msg__inmsg"
	selMsgImage  = ".message-image img"
	selMsgAuthor = ".lv-talks__message-author"

	loadBuddiesPathFmt = "/ajax?request=loadBuddiesList&before=0&limit=%d&anticache=%d"
	talksHistoryLimit  = 20 // MSG_LIMIT сервера на страницу
)

// TalkDialog — один диалог в списке talks (метаданные для поллера и списка).
type TalkDialog struct {
	PassportID   string    // адресат диалога (/talks/<passport_id>)
	ProfileID    string    // id анкеты /profile/<id>/, если сайт его отдаёт
	Nick         string    // ник собеседника
	AvatarURL    string    // аватар собеседника (опц.)
	LastMsgID    string    // id последнего сообщения — сравниваем с курсором
	Unread       int       // непрочитанных (справочно; сигнал — курсор)
	LastActivity time.Time // время последней активности; zero — неизвестно
}

// TalkMessage — одно сообщение диалога talks.
type TalkMessage struct {
	SiteMsgID string    // id сообщения на сайте (24-hex)
	FromSelf  bool      // true — исходящее (написано владельцем сессии)
	Text      string    // текст сообщения
	MediaURL  string    // вложение (фото), если есть
	SentAt    time.Time // время по сайту; zero — неизвестно
}

// TalksDialogs возвращает список диалогов (собеседников) владельца сессии.
// limit ≤ 0 → talksLimit сайта (10). ErrUnauthorized — сессия истекла.
func (c *Client) TalksDialogs(ctx context.Context, cookies []*http.Cookie, limit int) ([]TalkDialog, error) {
	if limit <= 0 {
		limit = 10
	}
	path := fmt.Sprintf(loadBuddiesPathFmt, limit, time.Now().UnixMilli())
	body, err := c.getJSONBody(ctx, path, cookies)
	if err != nil {
		return nil, err
	}
	// Разметка списка лежит в loadBuddiesList.html (снято живьём в Ф0-прогоне);
	// data — сопутствующий объект {user_ids, html:""}, его html пустой. Раньше
	// парсер читал data.html и молча получал 0 диалогов.
	var env struct {
		LoadBuddiesList struct {
			HTML string `json:"html"`
		} `json:"loadBuddiesList"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, &SchemaError{Op: "loadBuddiesList", Detail: err.Error()}
	}
	return parseBuddies(env.LoadBuddiesList.HTML)
}

// TalksHistory возвращает сообщения диалога (страница 1 = последние 20). Сервер
// пагинирует по страницам, а не «после id», поэтому afterMsgID здесь не нужен —
// дедуп новых делает поллер по SiteMsgID. Сообщения — в порядке документа
// (старые→новые).
func (c *Client) TalksHistory(ctx context.Context, cookies []*http.Cookie, passportID, _ string, _ int) ([]TalkMessage, error) {
	pid, err := strconv.ParseInt(passportID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("talks history: паспорт %q: %w", passportID, err)
	}
	raw, err := c.rpc(ctx, cookies, "getMessagesHistory", pid, 1)
	if err != nil {
		return nil, err
	}
	var res struct {
		HTML string `json:"html"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, &SchemaError{Op: "getMessagesHistory", Detail: "result не {html}: " + err.Error()}
	}
	return parseMessages(res.HTML)
}

// TalksSend отправляет текстовое сообщение собеседнику от имени сессии. Ответ
// сервера — HTML созданного сообщения, из него достаём его site-id.
func (c *Client) TalksSend(ctx context.Context, cookies []*http.Cookie, passportID, text string) (TalkMessage, error) {
	pid, err := strconv.ParseInt(passportID, 10, 64)
	if err != nil {
		return TalkMessage{}, fmt.Errorf("talks send: паспорт %q: %w", passportID, err)
	}
	raw, err := c.rpc(ctx, cookies, "sendMessage", pid, text, []any{})
	if err != nil {
		return TalkMessage{}, err
	}
	// sendMessage отдаёт result строкой-HTML (в отличие от history {html}).
	html, err := resultHTML(raw)
	if err != nil {
		return TalkMessage{}, &SchemaError{Op: "sendMessage", Detail: err.Error()}
	}
	msgs, err := parseMessages(html)
	if err != nil || len(msgs) == 0 {
		// Отправлено, но id не распарсили — не критично (придёт историей/пушем).
		return TalkMessage{FromSelf: true, Text: text}, nil
	}
	m := msgs[len(msgs)-1]
	m.FromSelf = true
	return m, nil
}

// SiteIdentity снимает site-идентичность владельца сессии с авторизованной
// главной: id анкеты (Love.user), passport_id и ник (dataFromBlade.layout).
func (c *Client) SiteIdentity(ctx context.Context, cookies []*http.Cookie) (profileID, passportID, nick string, err error) {
	body, err := c.get(ctx, "/", cookies...)
	if err != nil {
		return "", "", "", err
	}
	profileID = firstSubmatch(reLoveUser, body)
	passportID = firstSubmatch(rePassportID, body)
	nick = firstSubmatch(reLayoutNick, body)
	if profileID == "" && passportID == "" {
		return "", "", "", &SchemaError{Op: "identity", Detail: "Love.user/passport_id не найдены (гость?)"}
	}
	return profileID, passportID, nick, nil
}

// resultHTML достаёт HTML из result RPC: либо строка, либо {html}.
func resultHTML(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var obj struct {
		HTML string `json:"html"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", err
	}
	return obj.HTML, nil
}

func parseBuddies(html string) ([]TalkDialog, error) {
	if strings.TrimSpace(html) == "" {
		return nil, nil // диалогов нет
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, &SchemaError{Op: "loadBuddiesList", Detail: err.Error()}
	}
	var dialogs []TalkDialog
	doc.Find(selBuddyItem).Each(func(_ int, s *goquery.Selection) {
		passport, _ := s.Attr(attrBuddyPassport)
		if passport == "" {
			return
		}
		nick, _ := s.Attr(attrBuddyNick)
		profileID, _ := s.Attr(attrBuddyProfileID)
		unread := 0
		if u := strings.TrimSpace(s.Find(selBuddyUnread).First().Text()); u != "" {
			unread, _ = strconv.Atoi(u)
		}
		dialogs = append(dialogs, TalkDialog{
			PassportID: passport,
			ProfileID:  profileID,
			Nick:       strings.TrimSpace(nick),
			Unread:     unread,
		})
	})
	return dialogs, nil
}

func parseMessages(html string) ([]TalkMessage, error) {
	if strings.TrimSpace(html) == "" {
		return nil, nil
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, &SchemaError{Op: "messages", Detail: err.Error()}
	}
	var msgs []TalkMessage
	doc.Find(selMsgItem).Each(func(_ int, s *goquery.Selection) {
		box := s.Find(selMsgBox).First()
		id, _ := box.Attr(attrMsgID)
		if id == "" {
			return
		}
		out := s.HasClass(classMsgOut)
		msgDiv := box.Find(selMsgIn).First()
		if out {
			msgDiv = box.Find(selMsgOut).First()
		}
		media, _ := s.Find(selMsgImage).First().Attr("src")
		msgs = append(msgs, TalkMessage{
			SiteMsgID: id,
			FromSelf:  out,
			Text:      messageText(msgDiv),
			MediaURL:  media,
		})
	})
	return msgs, nil
}

// messageText извлекает текст сообщения: в `.lv-msg__(in|out)msg` текст лежит
// голым узлом рядом со служебными <div> (автор, метка прочтения, картинки) —
// удаляем дочерние <div> и берём остаток.
func messageText(sel *goquery.Selection) string {
	if sel.Length() == 0 {
		return ""
	}
	clone := sel.Clone()
	clone.Find("div").Remove()
	return strings.TrimSpace(clone.Text())
}
