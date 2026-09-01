package dmbot

// Вход на площадку «Зазеркалье» ссылкой из бота (/site).
//
// Ключ идёт БОТ → ЧЕЛОВЕК → ПЛОЩАДКА, и направление здесь несущее, а не
// удобное. Обратное — «площадка показала ссылку, человек открыл её в боте» —
// ломается подсунутой ссылкой: жертва подтвердила бы чужой вход своим же
// аккаунтом, и войти под её именем смог бы тот, кто ссылку прислал. При нашем
// направлении подсовывать нечего: ключ рождается в личном разговоре с тем, кто
// его получит.
//
// Доказательством права на анкету служит то, что у бота уже ЛЕЖИТ живая сессия
// НГС этого человека: пароль он отдал однажды и осознанно, вот этому боту.
// Площадка пароля не спрашивает и не будет — вводить пароль сайта на чужом
// домене есть привычка, из-за которой одна подделка адреса собирает пароли
// всего сообщества.

import (
	"context"
	"strconv"
	"time"
)

// SiteLogin (опц.) — площадка, умеющая впустить владельца анкеты по одноразовой
// ссылке. Способность необязательная: без площадки (или без её адреса) команды
// /site просто нет, как нет /profile у клиента без ProfileControl.
//
// Возвращается ГОТОВАЯ ссылка, а не ключ: собрать её можно только зная base_url
// площадки, а диалоговое ядро о существовании адресов не знает и знать не
// должно — оно и про НГС-то знает лишь через интерфейсы.
type SiteLogin interface {
	BotLoginLink(ctx context.Context, profileID int64, nick, messenger string, messengerUserID int64) (string, time.Time, error)
}

// SetSiteLogin подключает выдачу ссылок входа: без неё команды /site нет ни в
// меню, ни в разборе. Как и все Set*-инжекции, зовётся строго до старта
// поллеров (фаза wire в runDaemon) — поля не под мьютексом.
func (l *Logic) SetSiteLogin(s SiteLogin) {
	if s == nil {
		return
	}
	l.siteLogin = s
}

// handleSite выдаёт ссылку входа на площадку.
func (l *Logic) handleSite(ctx context.Context, userID int64) {
	if l.siteLogin == nil {
		return
	}
	// Сессия нужна ЖИВАЯ, а не когда-то бывшая: ключ входа выдаётся под
	// доказательство, а протухшая кука ничего не доказывает. Отказ объясняет
	// себя сам и зовёт к /login.
	if _, ok := l.siteCookies(ctx, userID); !ok {
		return
	}
	profileID, nick, ok := l.siteProfileID(ctx, userID)
	if !ok {
		return
	}
	link, expires, err := l.siteLogin.BotLoginLink(ctx, profileID, nick, l.messenger, userID)
	if err != nil {
		l.log.Error("ссылка входа на площадку", "user", userID, "profile", profileID, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	// Минуты считаем от полученного срока, а не пишем «десять» словом: число
	// живёт в ядре площадки (platform.BotLoginTTL), и разошедшись с ним текст
	// стал бы врать — то же правило, по которому справка берёт окно правки из
	// ядра, а не пишет его цифрой.
	mins := int(time.Until(expires).Round(time.Minute) / time.Minute)
	l.tr.Send(ctx, userID,
		"Вход на «Зазеркалье» — по этой ссылке:\n"+link+
			"\n\nОна одноразовая и живёт "+strconv.Itoa(mins)+" мин. "+
			"Открывать её нужно там, где вы хотите войти. Никому не пересылайте: "+
			"кто перешёл, тот и вошёл под вашим именем.")
}

// siteProfileID — номер анкеты владельца сессии. Снимается со страницы сайта при
// /login, но у сессий прошлых релизов его нет вовсе — тогда снимаем сейчас, раз
// куки живые, и перечитываем. Молча вернуть «не получилось» здесь нельзя: без
// номера анкеты входить некому.
func (l *Logic) siteProfileID(ctx context.Context, userID int64) (int64, string, bool) {
	profileID, _, nick, err := l.st.SessionIdentity(ctx, l.messenger, userID)
	if err != nil {
		l.log.Error("чтение site-идентичности", "user", userID, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return 0, "", false
	}
	if profileID == "" {
		if cookies, ok := l.siteCookies(ctx, userID); ok {
			l.captureIdentity(ctx, userID, cookies)
			profileID, _, nick, err = l.st.SessionIdentity(ctx, l.messenger, userID)
			if err != nil {
				l.log.Error("чтение site-идентичности", "user", userID, "err", err)
				l.tr.Send(ctx, userID, msgInternalError)
				return 0, "", false
			}
		}
	}
	id, convErr := strconv.ParseInt(profileID, 10, 64)
	if profileID == "" || convErr != nil || id <= 0 {
		l.tr.Send(ctx, userID,
			"Не удалось определить номер вашей анкеты на сайте. "+
				"Попробуйте войти заново: /login")
		return 0, "", false
	}
	return id, nick, true
}
