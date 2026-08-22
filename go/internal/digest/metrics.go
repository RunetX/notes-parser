package digest

// Расчёт рубрик выпуска: метрики окна считаются в Go по сырым комментариям,
// сравнительная история — выборками источника (Source).

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// groupByNote раскладывает комментарии по заметкам, сохраняя порядок по id.
func groupByNote(comments []Comment) map[string][]Comment {
	byNote := make(map[string][]Comment)
	for _, c := range comments {
		byNote[c.NoteID] = append(byNote[c.NoteID], c)
	}
	return byNote
}

func distinctLinks(comments []Comment) int {
	links := make(map[string]bool)
	for _, c := range comments {
		if c.Author != "" {
			links[c.Author] = true
		}
	}
	return len(links)
}

// buildNoteStats считает метрики окна по каждой обсуждавшейся заметке.
// Заметки без шапки в БД (удалённые вручную) пропускаются.
func buildNoteStats(byNote map[string][]Comment, heads map[string]Note) []NoteStat {
	var stats []NoteStat
	for noteID, cs := range byNote {
		head, ok := heads[noteID]
		if !ok {
			continue
		}
		stats = append(stats, noteStat(head, cs))
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].Note.ID < stats[j].Note.ID })
	return stats
}

// noteStat — метрики окна одной заметки по её комментариям (в порядке id).
func noteStat(head Note, cs []Comment) NoteStat {
	s := NoteStat{Note: head, Comments: len(cs), PingPong: pingPongPairs(cs)}
	links := make(map[string]bool)
	hours := make(map[time.Time]int)
	for _, c := range cs {
		t := c.PublishedAt
		if c.Author != "" {
			links[c.Author] = true
		}
		if s.FirstAt.IsZero() || t.Before(s.FirstAt) {
			s.FirstAt = t
		}
		if t.After(s.LastAt) {
			s.LastAt = t
		}
		hour := t.UTC().Truncate(time.Hour)
		hours[hour]++
		if n := hours[hour]; n > s.PeakHourN {
			s.PeakHourN, s.PeakHour = n, hour
		}
	}
	s.Commenters = len(links)
	return s
}

// pingPongPairs — число соседних по id пар реплик разных авторов с зазором
// не больше pingPongGap (признак перепалки).
func pingPongPairs(cs []Comment) int {
	pairs := 0
	for i := 1; i < len(cs); i++ {
		if cs[i-1].Author == cs[i].Author {
			continue
		}
		if gap := cs[i].PublishedAt.Sub(cs[i-1].PublishedAt); gap >= 0 && gap <= pingPongGap {
			pairs++
		}
	}
	return pairs
}

// computeHeat заполняет Heat — эвристику накала треда: нормированная сумма
// пиковой скорости (0.4), плотности реплик на участника (0.3) и «пинг-понга»
// (0.3). Нормировка — по максимуму окна.
func computeHeat(stats []NoteStat) {
	var maxPeak, maxDensity, maxPing float64
	density := func(s NoteStat) float64 {
		people := s.Commenters
		if people < 1 {
			people = 1
		}
		return float64(s.Comments) / float64(people)
	}
	for _, s := range stats {
		maxPeak = max(maxPeak, float64(s.PeakHourN))
		maxDensity = max(maxDensity, density(s))
		maxPing = max(maxPing, float64(s.PingPong))
	}
	norm := func(v, max float64) float64 {
		if max == 0 {
			return 0
		}
		return v / max
	}
	for i := range stats {
		s := &stats[i]
		s.Heat = 0.4*norm(float64(s.PeakHourN), maxPeak) +
			0.3*norm(density(*s), maxDensity) +
			0.3*norm(float64(s.PingPong), maxPing)
	}
}

// pickTopNote — «заметка недели»: комментарии → участники → длительность.
func pickTopNote(stats []NoteStat) *NoteStat {
	if len(stats) == 0 {
		return nil
	}
	best := stats[0]
	for _, s := range stats[1:] {
		if better(s, best) {
			best = s
		}
	}
	return &best
}

func better(a, b NoteStat) bool {
	if a.Comments != b.Comments {
		return a.Comments > b.Comments
	}
	if a.Commenters != b.Commenters {
		return a.Commenters > b.Commenters
	}
	da, db := a.LastAt.Sub(a.FirstAt), b.LastAt.Sub(b.FirstAt)
	if da != db {
		return da > db
	}
	return a.Note.ID < b.Note.ID
}

