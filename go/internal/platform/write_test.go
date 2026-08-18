package platform

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Окно правки закрывают три вещи, и любая из них — насовсем. Правило проверяется
// здесь, а не только в интеграционных тестах, потому что по нему живут ДВОЕ:
// ядро (пропустить правку) и страница (показать ссылку). Разъехавшись, они дали
// бы кнопку, которая отвечает отказом.
func TestEditWindowClosers(t *testing.T) {
	fresh := time.Now().Add(-time.Minute)
	edited := fresh.Add(30 * time.Second)
	base := NoteView{
		ID:          NativeIDBase + 7,
		Own:         true,
		Status:      StatusVisible,
		PublishedAt: fresh,
	}
	cases := []struct {
		name string
		fix  func(*NoteView)
		want bool
	}{
		{"своя, свежая, без ответов", func(*NoteView) {}, true},
		{"чужая", func(n *NoteView) { n.Own = false }, false},
		{"первый комментарий", func(n *NoteView) { n.CommentCount = 1 }, false},
		{"уже правили", func(n *NoteView) { n.EditedAt = &edited }, false},
		{"десять минут вышли", func(n *NoteView) { n.PublishedAt = time.Now().Add(-EditWindow) }, false},
		{"скрыта", func(n *NoteView) { n.Status = StatusHiddenMod }, false},
		// Зеркальную заметку писали не у нас, и править её тут значило бы
		// расходиться с оригиналом молча.
		{"зеркальная", func(n *NoteView) { n.ID = 312811 }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := base
			c.fix(&n)
			if got := n.Editable(time.Now()); got != c.want {
				t.Errorf("Editable = %v, ожидалось %v", got, c.want)
			}
		})
	}
}

func TestCleanNick(t *testing.T) {
	cases := []struct {
		in   string
		want string
		bad  bool
	}{
		{"  Рио  ", "Рио", false},
		// Пробелы внутри разрешены: на НГС такие ники обычны.
		{"Мадам   Рыжинская", "Мадам Рыжинская", false},
		{"", "", true},
		{"   ", "", true},
		// Невидимый разделитель (U+200B) — им подпись подделывается под чужую:
		// два ника на экране неотличимы.
		{"Ри​о", "", true},
		{strings.Repeat("я", MaxNickRunes+1), "", true},
	}
	for _, c := range cases {
		got, err := cleanNick(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("cleanNick(%q) = %q, ожидался отказ", c.in, got)
			} else if !errors.Is(err, ErrBadNick) {
				t.Errorf("cleanNick(%q): ошибка %v, ожидалась ErrBadNick", c.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("cleanNick(%q): %v", c.in, err)
		} else if got != c.want {
			t.Errorf("cleanNick(%q) = %q, ожидалось %q", c.in, got, c.want)
		}
	}
}
