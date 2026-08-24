package morning

// Суточный слот. Своя арифметика, а не `digest.SlotFor`: там окно жёстко равно
// «слот минус семь дней», а метка выпуска — ISO-неделя. Здесь метка это ДЕНЬ, и
// он же первичный ключ строки в БД, то есть вся однократность.

import "time"

// DefaultTZ — пояс слота. Тот же, что у дайджеста: у площадки один часовой
// пояс, новосибирский, и «утро» считается по нему, а не по хосту (хост живёт по
// Москве, и в логах это сбивает с толку).
const DefaultTZ = "Asia/Novosibirsk"

// DayLayout — как выглядит метка дня: она уезжает в первичный ключ, поэтому
// формат сортируемый и один на весь пакет.
const DayLayout = "2006-01-02"

// SlotFor — последний НАСТУПИВШИЙ слот и его день. Слот сегодняшний, если час
// уже прошёл; иначе вчерашний — ровно то, что нужно догону после рестарта.
func SlotFor(now time.Time, loc *time.Location, hour int) (day string, slot time.Time) {
	local := now.In(loc)
	slot = time.Date(local.Year(), local.Month(), local.Day(), hour, 0, 0, 0, loc)
	if slot.After(now) {
		slot = slot.AddDate(0, 0, -1)
	}
	return slot.Format(DayLayout), slot
}

// NextSlot — ближайший слот строго после now (для таймера демона).
func NextSlot(now time.Time, loc *time.Location, hour int) time.Time {
	_, slot := SlotFor(now, loc, hour)
	return slot.AddDate(0, 0, 1)
}

// DayBounds — календарные сутки слота. «Сегодня» для поиска чужого «доброго
// утра» считается сутками, а не окном от слота: приветствие, написанное в
// пять утра, — это сегодняшнее приветствие, хотя наш слот в семь.
func DayBounds(slot time.Time) (start, end time.Time) {
	start = time.Date(slot.Year(), slot.Month(), slot.Day(), 0, 0, 0, 0, slot.Location())
	return start, start.AddDate(0, 0, 1)
}

// PrevDay — метка предыдущего дня. Ею проверяют, жива ли вчерашняя заметка:
// отдельного таймера для этого не нужно, снос виден при следующем выходе.
func PrevDay(day string) string {
	t, err := time.Parse(DayLayout, day)
	if err != nil {
		return ""
	}
	return t.AddDate(0, 0, -1).Format(DayLayout)
}
