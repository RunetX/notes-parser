package platform

import (
	"go/build"
	"strings"
	"testing"
)

// TestPlatformNeverImportsArchive стережёт инвариант эпика E: площадка не видит
// персон-аналитику архива ни прямо, ни через посредника.
//
// Это не вкусовщина про слои. archive хранит выводы о живых людях, сделанные без
// их согласия (имена, пол, альт-анкеты, интересы, связи), и на публичной площадке
// им не место — граница юридическая. Проверка стоит здесь, а не в CI-скрипте,
// потому что CI этого проекта — это `go test ./...` на машине разработчика.
//
// Обходим только пакеты самого модуля: чужая библиотека наш archive
// импортировать не может по построению.
func TestPlatformNeverImportsArchive(t *testing.T) {
	const (
		modulePrefix = "lovegw/"
		root         = "lovegw/internal/platform"
		forbidden    = "lovegw/internal/archive"
	)

	seen := map[string]bool{}
	var walk func(pkg string, chain []string)
	walk = func(pkg string, chain []string) {
		if seen[pkg] {
			return
		}
		seen[pkg] = true

		if pkg == forbidden || strings.HasPrefix(pkg, forbidden+"/") {
			t.Errorf("площадка видит персон-аналитику архива: %s", strings.Join(append(chain, pkg), " → "))
			return
		}

		p, err := build.Default.Import(pkg, "", 0)
		if err != nil {
			// Пакет, которого ещё нет (или собран под другую платформу), — не
			// повод валить инвариант: проверяем то, что есть.
			t.Logf("пропущен %s: %v", pkg, err)
			return
		}
		next := append(chain, pkg)
		for _, imp := range append(append([]string{}, p.Imports...), p.TestImports...) {
			if strings.HasPrefix(imp, modulePrefix) {
				walk(imp, next)
			}
		}
	}
	walk(root, nil)
}
