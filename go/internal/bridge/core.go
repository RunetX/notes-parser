package bridge

// Core — мессенджер-агностичное ядро моста «ответ пользователя в треде →
// комментарий на сайте от его имени». Обработчики мессенджеров (Handler для
// Telegram, диспетчер maxx) разбирают свои апдейты и зовут ProcessReply со
// строковыми id сообщений (в Telegram — числа десятичной строкой, в MAX —
// mid).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

type Core struct {
	st        *store.Store
	site      SitePoster
	notify    Notify
	messenger string
	log       *slog.Logger
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
	return &Core{st: st, site: site, notify: notify, messenger: messenger, log: log}
}

// ProcessReply отправляет ответ пользователя на сайт как комментарий от его
// имени. replyMsgID — id сообщения-ответа, replyToID — id сообщения, на
// которое ответили. At-most-once: сначала фиксация в processed_replies,
// потом POST — потерянный при сбое комментарий лучше задвоенного.
func (c *Core) ProcessReply(ctx context.Context, replyMsgID string, userID int64, replyToID, text string) {
	noteID, comAPIID, text, ok := c.resolveTarget(ctx, replyToID, text)
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

	cookies, ok := c.userCookies(ctx, userID)
	if !ok {
		return
	}
	if err := c.site.PostComment(ctx, cookies, noteID, comAPIID, text); err != nil {
		c.log.Warn("комментарий не ушёл на сайт", "note", noteID, "user", userID, "err", err)
		if isAuthError(err) {
			c.invalidateSession(ctx, userID)
		}
		return
	}
	if err := c.st.SetSessionValid(ctx, c.messenger, userID, true, time.Now()); err != nil {
		c.log.Error("отметка last_ok_at", "user", userID, "err", err)
	}
	c.log.Info("ответ отправлен на сайт",
		"note", noteID, "com_api_id", comAPIID, "user", userID, "messenger", c.messenger)
}

// resolveTarget определяет, куда адресован ответ: в корень заметки (реплай
// на корень треда) или на конкретный комментарий (реплай на сообщение бота
// с комментарием — текст получает префикс «Автор, ...»).
func (c *Core) resolveTarget(ctx context.Context, replyToID, text string) (noteID, comAPIID, outText string, ok bool) {
	if n, err := c.st.NoteByThread(ctx, c.messenger, replyToID); err == nil {
		return n.ID, "", text, true
	} else if !errors.Is(err, store.ErrNotFound) {
		c.log.Error("поиск заметки по треду", "thread", replyToID, "err", err)
		return "", "", "", false
	}

	cm, err := c.st.CommentByTarget(ctx, c.messenger, replyToID)
	if errors.Is(err, store.ErrNotFound) {
		return "", "", "", false // ответ не на наше сообщение
	}
	if err != nil {
		c.log.Error("поиск комментария", "message", replyToID, "err", err)
		return "", "", "", false
	}
	return cm.NoteID, fmt.Sprintf("%d", cm.ID),
		fmt.Sprintf("%s, %s", cm.AuthorName, text), true
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
