package digest

import (
	"fmt"
	"time"
)

// DefaultTZ — пояс слота по умолчанию. День и час дефолтного слота живут в
// одном месте, `config.Defaults` (суббота 09:00 Нск): здесь они раньше
// дублировались мёртвыми константами, которые никто не читал.
//
// Почему суббота утром, а не вечер пятницы (замер по архиву 11.08.2026,
// 10,7 млн комментариев): слот задаёт не только время публикации, но и шов
// недели, а `Window.Start` жёстко равен `End − 7 дней`. В 19:00 пятницы
// площадка идёт НА ПОДЪЁМ к своему суточному пику 21:00–22:00, так что срез
// рассекал живой вечер и каждую неделю сдвигал его лучшие часы в следующий
// выпуск. Субботние 09:00 — провал (0,48 % недели против 0,81 % в пятницу
// 19:00) перед утренним подъёмом: шов проходит по тишине, а читатель
// приходит уже после публикации. Плюс по НОВЫМ ЗАМЕТКАМ (именно они топят
// пост в канале) суббота — самый пустой день недели.
const DefaultTZ = "Asia/Novosibirsk"

// Window — окно выпуска (Start, End]: Start исключительно, End (момент слота
// публикации) включительно. Границы несут часовой пояс слота.
//
// ID — метка выпуска: ISO-неделя дня слота в его часовом поясе ("2026-W31").
// Окно пятница→пятница пересекает границу ISO-недель; конвенция — выпуск
// маркируется неделей, которой принадлежит день слота.
type Window struct {
	Start, End time.Time
	ID         string
}

// SlotFor возвращает окно последнего наступившего слота (weekday, hour:00 в
// loc) не позже now, смещённого на weeks недель назад (weeks <= 0).
func SlotFor(now time.Time, loc *time.Location, weekday time.Weekday, hour, weeks int) Window {
	local := now.In(loc)
	slot := time.Date(local.Year(), local.Month(), local.Day(), hour, 0, 0, 0, loc)
	for slot.Weekday() != weekday {
		slot = slot.AddDate(0, 0, -1)
	}
	if slot.After(now) { // слот этой недели ещё не наступил
		slot = slot.AddDate(0, 0, -7)
	}
	slot = slot.AddDate(0, 0, 7*weeks)
	y, w := slot.ISOWeek()
	return Window{
		Start: slot.AddDate(0, 0, -7),
		End:   slot,
		ID:    fmt.Sprintf("%d-W%02d", y, w),
	}
}

// NextSlot — ближайший слот строго после now (для таймера демона).
func NextSlot(now time.Time, loc *time.Location, weekday time.Weekday, hour int) time.Time {
	return SlotFor(now, loc, weekday, hour, 0).End.AddDate(0, 0, 7)
}
