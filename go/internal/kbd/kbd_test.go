package kbd

import "testing"

func TestPackParse(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		verb    string
		arg     string
		ok      bool
	}{
		{"глагол без аргумента", Pack("menu", ""), "menu", "", true},
		{"глагол с аргументом", Pack("note", "anon"), "note", "anon", true},
		{"аргумент с двоеточием", "1:news:2026-08-04:12", "news", "2026-08-04:12", true},
		{"пусто", "", "", "", false},
		{"без версии", "menu", "", "", false},
		{"чужая версия", "2:menu", "", "", false},
		{"пустой глагол", "1:", "", "", false},
		{"глагол потерян", "1::anon", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			verb, arg, ok := Parse(c.payload)
			if ok != c.ok || verb != c.verb || arg != c.arg {
				t.Errorf("Parse(%q) = %q, %q, %v; ждали %q, %q, %v",
					c.payload, verb, arg, ok, c.verb, c.arg, c.ok)
			}
		})
	}
}

func TestPackFits(t *testing.T) {
	// Самый длинный payload эпика: подтверждение новости с её id (15 знаков).
	long := Pack("news", "20260804-193012")
	if !Fits(long) {
		t.Errorf("payload новости не влез: %q (%d байт)", long, len(long))
	}
	for _, r := range long {
		if r > 127 {
			t.Fatalf("payload должен быть ASCII: %q", long)
		}
	}
	// Свободный текст в payload не кладут: вот почему.
	if Fits(Pack("subs", "очень длинное ключевое слово подписки на весь твит")) {
		t.Error("кириллический аргумент не должен влезать в предел")
	}
}

func TestKeyboardBuild(t *testing.T) {
	kb := New().Row(Button{Text: "Один", Payload: Pack("one", "")}).Row()
	if len(kb.Rows) != 1 {
		t.Fatalf("пустая строка не должна попадать в клавиатуру: %+v", kb.Rows)
	}
	if kb.Empty() {
		t.Error("клавиатура с кнопкой не пуста")
	}
	var nilKB *Keyboard
	if !nilKB.Empty() {
		t.Error("nil-клавиатура пуста")
	}
}
