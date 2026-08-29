package narod

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureDir = "testdata/cards"

func TestLoadCardsReadsCatalog(t *testing.T) {
	cards, err := LoadCards(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 {
		t.Fatalf("карточек %d, ожидалась 1", len(cards))
	}
	c := cards[0]
	if c.ID != "zavhoz" || c.Persona.Nick != "Завхоз" {
		t.Fatalf("прочиталось не то: %s / %s", c.ID, c.Persona.Nick)
	}
	if c.Register.Runes.Median != 61 {
		t.Errorf("медиана длины %d, в файле 61", c.Register.Runes.Median)
	}
	if len(c.Errors) != 2 {
		t.Errorf("ошибок %d, в файле 2", len(c.Errors))
	}
}

// Каталог — это данные: чтобы добавить жителя, кода трогать не надо (DoD брифа).
// Тест проверяет это буквально — вторая карточка кладётся файлом.
func TestCatalogScalesByFilesAlone(t *testing.T) {
	dir := t.TempDir()
	base, err := os.ReadFile(filepath.Join(fixtureDir, "zavhoz.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "zavhoz.json"), base, 0o600); err != nil {
		t.Fatal(err)
	}
	second := strings.NewReplacer(`"id": "zavhoz"`, `"id": "vetrenaya"`,
		`"nick": "Завхоз"`, `"nick": "Ветреная"`).Replace(string(base))
	if err := os.WriteFile(filepath.Join(dir, "vetrenaya.json"), []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}

	cards, err := LoadCards(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 {
		t.Fatalf("карточек %d, ожидалось 2", len(cards))
	}
	// Порядок обязан быть устойчивым: от него зависят броски кубика в тестах.
	if cards[0].ID != "vetrenaya" || cards[1].ID != "zavhoz" {
		t.Errorf("порядок каталога не по имени файла: %s, %s", cards[0].ID, cards[1].ID)
	}
}

func TestLoadCardsRejectsDuplicateNick(t *testing.T) {
	dir := t.TempDir()
	base, err := os.ReadFile(filepath.Join(fixtureDir, "zavhoz.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.json"), base, 0o600); err != nil {
		t.Fatal(err)
	}
	twin := strings.Replace(string(base), `"id": "zavhoz"`, `"id": "dvojnik"`, 1)
	if err := os.WriteFile(filepath.Join(dir, "b.json"), []byte(twin), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCards(dir); err == nil {
		t.Fatal("два жителя с одним ником приняты — адресация «Ник, …» в треде стала бы неразрешимой")
	}
}

// Наружу выходят только композиты. Правило записано дважды (здесь и в проверке
// конфигурации), потому что цена ошибки — не поломка, а опубликованная под
// чужой манерой реплика.
func TestCheckLiveRejectsSnapshot(t *testing.T) {
	cards, err := LoadCards(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckLive(cards); err != nil {
		t.Fatalf("композит не пущен в live: %v", err)
	}
	cards[0].Kind = KindSnapshot
	if err := CheckLive(cards); err == nil {
		t.Fatal("слепок реального участника пущен в live")
	}
	if err := CheckLive(nil); err == nil {
		t.Fatal("пустой каталог принят: играть некому, а служба молчала бы как исправная")
	}
}

func TestValidate(t *testing.T) {
	good, err := LoadCard(filepath.Join(fixtureDir, "zavhoz.json"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		bad  func(*Card)
	}{
		{"пустой id", func(c *Card) { c.ID = "" }},
		{"id не slug", func(c *Card) { c.ID = "Завхоз" }},
		{"неизвестный сорт", func(c *Card) { c.Kind = "actor" }},
		{"короткий ник у жителя", func(c *Card) { c.Persona.Nick = "Ку" }},
		{"без ника", func(c *Card) { c.Persona.Nick = "" }},
		{"ник с пробелами", func(c *Card) { c.Persona.Nick = " Завхоз " }},
		{"нет замера длины", func(c *Card) { c.Register.Runes.Median = 0 }},
		{"квантили наоборот", func(c *Card) { c.Register.Runes.P10, c.Register.Runes.P90 = 200, 20 }},
		{"вероятность вне [0,1]", func(c *Card) { c.Dice.ComeToNote = 1.4 }},
		{"образцы у композита", func(c *Card) { c.Samples = []Sample{{Text: "чужая фраза"}} }},
		{"отрицательная частота ошибки", func(c *Card) { c.Errors[0].Rate = -1 }},
		{"фрустрация не из списка", func(c *Card) { c.Persona.Frustration = "лёгкая грусть" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := good
			c.Errors = append([]ErrorPattern(nil), good.Errors...)
			tc.bad(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("принята карточка: %s", tc.name)
			}
		})
	}
}

// Фрустрация — величина БЕЗ замера, и потому список её видов закрыт: свободная
// строка через десяток жителей даёт «взаимное уважение с оттенком иронии», и
// сравнивать миры между собой становится нечем. Тот же довод, что у EpisodeKinds.
func TestFrustrationIsAClosedList(t *testing.T) {
	good, err := LoadCard(filepath.Join(fixtureDir, "zavhoz.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range FrustrationKinds {
		c := good
		c.Persona.Frustration = k
		if err := c.Validate(); err != nil {
			t.Errorf("вид %q из собственного списка отвергнут: %v", k, err)
		}
	}
	// Пусто — законно: человек без больного места тоже человек.
	c := good
	c.Persona.Frustration = ""
	if err := c.Validate(); err != nil {
		t.Errorf("карточка без больного места отвергнута: %v", err)
	}
}

// Короткий ник запрещён жителю, но не слепку: правило это про площадку (она
// раздаёт по нику поводы «вас упомянули»), а слепок на площадку не попадает
// никогда — и ник у него не наш, а тот, что был у человека.
func TestShortNickAllowedForSnapshotOnly(t *testing.T) {
	c, err := LoadCard(filepath.Join(fixtureDir, "zavhoz.json"))
	if err != nil {
		t.Fatal(err)
	}
	c.Persona.Nick = "ДВ" // настоящий ник из архива
	if err := c.Validate(); err == nil {
		t.Error("житель с двухбуквенным ником принят")
	}
	c.Kind, c.Samples = KindSnapshot, nil
	if err := c.Validate(); err != nil {
		t.Errorf("слепок с настоящим коротким ником отвергнут: %v", err)
	}
}

// Опечатка в имени поля обнулила бы замер молча — а карточка без замера
// неотличима от карточки с нулём.
func TestLoadCardRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	base, err := os.ReadFile(filepath.Join(fixtureDir, "zavhoz.json"))
	if err != nil {
		t.Fatal(err)
	}
	typo := strings.Replace(string(base), `"vocab_rate"`, `"vocabrate"`, 1)
	path := filepath.Join(dir, "typo.json")
	if err := os.WriteFile(path, []byte(typo), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCard(path); err == nil {
		t.Fatal("карточка с неизвестным полем принята")
	}
}

func TestWriteCardBrief(t *testing.T) {
	c, err := LoadCard(filepath.Join(fixtureDir, "zavhoz.json"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := WriteCardBrief(&b, c); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"Завхоз", "Бердск", "54 года", "61 знак", "sd 5.4", "Эмодзи: не ставишь"} {
		if !strings.Contains(out, want) {
			t.Errorf("в брифе нет %q:\n%s", want, out)
		}
	}
	// Бриф уходит в промпт, а у композита дословных образцов нет вовсе — значит
	// и у слепка бриф не вправе их показывать: калибровка и живая игра обязаны
	// идти по одному описанию.
	c.Kind, c.Samples = KindSnapshot, []Sample{{Text: "дословная фраза донора"}}
	b.Reset()
	if err := WriteCardBrief(&b, c); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "дословная фраза донора") {
		t.Error("бриф печатает дословный образец донора — он уходит в промпт")
	}
}

func TestNicks(t *testing.T) {
	cards, err := LoadCards(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := Nicks(cards); len(got) != 1 || got[0] != "Завхоз" {
		t.Errorf("ники каталога: %v", got)
	}
}

// Знакомство накладывается МНОЖИТЕЛЕМ, а не готовой вероятностью: позиция в
// треде и знакомство отвечают на разные вопросы, а обе готовые вероятности
// содержат общую базу, которая учлась бы дважды.
func TestFamiliarLift(t *testing.T) {
	r := ReplyRate{Familiar: []RateBucket{
		{Upto: 0, Chances: 1000, Answers: 5, ToHimChances: 100, ToHimAnswers: 55},
		{Upto: 1 << 30, Chances: 1000, Answers: 15, ToHimChances: 100, ToHimAnswers: 60},
	}}
	// Среднее по обеим корзинам — 20/2000 = 1 %.
	if lift, ok := r.FamiliarLift(0, false); !ok || lift != 0.5 {
		t.Errorf("незнакомый: множитель %v (ok=%v), ожидалось 0.5", lift, ok)
	}
	if lift, ok := r.FamiliarLift(100, false); !ok || lift != 1.5 {
		t.Errorf("свой: множитель %v (ok=%v), ожидалось 1.5", lift, ok)
	}
	// У прямого обращения замер показал почти ровную линию — множитель около
	// единицы, и это правильно: знакомство решает, влезешь ли ты в ЧУЖОЙ
	// разговор, а не ответишь ли, когда обратились к тебе.
	lift, ok := r.FamiliarLift(0, true)
	if !ok || lift < 0.9 || lift > 1.1 {
		t.Errorf("обращение к незнакомому: множитель %v (ok=%v), ожидалось около 1", lift, ok)
	}
}

// Без замера множитель РАВЕН ЕДИНИЦЕ и говорит об этом: подставить сюда ноль
// значило бы молча запретить персонажу отвечать.
func TestFamiliarLiftWithoutMeasurement(t *testing.T) {
	if lift, ok := (ReplyRate{}).FamiliarLift(5, false); ok || lift != 1 {
		t.Errorf("по пустому замеру множитель %v (ok=%v)", lift, ok)
	}
	thin := ReplyRate{Familiar: []RateBucket{{Upto: 1 << 30, Chances: 5, Answers: 1}}}
	if lift, ok := thin.FamiliarLift(0, false); ok || lift != 1 {
		t.Errorf("по тощей корзине множитель %v (ok=%v)", lift, ok)
	}
}
