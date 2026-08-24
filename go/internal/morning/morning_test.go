package morning

import (
	"strings"
	"testing"
	"time"

	"lovegw/internal/holidays"
	"lovegw/internal/love"
	"lovegw/internal/sitetext"
	"lovegw/internal/store"
)

func nsk(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(DefaultTZ)
	if err != nil {
		t.Fatalf("пояс: %v", err)
	}
	return loc
}

// TestSlotForAroundMidnight — слот привязан к местному дню, а не к суткам
// хоста: хост живёт по Москве, и без пояса «утро» уезжало бы на четыре часа.
func TestSlotForAroundMidnight(t *testing.T) {
	loc := nsk(t)
	cases := []struct {
		name string
		now  time.Time
		day  string
	}{
		{"до слота — вчерашний", time.Date(2026, 8, 24, 6, 59, 0, 0, loc), "2026-08-23"},
		{"ровно в слот", time.Date(2026, 8, 24, 7, 0, 0, 0, loc), "2026-08-24"},
		{"днём — сегодняшний", time.Date(2026, 8, 24, 19, 0, 0, 0, loc), "2026-08-24"},
		{"сразу после полуночи — вчерашний", time.Date(2026, 8, 25, 0, 10, 0, 0, loc), "2026-08-24"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			day, slot := SlotFor(c.now, loc, 7)
			if day != c.day {
				t.Errorf("день %q, ожидался %q", day, c.day)
			}
			if slot.After(c.now) {
				t.Errorf("слот %v в будущем относительно %v", slot, c.now)
			}
		})
	}
}

func TestNextSlotIsTomorrow(t *testing.T) {
	loc := nsk(t)
	now := time.Date(2026, 8, 24, 7, 30, 0, 0, loc)
	next := NextSlot(now, loc, 7)
	want := time.Date(2026, 8, 25, 7, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Errorf("следующий слот %v, ожидался %v", next, want)
	}
}

func TestPrevDay(t *testing.T) {
	if got := PrevDay("2026-03-01"); got != "2026-02-28" {
		t.Errorf("предыдущий день %q, ожидался 2026-02-28", got)
	}
	if got := PrevDay("не дата"); got != "" {
		t.Errorf("мусор разобран как дата: %q", got)
	}
}

// TestDecide — таблица по всем веткам. Порядок важен: он и есть приоритет.
func TestDecide(t *testing.T) {
	loc := nsk(t)
	slot := time.Date(2026, 8, 24, 7, 0, 0, 0, loc)
	base := decideInput{
		Enabled: true, Now: slot.Add(time.Minute), Slot: slot,
		Grace: 3 * time.Hour, HasSession: true,
	}
	with := func(f func(*decideInput)) decideInput {
		in := base
		f(&in)
		return in
	}
	cases := []struct {
		name   string
		in     decideInput
		action action
		reason string
	}{
		{"обычное утро", base, actPost, ""},
		{"выключено", with(func(i *decideInput) { i.Enabled = false }), actIdle, reasonDisabled},
		{"день уже закрыт", with(func(i *decideInput) { i.Done = true }), actIdle, reasonDone},
		{"слот ещё не наступил", with(func(i *decideInput) { i.Now = slot.Add(-time.Minute) }), actIdle, reasonEarly},
		{"проспали", with(func(i *decideInput) { i.Now = slot.Add(4 * time.Hour) }), actMark, reasonTooLate},
		{"утро уже сказано", with(func(i *decideInput) { i.Foreign = "313100" }), actMark, reasonSomeone},
		{"нет сессии", with(func(i *decideInput) { i.HasSession = false }), actMark, reasonNoSession},
		// Выключенный тумблер сильнее всего остального: выключили — значит
		// молчим, а не «молчим и пишем в базу пропуск».
		{"выключено важнее опоздания", with(func(i *decideInput) {
			i.Enabled = false
			i.Now = slot.Add(4 * time.Hour)
		}), actIdle, reasonDisabled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := decide(c.in)
			if v.Action != c.action || v.Reason != c.reason {
				t.Errorf("решение %v/%q, ожидалось %v/%q", v.Action, v.Reason, c.action, c.reason)
			}
		})
	}
	// Чужая заметка едет в ЛС, поэтому её id обязан дойти до вердикта.
	if v := decide(with(func(i *decideInput) { i.Foreign = "313100" })); v.Detail != "313100" {
		t.Errorf("id чужой заметки потерян: %q", v.Detail)
	}
}

