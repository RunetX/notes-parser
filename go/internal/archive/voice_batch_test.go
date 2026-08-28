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

// Треды калибровки берутся РАВНОМЕРНО по размаху разговорчивости, а не с
// верхушки. Правило оплачено замером 28.08.2026: с верхушки мерился потолок
// кубика, а не его догадка.
func TestSpreadPicksTakesBothEdges(t *testing.T) {
	all := make([]ThreadPick, 20)
	for i := range all {
		all[i] = ThreadPick{NoteID: int64(i + 1), Said: i + 5}
	}
	got := spreadPicks(all, 5)
	if len(got) != 5 {
		t.Fatalf("отобрано %d тредов", len(got))
	}
	// Оба края на месте: скромный тред показывает кубик там, где потолок не
	// мешает, людный — там, где мешает.
	if got[0].Said != 5 || got[4].Said != 24 {
		t.Errorf("края выборки %d и %d, ожидались 5 и 24", got[0].Said, got[4].Said)
	}
	// И середина не сбита в один угол.
	if got[2].Said != 14 {
		t.Errorf("середина выборки %d, ожидалось 14", got[2].Said)
	}
}

// Просить больше, чем есть, — не ошибка: отдаётся всё, что подошло.
func TestSpreadPicksShortList(t *testing.T) {
	all := []ThreadPick{{Said: 5}, {Said: 9}}
	if got := spreadPicks(all, 5); len(got) != 2 {
		t.Errorf("на коротком списке отобрано %d", len(got))
	}
	if got := spreadPicks(all, 1); len(got) != 1 || got[0].Said != 9 {
		t.Errorf("единственный тред берётся из середины: %+v", got)
	}
	if got := spreadPicks(nil, 5); got != nil {
		t.Errorf("на пустом списке %v", got)
	}
}

// Заданная длина двигает и ГРАНИЦЫ приёмки: жребий берёт хвост распределения, а
// рамка по разбросу автора отсекала бы как раз длинные реплики — то самое, ради
// чего задание и заведено.
func TestLengthBandFollowsTarget(t *testing.T) {
	sh := VoiceShape{Runes: Dist{P10: 30, Median: 75, P90: 174, Max: 970}}

	lo, hi := lengthBand(sh, VoiceRequest{})
	if lo != 18 || hi != 278 {
		t.Errorf("без задания рамка %d–%d, ожидалась 18–278", lo, hi)
	}
	// Длинный жребий прежней рамкой был бы забракован, новой — принят.
	lo, hi = lengthBand(sh, VoiceRequest{TargetRunes: 400})
	if lo != 160 || hi != 1000 {
		t.Errorf("на задании 400 рамка %d–%d, ожидалась 160–1000", lo, hi)
	}
	// И короткий тоже: 30 знаков в прежнюю рамку попадали, но допуск теперь свой.
	if lo, hi = lengthBand(sh, VoiceRequest{TargetRunes: 30}); lo != 12 || hi != 75 {
		t.Errorf("на задании 30 рамка %d–%d, ожидалась 12–75", lo, hi)
	}
	// Пустой замер рамки не даёт вовсе — проверять нечем.
	if lo, hi = lengthBand(VoiceShape{}, VoiceRequest{}); lo != 0 || hi != 0 {
		t.Errorf("по пустому замеру рамка %d–%d", lo, hi)
	}
}