// pickDisputes — шорт-лист «спора недели»: топ по Heat, без заметки недели
// и без мелких тредов.
func pickDisputes(stats []NoteStat, top *NoteStat) []NoteStat {
	var candidates []NoteStat
	for _, s := range stats {
		if top != nil && s.Note.ID == top.Note.ID {
			continue
		}
		if s.Comments >= minDisputeSize {
			candidates = append(candidates, s)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Heat != candidates[j].Heat {
			return candidates[i].Heat > candidates[j].Heat
		}
		return candidates[i].Note.ID < candidates[j].Note.ID
	})
	if len(candidates) > topDisputes {
		candidates = candidates[:topDisputes]
	}
	return candidates
}

// pickQuotes — шорт-лист «цитаты недели»: комментарии внятной длины,
// ранжированные по числу ответов других авторов сразу после.
func pickQuotes(byNote map[string][]Comment) []Quote {
	var quotes []Quote
	for _, cs := range byNote {
		for _, c := range cs {
			runes := len([]rune(c.Text))
			if runes < quoteMinRunes || runes > quoteMaxRunes {
				continue
			}
			quotes = append(quotes, Quote{Comment: c, RepliesAfter: repliesAfter(cs, c)})
		}
	}
	sort.Slice(quotes, func(i, j int) bool {
		if quotes[i].RepliesAfter != quotes[j].RepliesAfter {
			return quotes[i].RepliesAfter > quotes[j].RepliesAfter
		}
		li, lj := len([]rune(quotes[i].Comment.Text)), len([]rune(quotes[j].Comment.Text))
		if li != lj {
			return li > lj
		}
		return quotes[i].Comment.ID < quotes[j].Comment.ID
	})
	if len(quotes) > topQuotes {
		quotes = quotes[:topQuotes]
	}
	return quotes
}

// repliesAfter — комментарии других авторов того же треда в окно
// quoteReplyWindow после реплики c (грубая мера «реакции»).
func repliesAfter(cs []Comment, c Comment) int {
	t := c.PublishedAt
	replies := 0
	for _, c2 := range cs {
		t2 := c2.PublishedAt
		if c2.ID != c.ID && c2.Author != c.Author &&
			t2.After(t) && !t2.After(t.Add(quoteReplyWindow)) {
			replies++
		}
	}
	return replies
}

// fillPersons заполняет «новые лица» и «возвращение недели»: комментаторы и
// авторы заметок окна, слитые по человеку.
//
// Слияние идёт по ключу автора, каким его знает ИСТОЧНИК: у зеркала это ссылка
// на анкету (числового id у комментария там нет вовсе), у площадки — номер
// строки. Выпуску различие не видно и видно быть не должно: он спрашивает
// адрес анкеты у источника и печатает его, если тот есть.
func fillPersons(ctx context.Context, src Source, is *Issue) error {
	w := is.Window
	commenters, err := src.CommenterHistory(ctx, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("история комментаторов: %w", err)
	}
	authors, err := src.NoteAuthorHistory(ctx, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("история авторов: %w", err)
	}

	merged := make(map[string]*Person) // ключ — автор глазами источника
	add := func(key, name string, notes, comments int, prev time.Time) {
		if key == "" {
			return // без анкеты человека не опознать: в рубрику он не идёт
		}
		p, ok := merged[key]
		if !ok {
			p = &Person{Name: name, ProfileURL: src.ProfileURL(key)}
			merged[key] = p
		}
		p.Notes += notes
		p.Comments += comments
		// Более поздняя активность до окна побеждает: новичок — только тот,
		// кто не появлялся вообще (ни заметкой, ни комментарием).
		if prev.After(p.PrevSeenAt) {
			p.PrevSeenAt = prev
		}
	}
	for _, c := range commenters {
		add(c.Author, c.Name, 0, c.InWindow, c.PrevSeenAt)
	}
	for _, a := range authors {
		add(a.Author, a.Name, a.NotesInWindow, 0, a.PrevNoteAt)
	}

	type keyed struct {
		key string
		p   Person
	}
	var persons []keyed
	for k, p := range merged {
		persons = append(persons, keyed{key: k, p: *p})
	}
	// Вес для ранжирования: заметка «дороже» комментария.
	weight := func(p Person) int { return 3*p.Notes + p.Comments }
	sort.Slice(persons, func(i, j int) bool {
		if wi, wj := weight(persons[i].p), weight(persons[j].p); wi != wj {
			return wi > wj
		}
		return persons[i].key < persons[j].key
	})
	for _, k := range persons {
		p := k.p
		switch {
		case p.PrevSeenAt.IsZero():
			if len(is.Newcomers) < topPersons {
				is.Newcomers = append(is.Newcomers, p)
			}
		case !p.PrevSeenAt.After(w.Start.Add(-returneeGap)):
			if len(is.Returnees) < topPersons {
				is.Returnees = append(is.Returnees, p)
			}
		}
	}
	return nil
}