// TestIsGreeting — приветствие узнаётся по НАЧАЛУ тела. Ошибка в любую сторону
// стоит дорого: не узнали — вышли вторым утром подряд, узнали лишнего —
// промолчали на весь день.
func TestIsGreeting(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"Доброе утро, друзья!", true},
		{"С добрым утром)))", true},
		{"доброго утра всем, кто проснулся", true},
		{"Всем утра!", true},
		{"Утречко!", true},
		{"ДОБРОЕ УТРО!!! Как спалось?", true},
		// Те же слова в середине рассказа — не приветствие.
		{"Вчера поругались, а сегодня он сказал мне доброе утро, и всё как будто прошло. " +
			"Не знаю, что теперь думать, потому что вчера было очень обидно и я всю ночь не спала", false},
		{"Утренняя пробежка меня доконала", false},
		{"С утра болит голова, посоветуйте что-нибудь", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isGreeting(c.text); got != c.want {
			t.Errorf("isGreeting(%.40q) = %v, ожидалось %v", c.text, got, c.want)
		}
	}
}

// TestNotePresence — правило охвата: за нижним краем ленты о заметке судить
// нельзя. Без него предохранитель срабатывал бы сам собой к концу суток.
func TestNotePresence(t *testing.T) {
	feed := []love.Note{{ID: "313100"}, {ID: "313090"}, {ID: "313080"}}
	cases := []struct {
		note string
		want presence
	}{
		{"313090", presenceThere},
		{"313095", presenceGone},    // окно её покрывает, а её нет
		{"313070", presenceUnknown}, // старше самой старой в окне
		{"", presenceUnknown},
	}
	for _, c := range cases {
		if got := notePresence(feed, c.note); got != c.want {
			t.Errorf("notePresence(%q) = %v, ожидалось %v", c.note, got, c.want)
		}
	}
	if got := notePresence(nil, "313090"); got != presenceUnknown {
		t.Errorf("пустая лента: %v, ожидалось «судить не по чему»", got)
	}
}

func TestFindOwnNote(t *testing.T) {
	// Своя заметка ищется по автору и НАЧАЛУ текста: id своей публикации сайт
	// не возвращает, а начало переживает и переносы, и обрезку ленты.
	own := "Доброе утро! ☀️ Понедельник, 24 августа. Сегодня день странной музыки."
	feed := []love.Note{
		{ID: "313100", AuthorID: "999", Text: "Доброе утро! Всем хорошего дня."},
		{ID: "313101", AuthorID: "1472546", Text: own + " Что у вас сегодня?"},
	}
	if got := findOwnNote(feed, "1472546", own); got != "313101" {
		t.Errorf("своя заметка не найдена: %q", got)
	}
	if got := findOwnNote(feed, "1472546", "Доброе утро! Совсем другой текст заметки."); got != "" {
		t.Errorf("чужой текст принят за свой: %q", got)
	}
	if got := findOwnNote(feed, "1472547", own); got != "" {
		t.Errorf("заметка чужого автора принята за свою: %q", got)
	}
}

