// Пакет chantext — общее для всех публикаций в каналы HTML-подмножество
// мессенджеров: что разрешено в тексте, как считать его видимую длину и как
// обрезать, не ломая разметку. Им пользуются дайджест (digest) и новости
// проекта (news) — правила у них одни, расходятся только источники текста.
//
// Разрешены только парные <b>, <i>, <a href="…"> — пересечение возможностей
// Telegram и MAX; всё остальное должно быть экранировано (&lt;, &gt;).
package chantext

import (
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MessageBudget — бюджет ВИДИМЫХ рун одного сообщения: запас от 4096 на
// эмодзи (в UTF-16 считаются за два) и служебные суффиксы.
//
// Мера верна для Telegram, который считает текст уже после разбора разметки.
// Для MAX она НЕ подходит: там предел 4000 наложен на сырую строку вместе с
// тегами (выяснено 10.08.2026, см. `maxx/compose.go`), и текст на 3500 видимых
// рун с полусотней ссылок за него вылезает. Поэтому `maxx` меряет своей
// `apiLen` и подрезает `fitHTML` — не полагайтесь на эту константу как на
// гарантию для MAX.
const MessageBudget = 3500

var tagRe = regexp.MustCompile(`</?([a-zA-Z0-9]+)[^<>]*>`)

// Открывающие теги разбираем строго: у <b> и <i> атрибутов быть не должно, у
// <a> допустим ровно один — href в двойных кавычках. Иначе проверка тегов
// пропускала бы внутрь любой атрибут, включая обработчики событий.
var (
	plainOpenRe  = regexp.MustCompile(`^<[a-zA-Z0-9]+\s*>$`)
	anchorOpenRe = regexp.MustCompile(`^<[aA]\s+href="([^"<>]*)"\s*>$`)
)

// VisibleLen — видимая длина HTML в рунах: теги отбрасываются, сущности
// (&lt; и т.п.) считаются одним символом. Методика visibleNoteLen из tgx.
func VisibleLen(s string) int {
	return len([]rune(html.UnescapeString(tagRe.ReplaceAllString(s, ""))))
}

// VisibleUTF16Len — видимая длина в единицах UTF-16: тем и другим сразу.
// Именно так считает сообщение Telegram — он меряет текст ПОСЛЕ разбора
// разметки (теги в предел 4096 не идут), но считает его в кодовых единицах, а
// не в рунах, поэтому эмодзи стоит двух. Мера MAX другая и живёт у него
// (сырая строка вместе с тегами, `maxx.apiLen`).
func VisibleUTF16Len(s string) int {
	return UTF16Len(html.UnescapeString(tagRe.ReplaceAllString(s, "")))
}

// ValidateHTML проверяет текст: только разрешённые парные теги, никаких
// неэкранированных < и > вне них.
func ValidateHTML(s string) error {
	var stack []string
	for _, m := range tagRe.FindAllString(s, -1) {
		name := strings.ToLower(tagRe.FindStringSubmatch(m)[1])
		switch name {
		case "b", "i", "a":
		default:
			return fmt.Errorf("тег <%s> не поддерживается (можно <b>, <i>, <a>)", name)
		}
		if strings.HasPrefix(m, "</") {
			if len(stack) == 0 || stack[len(stack)-1] != name {
				return fmt.Errorf("непарный тег </%s>", name)
			}
			stack = stack[:len(stack)-1]
		} else {
			if err := validateOpenTag(name, m); err != nil {
				return err
			}
			stack = append(stack, name)
		}
	}
	if len(stack) > 0 {
		return fmt.Errorf("незакрытый тег <%s>", stack[len(stack)-1])
	}
	if rest := tagRe.ReplaceAllString(s, ""); strings.ContainsAny(rest, "<>") {
		return errors.New("символы < и > вне тегов нужно экранировать (&lt; и &gt;)")
	}
	return nil
}

// validateOpenTag проверяет атрибуты открывающего тега.
func validateOpenTag(name, tag string) error {
	if name == "a" {
		m := anchorOpenRe.FindStringSubmatch(tag)
		if m == nil {
			return errors.New(`у тега <a> допустим ровно один атрибут — href="…"`)
		}
		return validateHref(m[1])
	}
	if !plainOpenRe.MatchString(tag) {
		return fmt.Errorf("у тега <%s> не должно быть атрибутов", name)
	}
	return nil
}

// validateHref: ссылка обязана вести в веб. Мессенджер javascript: и data: не
// исполнит, но текст уходит и в другие места (черновики, HTML-отчёты), а
// проверка тут стоит дёшево.
func validateHref(href string) error {
	u, err := url.Parse(html.UnescapeString(href))
	if err != nil {
		return fmt.Errorf("ссылка %q не разбирается", href)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("ссылка %q: в href допустимы только http и https", href)
	}
	return nil
}

// FitMeasured укладывает готовый HTML в предел ЧУЖОЙ меры — той, которой
// сообщение считает сам мессенджер. MAX меряет сырую строку вместе с тегами, а
// Truncate режет по видимым рунам; прямой формулы между мерами нет, потому что
// разметка приходится на заранее неизвестные места. Поэтому нужную видимую
// длину подбираем двоичным поиском по фактической длине результата.
//
// Второе значение — признак того, что кусок текста потерян. Молчать о нём
// нельзя: у дайджеста это вырезанный по живому раздел выпуска, а у зеркала —
// непринятое сообщение, то есть вечная пробка в треде.
func FitMeasured(s string, limit int, length func(string) int) (string, bool) {
	if length(s) <= limit {
		return s, false
	}
	lo, hi, best := 0, VisibleLen(s), ""
	for lo <= hi {
		mid := (lo + hi) / 2
		if cand := Truncate(s, mid); length(cand) <= limit {
			best, lo = cand, mid+1
		} else {
			hi = mid - 1
		}
	}
	if best == "" {
		// Разметка не оставила места под текст — случай теоретический, но
		// молчать нельзя: пустой текст мессенджер не примет.
		return "…", true
	}
	return best, true
}

// Truncate обрезает HTML до limit видимых рун, не ломая разметку: теги
// копируются целиком, сущность считается одной руной, в конце — «…» и
// закрытие незакрытых тегов. Работает по валидированному тексту (только
// парные <b>, <i>, <a>).
func Truncate(s string, limit int) string {
	var b strings.Builder
	var stack []string
	visible := 0
	i := 0
	for i < len(s) && visible < limit {
		switch {
		case s[i] == '<':
			end := strings.IndexByte(s[i:], '>')
			if end < 0 { // повреждённый тег — дальше не копируем
				i = len(s)
				continue
			}
			tag := s[i : i+end+1]
			b.WriteString(tag)
			if m := tagRe.FindStringSubmatch(tag); m != nil {
				name := strings.ToLower(m[1])
				if strings.HasPrefix(tag, "</") {
					if len(stack) > 0 && stack[len(stack)-1] == name {
						stack = stack[:len(stack)-1]
					}
				} else {
					stack = append(stack, name)
				}
			}
			i += end + 1
		case s[i] == '&':
			if semi := strings.IndexByte(s[i:], ';'); semi > 0 && semi <= 8 {
				b.WriteString(s[i : i+semi+1])
				visible++
				i += semi + 1
				continue
			}
			fallthrough
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			b.WriteRune(r)
			visible++
			i += size
		}
	}
	b.WriteString("…")
	for j := len(stack) - 1; j >= 0; j-- {
		b.WriteString("</" + stack[j] + ">")
	}
	return b.String()
}
