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
		Forms: quipForms, NoteText: "Муж ушёл к другой, а я осталась с ипотекой и котом",
	}
	ok := "Обида кормится вниманием. Не корми - и она уйдёт следом за ним."

	cases := []struct {
		name string
		text string
		form string
		hook string // деталь из заметки; пусто — проверять нечего
		cfg  validateConfig
		bad  bool
	}{
		{name: "годная реплика", text: ok, form: "буквально", cfg: cfg},
		{name: "пустой текст", text: "   ", form: "буквально", cfg: cfg, bad: true},
		{name: "коротко", text: "Терпи.", form: "буквально", cfg: cfg, bad: true},
		{name: "длинно", text: strings.Repeat("слово ", 100), form: "буквально", cfg: cfg, bad: true},
		{name: "BB-код", text: "[b]Гордыня[/b] тебя и подвела, не он.", form: "буквально", cfg: cfg, bad: true},
		{name: "markdown", text: "**Гордыня** тебя и подвела, а вовсе не он.", form: "буквально", cfg: cfg, bad: true},
		{name: "HTML", text: "<b>Гордыня</b> тебя и подвела, а вовсе не он.", form: "буквально", cfg: cfg, bad: true},
		{
			name: "служебный тег размышления",
			text: "<thinking>надо помягче</thinking> Гордыня тебя и подвела.",
			form: "буквально", cfg: cfg, bad: true,
		},
		{
			name: "длинное тире дожило до валидатора",
			text: "Обида — не любовь, и кормить её не надо ни днём, ни ночью.",
			form: "буквально", cfg: cfg, bad: true,
		},
		{
			name: "обращение к автору в теле",
			text: "Марина, обида кормится вниманием, и ты кормишь её щедро.",
			form: "буквально",
			cfg: validateConfig{MinRunes: 10, MaxRunes: 200, MaxLines: 12,
				AllowEmoji: true, Forms: quipForms, Nicks: []string{"Марина", myNick}},
			bad: true,
		},
		{
			// Оборот перед запятой обращением не считается: сайт выделяет
			// жирным настоящие ники, а переспрос стоит тех самых секунд.
			name: "оборот перед запятой — не обращение",
			text: "Смирение не в том, чтобы молчать, когда стоило бы уйти.",
			form: "буквально",
			cfg: validateConfig{MinRunes: 10, MaxRunes: 200, MaxLines: 12,
				AllowEmoji: true, Forms: quipForms, Nicks: []string{"Марина", myNick}},
		},
		{
			name: "чужая форма",
			text: ok, form: "хайку", cfg: cfg, bad: true,
		},
		{
			// Модель пишет название формы своей строкой, и заглавная буква
			// (как и «е» вместо «ё») — не повод жечь секунды на переспрос.
			name: "форма в другом регистре — та же форма",
			text: ok, form: "Буквально", cfg: cfg,
		},
		{
			// Из-за таких конструкций первые черновики прикольщика и вышли
			// пресными: симметрия читается как остроумие, а смеха не даёт.
			name: "афоризм «дело не в X, а в Y»",
			text: "Дело не в очереди, а в том, что тебе некуда было спешить.",
			form: "буквально", cfg: cfg, bad: true,
		},
		{
			name: "наблюдение сверху",
			text: "Тревожит не то, что ты слушал. Тревожит, что ушёл до конца истории.",
			form: "буквально", cfg: cfg, bad: true,
		},
		{
			// Оборот сам по себе законный: бракуется симметрия целиком, а не
			// первые два слова.
			name: "похожее начало без симметрии",
			text: "Дело не пошло, и я до сих пор храню его зарядку в тумбочке.",
			form: "буквально", cfg: cfg,
		},
		{
			// Замечено на живых черновиках 16.08.2026: с усилием модель роняет
			// в поле text обрывки хода мысли, и они ушли бы на сайт текстом.
			name: "обрывок размышления",
			text: "Сидел, перебирал версии, себя подозревал. Wait, no. Пусть будет так.",
			form: "буквально", cfg: cfg, bad: true,
		},
		{
			name: "слово из двух алфавитов",
			text: "Скобку в конце убрать не успел, а uправила уже нет и не будет.",
			form: "буквально", cfg: cfg, bad: true,
		},
		{
			name: "метка шутки в скобках",
			text: "Кофе тут ни при чём, конечно (сарказм). Дело в очереди.",
			form: "буквально", cfg: cfg, bad: true,
		},
		{
			name: "смех вместо шутки",
			text: "Ахаха, ну ты даёшь, сосед с перфоратором это сильно.",
			form: "буквально", cfg: cfg, bad: true,
		},
		{
			// Скобка-улыбка — родная пунктуация площадки, и она не метка.
			name: "скобка-улыбка не метка",
			text: "Сосед с перфоратором тоже ищет свою половину, судя по темпу)",
			form: "буквально", cfg: cfg,
		},
		{
			name: "пересказ заметки",
			text: "Муж ушёл к другой, а я осталась с ипотекой и котом. Так бывает.",
			form: "буквально", cfg: cfg, bad: true,
		},
		{
			name: "эмодзи при запрете",
			text: "Обида кормится вниманием. Не корми - и она уйдёт следом 🙏",
			form: "буквально",
			cfg: validateConfig{MinRunes: 10, MaxRunes: 200, MaxLines: 12,
				AllowEmoji: false, Forms: quipForms},
			bad: true,
		},
		{
			name: "широкий рисунок",
			text: "Смирение приходит не сразу.\n~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~\nЖди.",
			form: "сценка", cfg: cfg, bad: true,
		},
		{
			name: "узкий рисунок годится",
			text: "Смирение приходит не сразу.\n~ ~ ~\nЖди и не проси лишнего.",
			form: "сценка", cfg: cfg,
		},
		{
			// Деталь названа словами автора, пусть и в другом падеже.
			name: "деталь из заметки",
			text: "Кота-то за что. Он ипотеку не брал.",
			form: "буквально", hook: "с ипотекой и котом", cfg: cfg,
		},
		{
			// Шутка выросла из темы вообще, а деталь придумана: в заметке нет ни
			// дачи, ни тёщи.
			name: "выдуманная деталь",
			text: "Кота-то за что. Он ипотеку не брал.",
			form: "буквально", hook: "дача тёщи", cfg: cfg, bad: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := validate(quip{Text: tc.text, Form: tc.form, Hook: tc.hook}, tc.cfg)
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
	reason := validate(quip{Text: "Гляди в оба.\n" + art, Form: "сценка"}, validateConfig{
		MinRunes: 5, MaxRunes: 400, MaxLines: 20, AllowEmoji: true, Forms: quipForms,
	})
	if reason == "" {
		t.Error("высокий рисунок должен браковаться")
	}
}

// TestPreferFunny — образцы манеры берутся из тех своих комментариев, где
// владелец смеялся. Отметка смеха на площадке — незакрытая скобка, а обычные
// скобки баланс не рушат.
func TestPreferFunny(t *testing.T) {
	funny := make([]string, 0, minFunnyPool)
	for range minFunnyPool {
		funny = append(funny, "ну ты даёшь)")
	}
	pool := append([]string{
		"Считаю, что тут всё сложнее (и это видно по треду).",
		"Никакой улыбки, сплошная серьёзность.",
	}, funny...)

	got := preferFunny(pool)
	if len(got) != minFunnyPool {
		t.Fatalf("осталось %d образцов, ожидалось %d: %v", len(got), minFunnyPool, got)
	}
	for _, s := range got {
		if strings.Count(s, ")") <= strings.Count(s, "(") {
			t.Errorf("в отбор попал комментарий без смеха: %q", s)
		}
	}

	// Смешного мало — берём пул целиком: четыре образца из горстки это уже не
	// манера, а заучивание конкретных шуток.
	small := []string{"ну ты даёшь)", "серьёзно и по делу", "(в скобках)"}
	if got := preferFunny(small); len(got) != len(small) {
		t.Errorf("маленький пул должен возвращаться целиком: %v", got)
	}
}

func TestPickForms(t *testing.T) {
	all := []string{"буквально", "гипербола", "сценка", "самоирония"}

	got := pickForms([]string{"буквально", "гипербола"}, all, 2)
	if contains(got, "буквально") || contains(got, "гипербола") {
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
	got = pickForms([]string{"буквально"}, all, 5)
	if contains(got, "буквально") || len(got) != 3 {
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
