package talks

// Доставка входящих ЛС в мессенджер: запись в БД, дедуп, распознавание
// «доставлять некуда», ручной бэкфилл.

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// peerFor заводит/обновляет собеседника по диалогу сайта и возвращает его
// строку целиком (в ней курсор и время последнего события).
func (w *Watcher) peerFor(ctx context.Context, messenger string, owner int64, d love.TalkDialog) (store.TalkPeer, error) {
	peerID, err := w.st.UpsertTalkPeer(ctx, store.TalkPeer{
		Messenger: messenger, OwnerUserID: owner, PassportID: d.PassportID,
		ProfileID: d.ProfileID, Nick: d.Nick, AvatarURL: d.AvatarURL,
	})
	if err != nil {
		return store.TalkPeer{}, fmt.Errorf("upsert собеседника talks: %w", err)
	}
	return w.st.TalkPeerByID(ctx, peerID)
}

// deliverBatch записывает сообщения диалога в БД по порядку и доставляет
// входящие в чат target. upto — сколько сообщений обработано полностью: дальше
// него курсор двигать нельзя, там осталось недоставленное. Обрыв на первой
// неудаче намеренный: доставку валит либо ошибка БД, либо недостижимый чат —
// в обоих случаях следующие сообщения упадут так же, а порядок в ЛС важнее
// полноты одного такта.
func (w *Watcher) deliverBatch(ctx context.Context, tr PMTransport, target int64, peer store.TalkPeer, msgs []love.TalkMessage) (delivered, upto int, err error) {
	for _, m := range msgs {
		rowID, _, err := w.st.InsertTalkMessage(ctx, toStoreMsg(peer.ID, m, w.cfg.StoreText))
		if err != nil {
			return delivered, upto, err
		}
		ok, stop := w.deliverOne(ctx, tr, target, peer, m, rowID)
		if ok {
			delivered++
		}
		if stop {
			return delivered, upto, nil
		}
		upto++
	}
	return delivered, upto, nil
}

// deliverOne доставляет одно входящее сообщение (идемпотентно по
// message_targets). stop=true — доставка не удалась, курсор двигать нельзя.
func (w *Watcher) deliverOne(ctx context.Context, tr PMTransport, owner int64, peer store.TalkPeer, m love.TalkMessage, rowID int64) (delivered, stop bool) {
	if m.FromSelf || rowID == 0 {
		return false, false // своё исходящее не доставляем; курсор идёт дальше
	}
	_, _, found, err := w.st.Target(ctx, tr.Name(), store.TargetPMMessage, itoa(rowID))
	if err != nil {
		// Эта проверка — единственный дедуп доставки: ошибка БД здесь не
		// «не доставлено», а «неизвестно». Останавливаемся, не двигая курсор:
		// задержка лучше дубля в ЛС.
		w.log.Error("проверка доставки ЛС talks", "peer", peer.ID, "row", rowID, "err", err)
		return false, true
	}
	if found {
		return false, false // уже доставлено ранее
	}
	msgID, err := tr.SendPM(ctx, owner, formatIncoming(w.cfg.BaseURL, peer, m))
	if err != nil {
		w.log.Warn("доставка ЛС talks не удалась", "user", owner, "err", err)
		if isUnreachable(err) {
			w.markUnreachable(ctx, tr.Name(), owner, err)
		}
		return false, true
	}
	if err := w.st.SetTarget(ctx, tr.Name(), store.TargetPMMessage, itoa(rowID), msgID, ""); err != nil {
		w.log.Error("привязка доставленного ЛС talks", "err", err)
	}
	return true, false
}

