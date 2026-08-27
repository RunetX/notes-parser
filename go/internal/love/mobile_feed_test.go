package love

import (
	"errors"
	"strings"
	"testing"
)

// Фикстура mobile_feed.html — живая мобильная лента 27.08.2026
// (m.love.ngs.ru/notes/ с мобильным User-Agent), тот же час, что и
// notes_feed_forbidden.html. Перезаписывать той же парой: они отвечают на
// один вопрос — что десктопная лента показала, а назвать не смогла.
func TestParseMobileFeedIDs(t *testing.T) {
	ids, err := ParseMobileFeedIDs(openFixture(t, "mobile_feed.html"))
	if err != nil {
		t.Fatal(err)
	}
	// 8 заметок при 11 ссылках на странице: у части заметок ссылок две.
	if len(ids) != 8 {
		t.Fatalf("id заметок: %d, ожидалось 8 — %v", len(ids), ids)
	}
	// Ради этой строки всё и заведено: 313096 — заметка с запрещёнными
	// комментариями, которую десктопная лента показала без id.
	if ids[0] != "313096" {
		t.Errorf("первый id: %q, ожидался 313096", ids[0])
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			t.Errorf("id %s повторяется: список должен быть без повторов — %v", id, ids)
		}
		seen[id] = true
	}
	// Тот же список лежит на странице второй раз, экранированной строкой
	// внутри <script>. Разбор обязан его не заметить: иначе каждый id придёт
	// дважды, а «лишний» id зеркало примет за незамеченную заметку.
	if len(ids) != len(seen) {
		t.Errorf("копия списка из <script> попала в выборку: %v", ids)
	}
}

func TestParseMobileFeedIDsMarkupDrift(t *testing.T) {
	_, err := ParseMobileFeedIDs(strings.NewReader("<html><body><p>ничего</p></body></html>"))
	var me *MarkupError
	if !errors.As(err, &me) {
		t.Fatalf("ожидался MarkupError, получено %v", err)
	}
}
