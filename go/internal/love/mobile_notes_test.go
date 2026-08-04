package love

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// mobile_note_312870.html — реально записанная страница мобильной версии
// (заметка 312870, 248 комментариев). Перезаписывать при дрейфе вёрстки.
func TestParseMobileReplyTree(t *testing.T) {
	f, err := os.Open("testdata/mobile_note_312870.html")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	tree, err := ParseMobileReplyTree(f, "312870")
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 248 {
		t.Fatalf("комментариев: %d, ожидалось 248", len(tree))
	}

	roots := 0
	for _, p := range tree {
		if p == 0 {
			roots++
		}
	}
	if roots != 71 {
		t.Errorf("реплик верхнего уровня: %d, ожидался 71", roots)
	}

	// Опорные пары сняты с живой страницы. Первые три — как раз те, где
	// десктопный data-parent-comment-id указывает на другого человека.
	for _, c := range []struct{ id, parent int64 }{
		{63208848, 0},        // верхний уровень
		{63208870, 63208863}, // «Елена-Милена, …» → её реплика
		{63208903, 63208887}, // «БАПКА ПЕТРИК , …» → его реплика
		{63208941, 63208859}, // «Елена-Милена, куда он тебе написал?»
		{63208894, 63208888}, // ответ на собственную реплику
	} {
		if got := tree[c.id]; got != c.parent {
			t.Errorf("родитель %d: got %d, want %d", c.id, got, c.parent)
		}
	}

	// Дерево глубже двух уровней — ради этого всё и затевалось.
	depth := func(id int64) int {
		d := 0
		for p := tree[id]; p != 0; p = tree[p] {
			d++
		}
		return d
	}
	deepest := 0
	for id := range tree {
		if d := depth(id); d > deepest {
			deepest = d
		}
	}
	if deepest < 3 {
		t.Errorf("максимальная глубина %d — дерево должно быть глубже двухуровневого", deepest)
	}
}

// Страница без заметки (заглушка, редирект, чужая разметка) — дрейф вёрстки,
// а не пустой тред: иначе обогащение молча запишет «комментариев нет».
func TestParseMobileReplyTreeMarkupError(t *testing.T) {
	_, err := ParseMobileReplyTree(strings.NewReader("<html><body><p>ничего</p></body></html>"), "1")
	var me *MarkupError
	if !errors.As(err, &me) {
		t.Fatalf("ожидалась MarkupError, получено: %v", err)
	}
}

// Заметка без комментариев — законно пустое дерево.
func TestParseMobileReplyTreeEmpty(t *testing.T) {
	html := `<div class="lvmb-notes__note-text">текст заметки</div>
	         <ul notes="comments_list" class="lvmb-notes__comment-list"></ul>`
	tree, err := ParseMobileReplyTree(strings.NewReader(html), "1")
	if err != nil {
		t.Fatalf("пустой тред не должен быть ошибкой: %v", err)
	}
	if len(tree) != 0 {
		t.Errorf("ожидалось пустое дерево, получено %d", len(tree))
	}
}
