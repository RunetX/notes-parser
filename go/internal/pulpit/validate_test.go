package pulpit

import (
	"strings"
	"testing"
)

func TestNormalizeTypography(t *testing.T) {
	got := normalize("Он сказал: «люблю» — и ушёл")
	want := `Он сказал: "люблю" - и ушёл`
	if got != want {
		t.Errorf("нормализация тире и ёлочек:\nполучено %q\nожидалось %q", got, want)
	}
}

// TestNormalizeIndentIdempotent — NBSP ставит инструмент, и ставит один раз:
// повторная генерация после брака не должна удваивать отступы.
func TestNormalizeIndentIdempotent(t *testing.T) {
	src := "  Отступ в две клетки\nобычный текст\nслово    и ещё"
	once := normalize(src)
	twice := normalize(once)
	if once != twice {
		t.Fatalf("нормализация не идемпотентна:\n1) %q\n2) %q", once, twice)
	}
	if !strings.HasPrefix(once, string(nbsp)+string(nbsp)) {
		t.Errorf("ведущие пробелы не стали неразрывными: %q", once)
	}
	if strings.Contains(once, "  ") {
		t.Errorf("обычные пробеги пробелов остались (на сайте схлопнутся): %q", once)
	}
	// Одиночные пробелы между словами не трогаем: иначе текст станет
	// неразрывным полотном.
	if !strings.Contains(once, "Отступ в две") {
		t.Errorf("одиночные пробелы перебиты: %q", once)
	}
}

func TestNormalizeBlankLines(t *testing.T) {
	got := normalize("абзац\n\n\n\n\nвторой   \n\n")
	if strings.Count(got, "\n\n\n\n") > 0 {
		t.Errorf("пустые строки не схлопнуты: %q", got)
	}
	if strings.HasSuffix(got, "\n") {
		t.Errorf("хвостовые переводы строк остались: %q", got)
	}
}

func TestValidate(t *testing.T) {
	cfg := validateConfig{
		MinRunes: 10, MaxRunes: 200, MaxLines: 12, AllowEmoji: true,
		Forms: sermonForms, NoteText: "Муж ушёл к другой, а я осталась с ипотекой и котом",
	}
	ok := "Обида кормится вниманием. Не корми - и она уйдёт следом за ним."

	cases := []struct {
		name string
		text string
		form string
		cfg  validateConfig
		bad  bool
	}{
		{name: "годная реплика", text: ok, form: "укор", cfg: cfg},
		{name: "пустой текст", text: "   ", form: "укор", cfg: cfg, bad: true},
		{name: "коротко", text: "Терпи.", form: "укор", cfg: cfg, bad: true},
		{name: "длинно", text: strings.Repeat("слово ", 100), form: "укор", cfg: cfg, bad: true},
		{name: "BB-код", text: "[b]Гордыня[/b] тебя и подвела, не он.", form: "укор", cfg: cfg, bad: true},
		{name: "markdown", text: "**Гордыня** тебя и подвела, а вовсе не он.", form: "укор", cfg: cfg, bad: true},
		{name: "HTML", text: "<b>Гордыня</b> тебя и подвела, а вовсе не он.", form: "укор", cfg: cfg, bad: true},
		{
			name: "служебный тег размышления",
			text: "<thinking>надо помягче</thinking> Гордыня тебя и подвела.",
			form: "укор", cfg: cfg, bad: true,
		},
		{
			name: "длинное тире дожило до валидатора",
			text: "Обида — не любовь, и кормить её не надо ни днём, ни ночью.",
			form: "укор", cfg: cfg, bad: true,
		},
		{
			name: "обращение к автору в теле",
			text: "Марина, обида кормится вниманием, и ты кормишь её щедро.",
			form: "укор",
			cfg: validateConfig{MinRunes: 10, MaxRunes: 200, MaxLines: 12,
				AllowEmoji: true, Forms: sermonForms, Nicks: []string{"Марина", myNick}},
			bad: true,
		},
		{
			// Оборот перед запятой обращением не считается: сайт выделяет
			// жирным настоящие ники, а переспрос стоит тех самых секунд.
			name: "оборот перед запятой — не обращение",
			text: "Смирение не в том, чтобы молчать, когда стоило бы уйти.",
			form: "укор",
			cfg: validateConfig{MinRunes: 10, MaxRunes: 200, MaxLines: 12,
				AllowEmoji: true, Forms: sermonForms, Nicks: []string{"Марина", myNick}},
		},
		{
			name: "чужая форма",
			text: ok, form: "хайку", cfg: cfg, bad: true,
		},
		{
			name: "пересказ заметки",
			text: "Муж ушёл к другой, а я осталась с ипотекой и котом. Так бывает.",
			form: "укор", cfg: cfg, bad: true,
		},
		{
			name: "эмодзи при запрете",
			text: "Обида кормится вниманием. Не корми - и она уйдёт следом 🙏",
			form: "укор",
			cfg: validateConfig{MinRunes: 10, MaxRunes: 200, MaxLines: 12,
				AllowEmoji: false, Forms: sermonForms},
			bad: true,
		},
		{
			name: "широкий рисунок",
			text: "Смирение приходит не сразу.\n~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~\nЖди.",
			form: "притча", cfg: cfg, bad: true,
		},
		{
			name: "узкий рисунок годится",
			text: "Смирение приходит не сразу.\n~ ~ ~\nЖди и не проси лишнего.",
			form: "притча", cfg: cfg,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := validate(tc.text, tc.form, tc.cfg)
			if tc.bad && reason == "" {
				t.Errorf("ожидался брак, реплика принята: %q", tc.text)
			}
			if !tc.bad && reason != "" {
				t.Errorf("реплика забракована зря: %s", reason)
			}
		})
	}
}

