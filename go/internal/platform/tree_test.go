package platform

import (
	"errors"
	"sort"
	"strings"
	"testing"
)

func TestRootAndChildPath(t *testing.T) {
	root := RootPath(63207290)
	if root != "0000063207290" {
		t.Fatalf("RootPath = %q, ожидалось 0000063207290", root)
	}
	child, err := ChildPath(root, 63207431)
	if err != nil {
		t.Fatalf("ChildPath: %v", err)
	}
	if child != "0000063207290.0000063207431" {
		t.Fatalf("ChildPath = %q", child)
	}
	if got := PathDepth(child); got != 2 {
		t.Fatalf("PathDepth = %d, ожидалось 2", got)
	}
}

// Ответ без родителя — рабочий случай, а не ошибка: родителя могла снести
// модерация, и комментарий просто становится корневым.
func TestChildPathOrphanBecomesRoot(t *testing.T) {
	got, err := ChildPath("", 42)
	if err != nil {
		t.Fatalf("ChildPath: %v", err)
	}
	if got != RootPath(42) {
		t.Fatalf("сирота получила путь %q, ожидался корневой", got)
	}
}

func TestChildPathTooDeep(t *testing.T) {
	path := RootPath(1)
	for i := int64(2); PathDepth(path) < MaxDepth; i++ {
		var err error
		if path, err = ChildPath(path, i); err != nil {
			t.Fatalf("построение глубины: %v", err)
		}
	}
	if _, err := ChildPath(path, 999); !errors.Is(err, ErrTooDeep) {
		t.Fatalf("на глубине %d ожидалась ErrTooDeep, получено %v", MaxDepth, err)
	}
}

// Главное свойство пути, ради которого он вообще заведён: побайтовая сортировка
// даёт «дерево, братья в хронологии». Если это сломается, страница треда
// перестанет быть одним range-scan — а треды на 848 комментариев роняют сам НГС.
func TestPathSortsAsTree(t *testing.T) {
	r1 := RootPath(100)
	r2 := RootPath(101)
	c1, _ := ChildPath(r1, 200)
	c2, _ := ChildPath(r1, 201)
	gc, _ := ChildPath(c1, 300)

	got := []string{r2, gc, c2, r1, c1}
	sort.Strings(got) // strings.Compare = побайтовое сравнение, как COLLATE "C"

	want := []string{r1, c1, gc, c2, r2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("порядок разошёлся на позиции %d:\nполучено %v\nожидалось %v", i, got, want)
		}
	}
}

// Разная разрядность id не должна путать порядок — ради этого сегмент и
// дополняется нулями до фиксированной ширины.
func TestPathWidthKeepsNumericOrder(t *testing.T) {
	small := RootPath(9)
	big := RootPath(100000000000)
	if !(small < big) {
		t.Fatalf("порядок нарушен: %q не меньше %q", small, big)
	}
	if len(small) != len(big) {
		t.Fatalf("сегменты разной длины: %d и %d", len(small), len(big))
	}
}

func TestParentPathAndIDs(t *testing.T) {
	root := RootPath(100)
	child, _ := ChildPath(root, 200)

	if _, ok := ParentPath(root); ok {
		t.Fatal("у корневого пути не должно быть родителя")
	}
	parent, ok := ParentPath(child)
	if !ok || parent != root {
		t.Fatalf("ParentPath = %q, %v; ожидалось %q, true", parent, ok, root)
	}

	ids, err := PathIDs(child)
	if err != nil {
		t.Fatalf("PathIDs: %v", err)
	}
	if len(ids) != 2 || ids[0] != 100 || ids[1] != 200 {
		t.Fatalf("PathIDs = %v", ids)
	}
	if got, err := BranchRootID(child); err != nil || got != 100 {
		t.Fatalf("BranchRootID = %d, %v; ожидалось 100", got, err)
	}
}

func TestPathIDsRejectsGarbage(t *testing.T) {
	if _, err := PathIDs("0000000000100.мусор"); err == nil {
		t.Fatal("испорченный путь принят молча — это повреждение данных, а не ввод")
	}
}

func TestSubtreePrefixEscapesLike(t *testing.T) {
	got := SubtreePrefix("0000000000100")
	if !strings.HasSuffix(got, "%") {
		t.Fatalf("префикс без подстановки: %q", got)
	}
	if esc := SubtreePrefix(`a_b%c`); esc != `a\_b\%c%` {
		t.Fatalf("подстановочные знаки не экранированы: %q", esc)
	}
}
