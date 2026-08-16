package main

import (
	"flag"
	"slices"
	"testing"
)

// reorderArgs узнаёт у самого набора флагов, кто из них берёт значение.
// Ручной список имён рядом с flag.String отставал от него молча: пропущенное
// имя означает, что значение флага уедет в позиционные аргументы, а действие
// команды — в значение флага.
func TestReorderArgsAsksTheFlagSet(t *testing.T) {
	newFS := func() *flag.FlagSet {
		fs := flag.NewFlagSet("тест", flag.ContinueOnError)
		fs.String("db", "", "путь к БД")
		fs.Int("top", 0, "сколько строк")
		fs.Bool("html", false, "собрать HTML")
		return fs
	}
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"значение и действие", []string{"-db", "a.db", "portrait", "u1"}, []string{"-db", "a.db", "portrait", "u1"}},
		{"флаги после действия", []string{"portrait", "-db", "a.db", "u1"}, []string{"-db", "a.db", "portrait", "u1"}},
		{"булев не съедает действие", []string{"-html", "report"}, []string{"-html", "report"}},
		{"булев перед действием в хвосте", []string{"report", "-html"}, []string{"-html", "report"}},
		{"через знак равенства", []string{"portrait", "-top=5", "u1"}, []string{"-top=5", "portrait", "u1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reorderArgs(c.in, newFS())
			if !slices.Equal(got, c.want) {
				t.Errorf("reorderArgs(%q) = %q, ждали %q", c.in, got, c.want)
			}
			fs := newFS()
			if err := fs.Parse(got); err != nil {
				t.Fatalf("разбор %q: %v", got, err)
			}
			if fs.NArg() == 0 {
				t.Errorf("действие пропало из позиционных: %q", got)
			}
		})
	}
}