// TestFuseVerdict — предохранитель. Полоса ограничена не только числом
// промахов, но и ВРЕМЕНЕМ: урок ложного выключения амвона 23.08.2026, где два
// промаха с разницей в неделю сложились в «полосу».
func TestFuseVerdict(t *testing.T) {
	now := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	row := func(day, state, reason string, ago time.Duration) store.MorningNote {
		return store.MorningNote{Day: day, State: state, Reason: reason, CreatedAt: now.Add(-ago)}
	}
	cases := []struct {
		name string
		rows []store.MorningNote
		off  bool
	}{
		{"два промаха подряд гасят", []store.MorningNote{
			row("2026-08-24", store.MorningMissing, "не появилась в ленте", time.Hour),
			row("2026-08-23", store.MorningMissing, "не появилась в ленте", 25*time.Hour),
		}, true},
		{"удача разрывает полосу", []store.MorningNote{
			row("2026-08-24", store.MorningMissing, "не появилась в ленте", time.Hour),
			row("2026-08-23", store.MorningConfirmed, "", 25*time.Hour),
			row("2026-08-22", store.MorningMissing, "не появилась в ленте", 49*time.Hour),
		}, false},
		{"сбой отправки нейтрален", []store.MorningNote{
			row("2026-08-24", store.MorningMissing, store.MorningReasonSendFailed, time.Hour),
			row("2026-08-23", store.MorningMissing, store.MorningReasonSendFailed, 25*time.Hour),
		}, false},
		{"старый промах за горизонтом не считается", []store.MorningNote{
			row("2026-08-24", store.MorningMissing, "не появилась в ленте", time.Hour),
			row("2026-08-17", store.MorningMissing, "не появилась в ленте", 7*24*time.Hour),
		}, false},
		{"пропуски полосу не растят", []store.MorningNote{
			row("2026-08-24", store.MorningSkipped, reasonSomeone, time.Hour),
			row("2026-08-23", store.MorningSkipped, reasonSomeone, 25*time.Hour),
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			off, reason := fuseVerdict(c.rows, now, 2)
			if off != c.off {
				t.Errorf("выключение %v, ожидалось %v (причина %q)", off, c.off, reason)
			}
		})
	}
}

// TestTrimFactsKeepsNames — в людный день праздников больше дюжины, а именины
// стоят после них: без своего места поздравлять было бы некого.
func TestTrimFactsKeepsNames(t *testing.T) {
	var list []holidays.Occasion
	for i := 0; i < 12; i++ {
		list = append(list, holidays.Occasion{Title: "Праздник", Kind: holidays.KindHoliday})
	}
	list = append(list, holidays.Occasion{Title: "Мария, Ульяна", Kind: holidays.KindName})
	list = append(list, holidays.Occasion{Title: "Событие", Kind: holidays.KindHistory, Year: 1853})

	got := trimFacts(list, 12)
	if len(got) != 12 {
		t.Fatalf("поводов %d, ожидалось 12", len(got))
	}
	if !hasKind(got, holidays.KindName) {
		t.Error("именины не доехали до промпта")
	}
	// Короткий список не трогаем вовсе.
	short := list[:3]
	if len(trimFacts(short, 12)) != 3 {
		t.Error("короткий список обрезан")
	}
}

// --------------------------------------------------------------- валидатор

func factsFixture() []holidays.Occasion {
	return []holidays.Occasion{
		{Title: "День странной музыки", Kind: holidays.KindHoliday, Sources: []string{"calend.ru"}},
		{Title: "День бизнес-наставника", Kind: holidays.KindHoliday, Scope: holidays.ScopeRussia},
		{Title: "Александр, Мария, Ульяна", Kind: holidays.KindName},
		{Title: "Американским поваром Джорджем Крамом изобретены картофельные чипсы",
			Kind: holidays.KindHistory, Year: 1853},
	}
}

func validCfg() validateConfig {
	return validateConfig{
		MinRunes: 50, MaxRunes: 1200, MaxLines: 18,
		Day: 24, Month: 8, Weekday: "понедельник",
		Facts: factsFixture(),
	}
}

