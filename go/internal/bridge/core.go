package bridge

// Core — мессенджер-агностичное ядро моста «ответ пользователя в треде →
// комментарий от его имени». Обработчики мессенджеров (Handler для Telegram,
// диспетчер maxx) разбирают свои апдейты и зовут ProcessReply со строковыми id
// сообщений (в Telegram — числа десятичной строкой, в MAX — mid).
//
// Куда уходит ответ, решает НЕ настройка, а то, чему он адресован:
//
//   - заметка или реплика НГС → на сайт, как и раньше; отказ сайта — не конец,
//     ответ уходит на площадку (с 17.08.2026 сайт отвечает 500 на любой
//     комментарий, и до этой ветки чужие ответы просто пропадали);
//   - заметка или реплика, написанная на площадке → сразу на площадку. Иначе и
//     быть не может: на НГС такой заметки нет вовсе, отвечать там нечему.
//
// Порядок «сначала сайт» выбран сознательно, хотя сегодня сайт мёртв. Пока он
// принимает комментарии, ответ обязан появиться и там: зеркало принесёт его
// обратно на площадку само, и копия выйдет ровно одна. Поменяй порядок — и при
// воскрешении НГС аудитория сайта перестала бы видеть ответы из мессенджеров.
//
// Цена ветки записана честно: 500 не означает, что сайт комментарий НЕ принял,
// поэтому в редком случае «принял и соврал» на площадке окажется две копии —
// наша и приехавшая зеркалом. Размен сделан в пользу «ответ не пропадает»:
// потерянная реплика живого человека хуже видимого дубля, который к тому же
// умеет убрать модератор.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/platform"
	"lovegw/internal/store"
)

// Platform — то, что мосту нужно от собственной площадки. Интерфейс, а не
// *platform.Platform, ради тестов: поднимать Postgres, чтобы проверить выбор
// адресата, было бы несоразмерно.
type Platform interface {
	CreateComment(ctx context.Context, in platform.NewComment) (int64, error)
	CommentRow(ctx context.Context, id int64) (platform.Comment, error)
}

type Core struct {
	st        *store.Store
	site      SitePoster
	notify    Notify
	messenger string
	log       *slog.Logger

	plat    Platform
	platURL string

	// told — кому уже сказали, что ответ уехал на площадку. В памяти, а не в
	// базе: сказать надо ОДИН раз (иначе при мёртвом НГС это письмо на каждую
	// реплику), но и схему заводить под такое не стоит — лишнее напоминание
	// после рестарта дешевле новой таблицы.
	mu   sync.Mutex
	told map[int64]bool
}

// NewCore создаёт ядро моста для одного мессенджера. notify (может быть
// nil) шлёт пользователю ЛС-подсказки про /login.
func NewCore(st *store.Store, site SitePoster, notify Notify, messenger string, log *slog.Logger) *Core {
	if log == nil {
		log = slog.Default()
	}
	if notify == nil {
		notify = func(context.Context, int64, string) {
			// ЛС недоступны — уведомления пользователям некуда слать.
		}
	}
	return &Core{st: st, site: site, notify: notify, messenger: messenger, log: log,
		told: map[int64]bool{}}
}

// SetPlatform подключает площадку запасным (а для своих заметок — единственным)
// адресатом ответов. Отдельным вызовом, а не параметром конструктора: мост
// поднимается вместе с ботами, а площадка позже и может не подняться вовсе.
func (c *Core) SetPlatform(p Platform, baseURL string) {
	c.plat = p
	c.platURL = strings.TrimSuffix(baseURL, "/")
}

// target — куда адресован ответ. Полей два комплекта, потому что сайт и
// площадка хранят обращение по-разному: сайту нужен префикс «Ник, » в ТЕЛЕ
// (иначе адресат потеряется), а площадка держит его ребром и дорисовывает на
// показе — там префикс в теле дал бы «Ник, Ник, ».
type target struct {
	noteID    string // id заметки: у зеркальной равен id на НГС
	comAPIID  string // id реплики НГС для POST; пусто — в корень заметки
	siteText  string // текст для сайта, с обращением
	body      string // текст для площадки, без обращения
	replyToID int64  // id реплики площадки-адресата; 0 — в корень заметки
	native    bool   // отвечают тому, чего на НГС нет: только на площадку
}

