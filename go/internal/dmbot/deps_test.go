package dmbot

import (
	"go/build"
	"strings"
	"testing"
)

// TestDMBotNeverImportsPlatform стережёт границу двух миров.
//
// Диалоговое ядро знает про площадку ровно столько, сколько назвали его узкие
// интерфейсы (SiteLogin, SiteBinding), а отказы ядра переводит адаптер в
// cmd/lovegw — единственном месте, которое и так видит оба мира. Тем же приёмом
// живёт web.ErrNoProfile.
//
// Держалось это до сих пор ОДНОЙ ДИСЦИПЛИНОЙ: правило стояло комментарием в
// bind.go, а первая редакция привязки его и нарушила — dmbot импортировал
// platform, и поймал это не компилятор, а ревью. Комментарий не уберёг, значит
// нужен тест: у соседей (platform, narod) он ровно такой и по тому же доводу.
//
// Цена нарушения не в опрятности слоёв. Демон собирается и без площадки (у неё
// свой конфиг, свой Postgres и свой хост), а dmbot вдобавок гоняется в тестах
// без единой строки SQL; потянув за собой pgx и всё ядро, он превратил бы
// быстрый набор тестов диалогов в интеграционный.
//
// TestImports тоже обходятся: подделка в тесте, потянувшая ядро, заводит ту же
// зависимость через заднюю дверь.
func TestDMBotNeverImportsPlatform(t *testing.T) {
	const (
		modulePrefix = "lovegw/"
		root         = "lovegw/internal/dmbot"
		forbidden    = "lovegw/internal/platform"
	)

	seen := map[string]bool{}
	var walk func(pkg string, chain []string)
	walk = func(pkg string, chain []string) {
		if seen[pkg] {
			return
		}
		seen[pkg] = true

		if pkg == forbidden || strings.HasPrefix(pkg, forbidden+"/") {
			t.Errorf("диалоговое ядро видит площадку: %s", strings.Join(append(chain, pkg), " → "))
			return
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
