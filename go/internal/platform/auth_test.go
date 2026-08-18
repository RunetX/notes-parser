package platform

import (
	"strings"
	"testing"
)

// Код диктуют по телефону и набирают, не переключив раскладку. «Т3Н» вместо
// «T3H» — ошибка не человека, а наша, если мы её не приняли.
func TestNormalizeCode(t *testing.T) {
	want := "T3H7K3MQ2XZ"
	for _, in := range []string{
		"T3H-7K3M-Q2XZ",
		"t3h-7k3m-q2xz",
		"  T3H 7K3M Q2XZ  ",
		"«T3H-7K3M-Q2XZ»",
		"Т3Н-7К3М-Q2XZ", // кириллические Т, Н, К, М
	} {
		if got := normalizeCode(in); got != want {
			t.Errorf("normalizeCode(%q) = %q, ожидалось %q", in, got, want)
		}
	}
}

// Сайт волен трогать текст поля «о себе»: схлопывать пробелы, менять эмодзи на
// картинки, переносить строки. Совпадение ищется по канону обеих сторон.
func TestContainsCode(t *testing.T) {
	code := "T3H-7K3M-Q2XZ"
	yes := []string{
		code,
		"люблю кино\n" + code + "\nи море",
		"код: t3h 7k3m q2xz",
		"…" + strings.ToLower(code) + "…",
	}
	for _, in := range yes {
		if !containsCode(in, code) {
			t.Errorf("код не найден в %q", in)
		}
	}
	no := []string{"", "о себе: ничего", "T3H-7K3M-Q2XY", "7K3MQ2XZ"}
	for _, in := range no {
		if containsCode(in, code) {
			t.Errorf("код ложно найден в %q", in)
		}
	}
}

// Алфавит кода — без 0/O/1/I/L: его переписывают руками, и спутанная буква
// стоит человеку ещё одного похода на НГС.
func TestCodeShapeIsReadable(t *testing.T) {
	for _, bad := range []string{"0", "O", "1", "I", "L"} {
		if strings.Contains(codeAlphabet, bad) {
			t.Errorf("в алфавите кода есть спорный знак %q", bad)
		}
	}
	code, err := newCode()
	if err != nil {
		t.Fatal(err)
	}
	if !CodeRe.MatchString(code) {
		t.Fatalf("выданный код %q не подходит под собственный шаблон", code)
	}
	// Два подряд не совпадают — иначе «случайность» была бы декоративной.
	other, err := newCode()
	if err != nil {
		t.Fatal(err)
	}
	if code == other {
		t.Error("два кода подряд совпали")
	}
}

func TestPrefixedColumns(t *testing.T) {
	got := prefixed("u", "id, nick,\n\tkind")
	if got != "u.id, u.nick, u.kind" {
		t.Errorf("prefixed = %q", got)
	}
}