// ProcessReply отправляет ответ пользователя как комментарий от его имени.
// replyMsgID — id сообщения-ответа, replyToID — id сообщения, на которое
// ответили. At-most-once: сначала фиксация в processed_replies, потом отправка
// — потерянный при сбое комментарий лучше задвоенного.
func (c *Core) ProcessReply(ctx context.Context, replyMsgID string, userID int64, replyToID, text string) {
	t, ok := c.resolveTarget(ctx, replyToID, text)
	if !ok {
		return
	}

	fresh, err := c.st.TryMarkReplyProcessed(ctx, c.messenger, replyMsgID, time.Now())
	if err != nil {
		c.log.Error("дедуп ответа", "message", replyMsgID, "err", err)
		return
	}
	if !fresh {
		return // повторная доставка после рестарта
	}

	if t.native {
		// На НГС этой заметки (или этой реплики) нет вовсе — там отвечать нечему.
		// И объявлять о переезде ответа не надо: человек и так отвечает нашему.
		c.toPlatform(ctx, replyMsgID, userID, t, leadNative, false)
		return
	}

	cookies, ok := c.userCookies(ctx, userID)
	if !ok {
		return
	}
	if err := c.site.PostComment(ctx, cookies, t.noteID, t.comAPIID, t.siteText); err != nil {
		c.log.Warn("комментарий не ушёл на сайт", "note", t.noteID, "user", userID, "err", err)
		if isAuthError(err) {
			// Протухшая сессия — не повод нести реплику на площадку: человеку
			// в любом случае идти делать /login, и своя подсказка у этого своя.
			c.invalidateSession(ctx, userID)
			return
		}
		c.toPlatform(ctx, replyMsgID, userID, t, leadSiteRefused, true)
		return
	}
	if err := c.st.SetSessionValid(ctx, c.messenger, userID, true, time.Now()); err != nil {
		c.log.Error("отметка last_ok_at", "user", userID, "err", err)
	}
	c.log.Info("ответ отправлен на сайт",
		"note", t.noteID, "com_api_id", t.comAPIID, "user", userID, "messenger", c.messenger)
}

// Начало сообщений человеку: почему его ответ вообще оказался на площадке.
// Строками рядом, а не по месту, потому что читать их надо ВМЕСТЕ — они делят
// одно продолжение (joinInvite) и не должны повторять друг друга.
const (
	leadSiteRefused = "НГС ваш ответ не принял. "
	leadNative      = "Эта заметка написана на площадке — на НГС её нет. "
)

// toPlatform публикует ответ на площадке от имени владельца анкеты. lead —
// начало сообщения человеку, announce — говорить ли ему об успехе (у своей
// заметки не о чем: он и так отвечает нашему).
func (c *Core) toPlatform(ctx context.Context, replyMsgID string, userID int64,
	t target, lead string, announce bool) {
	if c.plat == nil {
		c.notify(ctx, userID, lead+"Повторите позже — сам я не перешлю, чтобы не задвоить комментарий.")
		return
	}
	noteID, err := strconv.ParseInt(t.noteID, 10, 64)
	if err != nil {
		c.log.Error("id заметки для площадки", "note", t.noteID, "err", err)
		return
	}
	author, ok := c.platformAuthor(ctx, userID, lead)
	if !ok {
		return
	}

	id, err := c.plat.CreateComment(ctx, platform.NewComment{
		NoteID: noteID, AuthorID: author, Body: t.body, ReplyToID: t.replyToID,
	})
	if err != nil {
		c.refusedByPlatform(ctx, userID, lead, err)
		return
	}
	// Реплика УЖЕ стоит в треде — это сообщение самого человека. Отмечаем её
	// отправленной в этот мессенджер, и исходящий обход площадки копию сюда не
	// принесёт; в остальные — принесёт, там её ещё нет.
	if err := c.st.SetTarget(ctx, c.messenger, store.TargetComment,
		strconv.FormatInt(id, 10), replyMsgID, ""); err != nil {
		c.log.Error("отметка своей реплики в мессенджере", "comment", id, "err", err)
	}
	c.log.Info("ответ отправлен на площадку",
		"note", noteID, "comment", id, "reply_to", t.replyToID, "user", userID, "messenger", c.messenger)
	if announce {
		c.tellOnce(ctx, userID, lead)
	}
}

