package archive

import "testing"

// Пачка набирается до порога и хвост НЕ дописывается: недобранный кусок это
// ровно тот шум, ради ухода от которого пачки заведены.
func TestChunkStringsDropsShortTail(t *testing.T) {
	long := rep("а", 400)
	got := chunkStrings([]string{long, long, long})
	if len(got) != 1 {
		t.Fatalf("пачек %d, ожидалась 1 (две по 400 знаков дают одну, третья — хвост)", len(got))
	}
	if n := len([]rune(got[0])); n < voiceBatchRunes {
		t.Errorf("пачка в %d знаков, порог %d", n, voiceBatchRunes)
	}
}

// Правило одно на обе стороны весов: полоса набирается тем же порогом, что и
// наши реплики. Разойдись они — сравнивались бы тексты разной длины, а длина в
// стилометрии решает больше манеры.
func TestChunkHeldUsesSameThreshold(t *testing.T) {
	held := make([]voiceText, 6)
	for i := range held {
		held[i] = voiceText{id: int64(i + 1), author: 42, kind: VoiceKindComments, text: rep("б", 200)}
	}
	got := chunkHeld(held)
	if len(got) != 2 {
		t.Fatalf("пачек полосы %d, ожидалось 2", len(got))
	}
	for i, c := range got {
		if n := len([]rune(c.text)); n < voiceBatchRunes {
			t.Errorf("пачка %d в %d знаков, порог %d", i, n, voiceBatchRunes)
		}
		if c.author != 42 {
			t.Errorf("пачка %d потеряла автора: %d", i, c.author)
		}
	}
	// Идентификатор берётся у ПЕРВОГО текста пачки — по нему её потом искать.
	if got[0].id != 1 || got[1].id != 4 {
		t.Errorf("id пачек %d и %d, ожидались 1 и 4", got[0].id, got[1].id)
	}
}

// Пустой вход мерку не роняет и чисел не выдумывает.
func TestChunkEmpty(t *testing.T) {
	if got := chunkStrings(nil); got != nil {
		t.Errorf("на пустом входе пачки %v", got)
	}
	if got := chunkHeld(nil); got != nil {
		t.Errorf("на пустой полосе пачки %v", got)
	}
}

// Портрет нашего текста снимается ТЕМИ ЖЕ измерителями, что и портрет донора, —
// иначе сравнивать колонки в отчёте было бы нельзя.
func TestMeasureTextsFillsShape(t *testing.T) {
	sh := MeasureTexts([]string{"короткая", "а эта подлиннее будет, слов побольше"}, VoiceKindComments)
	if sh.Texts != 2 {
		t.Errorf("текстов в замере %d", sh.Texts)
	}
	if sh.Runes.Median == 0 {
		t.Error("медиана длины не посчитана")
	}
	if sh.Kind != VoiceKindComments {
		t.Errorf("род текста %q", sh.Kind)
	}
}

func rep(s string, n int) string {
	out := make([]rune, n)
	r := []rune(s)[0]
	for i := range out {
		out[i] = r
	}
	return string(out)
}
