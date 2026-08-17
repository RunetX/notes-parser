package talks

// Читать ли переписку и куда её носить.
//
// Читать — только с согласия человека: обход ходит по сайту под его кукой, сайт
// при чтении истории помечает сообщения прочитанными (собеседник видит
// «просмотрено») и всё это время держит человека онлайн. Пока согласия нет,
// сайт-аккаунт не трогаем вовсе — ни списка диалогов, ни истории — и один раз
// спрашиваем (`sessions.talks_scan`, миграция v10).
//
// Носить — ровно в один мессенджер: то же самое чтение истории гасит
// непрочитанное, и второй сессии достанется пустота (жалоба «просмотрены, но не
// отправлены», разбор 11.08.2026). Выбирает человек кнопкой в ЛС (dmbot,
// `/delivery`); нажатая в ответ на вопрос кнопка «читать и присылать сюда» —
// это сразу и согласие, и выбор.

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
		if !store.ScanAllowed(group) {
			// Согласия читать переписку нет — спрашиваем один раз и уходим.
			// Проверка стоит после live(): у отсеянного админом ничего не
			// спрашиваем, за него уже решили.
			w.askScan(ctx, group)
			continue
		}
		win, ok := store.PickDelivery(group)
		if !ok {
			continue // человек отказался от доставки во всех мессенджерах
		}
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

// askScan один раз спрашивает у человека согласия читать его переписку.
// Сайт-аккаунт, залогиненный в нескольких мессенджерах, спрашиваем во всех: где
// нажмут, туда ЛС и понесём (нажатая кнопка отвечает сразу на оба вопроса).
// Отметка «спросили» живёт в БД, иначе каждый рестарт демона переспрашивал бы
// заново; неотвеченный вопрос оставляет переписку непрочитанной — настройка
// ждёт человека под `/delivery`.
func (w *Watcher) askScan(ctx context.Context, group []store.TalksOwner) {
	if w.cfg.AskScan == nil {
		return
	}
	for _, o := range group {
		if o.Asked {
			continue
		}
		w.cfg.AskScan(ctx, o.Messenger, o.UserID, len(group) > 1)
		if err := w.st.MarkTalksAsked(ctx, o.Messenger, o.UserID, time.Now()); err != nil {
			w.log.Error("отметка вопроса о чтении переписки", "messenger", o.Messenger, "user", o.UserID, "err", err)
		}
		w.log.Info("спросил согласия читать личную переписку",
			"messenger", o.Messenger, "user", o.UserID, "сессий_у_аккаунта", len(group))
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
