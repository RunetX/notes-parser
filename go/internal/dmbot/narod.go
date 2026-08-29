package dmbot

// /narod — ручка народа (эпик «народ»): жители площадки, реплики которых пишет
// модель. Команда админская, как /news, /pulpit и /morning: посторонним она
// отвечает как несуществующая и в списке команд не значится.
//
// Пакет narod отсюда НЕ импортируется — по тому же правилу, что и у соседей:
// ручке нужны «переключить» и «показать отчёт», а отчёт собирает тот, у кого на
// руках мир и каталог карточек.
//
// ПОДТВЕРЖДЕНИЯ У ВКЛЮЧЕНИЯ ЗДЕСЬ НЕТ, и это отличие от амвона с утром не
// недосмотр. Там кнопка гасится ПРЕДОХРАНИТЕЛЕМ — на подозрении, что нам
// запретили писать в раздел, — и второй заход под запретом стоит второго бана,
// поэтому включение обязано быть осознанным. У народа предохранителя нет вовсе:
// он живёт в своей песочнице, где ни бана, ни чужой модерации не бывает, а
// перерасход останавливает суточный потолок, который сам же и отпускает в
// полночь. Спрашивать «вы уверены?» там, где цена ошибки — несколько центов,
// значит приучать нажимать «да» не глядя.
//
// Выключение мгновенное: если владелец решил, что хватит, спрашивать не о чем.

import (
	"context"
	"strconv"

	"lovegw/internal/kbd"
)

const (
	btnNarodOn  = "▶️ Включить"
	btnNarodOff = "⛔ Выключить"
)

// NarodControl — служба народа глазами ЛС-бота.
type NarodControl interface {
	// NarodStatus — готовый отчёт админу и состояние тумблера.
	NarodStatus(ctx context.Context) (report string, enabled bool)
	// SetNarodEnabled переключает тумблер; by — кто это сделал, для журнала.
	SetNarodEnabled(ctx context.Context, on bool, by string) error
}

// SetNarod подключает ручку: /narod станет доступна пользователю adminID. Как и
// все Set*-инжекции, зовётся строго до старта поллеров (фаза wire в runDaemon) —
// поля не под мьютексом.
func (l *Logic) SetNarod(svc NarodControl, adminID int64) {
	if svc == nil || adminID == 0 {
		return
	}
	l.narod, l.adminID = svc, adminID
}

// isNarodAdmin — можно ли этому пользователю трогать народ.
func (l *Logic) isNarodAdmin(userID int64) bool {
	return l.narod != nil && l.adminID != 0 && userID == l.adminID
}

// handleNarod показывает состояние и кнопку действия. cb == nil — пришли
// командой, шлём новое сообщение; иначе правим своё же.
func (l *Logic) handleNarod(ctx context.Context, userID int64, cb *kbd.Callback) {
	if !l.isNarodAdmin(userID) {
		l.tr.Send(ctx, userID, msgUnknownCommand)
		return
	}
	report, enabled := l.narod.NarodStatus(ctx)
	l.showNarod(ctx, userID, cb, report, enabled)
}

func (l *Logic) showNarod(ctx context.Context, userID int64, cb *kbd.Callback,
	report string, enabled bool) {
	l.show(ctx, userID, cb, report, narodKeyboard(enabled))
}

// narodKeyboard — одна кнопка под отчётом.
func narodKeyboard(enabled bool) *kbd.Keyboard {
	if enabled {
		return kbd.New().Row(kbd.Button{
			Text: btnNarodOff, Payload: kbd.Pack(verbNarodSet, argNarodOff),
		})
	}
	return kbd.New().Row(kbd.Button{
		Text: btnNarodOn, Payload: kbd.Pack(verbNarodSet, argNarodOn),
	})
}

// setNarod переключает тумблер и перерисовывает отчёт.
func (l *Logic) setNarod(ctx context.Context, userID int64, cb kbd.Callback, arg string) {
	if !l.isNarodAdmin(userID) {
		l.tr.Send(ctx, userID, msgUnknownCommand)
		return
	}
	on := arg == argNarodOn
	if err := l.narod.SetNarodEnabled(ctx, on, "admin:"+strconv.FormatInt(userID, 10)); err != nil {
		l.log.Error("переключение народа", "user", userID, "on", on, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	report, enabled := l.narod.NarodStatus(ctx)
	l.showNarod(ctx, userID, &cb, report, enabled)
}
