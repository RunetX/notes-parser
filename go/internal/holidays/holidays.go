// Package holidays — поводы дня: праздники, народный календарь и события из
// истории, собранные из нескольких интернет-календарей.
//
// Зачем не спрашиваем у модели. Праздник — это ФАКТ с датой, а факт, который
// нечем проверить, рано или поздно оказывается выдуманным: заметка выйдет с
// «Международным днём» несуществующего дела или с датой, уехавшей на неделю, и
// исправлять её будет уже поздно — она в ленте, в канале и в чужих ЛС. Поэтому
// факты приносит инструмент, а модель только пишет вокруг них текст.
//
// Почему источников НЕСКОЛЬКО, и это не подстраховка, а мера достоверности:
// повод, названный двумя календарями независимо, вернее одиночного, и Merge
// складывает такие поводы в один, копя список назвавших. Отказ одного источника
// день не отменяет.
//
// Берём ТОЛЬКО НАЗВАНИЯ. Факт «сегодня день вафель» никому не принадлежит, а
// абзац про историю вафель с чужого сайта — принадлежит; заодно это на порядок
// меньше мусора в промпте. Описания у источников есть, и мы их сознательно не
// читаем.
package holidays

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Kind — что это за повод. Порядок значений — порядок показа: праздник вперёд
// истории, потому что «сегодня день вафель» ближе человеку, чем 1853 год.
type Kind int

const (
	KindHoliday Kind = iota // праздник или памятный день
	KindFolk                // народный календарь
	KindName                // именины: чьё сегодня имя
	KindHistory             // событие из истории
)

func (k Kind) String() string {
	switch k {
	case KindHoliday:
		return "праздник"
	case KindFolk:
		return "народный календарь"
	case KindName:
		return "именины"
	case KindHistory:
		return "событие"
	default:
		return "?"
	}
}

// Scope — чей это повод. Нужен фильтру: чужие государственные и церковные даты
// в наше «доброе утро» не идут (см. filter.go).
type Scope int

const (
	ScopeWorld     Scope = iota // международный или без страны
	ScopeRussia                 // российский
	ScopeForeign                // государственный праздник другой страны
	ScopeReligious              // церковный
)

func (s Scope) String() string {
	switch s {
	case ScopeWorld:
		return "мир"
	case ScopeRussia:
		return "Россия"
	case ScopeForeign:
		return "другая страна"
	case ScopeReligious:
		return "церковный"
	default:
		return "?"
	}
}

// Occasion — один повод дня. Описания здесь нет намеренно (см. шапку пакета).
type Occasion struct {
	Title   string
	Kind    Kind
	Year    int      // для событий истории; 0 у праздников
	Scope   Scope    //
	Sources []string // кто назвал: чем больше, тем вернее
}

// Source — один календарь. Знает про свою разметку и ничего про заметку.
type Source interface {
	Name() string
	Fetch(ctx context.Context, day time.Time) ([]Occasion, error)
}

// MarkupError — обязательный кусок разметки не нашёлся. Отдельный тип по
// образцу love.MarkupError: дрейф чужой вёрстки надо отличать от «сегодня
// пусто», иначе календарь молча замолчит навсегда.
type MarkupError struct {
	Source   string
	Selector string
}

func (e *MarkupError) Error() string {
	return fmt.Sprintf("%s: вёрстка изменилась? не разобран %q", e.Source, e.Selector)
}

// Collect опрашивает календари и сливает их ответы. Ошибка одного источника —
// предупреждение в лог, а не конец дня: заметка выйдет по тому, что принесли
// остальные. Ошибка возвращается, только если не ответил НИКТО.
func Collect(ctx context.Context, srcs []Source, day time.Time, log *slog.Logger) ([]Occasion, error) {
	if log == nil {
		log = slog.Default()
	}
	lists := make([][]Occasion, 0, len(srcs))
	var errs []error
	for _, s := range srcs {
		got, err := s.Fetch(ctx, day)
		if err != nil {
			log.Warn("календарь не ответил", "источник", s.Name(), "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", s.Name(), err))
			continue
		}
		lists = append(lists, got)
	}
	if len(lists) == 0 {
		if len(errs) == 0 {
			return nil, errors.New("не задано ни одного календаря")
		}
		return nil, errors.Join(errs...)
	}
	return Merge(lists...), nil
}

