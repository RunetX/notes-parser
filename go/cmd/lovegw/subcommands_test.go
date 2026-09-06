package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// Белый список подкоманд и switch, который их исполняет, обязаны совпадать.
//
// Оплачено выкаткой 06.09.2026: `platform ages` был дописан в switch и забыт в
// platformSubcommands — команда собралась, прошла vet, прошла все тесты и
// ответила на бою «укажите подкоманду». Разъезжаются эти двое молча: switch
// уводит незнакомое в default, а список — в разбор аргументов, и ни один из них
// не знает о существовании другого.
//
// Проверяется ИСХОДНИКОМ, а не вызовом: запуск подкоманды требует конфига, базы
// и сети, а дефект живёт ровно в том, доедет ли имя от списка до switch. Тот же
// приём, что у personas_voice_test с импортами.
func TestPlatformSubcommandsMatchTheSwitch(t *testing.T) {
	inList := map[string]bool{}
	for name := range platformSubcommands {
		inList[name] = true
	}
	inSwitch := switchCases(t, "platform.go", "sub")

	for name := range inList {
		if !inSwitch[name] {
			t.Errorf("%q числится подкомандой, но switch её не разбирает — ответит «укажите подкоманду»", name)
		}
	}
	for name := range inSwitch {
		if !inList[name] {
			t.Errorf("switch разбирает %q, но в platformSubcommands её нет — до switch имя не доедет", name)
		}
	}
}

// switchCases собирает строковые метки case у switch по переменной varName.
// Пустой результат — сам по себе провал: значит разбор не нашёл того switch, о
// котором тест, и молча «проходить» ему нельзя.
func switchCases(t *testing.T, file, varName string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Clean(file), nil, 0)
	if err != nil {
		t.Fatalf("разбор %s: %v", file, err)
	}
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		id, ok := sw.Tag.(*ast.Ident)
		if !ok || id.Name != varName {
			return true
		}
		for _, stmt := range sw.Body.List {
			cl, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, expr := range cl.List {
				if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					out[lit.Value[1:len(lit.Value)-1]] = true
				}
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatalf("в %s не найден switch по %s — тест проверяет не то, что думает", file, varName)
	}
	return out
}
