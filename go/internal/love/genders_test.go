package love

import (
	"os"
	"testing"
)

// Пол читается с той же страницы, по которой и так идёт обход тредов, —
// отдельный заход в каждую анкету не нужен.
func TestParseGendersFromRecordedPage(t *testing.T) {
	f, err := os.Open("testdata/comments_312696.html")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got, err := ParseGenders(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 10 {
		t.Fatalf("на странице нашлось %d анкет с полом, ожидалось хотя бы 10", len(got))
	}
	// «Дочь самурая», u981563 — женщина: ник с классом _female в записанной странице.
	if got[981563] != GenderFemale {
		t.Errorf("u981563: пол %q, ожидался женский", got[981563])
	}
	var male, female int
	for _, g := range got {
		switch g {
		case GenderMale:
			male++
		case GenderFemale:
			female++
		}
	}
	if male == 0 || female == 0 {
		t.Errorf("на странице только один пол: мужчин %d, женщин %d", male, female)
	}
	// Ссылки верхней ленты анкет наверху страницы в выборку попадать не должны:
	// это люди, к заметке отношения не имеющие. У ленты нет ников — только
	// аватары, — и селектор берёт именно ники.
	if len(got) > 80 {
		t.Errorf("анкет %d — похоже, в выборку попала верхняя лента", len(got))
	}
}

func TestProfileIDFromHref(t *testing.T) {
	cases := map[string]int64{"/profile/1281493/": 1281493, "/profile/981563": 981563,
		"/notes/1/": 0, "": 0, "/profile/x/": 0}
	for in, want := range cases {
		if got := profileIDFromHref(in); got != want {
			t.Errorf("%q → %d, ожидалось %d", in, got, want)
		}
	}
}