// fillRecords заполняет «рекорды»: числа только через сравнение с историей.
// Неинтересные рекорды опускаются.
//
// История берётся за ГОРИЗОНТ (RecordHorizon), а не за всё время: на площадке
// лежит архив с 2009-го, и «за всю историю» означало бы рекорд 2013 года,
// который не побьют никогда.
func fillRecords(ctx context.Context, src Source, is *Issue, comments []Comment) error {
	w := is.Window
	since := w.End.Add(-RecordHorizon)
	if r, ok, err := longestThreadRecord(ctx, src, is, since); err != nil {
		return err
	} else if ok {
		is.Records = append(is.Records, r)
	}
	// Пик-час: рекорд только если максимум горизонта установлен в окне.
	histHour, histNote, histN, err := src.PeakCommentHour(ctx, since)
	if err != nil {
		return fmt.Errorf("пик-час истории: %w", err)
	}
	if histN >= minRecordPeakHour && histHour.After(w.Start) && !histHour.After(w.End) {
		is.Records = append(is.Records, Record{
			NoteID: histNote,
			Text:   fmt.Sprintf("%s за час — рекорд года.", nComments(histN)),
		})
	}
	// Самый шумный день окна против среднего дня.
	if r, ok := busiestDay(comments, w); ok {
		is.Records = append(is.Records, r)
	}
	return nil
}

// longestThreadRecord сравнивает заметку недели с итогами прошлых тредов
// горизонта.
func longestThreadRecord(ctx context.Context, src Source, is *Issue, since time.Time) (Record, bool, error) {
	top, w := is.TopNote, is.Window
	if top == nil || top.Comments < minRecordComments {
		return Record{}, false, nil
	}
	totals, err := src.NoteTotals(ctx, since)
	if err != nil {
		return Record{}, false, fmt.Errorf("итоги тредов: %w", err)
	}
	var lastBigger *NoteTotals // последняя заметка до окна с итогом больше
	for i := range totals {
		t := &totals[i]
		if t.NoteID == top.Note.ID || t.PublishedAt.After(w.Start) {
			continue
		}
		if t.Comments > top.Comments {
			lastBigger = t // totals идут по времени — останется последняя
		}
	}
	switch {
	case lastBigger == nil:
		return Record{
			NoteID: top.Note.ID,
			Text:   fmt.Sprintf("%s — самый длинный тред за год.", nComments(top.Comments)),
		}, true, nil
	case w.End.Sub(lastBigger.PublishedAt) > returneeGap:
		return Record{
			NoteID: top.Note.ID,
			Text: fmt.Sprintf("%s — самый длинный тред с %s.", nComments(top.Comments),
				monthSince(lastBigger.PublishedAt.In(w.End.Location()), w.End.Year())),
		}, true, nil
	}
	return Record{}, false, nil // больший тред был недавно — не рекорд
}

// busiestDay — «самый шумный день» окна, если он заметно выше среднего.
func busiestDay(comments []Comment, w Window) (Record, bool) {
	if len(comments) == 0 {
		return Record{}, false
	}
	loc := w.End.Location()
	byDay := make(map[time.Weekday]int)
	for _, c := range comments {
		byDay[c.PublishedAt.In(loc).Weekday()]++
	}
	var maxDay time.Weekday
	maxN := 0
	for d, n := range byDay {
		if n > maxN || (n == maxN && d < maxDay) {
			maxDay, maxN = d, n
		}
	}
	avg := len(comments) / 7
	if maxN < 20 || maxN < 2*avg {
		return Record{}, false
	}
	return Record{Text: fmt.Sprintf("Самый шумный день — %s: %s при среднем %d в день.",
		dayNames[maxDay], nComments(maxN), avg)}, true
}

// nComments — «N комментариев» с правильным склонением.
func nComments(n int) string {
	return fmt.Sprintf("%d %s", n, pluralRu(n, "комментарий", "комментария", "комментариев"))
}

var dayNames = map[time.Weekday]string{
	time.Sunday:    "воскресенье",
	time.Monday:    "понедельник",
	time.Tuesday:   "вторник",
	time.Wednesday: "среда",
	time.Thursday:  "четверг",
	time.Friday:    "пятница",
	time.Saturday:  "суббота",
}

var monthsGen = [...]string{"января", "февраля", "марта", "апреля", "мая", "июня",
	"июля", "августа", "сентября", "октября", "ноября", "декабря"}

// monthSince — «с апреля» / «с апреля 2025 года» для сравнительных рекордов.
func monthSince(t time.Time, slotYear int) string {
	name := monthsGen[t.Month()-1]
	if t.Year() != slotYear {
		return fmt.Sprintf("%s %d года", name, t.Year())
	}
	return name
}

// pluralRu — русское склонение существительного при числе.
func pluralRu(n int, one, few, many string) string {
	n %= 100
	if n >= 11 && n <= 14 {
		return many
	}
	switch n % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	default:
		return many
	}
}
