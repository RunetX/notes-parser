package digest

import (
	"strings"
	"testing"

	"lovegw/internal/chantext"
)

func TestSplitMessagesSingle(t *testing.T) {
	msgs := SplitMessages([]Block{
		{Text: "<b>Рубрика</b>", NewSection: true},
		{Text: "абзац"},
	}, MessageBudget)
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
	msgs := SplitMessages(blocks, 400)
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
	msgs := SplitMessages(blocks, 400)
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
	msgs := SplitMessages([]Block{{Text: giant, NewSection: true}}, 400)
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
