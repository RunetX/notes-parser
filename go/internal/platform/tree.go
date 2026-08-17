package platform

import (
	"fmt"
	"strconv"
	"strings"
)

// Материализованный путь комментария.
//
// Путь — это сегменты из id предков, дополненные нулями до фиксированной ширины
// и разделённые точкой: «0000063207290.0000063207431». Устройство выбрано не
// ради красоты, а ради одного свойства: id монотонно растут по времени и на НГС,
// и у нас, поэтому фиксированная ширина плюс побайтовое сравнение (в индексе —
// COLLATE "C") дают «дерево, братья в хронологии» безо всякой сортировки в
// памяти, а страница треда берётся одним range-scan по (note_id, path).
//
// Это не микрооптимизация: треды на 848 комментариев воспроизводимо роняют сам
// НГС, и повторять его ошибку незачем.

const (
	// pathWidth — ширина сегмента. 13 знаков покрывают весь план идентификаторов
	// (потолок 2e11 — 12 знаков) с запасом на порядок.
	pathWidth = 13
	// pathSep — разделитель сегментов. Точка меньше цифр в ASCII, поэтому предок
	// сортируется раньше любого потомка, а братья — по возрастанию id.
	pathSep = '.'
	// MaxDepth — потолок вложенности. На НГС наблюдалось максимум 7 уровней;
	// 12 оставляет запас и одновременно ограничивает длину пути 168 байтами,
	// чтобы индекс не распухал от вырожденных веток.
	MaxDepth = 12
)

// ErrTooDeep — ответ глубже допустимого. Не отказ в публикации: вызывающий
// подвешивает такой ответ к ближайшему разрешённому предку, как делает и сам
// сайт, схлопывая глубокие ветки.
var ErrTooDeep = fmt.Errorf("глубина ответа больше %d", MaxDepth)

// pathSegment — один сегмент пути для идентификатора.
func pathSegment(id int64) string {
	return fmt.Sprintf("%0*d", pathWidth, id)
}

// RootPath — путь комментария первого уровня.
func RootPath(id int64) string { return pathSegment(id) }

// ChildPath — путь ответа на комментарий с путём parent. Пустой parent даёт
// корневой путь: «ответ в никуда» — рабочий случай, родителя могла снести
// модерация.
func ChildPath(parent string, id int64) (string, error) {
	if parent == "" {
		return RootPath(id), nil
	}
	if PathDepth(parent) >= MaxDepth {
		return "", ErrTooDeep
	}
	var b strings.Builder
	b.Grow(len(parent) + 1 + pathWidth)
	b.WriteString(parent)
	b.WriteByte(pathSep)
	b.WriteString(pathSegment(id))
	return b.String(), nil
}

// PathDepth — глубина пути: 1 у корневого комментария, 0 у пустого.
func PathDepth(path string) int {
	if path == "" {
		return 0
	}
	return strings.Count(path, string(pathSep)) + 1
}

// ParentPath — путь родителя. Второе значение false у корневого пути.
func ParentPath(path string) (string, bool) {
	i := strings.LastIndexByte(path, pathSep)
	if i < 0 {
		return "", false
	}
	return path[:i], true
}

// PathIDs — идентификаторы предков и самого комментария, от корня ветки.
// Ошибка означает испорченный путь, а это уже повреждение данных, а не
// пользовательский ввод.
func PathIDs(path string) ([]int64, error) {
	if path == "" {
		return nil, nil
	}
	parts := strings.Split(path, string(pathSep))
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("путь %q: сегмент %q не число: %w", path, p, err)
		}
		out = append(out, id)
	}
	return out, nil
}

// BranchRootID — id корня ветки, то есть первый сегмент пути. Ноль, если путь
// пуст. Именно это значение сайт отдаёт как parent_id: его дерево двухуровневое.
func BranchRootID(path string) (int64, error) {
	ids, err := PathIDs(path)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	return ids[0], nil
}

// SubtreePrefix — префикс для выборки и правки поддерева:
// `WHERE note_id = $1 AND path LIKE SubtreePrefix(path)`. Экранирует
// подстановочные знаки LIKE, хотя в путях их быть не может, — на случай, если
// путь пришёл не оттуда, откуда мы думаем.
func SubtreePrefix(path string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(path) + "%"
}
