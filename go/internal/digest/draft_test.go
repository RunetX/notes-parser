package digest

import (
	"strings"
	"testing"
	"time"
)

// sampleIssue — выпуск со всеми рубриками, без БД.
func sampleIssue() *Issue {
	w := testWindow()
	base := w.Start.Add(24 * time.Hour)
	top := NoteStat{
		Note: Note{ID: "312696", Author: "u9", AuthorName: "Граф",
			Text: "Почему все мужчины <такие> и {что} с этим делать"},
		Comments: 87, Commenters: 19,
		FirstAt: base, LastAt: base.Add(4 * 24 * time.Hour), PeakHourN: 21,
	}
	dispute := NoteStat{
		Note:     Note{ID: "555", Author: "u10", AuthorName: "Барон", Text: "Спорная заметка"},
		Comments: 34, Commenters: 12, FirstAt: base, LastAt: base.Add(5 * time.Hour), PeakHourN: 9,
	}
	return &Issue{
		Window:   w,
		Stats:    Stats{Notes: 14, Comments: 380, Commenters: 52},
		TopNote:  &top,
		Disputes: []NoteStat{dispute},
		Quotes: []Quote{{
			Comment: Comment{ID: 1, NoteID: "555", AuthorName: "Некто",
				Text: strings.Repeat("яркая мысль ", 10)},
			RepliesAfter: 3,
		}},
		Newcomers: []Person{{Name: "Новая & Ко", Notes: 1, Comments: 2}},
		Returnees: []Person{{Name: "Старожил",
			Comments: 1, PrevSeenAt: w.Start.Add(-42 * 24 * time.Hour)}},
		Records: []Record{
			{Text: "87 комментариев — самый длинный тред с апреля.", NoteID: "312696"},
			{Text: "21 комментарий за час — рекорд за всю историю наблюдений.", NoteID: "777"},
		},
		StillAlive: []NoteStat{{
			Note:     Note{ID: "888", AuthorName: "Кто-то", Text: "Живая заметка"},
			Comments: 5,
		}},
		ThisWeekNotes: []NoteBrief{{Note: top.Note, Comments: 87}},
		PrevWeekNotes: []NoteBrief{{Note: dispute.Note, Comments: 3}},
		CommentsByNote: map[string][]Comment{
			"555": {{ID: 2, NoteID: "555", AuthorName: "Некто", Text: "реплика спора"}},
		},
	}
}

func draftText(t *testing.T, is *Issue) string {
	t.Helper()
	var b strings.Builder
	if err := WriteDraft(&b, is); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestDraftRoundTrip(t *testing.T) {
	text := draftText(t, sampleIssue())

	if !strings.Contains(text, "&lt;такие&gt;") {
		t.Error("текст заметки должен быть экранирован в маркере")
	}
	if strings.Contains(text, "{что}") {
		t.Error("символы разметки маркера должны быть вычищены из выдержки")
	}

	// strict: незаполненные плейсхолдеры — ошибка.
	if _, err := ParseDraft(strings.NewReader(text), true); err == nil ||
		!strings.Contains(err.Error(), "плейсхолдер") {
		t.Fatalf("strict должен падать на плейсхолдерах: %v", err)
	}

	// -force: секции с плейсхолдерами выпадают, остальное публикуемо.
	d, err := ParseDraft(strings.NewReader(text), false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Dropped != 4 { // week-summary, dispute, quote, topics
		t.Errorf("выброшено секций: %d, ожидалось 4", d.Dropped)
	}
	joined := ""
	for _, sec := range d.Sections {
		joined += strings.Join(sec, "\n") + "\n"
	}
	if strings.Contains(joined, llmMark) {
		t.Error("плейсхолдеры не должны переживать -force")
	}
	for _, want := range []string{
		"Дайджест недели", "{note:312696|", "Новые лица", "Новая &amp; Ко",
		"{note:777|тред}", "{note:888|", "после 6 недель тишины",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("в сухом выпуске нет %q", want)
		}
	}
	// Рекорд про заметку недели не дублирует ссылку на неё.
	if strings.Contains(joined, "{note:312696|тред}") {
		t.Error("рекорд заметки недели не должен получать вторую ссылку")
	}
}

func TestDraftFilledParsesStrict(t *testing.T) {
	text := draftText(t, sampleIssue())
	for _, ph := range []string{llmWeekSummary, llmDispute, llmQuote, llmTopics} {
		text = strings.Replace(text, ph, "Текст рубрики от редактора.", 1)
	}
	d, err := ParseDraft(strings.NewReader(text), true)
	if err != nil {
		t.Fatal(err)
	}
	// 10 рубрик: шапка, о неделе, заметка, спор, цитата, новые лица,
	// возвращение, темы, рекорды, ещё обсуждают.
	if d.Dropped != 0 || len(d.Sections) != 10 {
		t.Errorf("заполненный черновик: dropped=%d sections=%d", d.Dropped, len(d.Sections))
	}
}

func TestParseDraftStructure(t *testing.T) {
	text := "# служебная строка\nодин\nдва\n\nтри\n---\nчетыре\n"
	d, err := ParseDraft(strings.NewReader(text), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Sections) != 2 {
		t.Fatalf("секции: %+v", d.Sections)
	}
	if len(d.Sections[0]) != 2 || d.Sections[0][0] != "один\nдва" || d.Sections[0][1] != "три" {
		t.Errorf("абзацы первой секции: %q", d.Sections[0])
	}
	if len(d.Sections[1]) != 1 || d.Sections[1][0] != "четыре" {
		t.Errorf("вторая секция: %q", d.Sections[1])
	}
}

func TestParseDraftValidation(t *testing.T) {
	cases := []struct{ name, text, wantErr string }{
		{"чужой тег", "<u>х</u>", "не поддерживается"},
		{"незакрытый тег", "<b>жирно", "незакрытый"},
		{"непарное закрытие", "просто</b>", "непарный"},
		{"сырой символ", "5 < 6", "экранировать"},
		{"оборванный маркер", "смотри {note:1|обрыв", "повреждён маркер"},
	}
	for _, tc := range cases {
		_, err := ParseDraft(strings.NewReader(tc.text), true)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: err=%v, ожидалось %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestWriteMaterials(t *testing.T) {
	var b strings.Builder
	if err := WriteMaterials(&b, sampleIssue()); err != nil {
		t.Fatal(err)
	}
	text := b.String()
	for _, want := range []string{
		"Промпт 1", "Промпт 2", "Промпт 3", "Промпт 4",
		"лёгкая ирония",
		"Кандидат: заметка 555", "реплика спора",
		"яркая мысль",
		"[312696] Граф",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("в материалах нет %q", want)
		}
	}
}
