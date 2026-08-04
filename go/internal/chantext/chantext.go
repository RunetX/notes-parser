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
	"regexp"
	"strings"
	"unicode/utf8"
)

// MessageBudget — бюджет видимых рун одного сообщения: запас от 4096 на
// эмодзи (в UTF-16 считаются за два) и служебные суффиксы. Лимит MAX не
// задокументирован — режем консервативно тем же бюджетом.
const MessageBudget = 3500

var tagRe = regexp.MustCompile(`</?([a-zA-Z0-9]+)[^<>]*>`)

// VisibleLen — видимая длина HTML в рунах: теги отбрасываются, сущности
// (&lt; и т.п.) считаются одним символом. Методика visibleNoteLen из tgx.
func VisibleLen(s string) int {
	return len([]rune(html.UnescapeString(tagRe.ReplaceAllString(s, ""))))
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
