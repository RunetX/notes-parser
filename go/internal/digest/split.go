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

// Budget — чем и до какого предела приёмник меряет одно сообщение. Мессенджеры
// считают по-разному: Telegram — видимый текст после разбора разметки, MAX —
// сырую строку вместе с тегами. Выпуск с полусотней ссылок расходится в этих
// мерах на сотни знаков, и общий бюджет видимых рун проезжает мимо предела MAX
// — там сообщение дорезалось бы уже в транспорте, посреди фразы.
type Budget struct {
	Limit  int
	Length func(string) int
}

// RuneBudget — бюджет в видимых рунах (мера Telegram и дефолт для всех, кто не
// объявил свою).
func RuneBudget(limit int) Budget {
	return Budget{Limit: limit, Length: chantext.VisibleLen}
}

// SplitBudget — опциональная способность приёмника: объявить свою меру длины.
// Реализует maxx.Mirror; кто не реализует, получает RuneBudget(MessageBudget).
type SplitBudget interface {
	MessageBudget() (limit int, length func(string) int)
}

// BudgetFor — бюджет приёмника: своя мера, если он её объявил.
func BudgetFor(p Publisher) Budget {
	if b, ok := p.(SplitBudget); ok {
		limit, length := b.MessageBudget()
		return Budget{Limit: limit, Length: length}
	}
	return RuneBudget(MessageBudget)
}

// SplitMessages жадно пакует блоки в сообщения не длиннее бюджета приёмника.
// Абзац не рвётся (гигантский — обрезается по границе руны с «…» и закрытием
// тегов); рубрика, продолжившаяся в новом сообщении, получает повтор
// заголовка с пометкой «(продолжение)». При N > 1 сообщения нумеруются (k/N).
func SplitMessages(blocks []Block, b Budget) []string {
	limit := b.Limit - numberingReserve
	var msgs [][]string
	var cur []string
	curLen := 0
	header := "" // заголовок текущей рубрики для повтора при переносе
	add := func(t string) {
		if curLen > 0 {
			curLen += len("\n\n")
		}
		cur = append(cur, t)
		curLen += b.Length(t)
	}
	for _, blk := range blocks {
		if blk.NewSection {
			header = blk.Text
		}
		t := blk.Text
		if b.Length(t) > limit {
			t, _ = chantext.FitMeasured(t, limit, b.Length)
		}
		need := b.Length(t)
		if curLen > 0 && curLen+len("\n\n")+need > limit {
			msgs = append(msgs, cur)
			cur, curLen = nil, 0
			if !blk.NewSection && header != "" && header != blk.Text {
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
