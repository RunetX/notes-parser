package digest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"lovegw/internal/chantext"
)

func TestSplitMessagesSingle(t *testing.T) {
	msgs := SplitMessages([]Block{
		{Text: "<b>Рубрика</b>", NewSection: true},
		{Text: "абзац"},
	}, RuneBudget(MessageBudget))
	if len(msgs) != 1 {
		t.Fatalf("сообщений: %d", len(msgs))
	}
	if strings.Contains(msgs[0], "(1/1)") {
		t.Error("одиночное сообщение не нумеруется")
	}
	if msgs[0] != "<b>Рубрика</b>\n\nабзац" {
		t.Errorf("склейка абзацев: %q", msgs[0])
	}
}

func TestSplitMessagesContinuationHeader(t *testing.T) {
	blocks := []Block{
		{Text: "<b>Рубрика</b>", NewSection: true},
		{Text: strings.Repeat("а", 300)},
		{Text: strings.Repeat("б", 300)},
	}
	msgs := SplitMessages(blocks, RuneBudget(400))
	if len(msgs) < 2 {
		t.Fatalf("ожидалась серия: %d", len(msgs))
	}
	if !strings.HasPrefix(msgs[1], "<b>Рубрика</b> <i>(продолжение)</i>") {
		t.Errorf("повтор заголовка рубрики: %q", msgs[1][:60])
	}
	for i, m := range msgs {
		want := "(" + string(rune('1'+i)) + "/"
		if !strings.Contains(m, want) {
			t.Errorf("нумерация %s в сообщении %d: %q", want, i+1, m[len(m)-12:])
		}
	}
}

func TestSplitMessagesNewSectionNoHeaderRepeat(t *testing.T) {
	blocks := []Block{
		{Text: "<b>Первая</b>", NewSection: true},
		{Text: strings.Repeat("а", 340)},
		{Text: "<b>Вторая</b>", NewSection: true},
		{Text: "хвост"},
	}
	msgs := SplitMessages(blocks, RuneBudget(400))
	if len(msgs) != 2 {
		t.Fatalf("сообщений: %d", len(msgs))
	}
	if strings.Contains(msgs[1], "продолжение") {
		t.Errorf("новая рубрика не «продолжение»: %q", msgs[1])
	}
	if !strings.HasPrefix(msgs[1], "<b>Вторая</b>") {
		t.Errorf("вторая часть начинается с новой рубрики: %q", msgs[1])
	}
}

func TestTruncateHTMLKeepsMarkupSane(t *testing.T) {
	giant := "<b>Заголовок</b> " + strings.Repeat("оченьдлинно ", 100) +
		`<a href="https://x/">ссылка в самом конце</a>`
	msgs := SplitMessages([]Block{{Text: giant, NewSection: true}}, RuneBudget(400))
	if len(msgs) != 1 {
		t.Fatalf("гигантский абзац режется, а не множится: %d", len(msgs))
	}
	m := msgs[0]
	if !strings.Contains(m, "…") {
		t.Error("обрезка должна оставлять многоточие")
	}
	if strings.Count(m, "<b>") != strings.Count(m, "</b>") {
		t.Errorf("незакрытый <b>: %q", m)
	}
	if chantext.VisibleLen(m) > 400 {
		t.Errorf("видимая длина после обрезки: %d", chantext.VisibleLen(m))
	}
}

// rawLen — мера MAX в миниатюре: сырая строка вместе с разметкой.
func rawLen(s string) int { return len([]rune(s)) }

// budgetPub — приёмник, объявивший свою меру (как maxx.Mirror).
type budgetPub struct{ linkless }

func (budgetPub) MessageBudget() (int, func(string) int) { return 1000, rawLen }

type linkless struct{}

func (linkless) Name() string { return "тест" }
func (linkless) PostChannelHTML(_ context.Context, _ string) (string, error) {
	return "", errors.New("не публикуем")
}
func (linkless) ThreadLink(string) string { return "" }

// Приёмник со своей мерой получает её, остальные — общий бюджет видимых рун.
func TestBudgetForSink(t *testing.T) {
	if b := BudgetFor(linkless{}); b.Limit != MessageBudget || b.Length("<b>ок</b>") != 2 {
		t.Errorf("дефолтный бюджет: %d, длина %d", b.Limit, b.Length("<b>ок</b>"))
	}
	if b := BudgetFor(budgetPub{}); b.Limit != 1000 || b.Length("<b>ок</b>") != 9 {
		t.Errorf("бюджет приёмника: %d, длина %d", b.Limit, b.Length("<b>ок</b>"))
	}
}

// Сплит идёт в мере приёмника. Абзацы со ссылками в сырой мере длиннее, чем в
// видимой, — частей должно выйти больше. Иначе транспорт дорезал бы сообщение
// сам, посреди фразы: у MAX предел наложен на строку с тегами.
func TestSplitMessagesUsesSinkMeasure(t *testing.T) {
	var blocks []Block
	for i := 0; i < 30; i++ {
		blocks = append(blocks, Block{
			Text:       `<a href="https://love.ngs.ru/notes/312696/">заметка недели</a> ` + strings.Repeat("текст ", 12),
			NewSection: i == 0,
		})
	}
	visible := SplitMessages(blocks, RuneBudget(1000))
	raw := SplitMessages(blocks, Budget{Limit: 1000, Length: rawLen})
	if len(raw) <= len(visible) {
		t.Errorf("в сырой мере частей должно быть больше: %d против %d", len(raw), len(visible))
	}
	for i, m := range raw {
		if rawLen(m) > 1000 {
			t.Errorf("часть %d длиннее предела приёмника: %d", i+1, rawLen(m))
		}
	}
}
