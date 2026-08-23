package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Текст объявления читается из ФАЙЛА, а не из аргумента командной строки: в нём
// переносы строк и кавычки, и оболочка их калечит. Проверяется здесь именно это
// — что путь и «-» разбираются, а пустой аргумент отвечает подсказкой, а не
// публикацией пустой заметки.
func TestReadTextArg(t *testing.T) {
	path := filepath.Join(t.TempDir(), "note.txt")
	body := "Заголовок\n\nАбзац с «кавычками» и — тире.\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readTextArg(path)
	if err != nil || got != body {
		t.Errorf("из файла пришло %q (%v)", got, err)
	}

	if _, err := readTextArg(""); err == nil {
		t.Error("пустой аргумент принят: заметка ушла бы пустой")
	}
	if _, err := readTextArg(filepath.Join(t.TempDir(), "нет-такого")); err == nil {
		t.Error("несуществующий файл принят")
	}
}

// «-» означает stdin — так текст попадает в команду из пайпа, не оседая файлом
// на боевом хосте.
func TestReadTextArgFromStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })

	go func() {
		_, _ = w.WriteString("объявление со stdin")
		_ = w.Close()
	}()
	got, err := readTextArg("-")
	if err != nil || !strings.Contains(got, "со stdin") {
		t.Errorf("со stdin пришло %q (%v)", got, err)
	}
}