// isUnreachable — отказ мессенджера, который повтором не лечится: пользователь
// заблокировал бота или ни разу не открывал с ним диалог (первым бот писать не
// может). Сравниваем строки, а не типизированные ошибки: SDK у мессенджеров
// разные, общего кода отказа нет — в Telegram это «Forbidden: bot was blocked by
// the user» и «Bad Request: chat not found», в MAX — код `dialog.not.found`
// («sending message failed: dialog.not.found : Dialog not found», снято с боя
// 12.08.2026). Цена промаха здесь не «одно потерянное сообщение», а поток:
// нераспознанный отказ заставляет поллер читать переписку человека каждый такт,
// сайт при каждом чтении помечает её прочитанной, а доставки нет — ЛС пропадают
// молча. Неопознанный отказ всё же считаем временным: снять с обхода живого
// человека хуже, чем лишний раз попробовать.
func isUnreachable(err error) bool {
	s := strings.ToLower(err.Error())
	for _, m := range []string{
		"blocked by the user",
		"chat not found",
		"dialog.not.found", // MAX: человек не открывал диалог с ботом
		"user is deactivated",
		"bot can't initiate conversation",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// markUnreachable снимает владельца сессии с обхода и один раз сообщает об этом
// админу: сам пользователь уведомления не получит — доставлять ему как раз и
// нечем.
func (w *Watcher) markUnreachable(ctx context.Context, messenger string, owner int64, cause error) {
	k := ownerKey{messenger, owner}
	if w.unreachable[k] {
		return
	}
	w.unreachable[k] = true
	w.log.Warn("ЛС talks доставлять некуда — диалоги этого пользователя больше не опрашиваю",
		"messenger", messenger, "user", owner, "err", cause)
	if w.cfg.AlertSend != nil {
		w.cfg.AlertSend(ctx, fmt.Sprintf(
			"личная переписка не доставляется пользователю %d (%s): %v. Он заблокировал бота или не открывал с ним диалог; опрос его диалогов остановлен до рестарта демона.",
			owner, messenger, cause))
	}
}

// DeliverExisting принудительно доставляет последние perDialog ВХОДЯЩИХ из
// первых maxDialogs диалогов каждого владельца, игнорируя триггер непрочитанного
// — для обкатки/бэкфилла: показать уже существующую переписку в мессенджере,
// когда всё на сайте прочитано. Читаем под сессией владельца, а доставляем в
// deliverTo (0 — самому владельцу): удобно, когда демонстрируешь в другой
// аккаунт. Идемпотентно по message_targets (повтор не задваивает); на сайт
// ничего не пишет. Возвращает число доставленных сообщений.
func (w *Watcher) DeliverExisting(ctx context.Context, maxDialogs, perDialog int, deliverTo int64) (int, error) {
	total := 0
	plan := w.plan(ctx)
	for _, tr := range w.transports {
		for _, o := range plan[tr.Name()] {
			owner := o.UserID
			cookies, ok := w.cookies(ctx, tr.Name(), owner)
			if !ok {
				continue
			}
			target := deliverTo
			if target == 0 {
				target = owner
			}
			if err := w.limiter.Wait(ctx); err != nil {
				return total, err
			}
			dialogs, err := w.site.Dialogs(ctx, cookies, maxDialogs)
			if err != nil {
				return total, err
			}
			for i, d := range dialogs {
				if i >= maxDialogs {
					break
				}
				n, err := w.deliverTail(ctx, tr, owner, target, cookies, d, perDialog)
				if err != nil {
					return total, err
				}
				total += n
			}
		}
	}
	return total, nil
}

// deliverTail доставляет последние perDialog входящих одного диалога
// (perDialog ≤ 0 — все со страницы истории) в чат target, не двигая
// курсор/триггеры. owner — владелец сессии (ключ собеседника), target — куда слать.
func (w *Watcher) deliverTail(ctx context.Context, tr PMTransport, owner, target int64, cookies []*http.Cookie, d love.TalkDialog, perDialog int) (int, error) {
	peer, err := w.peerFor(ctx, tr.Name(), owner, d)
	if err != nil {
		return 0, err
	}
	if err := w.limiter.Wait(ctx); err != nil {
		return 0, err
	}
	msgs, err := w.site.History(ctx, cookies, d.PassportID, "", w.cfg.HistoryLimit)
	if err != nil {
		return 0, err
	}
	var incoming []love.TalkMessage
	for _, m := range msgs {
		if !m.FromSelf {
			incoming = append(incoming, m)
		}
	}
	if perDialog > 0 && len(incoming) > perDialog {
		incoming = incoming[len(incoming)-perDialog:]
	}
	n, _, err := w.deliverBatch(ctx, tr, target, peer, incoming)
	return n, err
}
