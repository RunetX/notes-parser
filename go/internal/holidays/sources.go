package holidays

// Сборка источников по именам из конфига.

import (
	"fmt"
	"net/http"
	"time"
)

// DefaultNames — календари, с которых начинаем. Порядок не важен: слияние
// от него не зависит, а показывает поводы Merge.
var DefaultNames = []string{SourceCalend, SourceWiki}

// Build собирает источники по именам. Незнакомое имя — ошибка конфига, а не
// молчаливый пропуск: «календарь настроен, но не опрашивается» ищут часами.
func Build(names []string, timeout time.Duration) ([]Source, error) {
	if len(names) == 0 {
		names = DefaultNames
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	out := make([]Source, 0, len(names))
	for _, n := range names {
		switch n {
		case SourceCalend:
			out = append(out, Calend{Client: client})
		case SourceWiki:
			out = append(out, Wiki{Client: client})
		default:
			return nil, fmt.Errorf("неизвестный календарь %q (известны: %v)", n, DefaultNames)
		}
	}
	return out, nil
}
