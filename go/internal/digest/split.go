package digest

// Разбиение выпуска на серию сообщений. Telegram ограничивает видимый текст
// 4096 символами (UTF-16, теги не считаются); лимит MAX не задокументирован —
// режем консервативно тем же бюджетом.

import (
	"fmt"
	"html"
	"strings"
	"unicode/utf8"
)

// MessageBudget — бюджет видимых рун одного сообщения: запас от 4096 на
// эмодзи (в UTF-16 считаются за два) и служебные суффиксы.
const MessageBudget = 3500

// numberingReserve — резерв под суффикс нумерации «(k/N)» и повтор заголовка.
const numberingReserve = 48

// SplitMessages жадно пакует блоки в сообщения не длиннее budget видимых рун.
// Абзац не рвётся (гигантский — обрезается по границе руны с «…» и закрытием
// тегов); рубрика, продолжившаяся в новом сообщении, получает повтор
// заголовка с пометкой «(продолжение)». При N > 1 сообщения нумеруются (k/N).
func SplitMessages(blocks []Block, budget int) []string {
	limit := budget - numberingReserve
	var msgs [][]string
	var cur []string
	curLen := 0
	header := "" // заголовок текущей рубрики для повтора при переносе
	add := func(t string) {
		if curLen > 0 {
			curLen += len("\n\n")
		}
		cur = append(cur, t)
		curLen += VisibleLen(t)
	}
	for _, b := range blocks {
		if b.NewSection {
			header = b.Text
		}
		t := b.Text
		if VisibleLen(t) > limit {
			t = truncateHTML(t, limit-1)
		}
		need := VisibleLen(t)
		if curLen > 0 && curLen+len("\n\n")+need > limit {
			msgs = append(msgs, cur)
			cur, curLen = nil, 0
			if !b.NewSection && header != "" && header != b.Text {
				add(header + " <i>(продолжение)</i>")
			}
		}
		add(t)
	}
	if len(cur) > 0 {
		msgs = append(msgs, cur)
	}

	out := make([]string, len(msgs))
	for i, parts := range msgs {
		text := strings.Join(parts, "\n\n")
		if len(msgs) > 1 {
			text += fmt.Sprintf("\n\n(%d/%d)", i+1, len(msgs))
		}
		out[i] = text
	}
	return out
}

// VisibleLen — видимая длина HTML в рунах: теги отбрасываются, сущности
// (&lt; и т.п.) считаются одним символом. Методика visibleNoteLen из tgx.
func VisibleLen(s string) int {
	return len([]rune(html.UnescapeString(htmlTagRe.ReplaceAllString(s, ""))))
}

// truncateHTML обрезает HTML до limit видимых рун, не ломая разметку:
// теги копируются целиком, сущность считается одной руной, в конце — «…» и
// закрытие незакрытых тегов. Работает по валидированному черновику
// (только парные <b>, <i>, <a>).
func truncateHTML(s string, limit int) string {
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
			if m := htmlTagRe.FindStringSubmatch(tag); m != nil {
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