// TestValidateArtBlockHeight — блочная картинка выше пяти строк не годится:
// шрифт на площадке пропорциональный.
func TestValidateArtBlockHeight(t *testing.T) {
	art := strings.Repeat("/\\_/\\\n", 6)
	reason := validate("Гляди в оба.\n"+art, "притча", validateConfig{
		MinRunes: 5, MaxRunes: 400, MaxLines: 20, AllowEmoji: true, Forms: sermonForms,
	})
	if reason == "" {
		t.Error("высокий рисунок должен браковаться")
	}
}

func TestPickForms(t *testing.T) {
	all := []string{"укор", "заповедь", "притча", "приговор"}

	got := pickForms([]string{"укор", "заповедь"}, all, 2)
	if contains(got, "укор") || contains(got, "заповедь") {
		t.Errorf("формы последних реплик должны выпасть: %v", got)
	}
	if len(got) != 2 {
		t.Errorf("остаться должны две формы: %v", got)
	}

	// Кулдаун длиннее набора: молчать из-за него нельзя.
	got = pickForms(all, all, 10)
	if len(got) != len(all) {
		t.Errorf("при полном запрете возвращаем весь набор: %v", got)
	}

	// История короче кулдауна — не паникуем и режем по факту.
	got = pickForms([]string{"укор"}, all, 5)
	if contains(got, "укор") || len(got) != 3 {
		t.Errorf("короткая история: %v", got)
	}
}

func TestPickSamplesDeterministic(t *testing.T) {
	pool := []string{"а", "б", "в", "г", "д"}
	first := pickSamples(pool, "312900", 3)
	again := pickSamples(pool, "312900", 3)
	if len(first) != 3 || strings.Join(first, "|") != strings.Join(again, "|") {
		t.Fatalf("выбор образцов не воспроизводится: %v против %v", first, again)
	}
	if pickSamples(nil, "312900", 3) != nil {
		t.Error("пустой пул — пустой выбор")
	}
	if len(pickSamples(pool, "1", 10)) != len(pool) {
		t.Error("просят больше, чем есть: отдаём всё")
	}
}