// platformAuthor — id участника площадки по владельцу сессии мессенджера.
// Он же номер анкеты НГС: полоса зеркальных идентификаторов устроена так, что
// id строки РАВЕН id на сайте, и вход на площадку не переносит ни строки.
func (c *Core) platformAuthor(ctx context.Context, userID int64, lead string) (int64, bool) {
	profile, _, _, err := c.st.SessionIdentity(ctx, c.messenger, userID)
	if errors.Is(err, store.ErrNotFound) || profile == "" {
		c.notify(ctx, userID, lead+c.joinInvite())
		return 0, false
	}
	if err != nil {
		c.log.Error("чтение анкеты сессии", "user", userID, "err", err)
		return 0, false
	}
	id, err := strconv.ParseInt(profile, 10, 64)
	if err != nil || !platform.IsNGS(id) {
		c.log.Error("id анкеты сессии не разобран", "user", userID, "profile", profile)
		return 0, false
	}
	return id, true
}

// refusedByPlatform объясняет человеку отказ площадки. Отдельно разобран только
// «ещё не участник»: это не поломка, а приглашение, и адресовано оно ровно
// тому, кто уже пишет в тред.
func (c *Core) refusedByPlatform(ctx context.Context, userID int64, lead string, err error) {
	if errors.Is(err, platform.ErrNotMember) {
		c.log.Info("ответ на площадку не ушёл: человек ещё не участник", "user", userID)
		c.notify(ctx, userID, lead+c.joinInvite())
		return
	}
	c.log.Warn("ответ на площадку не ушёл", "user", userID, "err", err)
	c.notify(ctx, userID, lead+"Площадка тоже не приняла: "+err.Error()+".")
}

// joinInvite — приглашение войти на площадку. Зовём того, кто уже пишет в
// тред: лучшего момента не будет, а без входа опубликовать его слова от его
// имени мы не вправе — согласие на распространение даётся на площадке и
// только самим человеком.
func (c *Core) joinInvite() string {
	if c.platURL == "" {
		return "Чтобы отвечать, нужно войти на площадку."
	}
	return "Чтобы отвечать, войдите на " + c.platURL +
		" по номеру своей анкеты НГС — и ваши ответы отсюда будут уходить туда."
}

// tellOnce говорит человеку, что ответ уехал на площадку, — один раз за жизнь
// процесса. Молчать нельзя (он писал, думая, что пишет на НГС), повторять на
// каждую реплику тоже: при мёртвом сайте это письмо на каждое сообщение.
func (c *Core) tellOnce(ctx context.Context, userID int64, lead string) {
	c.mu.Lock()
	seen := c.told[userID]
	c.told[userID] = true
	c.mu.Unlock()
	if seen {
		return
	}
	where := "на площадку"
	if c.platURL != "" {
		where = "на площадку " + c.platURL
	}
	c.notify(ctx, userID, lead+"Я отнёс его "+where+
		" — там его видят все. Дальше буду делать так же и молча.")
}

