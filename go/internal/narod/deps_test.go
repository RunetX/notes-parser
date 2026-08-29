package narod

import (
	"go/build"
	"strings"
	"testing"
)

// Ядро народа не импортирует ни площадку, ни архив — ни прямо, ни через
// посредника. Доводы разные, и оба существенные:
//
//   - без PLATFORM тот же оркестратор гоняется реплеем архивных тредов на машине
//     разработчика, где Postgres не поднят вовсе. Площадка приходит интерфейсом
//     Stage, и потому калибровка проверяет РОВНО ту механику, которая работает в
//     бою;
//   - без ARCHIVE карточка остаётся ДОКУМЕНТОМ-контрактом, а не проекцией
//     внутренностей майнера. Разъехавшись, они разъехались бы молча: карточку из
//     архивной карты письма собирает cmd/lovegw, который знает оба мира, а
//     полноту переноса стережёт парный тест.
//
// Тестовые импорты проверяются наравне с боевыми: тест, потянувший сюда архив,
// сделал бы невозможной сборку ядра без него.
func TestNarodImportsNeitherPlatformNorArchive(t *testing.T) {
	const (
		modulePrefix = "lovegw/"
		root         = "lovegw/internal/narod"
	)
	forbidden := map[string]string{
		"lovegw/internal/platform": "площадка приходит интерфейсом Stage — иначе реплей потребует Postgres",
		"lovegw/internal/archive":  "карточка это документ-контракт, а не проекция майнера",
	}

	seen := map[string]bool{}
	var walk func(pkg string, chain []string)
	walk = func(pkg string, chain []string) {
		if seen[pkg] {
			return
		}
		seen[pkg] = true
		for bad, why := range forbidden {
			if pkg == bad || strings.HasPrefix(pkg, bad+"/") {
				t.Errorf("%s: %s", strings.Join(append(chain, pkg), " → "), why)
				return
			}
		}
		p, err := build.Default.Import(pkg, "", 0)
		if err != nil {
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
