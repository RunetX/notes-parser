package dmbot

// /pulpit — ручка амвона: свои реплики под новыми заметками сайта (пакет
// pulpit). Команда админская, как /news: посторонним она отвечает как
// несуществующая и в списке команд не значится.
//
// Пакет pulpit отсюда НЕ импортируется: ручке нужны включить, выключить и
// показать отчёт, а отчёт служба собирает сама — иначе диалоговому ядру
// пришлось бы знать её состояния, формы и предохранитель.
//
// Выключение мгновенное: если владелец решил, что хватит, спрашивать не о чем.
// А вот включение после срабатывания предохранителя идёт через подтверждение —
// предохранитель гаснет только на подозрении в бане, и повторный заход под
// запретом стоит второго бана.

import (
	"context"
	"strconv"

	"lovegw/internal/kbd"
)

const (
	msgPulpitOff = "Амвон сейчас недоступен."
	btnPulpitOn  = "▶️ Включить"
	btnPulpitOff = "⛔ Выключить"
	btnPulpitYes = "▶️ Да, включить"
)

// PulpitControl — служба амвона глазами ЛС-бота (реализует *pulpit.Service).
type PulpitControl interface {
	// PulpitStatus — готовый отчёт админу, состояние тумблера и причина
	// последнего автоматического выключения ("" — предохранитель не срабатывал).
	PulpitStatus(ctx context.Context) (report string, enabled bool, offReason string)
	// SetPulpitEnabled переключает тумблер; by — кто это сделал, для журнала.
	SetPulpitEnabled(ctx context.Context, on bool, by string) error
}

// SetPulpit подключает ручку амвона: /pulpit станет доступна пользователю
// adminID. Ставится, как и SetNews, уже после старта поллера — тот же латентный
// дата-рейс, что и у всех остальных Set*-подключений (чинить его стоит разом
// для всех, отдельной задачей).
func (l *Logic) SetPulpit(svc PulpitControl, adminID int64) {
	if svc == nil || adminID == 0 {
		return
	}
	l.pulpit, l.adminID = svc, adminID
}

// isPulpitAdmin — можно ли этому пользователю трогать амвон.
func (l *Logic) isPulpitAdmin(userID int64) bool {
	return l.pulpit != nil && l.adminID != 0 && userID == l.adminID
}

// handlePulpit показывает состояние амвона и кнопку действия. cb == nil —
// пришли командой, шлём новое сообщение; иначе правим своё же.
func (l *Logic) handlePulpit(ctx context.Context, userID int64, cb *kbd.Callback) {
	if !l.isPulpitAdmin(userID) {
		l.tr.Send(ctx, userID, msgUnknownCommand)
		return
	}
	report, enabled, offReason := l.pulpit.PulpitStatus(ctx)
	l.showPulpit(ctx, userID, cb, report, enabled, offReason)
}

// showPulpit рисует отчёт с одной кнопкой: включить или выключить.
func (l *Logic) showPulpit(ctx context.Context, userID int64, cb *kbd.Callback,
	report string, enabled bool, offReason string) {
	kb := pulpitKeyboard(enabled, offReason)
	if cb == nil {
		l.tr.SendKeyboard(ctx, userID, report, kb)
		return
	}
	l.replace(ctx, userID, *cb, report, kb)
}

// pulpitKeyboard — кнопка под отчётом. Включение после предохранителя ведёт
// через подтверждение (см. askPulpitOn), обычное — сразу.
func pulpitKeyboard(enabled bool, offReason string) *kbd.Keyboard {
	if enabled {
		return kbd.New().Row(kbd.Button{
			Text: btnPulpitOff, Payload: kbd.Pack(verbPulpitSet, argPulpitOff),
		})
	}
	verb := verbPulpitSet
	if offReason != "" {
		verb = verbPulpitAsk
	}
	return kbd.New().Row(kbd.Button{
		Text: btnPulpitOn, Payload: kbd.Pack(verb, argPulpitOn),
	})
}

// askPulpitOn — подтверждение включения после срабатывания предохранителя:
// причина повторяется прямо в вопросе, чтобы её нельзя было не прочитать.
func (l *Logic) askPulpitOn(ctx context.Context, userID int64, cb kbd.Callback) {
	if !l.isPulpitAdmin(userID) {
		l.tr.Send(ctx, userID, msgUnknownCommand)
		return
	}
	report, enabled, offReason := l.pulpit.PulpitStatus(ctx)
	if enabled || offReason == "" {
		// Пока кнопка висела, состояние изменилось: показываем как есть.
		l.showPulpit(ctx, userID, &cb, report, enabled, offReason)
		return
	}
	l.replace(ctx, userID, cb,
		"Амвон выключился сам: "+offReason+"\n\nВключить снова? Если это был запрет "+
			"писать в раздел, вторая попытка под запретом стоит второго бана.",
		kbd.New().Row(
			kbd.Button{Text: btnPulpitYes, Payload: kbd.Pack(verbPulpitSet, argPulpitOn)},
			kbd.Button{Text: btnCancel, Payload: kbd.Pack(verbCancel, "")},
		))
}

// setPulpit переключает тумблер и перерисовывает отчёт.
func (l *Logic) setPulpit(ctx context.Context, userID int64, cb kbd.Callback, arg string) {
	if !l.isPulpitAdmin(userID) {
		l.tr.Send(ctx, userID, msgUnknownCommand)
		return
	}
	on := arg == argPulpitOn
	if err := l.pulpit.SetPulpitEnabled(ctx, on, "admin:"+strconv.FormatInt(userID, 10)); err != nil {
		l.log.Error("переключение амвона", "user", userID, "on", on, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	report, enabled, offReason := l.pulpit.PulpitStatus(ctx)
	l.showPulpit(ctx, userID, &cb, report, enabled, offReason)
}
