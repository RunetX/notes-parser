package morning

// Отчёт для ручки `/morning` в ЛС и для CLI `morning status`.
//
// Собирает его сама служба, а не диалоговое ядро: `dmbot` знает про утреннюю
// заметку ровно два глагола — «переключить» и «показать отчёт» (интерфейс
// MorningControl). Иначе ему пришлось бы знать её состояния и предохранитель.

import (
	"context"
	"fmt"
	"strings"

	"lovegw/internal/store"
)

// statusDays — сколько последних дней показываем. Неделя: по ней видно и ритм,
// и полосу промахов, и при этом сообщение остаётся читаемым с телефона.
const statusDays = 7

// Status — состояние службы человеческими словами.
func (s *Service) Status(ctx context.Context) (report string, enabled bool, offReason string) {
	enabled, err := s.Enabled(ctx)
	if err != nil {
		return "Утренняя заметка: не удалось прочитать состояние (" + err.Error() + ")", false, ""
	}
	offReason, _, _ = s.st.Flag(ctx, store.FlagMorningOffReason)

	var b strings.Builder
	if enabled {
		fmt.Fprintf(&b, "🌅 Утренняя заметка включена. Слот %02d:00 (%s), модель: %s.",
			s.cfg.Hour, s.cfg.Loc, s.cfg.Model)
	} else {
		b.WriteString("⛔ Утренняя заметка выключена." + s.offText(ctx, offReason))
	}
	rows, err := s.st.MorningRecent(ctx, statusDays)
	if err != nil {
		s.log.Error("утренняя заметка: чтение последних дней", "err", err)
		return b.String(), enabled, offReason
	}
	if len(rows) == 0 {
		b.WriteString("\n\nЗаметок ещё не было.")
		return b.String(), enabled, offReason
	}
	b.WriteString("\n\nПоследние дни:")
	for _, n := range rows {
		fmt.Fprintf(&b, "\n%s — %s", n.Day, stateText(n))
	}
	if last := firstWithText(rows); last.Day != "" {
		fmt.Fprintf(&b, "\n\nПоследняя (%s):\n%s", last.Day, last.Text)
		if last.NoteID != "" {
			b.WriteString("\n" + s.noteLink(last.NoteID))
		}
	}
	return b.String(), enabled, offReason
}

// stateText — состояние дня словами. Причина показывается рядом: «пропущено»
// без «кто-то уже написал» отвечает на вопрос ровно наполовину.
func stateText(n store.MorningNote) string {
	switch n.State {
	case store.MorningConfirmed:
		return "вышла"
	case store.MorningPosted, store.MorningPosting:
		return "отправлена, ищем в ленте"
	case store.MorningMissing:
		return "не найдена в ленте (" + n.Reason + ")"
	case store.MorningSkipped:
		return "промолчали: " + skipText(n.Reason)
	case store.MorningFailed:
		return "не вышла: " + n.Reason
	default:
		return n.State
	}
}

func skipText(reason string) string {
	switch reason {
	case reasonSomeone:
		return "утро уже сказал кто-то другой"
	case reasonTooLate:
		return "слот пропущен, догонять поздно"
	case reasonNoFacts:
		return "в поводах дня не нашлось светлого"
	default:
		return reason
	}
}

func firstWithText(rows []store.MorningNote) store.MorningNote {
	for _, n := range rows {
		if strings.TrimSpace(n.Text) != "" {
			return n
		}
	}
	return store.MorningNote{}
}

// offText — почему и когда выключили. Пустая причина бывает у ручного
// выключения: там объяснять нечего.
func (s *Service) offText(ctx context.Context, reason string) string {
	if reason == "" {
		return ""
	}
	out := "\nПричина: " + reason
	offAt, _, _ := s.st.Flag(ctx, store.FlagMorningOffAt)
	if offAt == "" {
		return out
	}
	if by, _, _ := s.st.Flag(ctx, store.FlagMorningOffBy); by != "" {
		return out + " (" + offAt + ", " + by + ")"
	}
	return out + " (" + offAt + ")"
}