// Merge сливает поводы разных календарей в один список. Чистая функция:
// правило склейки — единственное место, где можно ошибиться незаметно, и
// проверяется оно таблицей, а не боевым утром.
//
// Одинаковым считается повод с тем же ключом ЛИБО тот, чей ключ целиком
// содержится в чужом: «Национальный день вафель» у Википедии и «Национальный
// день вафель в США» у calend.ru — один и тот же день, а требовать дословного
// совпадения значило бы никогда не увидеть согласия источников. Порог длины
// нужен, чтобы «День семьи» не склеился с «Днём семьи, любви и верности», где
// это разные праздники: короткие ключи судятся только на точное равенство.
func Merge(lists ...[]Occasion) []Occasion {
	var out []Occasion
	for _, list := range lists {
		for _, o := range list {
			if o.Title = strings.TrimSpace(o.Title); o.Title == "" {
				continue
			}
			if i := findSame(out, o); i >= 0 {
				out[i].Sources = addSource(out[i].Sources, o.Sources...)
				// Более длинное название информативнее: «Национальный день
				// вафель в США» говорит больше, чем «Национальный день вафель».
				if len([]rune(o.Title)) > len([]rune(out[i].Title)) {
					out[i].Title = o.Title
				}
				// Церковное и чужое старше нейтрального: если хоть один
				// источник назвал повод церковным, фильтр обязан это увидеть.
				if o.Scope > out[i].Scope {
					out[i].Scope = o.Scope
				}
				continue
			}
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if len(out[i].Sources) != len(out[j].Sources) {
			return len(out[i].Sources) > len(out[j].Sources)
		}
		if out[i].Year != out[j].Year {
			// События — от НОВЫХ к старым. Порядок замечен на первом же живом
			// дне (24.08.2026): по возрастанию года в дюжину поводов попадали
			// Сардинское королевство 1720 и египетская надпись 394, а парашют
			// из стратосферы 1937 и первое употребление слова «мыльная опера»
			// 1938 оставались за краем. «В этот день» тем ближе человеку, чем
			// ближе год; античность лежит в хвосте и доезжает до промпта,
			// только если день пустой.
			return out[i].Year > out[j].Year
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// minContainKey — с какой длины ключа доверяем вхождению подстрокой. Ниже
// порога склейка идёт только по точному равенству. Длина в РУНАХ: в байтах
// порог вдвое мягче для кириллицы, и «День семьи» склеивался бы с «Днём семьи,
// любви и верности», где это разные праздники (поймано тестом).
const minContainKey = 12

func findSame(list []Occasion, o Occasion) int {
	key := mergeKey(o.Title)
	for i, x := range list {
		if x.Kind != o.Kind || x.Year != o.Year {
			continue
		}
		// У события истории ключ — сам ГОД: в один календарный день одного и
		// того же года двух разных событий не бывает, а формулировки у
		// календарей расходятся целиком («День рождения чипсов» против
		// «Американским поваром Джорджем Крамом изобретены картофельные
		// чипсы»). Без этого правила согласие источников по истории не видно
		// никогда, а модели один и тот же факт приезжает дважды.
		if o.Kind == KindHistory && o.Year > 0 {
			return i
		}
		xk := mergeKey(x.Title)
		if xk == key {
			return i
		}
		if utf8.RuneCountInString(key) >= minContainKey && strings.Contains(xk, key) {
			return i
		}
		if utf8.RuneCountInString(xk) >= minContainKey && strings.Contains(key, xk) {
			return i
		}
	}
	return -1
}

// mergeKey — название без регистра, пробелов и знаков: сравниваем содержание, а
// не пунктуацию календаря.
func mergeKey(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func addSource(list []string, add ...string) []string {
	for _, a := range add {
		found := false
		for _, s := range list {
			if s == a {
				found = true
				break
			}
		}
		if !found {
			list = append(list, a)
		}
	}
	return list
}
