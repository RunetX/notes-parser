package archive

// Мерка голоса на ПАЧКЕ реплик.
//
// Заведена 28.08.2026 по итогам первого платного прогона калибровки, и повод
// стоит записать целиком, потому что число там выглядело измерением, не будучи
// им. Отчёт печатал «медианный квантиль полосы 0.00» и «место в атрибуции 7932
// из 9361» — то есть будто бы приговор голосу. На деле полоса была НЕПРИГОДНА:
// она строится из отдельных комментариев автора, а комментарий у него медианой
// в 75 знаков — это 73 символьные 3-граммы против порога в 300, за которым ранг
// перестаёт что-либо значить. Об этом прямо сказано в bandUnusableWhy («26 из 30
// текстов ниже порога, медиана ранга 8391 из 9215»), но BandQuantile у
// непригодной полосы возвращает 0, и ноль этот неотличим от честного «хуже всех
// настоящих текстов».
//
// Отсюда единица измерения: не реплика, а ПАЧКА реплик, набранная до объёма, на
// котором атрибутор вообще работает. И у нашей стороны, и у полосы пачка
// набирается ОДНИМ И ТЕМ ЖЕ правилом — иначе сравнивались бы тексты разной
// длины, а длина в стилометрии решает больше манеры.
//
// Своей мерки это не заводит: считает та же связка (атрибутор жанра, состав
// личности, held-out полоса), что и цикл контроля у `personas voice`. Меняется
// только размер того, что кладут на весы.

import (
	"context"
	"fmt"
	"strings"
)

// voiceBatchRunes — до какого объёма набирается пачка. Порог шума —
// voiceShortNgrams (300 символьных 3-грамм ≈ столько же знаков), и брать его
// впритык нельзя: текст ровно на пороге уже шумит. Вдвое выше — запас, за
// который платят только числом пачек.
const voiceBatchRunes = 600

// VoiceBatch — оценка пачек: сколько их вышло, где они встали в атрибуции и
// пригодна ли полоса, с которой их сравнивают.
type VoiceBatch struct {
	Kind   string    `json:"kind"`
	Runes  int       `json:"runes"`  // сколько знаков написал житель всего
	Used   int       `json:"used"`   // сколько из них вошло в пачки; хвост короче пачки отброшен
	Chunks int       `json:"chunks"` // пачек с нашей стороны
	Ranks  []int     `json:"ranks"`
	Quants []float64 `json:"quants"`
	Band   VoiceBand `json:"band"`

	// Why — почему мерка не состоялась; пусто значит состоялась. Печатать
	// вместо чисел, а не рядом с ними: непригодная полоса даёт правдоподобный
	// ноль, и рядом с числом причина читается как примечание.
	Why string `json:"why,omitempty"`
}

// MedianRank / MedianQuantile — итог пачек. Медиана, а не среднее: рангов
// немного, и один провалившийся черновик утащил бы среднее целиком.
func (b VoiceBatch) MedianRank() int {
	if len(b.Ranks) == 0 {
		return 0
	}
	xs := make([]float64, len(b.Ranks))
	for i, r := range b.Ranks {
		xs[i] = float64(r)
	}
	return int(medianFloat(xs) + 0.5)
}

func (b VoiceBatch) MedianQuantile() float64 {
	if len(b.Quants) == 0 {
		return 0
	}
	return round4(medianFloat(append([]float64(nil), b.Quants...)))
}

// voiceContext — общая подготовка мерки: атрибутор жанра, состав личности,
// объём стиль-профиля. Вынесена из GenerateVoice, чтобы пачечная оценка считала
// ранг по ТОМУ ЖЕ знаменателю: разойдись они фильтром кандидатов или составом
// анкет, два числа в одном отчёте меряли бы разное, называясь одинаково.
func (s *Store) voiceContext(ctx context.Context, card *VoiceCard, req VoiceRequest) (
	*voiceScorer, map[int64]bool, map[int64]int, error) {

	v, err := s.newVoiceScorer(ctx, card.Genre, req.LexWeight, req.ActiveDays, req.MinAuthorNotes)
	if err != nil {
		return nil, nil, nil, err
	}
	// Анкеты берём У КАРТЫ, а не через identityMembers(identity): в solo-режиме
	// карта — одна анкета, и ранг обязан считаться по ней же, иначе полоса меряет
	// лучшую из склеенных (у кластера из 11 анкет медиана ранга вырождается в 1).
	if len(card.Accounts) == 0 {
		return nil, nil, nil, fmt.Errorf("voice: %s не резолвится в анкеты", card.Identity)
	}
	member := make(map[int64]bool, len(card.Accounts))
	accIDs := make([]int64, 0, len(card.Accounts))
	for _, a := range card.Accounts {
		member[a.ID] = true
		accIDs = append(accIDs, a.ID)
	}
	pn, err := s.profileNgrams(ctx, accIDs, card.Genre)
	if err != nil {
		return nil, nil, nil, err
	}
	return v, member, pn, nil
}

