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
	"fmt"
	"net/url"
	"strings"
	"time"

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
		bots++
	}
	if d.maxDM != nil {
		d.maxDM.SetSiteLogin(login)
		bots++
	}
	if bots > 0 {
		d.log.Info("вход на площадку из бота включён", "ботов", bots)
	}
}
