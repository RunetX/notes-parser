package textutil

import "testing"

func TestOneLine(t *testing.T) {
	if got := OneLine("первый абзац\n\n  второй\tабзац "); got != "первый абзац второй абзац" {
		t.Errorf("OneLine: %q", got)
	}
}

// Три обрезки различаются контрактом по длине — тест это и стережёт.
func TestTruncateContracts(t *testing.T) {
	const s = "абвгде "
	if got := Truncate(s, 3); got != "абв…" {
		t.Errorf("Truncate: %q", got)
	}
	if got := TruncateTrim("абв где", 4); got != "абв…" {
		t.Errorf("TruncateTrim должна снимать висящий пробел: %q", got)
	}
	if got := Fit(s, 3); got != "аб…" {
		t.Errorf("Fit должна укладываться в лимит целиком: %q", got)
	}
	if got := []rune(Fit(s, 3)); len(got) != 3 {
		t.Errorf("Fit вернула %d рун при лимите 3", len(got))
	}
	// Короткая строка не трогается ни одной из трёх.
	for name, fn := range map[string]func(string, int) string{
		"Truncate": Truncate, "TruncateTrim": TruncateTrim, "Fit": Fit,
	} {
		if got := fn("аб", 5); got != "аб" {
			t.Errorf("%s изменила короткую строку: %q", name, got)
		}
	}
	if got := Fit("абвг", 1); got != "…" {
		t.Errorf("Fit при лимите 1: %q", got)
	}
}