// resolveTarget определяет, куда адресован ответ: в корень заметки (реплай на
// корень треда) или на конкретную реплику (реплай на сообщение бота).
//
// Смотрит в message_targets НАПРЯМУЮ, а не через таблицы notes/comments:
// заметка и реплика, написанные на площадке, в зеркальной базе не существуют
// вовсе, и джойн с ней потерял бы ровно их.
func (c *Core) resolveTarget(ctx context.Context, replyToID, text string) (target, bool) {
	noteID, found, err := c.st.RefByThread(ctx, c.messenger, store.TargetNoteThread, replyToID)
	if err != nil {
		c.log.Error("поиск заметки по треду", "thread", replyToID, "err", err)
		return target{}, false
	}
	if found {
		return target{noteID: noteID, siteText: text, body: text,
			native: isNativeID(noteID)}, true
	}

	comID, found, err := c.st.RefByMessage(ctx, c.messenger, store.TargetComment, replyToID)
	if err != nil {
		c.log.Error("поиск реплики по сообщению", "message", replyToID, "err", err)
		return target{}, false
	}
	if !found {
		return target{}, false // ответ не на наше сообщение
	}
	if isNativeID(comID) {
		return c.nativeTarget(ctx, comID, text)
	}

	cm, err := c.st.CommentByTarget(ctx, c.messenger, replyToID)
	if err != nil {
		c.log.Error("чтение реплики", "message", replyToID, "comment", comID, "err", err)
		return target{}, false
	}
	id, _ := strconv.ParseInt(comID, 10, 64)
	return target{
		noteID: cm.NoteID, comAPIID: comID,
		// Сайту обращение нужно в теле — иначе адресат потеряется совсем.
		// Площадке — ребром: префикс она дорисует из ТЕКУЩЕГО ника адресата.
		siteText: fmt.Sprintf("%s, %s", cm.AuthorName, text), body: text,
		replyToID: id,
	}, true
}

// nativeTarget — ответ на реплику, написанную на площадке. Заметку спрашиваем у
// самой площадки: в зеркальной базе такой реплики нет, а знать, к какой заметке
// она относится, кроме площадки, некому.
func (c *Core) nativeTarget(ctx context.Context, comID, text string) (target, bool) {
	id, err := strconv.ParseInt(comID, 10, 64)
	if err != nil {
		return target{}, false
	}
	if c.plat == nil {
		return target{}, false
	}
	cm, err := c.plat.CommentRow(ctx, id)
	if err != nil {
		c.log.Error("чтение реплики площадки", "comment", id, "err", err)
		return target{}, false
	}
	return target{
		noteID: strconv.FormatInt(cm.NoteID, 10), body: text, siteText: text,
		replyToID: id, native: true,
	}, true
}

// isNativeID — идентификатор выдан площадкой, а не НГС. Полосы не
// пересекаются, поэтому вопрос решается самим числом.
func isNativeID(s string) bool {
	id, err := strconv.ParseInt(s, 10, 64)
	return err == nil && platform.IsNative(id)
}

// userCookies достаёт живые куки пользователя; при их отсутствии
// подсказывает пользователю сделать /login.
func (c *Core) userCookies(ctx context.Context, userID int64) ([]*http.Cookie, bool) {
	cookiesJSON, valid, err := c.st.SessionCookies(ctx, c.messenger, userID)
	if errors.Is(err, store.ErrNotFound) {
		c.log.Info("ответ без сессии", "user", userID)
		c.notify(ctx, userID, "Чтобы ваши ответы попадали на сайт, войдите: /login")
		return nil, false
	}
	if err != nil {
		c.log.Error("чтение сессии", "user", userID, "err", err)
		return nil, false
	}
	if !valid {
		c.notify(ctx, userID, "Сессия сайта истекла. Сделайте /login ещё раз")
		return nil, false
	}
	cookies, err := love.CookiesFromJSON([]byte(cookiesJSON), time.Now())
	if err != nil {
		c.log.Error("разбор кук", "user", userID, "err", err)
		return nil, false
	}
	if len(cookies) == 0 {
		c.invalidateSession(ctx, userID)
		return nil, false
	}
	return cookies, true
}

func (c *Core) invalidateSession(ctx context.Context, userID int64) {
	if err := c.st.SetSessionValid(ctx, c.messenger, userID, false, time.Now()); err != nil {
		c.log.Error("инвалидация сессии", "user", userID, "err", err)
	}
	c.notify(ctx, userID, "Сессия сайта истекла. Сделайте /login ещё раз")
}

// isAuthError — «сессия протухла»: сайт ответил 401/403 на POST. Типизированные
// ошибки ставит love.drainOK; 403 бывает и баном IP, но пользователю в обоих
// случаях поможет только /login.
func isAuthError(err error) bool {
	return errors.Is(err, love.ErrUnauthorized) || errors.Is(err, love.ErrForbidden)
}
