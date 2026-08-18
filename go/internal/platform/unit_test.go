package platform

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func TestClampParentKeepsRoomForChild(t *testing.T) {
	path := RootPath(1)
	for i := int64(2); PathDepth(path) < MaxDepth; i++ {
		var err error
		if path, err = ChildPath(path, i); err != nil {
			t.Fatalf("построение глубины: %v", err)
		}
	}
	clamped := ClampParent(path)
	if PathDepth(clamped) != MaxDepth-1 {
		t.Fatalf("укороченный путь глубины %d, ожидалась %d", PathDepth(clamped), MaxDepth-1)
	}
	if _, err := ChildPath(clamped, 999); err != nil {
		t.Fatalf("в укороченный путь ответ не влез: %v", err)
	}
	if !strings.HasPrefix(path, clamped) {
		t.Fatal("укорачивание увело ветку в сторону — потомок обязан остаться в своей ветке")
	}
	// Неглубокий путь трогать не за что.
	if got := ClampParent(RootPath(7)); got != RootPath(7) {
		t.Fatalf("ClampParent испортил короткий путь: %q", got)
	}
}

func TestCleanBody(t *testing.T) {
	if _, err := cleanBody("   \n\t "); !errors.Is(err, ErrEmptyBody) {
		t.Fatalf("пустой текст принят: %v", err)
	}
	if _, err := cleanBody(strings.Repeat("я", MaxBodyRunes+1)); !errors.Is(err, ErrTooLong) {
		t.Fatalf("текст сверх потолка принят: %v", err)
	}
	// Потолок считается в рунах, а не в байтах: кириллица иначе обрезалась бы
	// вдвое раньше латиницы.
	if _, err := cleanBody(strings.Repeat("я", MaxBodyRunes)); err != nil {
		t.Fatalf("текст ровно по потолку отвергнут: %v", err)
	}
	// Нулевой байт Postgres в text не хранит вовсе — вырезаем его сами, иначе
	// ошибка драйвера прилетит из середины транзакции приёма.
	got, err := cleanBody("  а\x00б  ")
	if err != nil || got != "аб" {
		t.Fatalf("cleanBody = %q, %v", got, err)
	}
}

func TestMediaURLAndExt(t *testing.T) {
	sha := bytes.Repeat([]byte{0xab}, 32)
	got := MediaURL(sha, "image/jpeg")
	want := MediaURLPrefix + "ab/" + strings.Repeat("ab", 32) + ".jpg"
	if got != want {
		t.Fatalf("MediaURL = %q, ожидалось %q", got, want)
	}
	// Байтов нет — ссылки нет. Подставлять вместо неё чужой адрес нельзя:
	// правило «ни одна страница не ходит на hsmedia.ru» держится этим.
	if got := MediaURL(nil, "image/jpeg"); got != "" {
		t.Fatalf("ссылка без байтов: %q", got)
	}
	if got := MediaURL(sha, ""); got != "" {
		t.Fatalf("ссылка без типа: %q", got)
	}
}

func TestDetectMIMERejectsNonImage(t *testing.T) {
	// Геоблок DDoS-Guard отдаёт на запрос картинки HTML с кодом 200 — такой
	// «аватар» обязан отлететь на приёме, а не осесть в хранилище битым.
	if mime := detectMIME([]byte("<!DOCTYPE html><html><body>403</body></html>")); strings.HasPrefix(mime, "image/") {
		t.Fatalf("HTML опознан как картинка: %s", mime)
	}
	if mime := detectMIME(testPNG(t, 4, 3)); mime != "image/png" {
		t.Fatalf("PNG опознан как %s", mime)
	}
}

func TestCommentViewName(t *testing.T) {
	cases := []struct {
		name string
		view CommentView
		want string
	}{
		{"аноним важнее всего", CommentView{Anonymous: true, Author: Author{ID: 5, Nick: "Кай"}}, "Аноним"},
		{"ник анкеты", CommentView{Author: Author{ID: 5, Nick: "Кай"}, Display: "старый"}, "Кай"},
		{"снимок безанкетного", CommentView{Display: "Гость"}, "Гость"},
		{"нет ничего", CommentView{}, "Без имени"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.view.Name(); got != c.want {
				t.Fatalf("Name = %q, ожидалось %q", got, c.want)
			}
		})
	}
}

func TestIDBands(t *testing.T) {
	if !IsNGS(312811) || IsNative(312811) {
		t.Fatal("id заметки НГС не попал в свою полосу")
	}
	if !IsNative(NativeIDBase) || IsNGS(NativeIDBase) {
		t.Fatal("начало нативной полосы опознано неверно")
	}
	if IsNGS(0) || IsNative(IDBandLimit) {
		t.Fatal("границы полос протекают")
	}
}

// testPNG — настоящая картинка: детектор типа смотрит на содержимое, поэтому
// подделка из строки тут не годится.
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 200, G: 30, B: 30, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("кодирование PNG: %v", err)
	}
	return buf.Bytes()
}

// Полосы идентификаторов не должны пересекаться, и вопрос «чьё это» обязан
// иметь ровно один ответ на каждый ключ. Проверка дешёвая, а цена ошибки —
// одиннадцать тысяч реплик 2010 года, ушедших в каналы как свежие.
func TestIDBandsDoNotOverlap(t *testing.T) {
	cases := []struct {
		id                          int64
		ngs, native, restored, band bool
	}{
		{312811, true, false, false, true},
		{NativeIDBase - 1, true, false, false, true},
		{NativeIDBase, false, true, false, true},
		{RestoredIDBase - 1, false, true, false, true},
		{RestoredIDBase, false, false, true, true},
		{IDBandLimit - 1, false, false, true, true},
		{IDBandLimit, false, false, false, false},
		{0, false, false, false, false},
		{-1, false, false, false, false},
	}
	for _, c := range cases {
		if got := IsNGS(c.id); got != c.ngs {
			t.Errorf("IsNGS(%d) = %v", c.id, got)
		}
		if got := IsNative(c.id); got != c.native {
			t.Errorf("IsNative(%d) = %v", c.id, got)
		}
		if got := IsRestored(c.id); got != c.restored {
			t.Errorf("IsRestored(%d) = %v", c.id, got)
		}
		if n := btoi(IsNGS(c.id)) + btoi(IsNative(c.id)) + btoi(IsRestored(c.id)); n > 1 {
			t.Errorf("ключ %d попал в %d полос сразу", c.id, n)
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestКолонкиПользователяСходятсяСоСканом — про случай, который стоил площадке
// вечера 18.08.2026. Ш7 добавил в users колонку ban_reason и вписал её в
// userColumns; получателей же было два списка — общий и дописанный от руки в
// SessionUser, — и второй остался прежним. pgx отвечает на это «13 полей против
// 12», то есть отказом на КАЖДЫЙ запрос вошедшего: сайт не падает, он молча
// объявляет гостями всех, и человек ходит по кругу «код принят → снова вход».
//
// Проверяется поэтому не поведение, а согласие двух списков: тест ловит правку,
// которая тронула один и забыла другой, и делает это без Postgres.
func TestКолонкиПользователяСходятсяСоСканом(t *testing.T) {
	var u User
	want := strings.Count(userColumns, ",") + 1
	if got := len(userDest(&u)); got != want {
		t.Fatalf("колонок в userColumns %d, получателей в userDest %d", want, got)
	}
}
