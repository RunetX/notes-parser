package holidays

import (
	"os"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

// Фикстуры — настоящие страницы, записанные 24.08.2026. День выбран не
// случайно: у обоих календарей он содержит и то, что нам нужно (день вафель,
// изобретение чипсов), и то, что мы обязаны отсечь (День независимости
// Украины, Варфоломеевская ночь).
func fixture(t *testing.T, name string) *goquery.Document {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	defer f.Close()
	doc, err := goquery.NewDocumentFromReader(f)
	if err != nil {
		t.Fatalf("разбор фикстуры: %v", err)
	}
	return doc
}

func find(list []Occasion, title string) (Occasion, bool) {
	for _, o := range list {
		if strings.Contains(o.Title, title) {
			return o, true
		}
	}
	return Occasion{}, false
}

func TestParseCalend(t *testing.T) {
	got, err := parseCalend(fixture(t, "calend_2026-08-24.html"))
	if err != nil {
		t.Fatalf("разбор calend.ru: %v", err)
	}
	cases := []struct {
		title string
		kind  Kind
		scope Scope
		year  int
	}{
		{"День странной музыки", KindHoliday, ScopeWorld, 0},
		{"День бизнес-наставника", KindHoliday, ScopeRussia, 0},
		{"День независимости Украины", KindHoliday, ScopeForeign, 0},
		{"День святого Варфоломея", KindHoliday, ScopeReligious, 0},
		{"Евпатий Коловрат", KindFolk, ScopeWorld, 0},
		{"чипсов", KindHistory, ScopeWorld, 1853},
	}
	for _, c := range cases {
		o, ok := find(got, c.title)
		if !ok {
			t.Errorf("не найден повод %q", c.title)
			continue
		}
		if o.Kind != c.kind {
			t.Errorf("%q: вид %v, ожидался %v", c.title, o.Kind, c.kind)
		}
		if o.Scope != c.scope {
			t.Errorf("%q: чей %v, ожидался %v", c.title, o.Scope, c.scope)
		}
		if o.Year != c.year {
			t.Errorf("%q: год %d, ожидался %d", c.title, o.Year, c.year)
		}
	}
	// Именины — ОДНА строка на день, а не повод на каждое имя: их бывает под
	// два десятка, и списком они вытеснили бы из промпта всё остальное.
	names := 0
	for _, o := range got {
		if o.Kind == KindName {
			names++
			for _, want := range []string{"Александр", "Мария", "Ульяна"} {
				if !strings.Contains(o.Title, want) {
					t.Errorf("в именинах нет %q: %q", want, o.Title)
				}
			}
			if strings.Contains(o.Title, "защитник") {
				t.Errorf("в именины попало толкование имени: %q", o.Title)
			}
		}
	}
	if names != 1 {
		t.Errorf("строк именин %d, ожидалась одна", names)
	}
	// «Ближайшие дни компаний» и «Ближайшие дни городов» размечены тем же
	// классом, что праздники, но это БУДУЩИЕ даты: попав в утро, они соврут.
	for _, o := range got {
		if strings.Contains(strings.ToLower(o.Title), "день города") {
			t.Errorf("в поводы дня затесалась будущая дата: %q", o.Title)
		}
	}
}

func TestParseWiki(t *testing.T) {
	got, err := parseWiki(fixture(t, "wiki_24_avgusta.html"))
	if err != nil {
		t.Fatalf("разбор Википедии: %v", err)
	}
	// Страна отделяется от названия, а раздел задаёт «чей праздник».
	if o, ok := find(got, "День независимости"); !ok {
		t.Error("не найден праздник из раздела «Национальные»")
	} else {
		if o.Scope != ScopeForeign {
			t.Errorf("«%s»: чей %v, ожидался чужой", o.Title, o.Scope)
		}
		if strings.Contains(o.Title, "Украина") {
			t.Errorf("страна осталась в названии: %q", o.Title)
		}
	}
	if o, ok := find(got, "святого Варфоломея"); !ok {
		t.Error("не найдена дата из раздела «Религиозные»")
	} else if o.Scope != ScopeReligious {
		t.Errorf("«%s»: чей %v, ожидался церковный", o.Title, o.Scope)
	}
	// События: год отделён, сноска снята.
	o, ok := find(got, "Везувия")
	if !ok {
		t.Fatal("не найдено событие про Везувий")
	}
	if o.Kind != KindHistory || o.Year != 79 {
		t.Errorf("событие: вид %v, год %d — ожидались событие и 79", o.Kind, o.Year)
	}
	for _, x := range got {
		if reWikiRef.MatchString(x.Title) {
			t.Errorf("сноска Википедии осталась в названии: %q", x.Title)
		}
	}
	// Разделы «Родились» и «Скончались» в поводы дня не идут.
	if _, ok := find(got, "Борхес"); ok {
		t.Error("в поводы дня попал раздел «Родились»")
	}
	// Незнакомый подзаголовок пропускается целиком: под «Именинами» лежит
	// поимённый список святых, и разбирать его как праздники нельзя.
	if o, ok := find(got, "синклитикия"); ok {
		t.Errorf("в поводы дня попали именины: %q", o.Title)
	}
}

// TestMergeCountsSources — согласие источников это и есть мера достоверности:
// повод, названный дважды, копит список назвавших и идёт выше одиночного.
func TestMergeCountsSources(t *testing.T) {
	a := []Occasion{
		{Title: "Национальный день вафель в США", Kind: KindHoliday, Sources: []string{"calend.ru"}},
		{Title: "День странной музыки", Kind: KindHoliday, Sources: []string{"calend.ru"}},
	}
	b := []Occasion{
		// То же самое, но названо короче — вхождение подстрокой обязано склеить.
		{Title: "Национальный день вафель", Kind: KindHoliday, Sources: []string{"wikipedia"}},
	}
	got := Merge(a, b)
	if len(got) != 2 {
		t.Fatalf("после слияния %d поводов, ожидалось 2: %+v", len(got), got)
	}
	if len(got[0].Sources) != 2 {
		t.Errorf("повод двух источников не поднялся наверх: %+v", got)
	}
	if got[0].Title != "Национальный день вафель в США" {
		t.Errorf("осталось менее информативное название: %q", got[0].Title)
	}
}

// TestMergeKeepsShortNamesApart — короткие названия склеиваются только точным
// совпадением: «День семьи» и «День семьи, любви и верности» — разные дни.
func TestMergeKeepsShortNamesApart(t *testing.T) {
	got := Merge(
		[]Occasion{{Title: "День семьи", Kind: KindHoliday, Sources: []string{"a"}}},
		[]Occasion{{Title: "День семьи, любви и верности", Kind: KindHoliday, Sources: []string{"b"}}},
	)
	if len(got) != 2 {
		t.Errorf("разные праздники склеились: %+v", got)
	}
}

// TestMergeTakesStricterScope — если хоть один источник назвал повод церковным,
// фильтр обязан это увидеть.
func TestMergeTakesStricterScope(t *testing.T) {
	got := Merge(
		[]Occasion{{Title: "День святого Варфоломея", Kind: KindHoliday, Scope: ScopeWorld, Sources: []string{"a"}}},
		[]Occasion{{Title: "День святого Варфоломея", Kind: KindHoliday, Scope: ScopeReligious, Sources: []string{"b"}}},
	)
	if len(got) != 1 || got[0].Scope != ScopeReligious {
		t.Errorf("строгая метка потерялась при слиянии: %+v", got)
	}
}

// TestFilterOnLiveDay — фильтр на настоящем дне: что осталось и что ушло.
func TestFilterOnLiveDay(t *testing.T) {
	calend, err := parseCalend(fixture(t, "calend_2026-08-24.html"))
	if err != nil {
		t.Fatalf("calend: %v", err)
	}
	wiki, err := parseWiki(fixture(t, "wiki_24_avgusta.html"))
	if err != nil {
		t.Fatalf("wiki: %v", err)
	}
	kept := Filter(Merge(calend, wiki))

	mustGo := []string{"День независимости", "Варфоломе", "Везувия", "Римской империи"}
	for _, bad := range mustGo {
		if o, ok := find(kept, bad); ok {
			t.Errorf("в утро прошёл повод %q (причина отказа: %q)", o.Title, Reject(o))
		}
	}
	mustStay := []string{"чипс", "вафельниц", "странной музыки", "бизнес-наставника"}
	for _, good := range mustStay {
		if _, ok := find(kept, good); !ok {
			t.Errorf("отфильтрован годный повод %q", good)
		}
	}
	if len(kept) == 0 {
		t.Fatal("после фильтра не осталось ничего")
	}
	// Плата за правило «чужие государственные — мимо», названная честно:
	// вместе с Днём независимости уходит и безобидный американский день
	// вафель. Разбирать чужие праздники на «государственные» и «весёлые»
	// нечем — рубрика у календаря одна, по стране.
	if o, ok := find(kept, "день вафель"); ok {
		t.Errorf("чужой праздник прошёл фильтр: %q", o.Title)
	}
	// Согласие источников по истории видно только при слиянии по году:
	// формулировки у календарей расходятся целиком.
	if o, ok := find(kept, "чипс"); ok && len(o.Sources) != 2 {
		t.Errorf("чипсы названы двумя календарями, а источников %v", o.Sources)
	}
	// Служебная вёрстка Википедии в повод не течёт.
	for _, o := range kept {
		if strings.Contains(o.Title, "mw-parser-output") {
			t.Errorf("в повод приехал CSS шаблона: %.80q", o.Title)
		}
	}
}

func TestRejectReasons(t *testing.T) {
	cases := []struct {
		o      Occasion
		reject bool
	}{
		{Occasion{Title: "День вафель", Scope: ScopeWorld}, false},
		{Occasion{Title: "День шахтёра", Scope: ScopeRussia}, false},
		{Occasion{Title: "День независимости Эстонии", Scope: ScopeForeign}, true},
		{Occasion{Title: "День святого Варфоломея", Scope: ScopeReligious}, true},
		{Occasion{Title: "День памяти жертв депортации", Scope: ScopeRussia}, true},
		{Occasion{Title: "Битва при Мардж Дабике", Kind: KindHistory}, true},
		// Замер списка: эти профессиональные даты светлые, и корни, которые их
		// убивали бы, из стоп-списка убраны.
		{Occasion{Title: "День оружейника", Scope: ScopeRussia}, false},
		{Occasion{Title: "День пожарной охраны", Scope: ScopeRussia}, false},
		{Occasion{Title: "День спасателя", Scope: ScopeRussia}, false},
	}
	for _, c := range cases {
		got := Reject(c.o)
		if (got != "") != c.reject {
			t.Errorf("Reject(%q) = %q, ожидался отказ=%v", c.o.Title, got, c.reject)
		}
	}
}

// TestParseOmens — приметы лежат внутри абзаца статьи народного календаря, а не
// списком: берём их предложениями и по признакам самой приметы. Жирным календарь
// метит одни, а не другие, поэтому опираться на вёрстку нельзя.
func TestParseOmens(t *testing.T) {
	got := parseOmens(fixture(t, "calend_narod_6705.html"))
	if len(got) == 0 {
		t.Fatal("приметы не разобраны")
	}
	if len(got) > maxOmens {
		t.Errorf("примет %d, потолок %d", len(got), maxOmens)
	}
	for _, o := range got {
		if o.Kind != KindOmen {
			t.Errorf("%q: вид %v", o.Title, o.Kind)
		}
		if strings.Contains(strings.ToLower(o.Title), "примечали") {
			t.Errorf("зачин не срезан: %q", o.Title)
		}
		t.Logf("примета: %s", o.Title)
	}
	if !strings.Contains(got[0].Title, "иней") {
		t.Errorf("первая примета не про иней: %q", got[0].Title)
	}
}
