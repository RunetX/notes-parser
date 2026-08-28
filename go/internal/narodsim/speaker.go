package narodsim

// Speaker поверх готового цикла контроля голоса (archive.GenerateVoice).
//
// Своего цикла реплей не заводит намеренно. Тот, что есть, оплачен эпиком
// `voice`: он строит held-out-полосу по настоящим текстам автора, прогоняет
// черновик через атрибутор того же архива и возвращает КВАНТИЛЬ — долю
// настоящих текстов, узнанных хуже нашего. Это единственная мерка голоса,
// которая не сводится к «на глаз похоже», и заводить рядом вторую значило бы
// получить два числа, расходящихся молча.

import (
	"context"
	"fmt"
	"math/rand/v2"

	"lovegw/internal/archive"
	"lovegw/internal/narod"
)

// branchLimit — сколько строк разговора показывать модели. Двадцать: столько же
// берёт `personas voice`, и расходиться с ним здесь нельзя — иначе калибровка
// мерила бы голос при одном контексте, а боевая генерация шла бы при другом.
const branchLimit = 20

// VoiceSpeaker — «что житель написал бы здесь», через цикл контроля.
type VoiceSpeaker struct {
	Store *archive.Store
	Gen   archive.JSONGenerator
	Card  *archive.VoiceCard
	// SelfIDs — анкеты самого жителя: по ним его прошлые слова в ветке
	// помечаются своими, иначе он противоречил бы сам себе.
	SelfIDs []int64
	// Req — шаблон запроса: черновики, раунды, пороги. Mode и Thread
	// подставляются на каждой точке.
	Req archive.VoiceRequest

	// Runes — разброс длины реплики у донора и зерно жребия по нему. Длина
	// называется НА КАЖДУЮ реплику отдельно: разброс живёт между репликами, а
	// не внутри одной, и промпт, просивший «обычную» длину, давал прогон, сбитый
	// к середине (замер 28.08.2026 — медиана на треть выше донорской при p90 на
	// четверть ниже, у всех трёх слепков).
	Runes narod.Dist
	// EmojiRate — доля реплик автора с эмодзи; ноль значит «жребий не бросаем».
	EmojiRate float64
	Seed      uint64
}

// Соли разводят жребии между собой: на одной и той же точке каждый берёт первое
// число из своего потока, и без соли длинная реплика приходилась бы ровно на
// приход жителя, а эмодзи — ровно на длинную реплику.
const (
	lengthSalt = 0x9E3779B97F4A7C15
	emojiSalt  = 0xBF58476D1CE4E5B9
)

// wantEmoji — есть ли эмодзи в ЭТОЙ реплике.
//
// Тот же приём, что с длиной, и по той же причине: доля — свойство десятка
// реплик, одной репликой она невыразима, и модель, прочитав «ставит изредка»,
// решает за весь прогон разом. На замере 28.08.2026 она решила «не ставить
// вовсе»: 0 % против 12 % у донора, — а у другого слепка в тот же прогон вышло
// 16 % против 18 %, то есть решение это ещё и случайное.
func (s *VoiceSpeaker) wantEmoji(commentID int64) *bool {
	if s.EmojiRate <= 0 {
		return nil
	}
	rng := rand.New(rand.NewPCG(s.Seed^emojiSalt, uint64(commentID)+1))
	want := rng.Float64() < s.EmojiRate
	return &want
}

// targetRunes — длина этой реплики жребием.
//
// Жребием, а не по настоящей реплике донора, хотя в реплее она известна: длина
// это крупная часть стилометрии, и подсказав её, мы мерили бы голос по тексту,
// которому половину ответа выдали заранее.
func (s *VoiceSpeaker) targetRunes(commentID int64) int {
	if s.Runes.Median <= 0 {
		return 0
	}
	rng := rand.New(rand.NewPCG(s.Seed^lengthSalt, uint64(commentID)+1))
	return int(quantileAt(s.Runes, rng.Float64()) + 0.5)
}

