package talks

// Обратное направление: ответ пользователя в мессенджере → сообщение на сайте.
//
// Согласием на чтение переписки (`talks_scan`, см. delivery.go) это направление
// НЕ закрыто, и намеренно: отправляет человек сам, своими руками, и на сайте от
// этого ничего не помечается прочитанным. Выключивший чтение просто перестаёт
// получать новые ЛС — старые диалоги (`/talks`, `/talk`) остаются при нём.

import (
	"context"
	"errors"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// HandleReply маршрутизирует ответ пользователя (реплай на доставленное ЛС) в
// диалог сайта. replyMsgID — id самого ответа (для at-most-once), replyToID —
// id сообщения, на которое ответили. Возвращает true, если сообщение относится
// к talks (обработано — дальше его никто не трогает).
func (w *Watcher) HandleReply(ctx context.Context, messenger, replyMsgID string, userID int64, replyToID, text string) bool {
	if text == "" {
		return false
	}
	peer, err := w.st.PeerByDeliveredPM(ctx, messenger, replyToID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			w.log.Error("поиск диалога по реплаю", "err", err)
		}
		return false // ответ не на talks-сообщение — не наш
	}
	if peer.OwnerUserID != userID {
		w.log.Warn("реплай на чужой диалог talks — игнор", "owner", peer.OwnerUserID, "from", userID)
		return true
	}
	tr := w.transportFor(messenger)
	if tr == nil {
		return false
	}
	if !w.cfg.AllowSend {
		tr.Confirm(ctx, userID, replyMsgID, false)
		return true
	}
	// At-most-once ДО отправки: потерянный ответ лучше задвоенного (как в bridge).
	fresh, err := w.st.TryMarkReplyProcessed(ctx, messenger, "dm:"+replyMsgID, time.Now())
	if err != nil || !fresh {
		return true
	}
	w.SendToPeer(ctx, tr, userID, peer, replyMsgID, text)
	return true
}

// SendToPeer отправляет текст в диалог сайта от имени сессии пользователя и
// подтверждает реакцией. Используется и реплаем (HandleReply), и командой /talk
// (Ф3, залипание на диалоге).
func (w *Watcher) SendToPeer(ctx context.Context, tr PMTransport, userID int64, peer store.TalkPeer, ackID, text string) {
	messenger := tr.Name()
	cookies, ok := w.cookies(ctx, messenger, userID)
	if !ok {
		tr.Confirm(ctx, userID, ackID, false)
		return
	}
	if err := w.limiter.Wait(ctx); err != nil {
		tr.Confirm(ctx, userID, ackID, false)
		return
	}
	sent, err := w.site.Send(ctx, cookies, peer.PassportID, text)
	if err != nil {
		if errors.Is(err, love.ErrUnauthorized) {
			w.invalidateOwner(ctx, tr, userID)
		} else {
			w.log.Warn("отправка ЛС на сайт не удалась", "user", userID, "err", err)
		}
		tr.Confirm(ctx, userID, ackID, false)
		return
	}
	_ = w.st.SetSessionValid(ctx, messenger, userID, true, time.Now())
	text2 := text
	if !w.cfg.StoreText {
		text2 = ""
	}
	if _, _, err := w.st.InsertTalkMessage(ctx, store.TalkMessage{
		PeerID: peer.ID, SiteMsgID: sent.SiteMsgID, Direction: store.TalkOut,
		Text: text2, MediaURL: sent.MediaURL, SentAt: sent.SentAt,
	}); err != nil {
		w.log.Error("запись исходящего talks", "err", err)
	}
	tr.Confirm(ctx, userID, ackID, true)
}

// SendToDialog отправляет текст в диалог по его id — команда /talk (залипание
// на собеседнике). Проверяет, что диалог принадлежит этому пользователю в этом
// мессенджере. false — диалог чужой/не найден. ackID — id сообщения-исходника
// для реакции-подтверждения.
func (w *Watcher) SendToDialog(ctx context.Context, messenger string, userID, peerID int64, ackID, text string) bool {
	if text == "" {
		return false
	}
	peer, err := w.st.TalkPeerByID(ctx, peerID)
	if err != nil || peer.Messenger != messenger || peer.OwnerUserID != userID {
		return false
	}
	tr := w.transportFor(messenger)
	if tr == nil {
		return false
	}
	if !w.cfg.AllowSend {
		tr.Confirm(ctx, userID, ackID, false)
		return true
	}
	w.SendToPeer(ctx, tr, userID, peer, ackID, text)
	return true
}

// TransportFor возвращает транспорт мессенджера (для проводки команд в Ф3).
func (w *Watcher) TransportFor(messenger string) PMTransport { return w.transportFor(messenger) }

func (w *Watcher) transportFor(messenger string) PMTransport {
	for _, tr := range w.transports {
		if tr.Name() == messenger {
			return tr
		}
	}
	return nil
}