// VoiceBatchBand — полоса, с которой сравнивают пачки. Спрашивается ОТДЕЛЬНО и
// ДО первого обращения к модели: непригодная полоса означает, что мерить нечем,
// а платить за измерение, которое ничего не измерит, — единственное, чего
// калибровке делать нельзя.
func (s *Store) VoiceBatchBand(ctx context.Context, card *VoiceCard, kind string,
	req VoiceRequest) (VoiceBand, error) {

	v, member, pn, err := s.voiceContext(ctx, card, req)
	if err != nil {
		return VoiceBand{}, err
	}
	return s.buildBatchBand(ctx, card, kind, v, member, pn)
}

func (s *Store) buildBatchBand(ctx context.Context, card *VoiceCard, kind string,
	v *voiceScorer, member map[int64]bool, pn map[int64]int) (VoiceBand, error) {

	return s.BuildVoiceBand(ctx, card.Identity, kind, chunkHeld(card.HeldOut()), v, member, pn)
}

// ScoreVoiceBatch кладёт на весы наши реплики пачками против пачек настоящих.
//
// texts — что написал житель, в порядке появления. Порядок важен: пачка это
// подряд идущие реплики одного разговора, а не случайная горсть.
func (s *Store) ScoreVoiceBatch(ctx context.Context, card *VoiceCard, kind string,
	req VoiceRequest, texts []string) (VoiceBatch, error) {

	out := VoiceBatch{Kind: kind}
	for _, t := range texts {
		out.Runes += len([]rune(t))
	}
	if len(texts) == 0 {
		out.Why = "модель не выдала ни одной реплики"
		return out, nil
	}

	v, member, pn, err := s.voiceContext(ctx, card, req)
	if err != nil {
		return out, err
	}
	band, err := s.buildBatchBand(ctx, card, kind, v, member, pn)
	if err != nil {
		return out, err
	}
	out.Band = band
	if !band.Usable {
		out.Why = "полоса непригодна: " + band.Why
		return out, nil
	}

	for _, chunk := range chunkStrings(texts) {
		sc := v.score(chunk, member, card.Identity)
		if sc.Rank == 0 {
			continue
		}
		out.Used += len([]rune(chunk))
		out.Chunks++
		out.Ranks = append(out.Ranks, sc.Rank)
		out.Quants = append(out.Quants, BandQuantile(band, sc.Rank))
	}
	if out.Chunks == 0 {
		out.Why = fmt.Sprintf("реплик набралось на %d знаков — меньше одной пачки (%d)",
			out.Runes, voiceBatchRunes)
	}
	return out, nil
}

// chunkStrings набирает пачки до voiceBatchRunes. Хвост короче порога
// ОТБРАСЫВАЕТСЯ: его ранг — тот самый шум, ради ухода от которого пачки и
// заведены, а посчитанный наравне с полными он тянул бы медиану вниз тем
// вернее, чем аккуратнее прогон.
func chunkStrings(texts []string) []string {
	var out []string
	var b strings.Builder
	n := 0
	for _, t := range texts {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(t)
		n += len([]rune(t))
		if n >= voiceBatchRunes {
			out = append(out, b.String())
			b.Reset()
			n = 0
		}
	}
	return out
}

// chunkHeld — то же правило для полосы. Тексты отложенные, то есть в промпт они
// не шли; склейка идёт в том порядке, в каком их отложили.
func chunkHeld(held []voiceText) []voiceText {
	var out []voiceText
	var b strings.Builder
	var first voiceText
	n := 0
	for _, t := range held {
		if b.Len() == 0 {
			first = t
		} else {
			b.WriteString("\n")
		}
		b.WriteString(t.text)
		n += len([]rune(t.text))
		if n >= voiceBatchRunes {
			first.text = b.String()
			out = append(out, first)
			b.Reset()
			n = 0
		}
	}
	return out
}

// MeasureTexts — механический портрет нашего же текста теми же измерителями,
// какими снят портрет донора. Нужен затем, что ранг говорит «не похоже», а
// длина строки говорит ЧЕМ: первый же прогон дал реплики вдвое длиннее
// медианы донора, и увидеть это в ранге было нельзя.
func MeasureTexts(texts []string, kind string) VoiceShape {
	vt := make([]voiceText, 0, len(texts))
	for _, t := range texts {
		vt = append(vt, voiceText{kind: kind, text: t})
	}
	return measureShape(vt, kind, nil)
}

// Род текста. Строки те же, что у shapeKind: наружу их выносит пачечная мерка,
// потому что зовущий её выбирает род сам, а второй набор литералов рядом с
// первым однажды разъехался бы опечаткой.
const (
	VoiceKindComments = "comments"
	VoiceKindNotes    = "notes"
)

// CardShape — портрет донора в этом роде текста, с чем и сравнивают наш.
func CardShape(card *VoiceCard, kind string) VoiceShape { return shapeOf(card, kind) }
