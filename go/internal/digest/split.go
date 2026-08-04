package digest

// Разбиение выпуска на серию сообщений. Правила HTML-подмножества, бюджет
// сообщения и обрезка — общие для публикаций в каналы, они живут в chantext.

import (
	"fmt"
	"strings"

	"lovegw/internal/chantext"
)

// MessageBudget — бюджет видимых рун одного сообщения выпуска.
const MessageBudget = chantext.MessageBudget

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
		curLen += chantext.VisibleLen(t)
	}
	for _, b := range blocks {
		if b.NewSection {
			header = b.Text
		}
		t := b.Text
		if chantext.VisibleLen(t) > limit {
			t = chantext.Truncate(t, limit-1)
		}
		need := chantext.VisibleLen(t)
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
