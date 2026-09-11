package main

// Вход на площадку ссылкой из бота — склейка двух миров.
//
// Право на анкету доказывается ТАМ, где лежит живая сессия НГС (SQLite демона),
// а впускает ПЛОЩАДКА (Postgres). Ни один из двух пакетов не должен знать про
// другой: dmbot видит узкий интерфейс SiteLogin, площадка — просто выдачу
// ключа, — а знание о том, что это один и тот же человек, живёт здесь, в сборке
// команды, которая и так знает оба мира.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"lovegw/internal/dmbot"
	"lovegw/internal/platform"
)

// botSiteLogin — реализация dmbot.SiteLogin поверх ядра площадки.
type botSiteLogin struct {
	p       *platform.Platform
	baseURL string
}

// BotLoginLink заводит одноразовый ключ и собирает ссылку.
//
// Тень заводится ЗДЕСЬ, до выдачи ключа, и это не мелочь: у человека, который на
// НГС ничего не писал, строки в users нет вовсе, а CompleteBotLogin намеренно
// отказывается заводить её сам — ника у него на руках нет, вышел бы участник без
// имени. Заодно EnsureShadow освежает ник: он же latest-wins и только у тени, то
// есть выбранный на площадке ник этим не переписывается.
func (b botSiteLogin) BotLoginLink(ctx context.Context, profileID int64, nick, messenger string, messengerUserID int64) (string, time.Time, error) {
	if nick == "" {
		// Ник нужен на случай, когда тени ещё нет: users.nick — NOT NULL, и
		// пустой оставил бы на площадке безымянного участника. Номер анкеты
		// честнее пустоты и сменить его человек может сам на /me.
		nick = fmt.Sprintf("Анкета %d", profileID)
	}
	if _, err := b.p.EnsureShadow(ctx, platform.MirroredAuthor{ID: profileID, Nick: nick}); err != nil {
		return "", time.Time{}, fmt.Errorf("тень для входа: %w", err)
	}
	key, expires, err := b.p.StartBotLogin(ctx, profileID, messenger, messengerUserID)
	if err != nil {
		return "", time.Time{}, err
	}
	return strings.TrimRight(b.baseURL, "/") + "/login/bot?key=" + url.QueryEscape(key), expires, nil
}

// BindOffer — чья запись стоит за кодом привязки. Код НЕ гасится: бот сперва
// показывает ник, и только нажатие «Привязать» его тратит.
func (b botSiteLogin) BindOffer(ctx context.Context, code string) (string, error) {
	_, nick, err := b.p.BindingOffer(ctx, code)
	return nick, bindErr(err)
}

// Bind гасит код и заводит привязку.
func (b botSiteLogin) Bind(ctx context.Context, code, messenger string, messengerUserID int64) (string, error) {
	_, nick, err := b.p.BindMessenger(ctx, code, messenger, messengerUserID)
	return nick, bindErr(err)
}

// BoundLoginLink — ссылка входа по УЖЕ существующей привязке: та же дорога, что
// у BotLoginLink, только право доказано не сессией НГС, а привязкой.
//
// EnsureShadow здесь не зовётся намеренно: привязка бывает только у участника,
// то есть строка в users заведомо есть, — а освежать ник у участника нельзя
// вовсе (ник, выбранный на площадке, сильнее ника с сайта).
func (b botSiteLogin) BoundLoginLink(ctx context.Context, messenger string, messengerUserID int64) (string, time.Time, error) {
	userID, _, err := b.p.MessengerLogin(ctx, messenger, messengerUserID)
	if err != nil {
		return "", time.Time{}, bindErr(err)
	}
	// Ключ помечается ПРИВЯЗКОЙ, а не анкетой: при завершении входа по нему
	// решается, делать ли обряд анкеты, — а пришедший этой дорогой про НГС
	// ничего не доказывал и анкеты может не иметь вовсе.
	key, expires, err := b.p.StartBoundLogin(ctx, userID, messenger, messengerUserID)
	if err != nil {
		return "", time.Time{}, err
	}
	return strings.TrimRight(b.baseURL, "/") + "/login/bot?key=" + url.QueryEscape(key), expires, nil
}

// bindErr переводит отказы ядра в отказы диалогового ядра. Перевод стоит ЗДЕСЬ,
// на границе, потому что dmbot не импортирует platform вовсе — про площадку он
// знает ровно столько, сколько назвал интерфейс. Тот же приём, что у web.ErrNoProfile.
func bindErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, platform.ErrBindCodeInvalid):
		return dmbot.ErrBindCodeInvalid
	case errors.Is(err, platform.ErrMessengerTaken):
		return dmbot.ErrMessengerTaken
	case errors.Is(err, platform.ErrNoBinding), errors.Is(err, platform.ErrUnknownMessenger):
		return dmbot.ErrNoBinding
	// bindableGuard зовётся ВТОРОЙ раз, уже при самой привязке, и к этому
	// моменту человек мог перестать годиться: администратор обезличил его, отзыв
	// согласия увёл запись в тень. Без перевода наружу уезжала бы сырая ошибка
	// ядра, а человек видел бы «внутреннюю ошибку» вместо причины.
	case errors.Is(err, platform.ErrAnonymized), errors.Is(err, platform.ErrNotMember),
		errors.Is(err, platform.ErrNotFound):
		return dmbot.ErrBindNotAllowed
	}
	return err
}

// setupSiteLogin подключает /site обоим ботам команд.
//
// Условий три, и каждое означает «команды просто нет», а не аварию: без площадки
// впускать некуда, без base_url ссылку не из чего собрать, без ботов её некому
// отдать. Молчаливое отсутствие здесь правильнее ошибки — площадка и демон
// живут и по отдельности.
func (d *daemon) setupSiteLogin() {
	if d.plat == nil || d.cfg.Platform.BaseURL == "" {
		return
	}
	login := botSiteLogin{p: d.plat, baseURL: d.cfg.Platform.BaseURL}
	bots := 0
	if d.dm != nil {
		d.dm.SetSiteLogin(login)
		d.dm.SetSiteBinding(login)
		bots++
	}
	if d.maxDM != nil {
		d.maxDM.SetSiteLogin(login)
		d.maxDM.SetSiteBinding(login)
		bots++
	}
	if bots > 0 {
		d.log.Info("вход на площадку из бота включён", "ботов", bots)
	}
}
