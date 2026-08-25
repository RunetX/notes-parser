package sitetext

import (
	"strings"
	"testing"
)

func TestNormalizeTypography(t *testing.T) {
	got := Normalize("Он сказал: «люблю» — и ушёл")
	want := `Он сказал: "люблю" - и ушёл`
	if got != want {
		t.Errorf("нормализация тире и ёлочек:\nполучено %q\nожидалось %q", got, want)
	}
}

// TestNormalizeIndentIdempotent — NBSP ставит инструмент, и ставит один раз:
// повторная генерация после брака не должна удваивать отступы.
func TestNormalizeIndentIdempotent(t *testing.T) {
	src := "  Отступ в две клетки\nобычный текст\nслово    и ещё"
	once := Normalize(src)
	twice := Normalize(once)
	if once != twice {
		t.Fatalf("нормализация не идемпотентна:\n1) %q\n2) %q", once, twice)
	}
	if !strings.HasPrefix(once, string(NBSP)+string(NBSP)) {
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
	got := Normalize("абзац\n\n\n\n\nвторой   \n\n")
	if strings.Count(got, "\n\n\n\n") > 0 {
		t.Errorf("пустые строки не схлопнуты: %q", got)
	}
	if strings.HasSuffix(got, "\n") {
		t.Errorf("хвостовые переводы строк остались: %q", got)
	}
}

// TestMachineTell — таблица общих примет генерации. Порядок в machineTells и
// есть порядок проверки, поэтому случаи проверяются по одному.
func TestMachineTell(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"чисто", "Доброе утро. Сегодня день вафель, и это не шутка.", false},
		{"тег размышления", "<thinking>надо пошутить</thinking> Доброе утро", true},
		{"обрывок хода мысли", "Доброе утро. Wait, no. Сегодня вторник", true},
		{"два алфавита", "Доброе uтро всем", true},
		{"обломок JSON", "Сегодня день вафель.}", true},
		{"BB-код", "[b]Доброе утро[/b]", true},
		{"HTML-тег", "<b>Доброе утро</b>", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MachineTell(c.text)
			if (got != "") != c.want {
				t.Errorf("MachineTell(%q) = %q, ожидалось срабатывание=%v", c.text, got, c.want)
			}
		})
	}
}

func TestMarkdownAndTypographyHit(t *testing.T) {
	if MarkdownHit("**жирный** текст") == "" {
		t.Error("markdown не пойман: на сайте он напечатается буквально")
	}
	if MarkdownHit("Обычный текст без разметки") != "" {
		t.Error("markdown почудился в чистом тексте")
	}
	// Страховка от дырки в Normalize: типографский знак, доживший до проверки.
	if TypographyHit("тире — здесь") == "" {
		t.Error("длинное тире не поймано")
	}
	if TypographyHit(Normalize("тире — здесь")) != "" {
		t.Error("после Normalize типографика остаться не должна")
	}
}

func TestLatinFragment(t *testing.T) {
	if LatinFragment("Календарь справился бы быстрее.pt") == "" {
		t.Error("латинский огрызок не пойман")
	}
	// Живые слова латиницей длиннее трёх букв — их не трогаем.
	if got := LatinFragment("Пишет в WhatsApp каждое утро"); got != "" {
		t.Errorf("живое слово латиницей забраковано: %q", got)
	}
}

// TestForeignScript — иероглиф посреди русской фразы. Поймано живым прогоном
// утренней заметки 25.08.2026: «улетел на край系 и прислал домой фотографии».
// Проверка «слово из двух алфавитов» его не видела — она про латиницу.
func TestForeignScript(t *testing.T) {
	cases := []struct {
		text string
		bad  bool
	}{
		{"улетел на край系 и прислал фотографии", true},
		{"обычная русская фраза про утро", false},
		{"Linux и Вояджер - латиница законна", false},
		{"кафе «Père» с диакритикой тоже", false},
		{"эмодзи ☀️ и цифры 1989 не буквы", false},
		{"иврит שלום в тексте", true},
	}
	for _, c := range cases {
		if got := ForeignScript(c.text) != ""; got != c.bad {
			t.Errorf("ForeignScript(%q) = %v, ожидалось %v", c.text, got, c.bad)
		}
		if c.bad && MachineTell(c.text) == "" {
			t.Errorf("MachineTell пропустил чужую письменность: %q", c.text)
		}
	}
}
