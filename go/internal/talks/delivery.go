package talks

// Куда носить личные сообщения. Одно входящее ЛС нельзя доставить в два
// мессенджера: сайт помечает сообщение прочитанным в тот момент, когда поллер
// забирает историю, — второй сессии достанется пустота (жалоба «просмотрены, но
// не отправлены», разбор 11.08.2026). Поэтому у сайт-аккаунта ровно один
// получатель. Выбирает его человек кнопкой в ЛС (dmbot, `/delivery`); пока
// выбора нет, носим в самый свежий вход и один раз спрашиваем.

import (
	"context"
	"net/http"
	"time"

	"lovegw/internal/store"
)

// plan — кому и в каком мессенджере носим ЛС в этом такте: мессенджер →
// владельцы сессий. Решение межмессенджерное, поэтому считается разом по всем
// сессиям, а не по одному мессенджеру за раз.
func (w *Watcher) plan(ctx context.Context) map[string][]store.TalksOwner {
	all, err := w.st.TalksOwners(ctx)
	if err != nil {
		w.log.Error("список сессий talks", "err", err)
		return nil
	}
	out := make(map[string][]store.TalksOwner, len(w.transports))
	for _, group := range store.GroupByAccount(w.live(all)) {
		win, ok := store.PickDelivery(group)
		if !ok {
			continue // человек отказался от доставки во всех мессенджерах
		}
		w.askDelivery(ctx, group, win)
		if w.unreachable[ownerKey{win.Messenger, win.UserID}] {
			// Выбранному доставлять некуда (заблокировал бота). Молча увести
			// личную переписку в другой мессенджер нельзя — это его выбор.
			continue
		}
		out[win.Messenger] = append(out[win.Messenger], win)
	}
	return out
}

// live отсеивает сессии, не участвующие в обходе вовсе: чужие в admin-only,
// отказавшиеся в конфиге и недостижимые. Недостижимость снимает сессию только
// пока выбора не было: у выбравшего заблокированный бот означает «ЛС не
// доставляются», а не «доставим в другой мессенджер».
func (w *Watcher) live(all []store.TalksOwner) []store.TalksOwner {
	live := make([]store.TalksOwner, 0, len(all))
	for _, o := range all {
		k := ownerKey{o.Messenger, o.UserID}
		switch {
		case w.cfg.AdminOnly && o.UserID != w.cfg.AdminIDs[o.Messenger]:
		case w.excluded[k]:
		case w.unreachable[k] && o.Delivery != store.DeliveryOn:
		default:
			live = append(live, o)
		}
	}
	return live
}

// askDelivery один раз спрашивает человека, куда носить ЛС, когда его
// сайт-аккаунт залогинен в нескольких мессенджерах. Спрашиваем во всех — где
// нажмут, туда и понесём; до ответа носим в win. Отметка «спросили» живёт в БД,
// иначе каждый рестарт демона переспрашивал бы заново.
func (w *Watcher) askDelivery(ctx context.Context, group []store.TalksOwner, win store.TalksOwner) {
	if w.cfg.AskDelivery == nil || len(group) < 2 {
		return
	}
	for _, o := range group {
		if o.Delivery != store.DeliveryUnset {
			return // выбор уже сделан — спрашивать не о чем
		}
	}
	for _, o := range group {
		if o.Asked {
			continue
		}
		w.cfg.AskDelivery(ctx, o.Messenger, o.UserID, win)
		if err := w.st.MarkTalksAsked(ctx, o.Messenger, o.UserID, time.Now()); err != nil {
			w.log.Error("отметка вопроса о доставке ЛС", "messenger", o.Messenger, "user", o.UserID, "err", err)
		}
		w.log.Info("спросил, куда носить личные сообщения",
			"messenger", o.Messenger, "user", o.UserID, "пока_носим_в", win.Messenger)
	}
}

// captureIdentity лениво снимает паспорт владельца сессии. Сессии, заведённые до
// появления talks, его не хранят, а без паспорта два входа одного человека не
// связать — и настройка доставки для них не работает вовсе. Одна попытка на
// сессию за запуск демона: страница отдаёт идентичность не всегда, а платить за
// это запросом к сайту каждый такт незачем.
func (w *Watcher) captureIdentity(ctx context.Context, o store.TalksOwner, cookies []*http.Cookie) {
	ident, ok := w.site.(SiteIdentifier)
	k := ownerKey{o.Messenger, o.UserID}
	if !ok || o.PassportID != "" || w.identityTried[k] {
		return
	}
	w.identityTried[k] = true
	if err := w.limiter.Wait(ctx); err != nil {
		return
	}
	profileID, passportID, nick, err := ident.SiteIdentity(ctx, cookies)
	if err != nil || passportID == "" {
		w.log.Debug("паспорт владельца сессии не снят",
			"messenger", o.Messenger, "user", o.UserID, "err", err)
		return
	}
	if err := w.st.SetSessionIdentity(ctx, o.Messenger, o.UserID, profileID, passportID, nick); err != nil {
		w.log.Error("сохранение site-идентичности",
			"messenger", o.Messenger, "user", o.UserID, "err", err)
		return
	}
	w.log.Info("паспорт владельца сессии снят задним числом",
		"messenger", o.Messenger, "user", o.UserID)
}
