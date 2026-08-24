package dmbot

// /morning — ручка утренней заметки (пакет morning). Команда админская, как
// /news и /pulpit: посторонним она отвечает как несуществующая и в списке
// команд не значится.
//
// Пакет morning отсюда НЕ импортируется: ручке нужны включить, выключить и
// показать отчёт, а отчёт служба собирает сама — иначе диалоговому ядру
// пришлось бы знать её состояния, календари и предохранитель.
//
// Выключение мгновенное, включение после срабатывания предохранителя — через
// подтверждение с текстом причины: предохранитель гаснет на подозрении, что
// нам запретили писать, и второй заход под запретом стоит второго бана.

import (
	"context"
	"strconv"

	"lovegw/internal/kbd"
)

const (
	btnMorningOn  = "▶️ Включить"
	btnMorningOff = "⛔ Выключить"
	btnMorningYes = "▶️ Да, включить"
)

// MorningControl — служба утренней заметки глазами ЛС-бота (реализует
// *morning.Service).
type MorningControl interface {
	// Status — готовый отчёт админу, состояние тумблера и причина последнего
	// автоматического выключения ("" — предохранитель не срабатывал).
	Status(ctx context.Context) (report string, enabled bool, offReason string)
	// SetEnabled переключает тумблер; by — кто это сделал, для журнала.
	SetEnabled(ctx context.Context, on bool, by string) error
}

// SetMorning подключает ручку: /morning станет доступна пользователю adminID.
// Как и все Set*-инжекции, зовётся строго до старта поллеров (фаза wire в
// runDaemon) — поля не под мьютексом.
func (l *Logic) SetMorning(svc MorningControl, adminID int64) {
	if svc == nil || adminID == 0 {
		return
	}
	l.morning, l.adminID = svc, adminID
}

// isMorningAdmin — можно ли этому пользователю трогать утреннюю заметку.
func (l *Logic) isMorningAdmin(userID int64) bool {
	return l.morning != nil && l.adminID != 0 && userID == l.adminID
}

// handleMorning показывает состояние и кнопку действия. cb == nil — пришли
// командой, шлём новое сообщение; иначе правим своё же.
func (l *Logic) handleMorning(ctx context.Context, userID int64, cb *kbd.Callback) {
	if !l.isMorningAdmin(userID) {
		l.tr.Send(ctx, userID, msgUnknownCommand)
		return
	}
	report, enabled, offReason := l.morning.Status(ctx)
	l.showMorning(ctx, userID, cb, report, enabled, offReason)
}

func (l *Logic) showMorning(ctx context.Context, userID int64, cb *kbd.Callback,
	report string, enabled bool, offReason string) {
	l.show(ctx, userID, cb, report, morningKeyboard(enabled, offReason))
}

// morningKeyboard — кнопка под отчётом. Включение после предохранителя ведёт
// через подтверждение, обычное — сразу.
func morningKeyboard(enabled bool, offReason string) *kbd.Keyboard {
	if enabled {
		return kbd.New().Row(kbd.Button{
			Text: btnMorningOff, Payload: kbd.Pack(verbMorningSet, argMorningOff),
		})
	}
	verb := verbMorningSet
	if offReason != "" {
		verb = verbMorningAsk
	}
	return kbd.New().Row(kbd.Button{
		Text: btnMorningOn, Payload: kbd.Pack(verb, argMorningOn),
	})
}

// askMorningOn — подтверждение включения после срабатывания предохранителя:
// причина повторяется прямо в вопросе, чтобы её нельзя было не прочитать.
func (l *Logic) askMorningOn(ctx context.Context, userID int64, cb kbd.Callback) {
	if !l.isMorningAdmin(userID) {
		l.tr.Send(ctx, userID, msgUnknownCommand)
		return
	}
	report, enabled, offReason := l.morning.Status(ctx)
	if enabled || offReason == "" {
		// Пока кнопка висела, состояние изменилось: показываем как есть.
		l.showMorning(ctx, userID, &cb, report, enabled, offReason)
		return
	}
	l.replace(ctx, userID, cb,
		"Утренняя заметка выключилась сама: "+offReason+"\n\nВключить снова? Если это был "+
			"запрет писать в раздел, вторая попытка под запретом стоит второго бана.",
		kbd.New().Row(
			kbd.Button{Text: btnMorningYes, Payload: kbd.Pack(verbMorningSet, argMorningOn)},
			kbd.Button{Text: btnCancel, Payload: kbd.Pack(verbCancel, "")},
		))
}

// setMorning переключает тумблер и перерисовывает отчёт.
func (l *Logic) setMorning(ctx context.Context, userID int64, cb kbd.Callback, arg string) {
	if !l.isMorningAdmin(userID) {
		l.tr.Send(ctx, userID, msgUnknownCommand)
		return
	}
	on := arg == argMorningOn
	if err := l.morning.SetEnabled(ctx, on, "admin:"+strconv.FormatInt(userID, 10)); err != nil {
		l.log.Error("переключение утренней заметки", "user", userID, "on", on, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	report, enabled, offReason := l.morning.Status(ctx)
	l.showMorning(ctx, userID, &cb, report, enabled, offReason)
}