// Speak генерирует реплику на месте настоящей.
func (s *VoiceSpeaker) Speak(ctx context.Context, p SpeechPoint) (Speech, error) {
	if s.Store == nil || s.Gen == nil || s.Card == nil {
		return Speech{}, fmt.Errorf("speaker: не задан архив, модель или карточка")
	}
	req := s.Req
	req.Mode = archive.VoiceModeComment
	req.Thread = archive.ScriptVoiceThread(p.Script, p.Upto, p.Truth.ReplyTo, s.SelfIDs, branchLimit)
	req.TargetRunes = s.targetRunes(p.Truth.ID)
	req.Emoji = s.wantEmoji(p.Truth.ID)

	run, err := s.Store.GenerateVoice(ctx, s.Gen, s.Card, req, p.Now)
	if err != nil {
		return Speech{}, err
	}
	return speechOf(run), nil
}

// speechOf переводит итог цикла в строку отчёта.
//
// Пустой Best — это не ошибка прогона: все черновики могли выбыть на проверках,
// и «модель ничего не дала» само по себе результат, который обязан попасть в
// отчёт с причиной. Потерять его значило бы посчитать медиану по одним удачам.
func speechOf(run *archive.VoiceRun) Speech {
	if run == nil {
		return Speech{Rejected: "прогон не состоялся"}
	}
	if run.Best == nil {
		why := run.Verdict
		if why == "" {
			why = "ни один черновик не прошёл проверки"
		}
		if run.Aborted != "" {
			why = run.Aborted
		}
		return Speech{Rejected: why}
	}
	return Speech{
		Got:      run.Best.Text,
		Quantile: run.Best.Quantile,
		Rank:     run.Best.Score.Rank,
		Of:       run.Best.Score.Of,
		Copy:     run.Best.Copy,
	}
}

// Voice — итог мерки голоса по всем репликам прогона.
//
// Единица здесь ПАЧКА, а не реплика, и это не выбор удобства. Комментарий
// донора медианой в 75 знаков — 73 символьные 3-граммы против порога в 300, за
// которым ранг атрибуции шумит; полоса из таких текстов честно объявляет себя
// непригодной, а BandQuantile у непригодной полосы возвращает ноль. Первый
// платный прогон 28.08.2026 напечатал этот ноль как измерение («квантиль 0.00,
// место 7932 из 9361») — приговор голосу, вынесенный измерителем, который в тот
// момент ничего не мерил.
type Voice struct {
	Batch archive.VoiceBatch `json:"batch"`

	// Got / Want — механический портрет нашего текста против донорского. Ранг
	// говорит «не похоже», портрет говорит ЧЕМ: в том же прогоне реплики вышли
	// длиннее донорской медианы, и в ранге этого было не видно.
	Got  archive.VoiceShape `json:"got"`
	Want archive.VoiceShape `json:"want"`
}

// Measurer — кто умеет померить голос. Оба метода здесь потому, что вопрос у
// них один и тот же, заданный до и после: полоса отвечает «есть ли чем мерить»
// ДО первого платного обращения, пачки — «что вышло» после.
type Measurer interface {
	// Band — полоса до первого обращения к модели: годится ли вообще мерить.
	Band(ctx context.Context) (archive.VoiceBand, error)
	Measure(ctx context.Context, texts []string) (Voice, error)
}

// Measure — пачки против пачек плюс портрет против портрета.
func (s *VoiceSpeaker) Measure(ctx context.Context, texts []string) (Voice, error) {
	kind := archive.VoiceKindComments
	batch, err := s.Store.ScoreVoiceBatch(ctx, s.Card, kind, s.Req, texts)
	if err != nil {
		return Voice{}, err
	}
	return Voice{Batch: batch, Got: archive.MeasureTexts(texts, kind), Want: archive.CardShape(s.Card, kind)}, nil
}

// Band — полоса до первого обращения к модели: годится ли вообще мерить.
func (s *VoiceSpeaker) Band(ctx context.Context) (archive.VoiceBand, error) {
	return s.Store.VoiceBatchBand(ctx, s.Card, archive.VoiceKindComments, s.Req)
}
