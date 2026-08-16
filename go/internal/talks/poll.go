package talks

// Обход сайта: цикл поллера, план на такт, дозабор истории диалога.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// undeliveredWindow — сколько ещё пытаться дослать входящее, застрявшее из-за
// сбоя мессенджера. Граница нужна: история сайта отдаёт только первую страницу
// (20 сообщений), и уехавшее за неё живым дозабором уже не достать — без окна
// такой хвост заставлял бы перезапрашивать диалог каждый такт вечно. Что старше
// — только руками: `lovegw talks watch -backfill N`.
const undeliveredWindow = 48 * time.Hour

// staleUnreadAfter — как долго верить залипшему счётчику непрочитанных, прежде
// чем перепроверить диалог живьём. Дельта счётчика слепа к случаю «сайт погасил
// непрочитанное нашим же чтением истории, а потом пришло одно новое»: счётчик
// возвращается к тому же значению, при котором мы читали, и новым не считается.
// Один лишний запрос в четверть часа на диалог с непрочитанным — дешевле
// потерянного сообщения; доставка дедуплицирована по message_targets.
const staleUnreadAfter = 15 * time.Minute

// Run крутит поллер до отмены контекста (или срабатывания kill-switch).
func (w *Watcher) Run(ctx context.Context) error {
	if len(w.transports) == 0 {
		return nil
	}
	w.log.Info("поллер talks запущен",
		"admin_only", w.cfg.AdminOnly, "allow_send", w.cfg.AllowSend,
		"store_text", w.cfg.StoreText, "interval", w.cfg.Interval)
	for ctx.Err() == nil {
		active := w.pollOnce(ctx)
		if w.stopped {
			return nil // kill-switch: зеркало продолжает работать
		}
		wait := w.cfg.IdleInterval
		if active {
			wait = w.cfg.Interval
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

// PollOnce делает один проход поллера и возвращает, была ли активность.
// Для отладочного/безопасного прогона `lovegw talks watch -once`.
func (w *Watcher) PollOnce(ctx context.Context) bool { return w.pollOnce(ctx) }

// pollOnce обходит все транспорты и их владельцев сессий один раз. План
// доставки считается разом на весь такт: у сайт-аккаунта, залогиненного в двух
// мессенджерах, получатель ровно один (см. delivery.go).
func (w *Watcher) pollOnce(ctx context.Context) bool {
	active := false
	plan := w.plan(ctx)
	for _, tr := range w.transports {
		for _, owner := range plan[tr.Name()] {
			if w.pollOwner(ctx, tr, owner) {
				active = true
			}
			if w.stopped {
				return active
			}
		}
	}
	return active
}

// pollOwner опрашивает список диалогов одного владельца и дозабирает новые.
func (w *Watcher) pollOwner(ctx context.Context, tr PMTransport, o store.TalksOwner) bool {
	messenger, owner := tr.Name(), o.UserID
	cookies, ok := w.cookies(ctx, messenger, owner)
	if !ok {
		return false
	}
	w.captureIdentity(ctx, o, cookies)
	if err := w.limiter.Wait(ctx); err != nil {
		return false
	}
	dialogs, err := w.site.Dialogs(ctx, cookies, w.cfg.MaxDialogs)
	if err != nil {
		if errors.Is(err, love.ErrUnauthorized) {
			w.invalidateOwner(ctx, tr, owner) // сессия этого юзера истекла — не роняем поллер
			return false
		}
		w.handleSiteError(ctx, err)
		return false
	}
	w.onSiteOK(ctx)

	active, fetches := false, 0
	for _, d := range dialogs {
		if fetches >= w.cfg.MaxDialogs {
			break
		}
		fetched, act := w.processDialog(ctx, tr, owner, cookies, d)
		if fetched {
			fetches++
		}
		if act {
			active = true
		}
		if w.stopped {
			break
		}
	}
	return active
}

// processDialog заводит/обновляет собеседника и, если есть новая активность,
// дозабирает историю после курсора и доставляет входящие. Курсор двигается
// только по успешно доставленным сообщениям — недоставленные переотправятся
// повторным History на следующем тике (важно при store_text=false: текст
// берётся живым из истории, а не из БД).
func (w *Watcher) processDialog(ctx context.Context, tr PMTransport, owner int64, cookies []*http.Cookie, d love.TalkDialog) (fetched, active bool) {
	peer, err := w.peerFor(ctx, tr.Name(), owner, d)
	if err != nil {
		w.log.Error("собеседник talks", "err", err)
		return false, false
	}
	if !w.needsFetch(ctx, tr.Name(), peer, d) {
		w.lastUnread[peer.ID] = d.Unread // фиксируем и без дозабора — см. коммент к полю
		return false, false
	}

	if err := w.limiter.Wait(ctx); err != nil {
		return false, false
	}
	msgs, err := w.site.History(ctx, cookies, d.PassportID, peer.CursorMsgID, w.cfg.HistoryLimit)
	fetched = true
	if err != nil {
		if errors.Is(err, love.ErrUnauthorized) {
			w.invalidateOwner(ctx, tr, owner)
			return fetched, false
		}
		w.handleSiteError(ctx, err)
		return fetched, false
	}
	w.onSiteOK(ctx)
	w.lastUnread[peer.ID] = d.Unread // дозабор состоялся — запоминаем счётчик
	w.lastFetchAt[peer.ID] = time.Now()

	delivered, upto, err := w.deliverBatch(ctx, tr, owner, peer, msgs)
	if err != nil {
		w.log.Error("запись сообщения talks", "err", err)
	}
	// Курсор — на последнем полностью обработанном сообщении: всё, что за ним,
	// придёт повторным History следующим тиком.
	if upto > 0 && msgs[upto-1].SiteMsgID != peer.CursorMsgID {
		last := msgs[upto-1]
		if err := w.st.SetPeerCursor(ctx, peer.ID, last.SiteMsgID, last.SentAt); err != nil {
			w.log.Error("сдвиг курсора talks", "err", err)
		}
	}
	return fetched, delivered > 0
}

// needsFetch решает, дозабирать ли историю диалога. Сигнал новой активности:
// либо last-msg-id диалога сдвинулся (если сайт его отдаёт — loadBuddiesList
// его НЕ отдаёт, поэтому в бою работает только вторая ветка), либо число
// непрочитанных отличается от виденного в прошлый раз.
//
// Третья ветка — недоставленное: сбой мессенджера не должен съедать сообщение.
// Курсор на нём стоит, но одного этого мало — дозабор запускают счётчики, а они
// после чтения истории уже погашены (сайт помечает сообщения прочитанными), и
// повторной попытки не наступало бы никогда. Поэтому спрашиваем БД: есть ли
// входящее без записи в message_targets. Признак переживает и рестарт демона,
// в отличие от любого флага в памяти.
func (w *Watcher) needsFetch(ctx context.Context, messenger string, peer store.TalkPeer, d love.TalkDialog) bool {
	if d.LastMsgID != "" && d.LastMsgID != peer.CursorMsgID {
		return true
	}
	if d.Unread > 0 && d.Unread != w.lastUnread[peer.ID] {
		return true
	}
	if d.Unread > 0 && time.Since(w.lastFetchAt[peer.ID]) >= staleUnreadAfter {
		return true // счётчик залип на прежнем значении — см. staleUnreadAfter
	}
	pending, err := w.st.HasUndeliveredIncoming(ctx, messenger, peer.ID, time.Now().Add(-undeliveredWindow))
	if err != nil {
		w.log.Error("проверка недоставленных ЛС talks", "peer", peer.ID, "err", err)
		return false
	}
	if pending {
		w.log.Debug("в диалоге есть недоставленное ЛС — переспрашиваю историю", "peer", peer.ID)
	}
	return pending
}
