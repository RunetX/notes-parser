package platform

import (
	"fmt"
	"sort"
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

// ClampParent укорачивает путь родителя так, чтобы ответ на него поместился в
// MaxDepth: лишние сегменты с конца отбрасываются. Ветка визуально схлопывается,
// как это делает и сам сайт, — но ребро reply_to_id при этом остаётся настоящим.
// Разделение важное: путь это раскладка, а адресат это факт, и терять факт из-за
// раскладки нельзя (иначе «кому ответили» переписывается вёрсткой).
func ClampParent(parent string) string {
	for PathDepth(parent) >= MaxDepth {
		p, ok := ParentPath(parent)
		if !ok {
			return ""
		}
		parent = p
	}
	return parent
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

// ---------------------------------------------------------------- порядок сестёр

// Путь упорядочивает дерево по НОМЕРАМ, и расчёт был на то, что номера растут по
// времени. Внутри одной полосы идентификаторов это правда, между полосами — нет:
// любой нативный номер больше любого ngs'ного. Пока НГС не принимал комментариев,
// разницы было не видно (зеркальное старое, своё новое), а с выносом на сайт
// (platngs) тред стал смешанным в обе стороны: я пишу здесь в 12:00, мне отвечают
// на НГС в 12:05, ответ приезжает зеркалом с номером в шесть раз меньше — и
// встаёт ВЫШЕ реплики, на которую отвечает сосед.
//
// Лечится это ПОКАЗОМ, а не переразметкой путей. Путь — идентичность строки:
// на нём держатся выборка поддерева, ClampParent и перекладка ApplyReplyTree, и
// переписывать его у 10,7 млн строк ради вида нечестно (тем же доводом обращение
// «Ник, » снимается показом, а не из тел). Сортировка же стоит микросекунды:
// «без сортировки в памяти» защищало БАЗУ от прохода по всей таблице, а здесь
// строки уже прочитаны и их не больше MaxThreadRows.

const (
	// timeKeyWidth — ширина одной доли ключа. 13 знаков покрывают и миллисекунды
	// эпохи (столько их будет до 2286 года), и весь план идентификаторов.
	timeKeyWidth = 13
	// timeKeySep — разделитель долей ключа. Тот же довод, что у pathSep: точка
	// меньше цифр в ASCII, поэтому предок сортируется раньше любого потомка.
	timeKeySep = '.'
)

// siblingKey — ключ одной реплики: время, затем номер. Номер здесь не украшение,
// а разрыв ничьей: у зеркальных реплик время с точностью до секунды (сайт больше
// и не отдаёт), и две реплики одной секунды обязаны встать в том же порядке, в
// каком стояли раньше, — иначе тред перетасовывался бы на каждой выкатке.
func siblingKey(c CommentView) string {
	// Ширина обязана быть постоянной, иначе сравнение строк перестаёт быть
	// сравнением чисел. Из базы время приходит NOT NULL и всегда после 2009 года,
	// но нулевое время дало бы минус и лишние два знака, — а порядок, поехавший
	// от одной кривой строки, потом не объяснить.
	ms := c.PublishedAt.UnixMilli()
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%0*d%0*d", timeKeyWidth, ms, timeKeyWidth, c.ID)
}

// OrderSiblingsByTime переставляет СЁСТЕР треда по времени публикации, оставляя
// само дерево нетронутым: ветка остаётся веткой, глубина прежняя, меняется лишь
// порядок ответов на одного родителя.
//
// Делается это не перестройкой дерева в памяти, а подменой ключа: у каждой
// строки путь из номеров переводится в путь из ключей «время+номер», и строки
// сортируются по нему. Тем самым относительный порядок двух реплик решает, как и
// раньше, ПЕРВЫЙ различающийся предок — то есть геометрия дерева воспроизводится
// точно, включая осиротевших (родителя снесла модерация, и строка стоит внутри
// ветки деда).
//
// Ключ несуществующей строки — ключ самой ранней ВИДИМОЙ реплики её поддерева:
// скрытую ветку ставим по тому, что в ней осталось. Иначе её пришлось бы
// сравнивать ключом другого устройства, а это уже не порядок, а лотерея.
func OrderSiblingsByTime(rows []CommentView) []CommentView {
	if len(rows) < 2 {
		return rows
	}
	own := make(map[string]string, len(rows))    // сегмент → ключ своей строки
	heir := make(map[string]string, len(rows)/8) // сегмент → ключ самой ранней в поддереве
	for _, c := range rows {
		own[pathSegment(c.ID)] = siblingKey(c)
	}
	for _, c := range rows {
		k := siblingKey(c)
		for _, seg := range ancestorSegments(c.Path) {
			if _, ok := own[seg]; ok {
				continue
			}
			if prev, ok := heir[seg]; !ok || k < prev {
				heir[seg] = k
			}
		}
	}
	keys := make(map[int64]string, len(rows))
	var b strings.Builder
	for _, c := range rows {
		b.Reset()
		for i, seg := range strings.Split(c.Path, string(pathSep)) {
			if i > 0 {
				b.WriteByte(timeKeySep)
			}
			k, ok := own[seg]
			if !ok {
				k = heir[seg]
			}
			b.WriteString(k)
		}
		keys[c.ID] = b.String()
	}
	out := make([]CommentView, len(rows))
	copy(out, rows)
	sort.Slice(out, func(i, j int) bool { return keys[out[i].ID] < keys[out[j].ID] })
	return out
}

// ancestorSegments — сегменты пути без последнего, то есть предки строки. Свой
// сегмент отбрасывается потому, что он всегда лежит в own: ключ строки — она
// сама, а не самый ранний её потомок.
func ancestorSegments(path string) []string {
	segs := strings.Split(path, string(pathSep))
	if len(segs) == 0 {
		return nil
	}
	return segs[:len(segs)-1]
}
