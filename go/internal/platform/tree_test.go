package platform

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
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

// Обращение образца 2013 года («Для [b][i]Ник[/i][/b] текст») рисовал сам
// сайт, поэтому ник в проверке не участвует: форма однозначна, а в теле стоит
// ник НА ТУ ДАТУ — сверять его с нынешним значило бы промахиваться на каждом
// переименовании. Кончилась эта форма 02.06.2014, см. web/bbcode.go.
func TestTrimLegacyAddress(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"Для [b][i]Сибирский кот[/i][/b] а вот и нет", "а вот и нет", true},
		{"Для [b][i]ХНа[/i][/b]   с отступом", "с отступом", true},
		// Обращение и больше ничего: пустая реплика хуже реплики с обращением.
		{"Для [b][i]ХНа[/i][/b]", "Для [b][i]ХНа[/i][/b]", false},
		// Не начало строки — не обращение, а разметка внутри фразы.
		{"как писал Для [b][i]ХНа[/i][/b] вчера", "как писал Для [b][i]ХНа[/i][/b] вчера", false},
		{"[b]просто жирный[/b] текст", "[b]просто жирный[/b] текст", false},
		{"Ник, обычное обращение", "Ник, обычное обращение", false},
	}
	for _, c := range cases {
		got, ok := TrimLegacyAddress(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("%q → %q (%v), ожидалось %q (%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// ---------------------------------------------------------------- порядок сестёр

// atMin — время в минутах от условного полудня: тесты порядка читаются по этим
// минутам, а не по номерам.
func atMin(m int) time.Time {
	return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC).Add(time.Duration(m) * time.Minute)
}

func row(id int64, path string, m int) CommentView {
	return CommentView{ID: id, Path: path, PublishedAt: atMin(m)}
}

func orderOf(rows []CommentView) []int64 {
	out := make([]int64, len(rows))
	for i, c := range rows {
		out[i] = c.ID
	}
	return out
}

func sameOrder(t *testing.T, got []CommentView, want []int64) {
	t.Helper()
	ids := orderOf(got)
	if len(ids) != len(want) {
		t.Fatalf("строк %d, ожидалось %d: %v", len(ids), len(want), ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("порядок разошёлся на позиции %d:\nполучено %v\nожидалось %v", i, ids, want)
		}
	}
}

// Тот самый случай, ради которого всё и заведено: своя реплика в 12:00,
// зеркальный ответ соседа в 12:05. По номерам зеркальный меньше нативного в
// шесть раз и встаёт ВЫШЕ; по времени — там, где сказан.
func TestSiblingsMixBandsByTime(t *testing.T) {
	native := row(100000000001, RootPath(100000000001), 0)
	ngs := row(63238900, RootPath(63238900), 5)

	got := OrderSiblingsByTime([]CommentView{ngs, native}) // порядок, каким его даёт путь
	sameOrder(t, got, []int64{100000000001, 63238900})
}

// Переставляются СЁСТРЫ, а не строки: ветка обязана остаться веткой, иначе
// «упорядочили по времени» превратится в линейный вид с отступами.
func TestSiblingOrderKeepsTheTree(t *testing.T) {
	g1 := RootPath(63238900)
	nc, _ := ChildPath(g1, 100000000002)
	gc, _ := ChildPath(g1, 63238910)

	got := OrderSiblingsByTime([]CommentView{
		row(63238900, g1, 5),
		row(63238910, gc, 7),
		row(100000000002, nc, 6),
		row(100000000001, RootPath(100000000001), 0),
	})
	// Своя корневая (12:00) встаёт первой, дети зеркальной — по своим минутам.
	sameOrder(t, got, []int64{100000000001, 63238900, 100000000002, 63238910})
}

// Чисто зеркальный тред — а таких в базе 117 тысяч — не должен шелохнуться:
// внутри одной полосы номер и время идут заодно. Отдельно проверяется НИЧЬЯ:
// у реплик сайта время с точностью до секунды, и две реплики одной секунды
// обязаны остаться в прежнем порядке, иначе тред перетасовывался бы просто так.
func TestMirrorOnlyThreadKeepsItsOrder(t *testing.T) {
	same := []CommentView{
		row(63238900, RootPath(63238900), 1),
		row(63238901, RootPath(63238901), 1), // та же минута
		row(63238902, RootPath(63238902), 2),
	}
	sameOrder(t, OrderSiblingsByTime(same), []int64{63238900, 63238901, 63238902})
}

// Родителя снесла модерация, и его дети остались в выдаче сиротами — строка
// стоит внутри ветки деда. Ключа у пропавшего нет, поэтому ветка ставится по
// самой ранней реплике, которая в ней уцелела.
func TestHiddenParentIsPlacedByItsEarliestVisibleChild(t *testing.T) {
	root := RootPath(63238900)
	hidden, _ := ChildPath(root, 63238950) // строки с этим id в выдаче нет
	orphan, _ := ChildPath(hidden, 63238999)
	native, _ := ChildPath(root, 100000000001)

	got := OrderSiblingsByTime([]CommentView{
		row(63238900, root, 0),
		row(63238999, orphan, 9), // уцелевшая внучка скрытой ветки
		row(100000000001, native, 1),
	})
	sameOrder(t, got, []int64{63238900, 100000000001, 63238999})
}
