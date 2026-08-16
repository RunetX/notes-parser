package talks

// Живучесть поллера: сессии владельцев, троттлинг алертов, kill-switch.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"lovegw/internal/love"
)

// Ключи уведомлений админу.
const (
	keyForbidden   = "доступ к сайту talks (403)"
	keyDrift       = "ошибка API talks"
	keyUnavailable = "сайт talks недоступен"
)

const sessionExpiredMsg = "🔒 Сессия НГС.Лав истекла — личные сообщения на паузе. Войдите снова: /login"

// cookies читает и разбирает куки сессии; невалидную/битую помечает invalid.
func (w *Watcher) cookies(ctx context.Context, messenger string, owner int64) ([]*http.Cookie, bool) {
	cookiesJSON, valid, err := w.st.SessionCookies(ctx, messenger, owner)
	if err != nil {
		// Чаще всего это «нет ключа шифрования» или чужой ключ: владелец молча
		// выпадает из обхода, и без записи в лог причину не найти.
		w.log.Error("не прочитать сессию владельца", "messenger", messenger,
			"owner", owner, "err", err)
		return nil, false
	}
	if !valid {
		return nil, false
	}
	cookies, err := love.CookiesFromJSON([]byte(cookiesJSON), time.Now())
	if err != nil || len(cookies) == 0 {
		_ = w.st.SetSessionValid(ctx, messenger, owner, false, time.Now())
		return nil, false
	}
	return cookies, true
}

// invalidateOwner помечает сессию владельца невалидной и уведомляет его о
// повторном входе. В мультисессии истёкшая сессия ОДНОГО пользователя (гостевой
// ответ talks, ErrUnauthorized) не должна ронять поллер для остальных — в отличие
// от 403/дрейфа, которые глобальны и ведут к kill-switch. Невалидная сессия
// выпадает из плана доставки (TalksOwners берёт только valid=1), поэтому
// уведомление уходит один раз.
func (w *Watcher) invalidateOwner(ctx context.Context, tr PMTransport, owner int64) {
	if err := w.st.SetSessionValid(ctx, tr.Name(), owner, false, time.Now()); err != nil {
		w.log.Error("сброс истёкшей сессии talks", "messenger", tr.Name(), "user", owner, "err", err)
	}
	if _, err := tr.SendPM(ctx, owner, sessionExpiredMsg); err != nil {
		w.log.Debug("уведомление о протухшей сессии не отправлено", "user", owner, "err", err)
	}
	w.log.Info("сессия talks истекла — на паузе до /login", "messenger", tr.Name(), "user", owner)
}

// handleSiteError троттлит алерт и после ForbiddenLimit подряд ошибок сайта
// глушит поллер (kill-switch), не трогая зеркало.
func (w *Watcher) handleSiteError(ctx context.Context, err error) {
	// Временный отказ (5xx фронта, обрыв связи) в kill-switch не идёт: он
	// проходит сам, а остановленный до рестарта поллер теряет входящие ЛС —
	// история сайта отдаёт только последнюю страницу, и уехавшее за неё живым
	// дозабором уже не достать. Такой сбой копится в алертере: три подряд —
	// одно сообщение админу, первый успех — «восстановилось». Поллинг при этом
	// продолжается на холостом интервале. Боевой случай: 502 на
	// loadBuddiesList 12.08.2026 — поллер лёг до ручного рестарта.
	if errors.Is(err, love.ErrSiteUnavailable) {
		w.alert.Fail(ctx, keyUnavailable, err.Error())
		return
	}
	w.errStreak++
	key, detail := keyDrift, err.Error()
	if errors.Is(err, love.ErrForbidden) {
		key, detail = keyForbidden, "сайт вернул 403 (геоблок или бан IP)"
	}
	if w.errStreak >= w.cfg.ForbiddenLimit {
		w.stop(ctx, key+": "+detail)
		return
	}
	w.alert.Fail(ctx, key, detail)
}

func (w *Watcher) onSiteOK(ctx context.Context) {
	w.errStreak = 0
	w.alert.OK(ctx, keyForbidden)
	w.alert.OK(ctx, keyDrift)
	w.alert.OK(ctx, keyUnavailable)
}

func (w *Watcher) stop(ctx context.Context, reason string) {
	if w.stopped {
		return
	}
	w.stopped = true
	w.log.Error("поллер talks остановлен (kill-switch)", "reason", reason)
	if w.cfg.AlertSend != nil {
		w.cfg.AlertSend(ctx, "поллер talks остановлен: "+reason+". Зеркало работает; включить снова — рестарт.")
	}
}
