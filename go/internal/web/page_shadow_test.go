package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestСтраницыНеПерекрываютПолейКаркаса — поле страницы с тем же именем, что у
// встроенного page, ЗАТЕНЯЕТ его в шаблоне: {{.Letters}} в base.gohtml берёт
// поле ВНЕШНЕЙ структуры, и каркас начинает печатать чужое.
//
// Оплачено боем 12.09.2026: dialogPage.Letters ([]platform.MessageView) перекрыл
// page.Letters (счётчик непрочитанных писем), и в меню участника вместо числа
// встал дамп среза — вместе с текстами писем, то есть ровно то, чего на этой
// странице показывать второй раз нельзя. Компилятор здесь молчит по устройству
// (оба поля законны), а тест на поведение ловил бы одну страницу из двадцати.
//
// Поэтому проверка читает ИСХОДНИК: всякая структура, встроившая page, обязана
// не повторять его имён, и новая страница попадает под правило сама, ничего не
// дописывая к этому тесту.
func TestСтраницыНеПерекрываютПолейКаркаса(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("разбор пакета: %v", err)
	}
	structs := map[string]*ast.StructType{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}
				if st, ok := ts.Type.(*ast.StructType); ok {
					structs[ts.Name.Name] = st
				}
				return true
			})
		}
	}
	frame := structs["page"]
	if frame == nil {
		t.Fatal("в пакете не нашлось структуры page — тест устарел вместе с каркасом")
	}
	own := map[string]bool{}
	for _, f := range frame.Fields.List {
		for _, name := range f.Names {
			own[name.Name] = true
		}
	}
	if len(own) == 0 {
		t.Fatal("у page не нашлось полей — разбор пошёл не туда")
	}
	// Встроившие page — и те, кто встроил встроившего: страница бывает
	// надстройкой над страницей.
	carries := map[string]bool{"page": true}
	for again := true; again; {
		again = false
		for name, st := range structs {
			if carries[name] {
				continue
			}
			for _, f := range st.Fields.List {
				id, ok := f.Type.(*ast.Ident)
				if ok && len(f.Names) == 0 && carries[id.Name] {
					carries[name] = true
					again = true
				}
			}
		}
	}
	var bad []string
	for name, st := range structs {
		if !carries[name] || name == "page" {
			continue
		}
		for _, f := range st.Fields.List {
			for _, fn := range f.Names {
				if own[fn.Name] {
					bad = append(bad, name+"."+fn.Name)
				}
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("поля страниц перекрывают поля каркаса и будут напечатаны вместо них: %s",
			strings.Join(bad, ", "))
	}
}
