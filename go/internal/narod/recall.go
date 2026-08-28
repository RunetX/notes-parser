package narod

// Что житель помнит, когда садится отвечать.
//
// Это оборотная сторона летописи: там разговор превращался в отношения, здесь
// отношения возвращаются в разговор. Без этого шага граф остаётся отчётностью —
// красивой таблицей, на которую никто не смотрит, — а «а помнишь, как ты…»
// взяться неоткуда.
//
// ЧИСЕЛ В ПАМЯТИ НЕТ, и это не украшение. Скажи модели «симпатия 6.2 из 10» —
// и она начнёт писать о шкале: «ты к ней хорошо относишься, поэтому ответь
// тепло». Человек же помнит не число, а СЛУЧАЙ и ощущение от него, и говорит
// исходя из него, ни разу его не назвав. Поэтому шкала переводится в слово, а
// вес несут эпизоды — там, где число сказало бы «плохо», эпизод говорит «за
// что», и только второе можно вспомнить вслух.

import (
	"context"
	"fmt"
	"strings"
)

// MemoryPeer — человек в разговоре, про которого спрашивают память.
type MemoryPeer struct {
	ActorID string
	Nick    string
}

// memoryEpisodes — сколько случаев на пару уходит в промпт. Двух хватает:
// «последнее» и «то, что до него», — а десять превратили бы реплику в отчёт о
// прошлом вместо ответа на сказанное сейчас.
const memoryEpisodes = 2

// memoryInner — сколько своих последних событий житель держит в голове.
const memoryInner = 3

// WriteMemory собирает блок памяти жителя про участников этого разговора.
//
// Пустая строка — законный и частый ответ: в мире, который только начался,
// никто ни с кем ещё не знаком, и подавать модели пустой заголовок «что ты
// помнишь» значило бы предлагать ей вспомнить хоть что-нибудь.
func WriteMemory(ctx context.Context, w *World, actorID string, peers []MemoryPeer) (string, error) {
	if w == nil || actorID == "" {
		return "", nil
	}
	var lines []string
	for _, p := range peers {
		if p.ActorID == "" || p.ActorID == actorID {
			continue
		}
		line, err := peerMemory(ctx, w, actorID, p)
		if err != nil {
			return "", err
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	inner, err := innerMemory(ctx, w, actorID)
	if err != nil {
		return "", err
	}
	if len(lines) == 0 && inner == "" {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("\n=== ЧТО ТЫ ПОМНИШЬ ===\n")
	if len(lines) > 0 {
		b.WriteString("Про тех, кто здесь говорит:\n")
		for _, l := range lines {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	if inner != "" {
		b.WriteString("Своего, за последние дни:\n")
		b.WriteString(inner)
	}
	b.WriteString("Вспоминать вслух — только если к слову. Пересказывать это списком не надо.\n")
	return b.String(), nil
}

// peerMemory — одна строка про одного человека.
func peerMemory(ctx context.Context, w *World, actorID string, p MemoryPeer) (string, error) {
	e, err := w.EdgeOf(ctx, actorID, p.ActorID)
	if err != nil {
		return "", err
	}
	mine, err := w.EpisodesOf(ctx, actorID, p.ActorID, memoryEpisodes)
	if err != nil {
		return "", err
	}
	his, err := w.EpisodesOf(ctx, p.ActorID, actorID, memoryEpisodes)
	if err != nil {
		return "", err
	}
	if e.Familiarity == 0 && len(mine) == 0 && len(his) == 0 {
		return "", nil // незнакомы — и вспоминать нечего
	}

	var b strings.Builder
	fmt.Fprintf(&b, "— %s: %s, %s.", p.Nick, metWord(e.Familiarity), toneWord(e.Tone()))
	for _, ep := range mine {
		fmt.Fprintf(&b, " Ты — %s: %s.", ep.Kind, ep.Summary)
	}
	for _, ep := range his {
		fmt.Fprintf(&b, " Он(а) — %s: %s.", ep.Kind, ep.Summary)
	}
	return b.String(), nil
}

// innerMemory — свои последние события: то, что случилось вне площадки.
func innerMemory(ctx context.Context, w *World, actorID string) (string, error) {
	all, err := w.Recall(ctx, actorID, 40)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	n := 0
	for _, e := range all {
		if e.Kind != JournalInner || strings.TrimSpace(e.Text) == "" {
			continue
		}
		fmt.Fprintf(&b, "— %s: %s\n", e.At.Format("02.01"), strings.TrimSpace(e.Text))
		if n++; n >= memoryInner {
			break
		}
	}
	return b.String(), nil
}

// metWord — знакомство словами.
//
// Границы взяты не с потолка, а из замера отклика: у знакомства корзины
// карточки идут по числу прошлых ответов, и «десяток» там уже верхняя — дальше
// вероятность не растёт. Значит и человеку различать выше десятка нечего.
func metWord(n float64) string {
	switch {
	case n <= 0:
		return "виделись, но не разговаривали"
	case n < 3:
		return "перекинулись парой слов"
	case n < 10:
		return "разговаривали не раз"
	}
	return "давние собеседники"
}

// toneWord — шкала словами. Порогов четыре, и они грубые нарочно: тонкая
// градация вернула бы в промпт то самое число, только прописью.
func toneWord(t float64) string {
	switch {
	case t <= -0.4:
		return "раздражает"
	case t < -0.1:
		return "держишься настороженно"
	case t <= 0.1:
		return "ровно"
	case t < 0.4:
		return "скорее нравится"
	}
	return "тепло"
}