func TestValidate(t *testing.T) {
	ok := `Доброе утро! ☀️ Сегодня понедельник, 24 августа.

🎶 День странной музыки: пора признаться, что у вас в наушниках.

🎂 Именины у Марии и Ульяны.

А ещё в 1853 году придумали чипсы. Что у вас сегодня?`
	used := []int{1, 3, 4}
	cases := []struct {
		name string
		d    draft
		bad  bool
	}{
		{"годная заметка", draft{Text: ok, Used: used}, false},
		{"пусто", draft{Text: "   ", Used: used}, true},
		{"коротко", draft{Text: "Доброе утро!", Used: used}, true},
		{"markdown", draft{Text: "**Доброе утро!** " + ok, Used: used}, true},
		{"BB-код", draft{Text: "[b]Доброе утро![/b] " + ok, Used: used}, true},
		{"длинное тире", draft{
			Text: strings.ReplaceAll(ok, "музыки: пора", "музыки — пора"), Used: used}, true},
		{"обломок размышления", draft{Text: "Wait, no. " + ok, Used: used}, true},
		// Эмодзи здесь вместо интонации, которой у текста нет: без них заметка
		// читается сводкой календаря (замечание владельца 24.08.2026).
		{"без эмодзи", draft{Text: stripEmoji(ok), Used: used}, true},
		// Главное, ради чего валидатор здесь и заведён: факты подаём мы, значит
		// враньё ловится механически.
		{"чужая дата", draft{
			Text: strings.ReplaceAll(ok, "24 августа", "25 августа"), Used: used}, true},
		{"чужой день недели", draft{
			Text: strings.ReplaceAll(ok, "понедельник", "вторник"), Used: used}, true},
		{"выдуманный год", draft{
			Text: strings.ReplaceAll(ok, "1853", "1854"), Used: used}, true},
		{"повода нет в списке", draft{Text: ok, Used: []int{1, 19}}, true},
		// Ритуал: заметку узнают по первым двум словам. Проверяется тем же
		// закрытым списком, которым мы узнаём ЧУЖОЕ утро в ленте.
		{"не поздоровались", draft{
			Text: strings.Replace(ok, "Доброе утро! ", "Здравствуйте. ", 1), Used: used}, true},
		{"поводы не названы вовсе", draft{Text: ok, Used: nil}, true},
		{"назван номер, а повода в тексте нет", draft{Text: ok, Used: []int{1, 2, 3, 4}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := validate(c.d, validCfg())
			if (got != "") != c.bad {
				t.Errorf("validate = %q, ожидался брак=%v", got, c.bad)
			}
		})
	}
}

// TestValidateWithoutFacts — день без поводов: заметка выходит, и спрашивать за
// used не с чего.
func TestValidateWithoutFacts(t *testing.T) {
	cfg := validCfg()
	cfg.Facts = nil
	text := "Доброе утро! ☀️ Понедельник, конец августа, и утро уже прохладное. " +
		"🍂 Кто как просыпается в такую погоду?"
	if got := validate(draft{Text: text}, cfg); got != "" {
		t.Errorf("заметка без поводов забракована: %q", got)
	}
}

// stripEmoji — тот же текст без значков: проверяем ровно одно правило, не
// переписывая заметку целиком.
func stripEmoji(text string) string {
	var b strings.Builder
	for _, r := range text {
		if sitetext.CountEmoji(string(r)) == 0 && r != 0xFE0F {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestBuildPromptKeepsFactNumbers — номера в промпте и номера в used обязаны
// значить одно и то же, иначе проверка «повод назван» бессмысленна.
func TestBuildPromptKeepsFactNumbers(t *testing.T) {
	p := buildPrompt(promptInput{
		Weekday: "понедельник", DateWord: "24 августа 2026 года",
		Facts: factsFixture(), MinRunes: 200, MaxRunes: 1200, MaxLines: 14,
	})
	for i, want := range []string{
		"1. [праздник] День странной музыки",
		"2. [праздник] День бизнес-наставника",
		"3. [именины] Александр, Мария, Ульяна",
		"4. [событие 1853 года] Американским поваром",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("в промпте нет строки %d: %q", i+1, want)
		}
	}
	if !strings.Contains(p, "Понедельник, 24 августа 2026 года") {
		t.Errorf("в промпте нет сегодняшней даты:\n%s", p)
	}
}

// TestBuildPromptWithoutFacts — календари молчат: модели прямо говорят писать
// без поводов, а не оставляют пустой раздел.
func TestBuildPromptWithoutFacts(t *testing.T) {
	p := buildPrompt(promptInput{Weekday: "среда", DateWord: "1 января 2027 года"})
	if !strings.Contains(p, "поводов нет") {
		t.Errorf("пустой список поводов не объяснён модели:\n%s", p)
	}
}
