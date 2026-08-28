package main

// Сборка карточки персонажа из архива.
//
// Живёт здесь, а не в одном из пакетов, по устройству эпика: ядро эмуляции
// (internal/narod) не знает про архив, архив не знает про эмуляцию, — а cmd и
// так знает оба мира. Карточка от этого остаётся ДОКУМЕНТОМ: разъехаться с
// внутренними структурами архива молча она не может, потому что перенос полей
// стережёт парный тест.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/archive"
	"lovegw/internal/narod"
)

// snapshotOpts — что настраивается при съёмке слепка.
type snapshotOpts struct {
	Recent      int // последних текстов в замер (0 — все)
	NormSample  int // комментариев в норму корпуса
	TopWords    int // характерных слов
	TopEdges    int // собеседников в стартовые отношения
	RateThreads int // последних тредов в замер отклика
	Seed        int64
}

func defaultSnapshotOpts() snapshotOpts {
	// RateThreads — 300 последних тредов: этого хватает на десятки тысяч
	// возможностей ответить даже у скромного участника, а хвост в тринадцать лет
	// утянул бы кривую к тому, каким человек был когда-то.
	return snapshotOpts{Recent: 2000, NormSample: 100000, TopWords: 40, TopEdges: 12,
		RateThreads: 300, Seed: 1}
}

// buildSnapshotCard снимает слепок реального участника архива.
//
// Слепок — инструмент калибровки, а не житель: наружу он не выходит никогда
// (narod.CheckLive), и марка do_not_publish стоит на нём с рождения.
func buildSnapshotCard(ctx context.Context, st *archive.Store, token string, opts snapshotOpts, now time.Time) (narod.Card, error) {
	p := archive.VoiceCardDefaults()
	// Жанр — весь корпус, а РОД текста — реплика: персонаж живёт в тредах, и
	// мерить его манеру по заметкам значило бы мерить не то, что он пишет.
	p.Genre, p.Kind = archive.GenreAll, "comments"
	p.Recent, p.TopWords, p.Seed = opts.Recent, opts.TopWords, opts.Seed

	voice, err := st.BuildVoiceCard(ctx, token, p, now)
	if err != nil {
		return narod.Card{}, err
	}
	accIDs := make([]int64, 0, len(voice.Accounts))
	for _, a := range voice.Accounts {
		accIDs = append(accIDs, a.ID)
	}

	card := narod.Card{
		Stamp:     narod.NewStamp("lovegw narod card", now),
		ID:        slugOfIdentity(voice.Identity),
		Kind:      narod.KindSnapshot,
		Sources:   []string{voice.Identity},
		Seed:      opts.Seed,
		VocabRate: voice.VocabRate,
	}
	// Ники соседей по треду — не слова автора. Обращение «Ник, …» стоит у неё в
	// каждой второй реплике, и без этого сита характерными словами выходят имена
	// живых людей (замер 27.08.2026: у Полынь-Травы из сорока слов ими были
	// тридцать). Список уходит в промпт — персонаж начал бы звать соседей
	// настоящими никами.
	nicks, err := st.SiteNickTokens(ctx)
	if err != nil {
		return narod.Card{}, err
	}
	card.Register = registerFromShape(voice.Comments)
	card.Register.Openings = dropNicks(card.Register.Openings, nicks)
	card.Rhythm = narod.Rhythm{TZ: voice.Rhythm.TZ, Hours: voice.Rhythm.Hours, Weekdays: voice.Rhythm.Weekdays}
	card.Vocab = vocabFromVoice(voice.Vocab, nicks)
	card.Samples = samplesFromVoice(voice.Samples)
	card.Persona = narod.Bio{Nick: strings.TrimSpace(voice.Label)}
	if card.Persona.Nick == "" && len(voice.Accounts) > 0 {
		card.Persona.Nick = strings.TrimSpace(voice.Accounts[0].Name)
	}

	// Пол и возраст берём быстрым путём: v_persona_activity на живом архиве
	// считается секундами на личность, а нужны отсюда два поля.
	if nodes, err := st.CohortNodes(ctx, []string{voice.Identity}); err == nil {
		if n, ok := nodes[voice.Identity]; ok {
			card.Persona.Gender = n.Gender
			card.Persona.Age = parseAge(n.Age)
		}
	}

	if card.Latency, err = latencyFromArchive(ctx, st, accIDs); err != nil {
		return narod.Card{}, err
	}
	norm, err := st.BuildCorpusNorm(ctx, opts.NormSample)
	if err != nil {
		return narod.Card{}, err
	}
	errs, err := st.MineErrors(ctx, accIDs, norm, opts.Recent)
	if err != nil {
		return narod.Card{}, err
	}
	card.Errors = errorsFromArchive(errs)

	if facts, err := st.IdentityFacts(ctx, voice.Identity); err == nil {
		card.Triggers = triggersFromFacts(facts)
	}
	if rels, err := st.IdentityRelations(ctx, voice.Identity, opts.TopEdges); err == nil {
		card.Relations = relationsFromArchive(rels)
	}
	load, err := st.MineThreadLoad(ctx, accIDs)
	if err != nil {
		return narod.Card{}, err
	}
	card.Dice = diceFromShape(voice.Comments, load)

	rate, err := st.MineReplyRate(ctx, accIDs, opts.RateThreads)
	if err != nil {
		return narod.Card{}, err
	}
	card.Rate = rateFromArchive(rate)

	if err := card.Validate(); err != nil {
		return narod.Card{}, fmt.Errorf("слепок %s: %w", voice.Identity, err)
	}
	return card, nil
}

// registerFromShape — перенос замеров манеры. Тот случай, когда одно поле в
// одно: расхождение здесь стережёт парный тест, потому что молчаливо потерянный
// замер выглядит как «персонаж пишет ровно», а не как поломка.
func registerFromShape(sh archive.VoiceShape) narod.Register {
	return narod.Register{
		Runes:         distFromArchive(sh.Runes),
		SentWords:     distFromArchive(sh.SentWords),
		SentWordSD:    sh.SentWordSD,
		ShortSents:    sh.ShortSents,
		LongSents:     sh.LongSents,
		Punct:         sh.Punct,
		ParenRuns:     sh.ParenRuns,
		Smileys:       countsFromArchive(sh.TopSmileys),
		SmileyRate:    sh.SmileyRate,
		EmojiRate:     sh.EmojiRate,
		AllLower:      sh.AllLower,
		StartsLower:   sh.StartsLower,
		NoFinalPunct:  sh.NoFinalPunct,
		YoRate:        sh.YoRate,
		Openings:      countsFromArchive(sh.TopOpenings),
		AddressPrefix: sh.AddressPrefix,
	}
}

func distFromArchive(d archive.Dist) narod.Dist {
	return narod.Dist{P10: d.P10, Median: d.Median, P90: d.P90, Max: d.Max}
}

func countsFromArchive(cs []archive.VoiceCount) []narod.Count {
	out := make([]narod.Count, 0, len(cs))
	for _, c := range cs {
		out = append(out, narod.Count{Text: c.Text, Share: c.Share})
	}
	return out
}

func vocabFromVoice(ws []archive.VoiceWord, nicks map[string]bool) []narod.Word {
	out := make([]narod.Word, 0, len(ws))
	for _, w := range ws {
		if nicks[w.Word] {
			continue
		}
		out = append(out, narod.Word{Word: w.Word, TFIDF: w.TFIDF})
	}
	return out
}

// dropNicks убирает из топ-списка чужие имена. Зачины ими зарастают по той же
// причине, что и словарь: обращение стоит в начале реплики, а значит первым
// словом.
func dropNicks(cs []narod.Count, nicks map[string]bool) []narod.Count {
	out := make([]narod.Count, 0, len(cs))
	for _, c := range cs {
		if nicks[strings.ToLower(c.Text)] {
			continue
		}
		out = append(out, c)
	}
	return out
}

func samplesFromVoice(ss []archive.VoiceSample) []narod.Sample {
	out := make([]narod.Sample, 0, len(ss))
	for _, s := range ss {
		out = append(out, narod.Sample{Text: s.Text, Context: s.Context, ContextAuthor: s.ContextAuthor})
	}
	return out
}

func latencyFromArchive(ctx context.Context, st *archive.Store, accIDs []int64) (narod.LatencyDist, error) {
	lat, err := st.MineLatency(ctx, accIDs)
	if err != nil {
		return narod.LatencyDist{}, err
	}
	return narod.LatencyDist{
		ToThreadSec: distFromArchive(lat.ToThread),
		ToReplySec:  distFromArchive(lat.ToReply),
	}, nil
}

func errorsFromArchive(errs []archive.VoiceError) []narod.ErrorPattern {
	out := make([]narod.ErrorPattern, 0, len(errs))
	for _, e := range errs {
		out = append(out, narod.ErrorPattern{ID: e.ID, Rate: e.Rate, Norm: e.Norm, Variant: e.Variant})
	}
	return out
}

// topicNames — темы лексикона архива по-русски. Карточка уходит в промпт и
// человеку в отчёт, а «alcohol, dacha, kids» не читается ни тем, ни другим.
var topicNames = map[string]string{
	"dogs": "собаки", "cats": "кошки", "sea": "море", "dacha": "дача",
	"fishing": "рыбалка", "cars": "машины", "kids": "дети", "sport": "спорт",
	"cooking": "готовка", "alcohol": "выпивка", "music": "музыка", "travel": "путешествия",
}

// maxTriggers — сколько тем оставлять. Замер на живом архиве отдал двенадцать
// тем из двенадцати возможных: у разговорчивого человека за годы всплывает
// всё, и список «цепляет всё» не отличает его ни от кого. Кубику нужны
// НЕСКОЛЬКО сильных тем, а не полный перечень упоминаний.
const maxTriggers = 6

// triggersFromFacts переводит интересы в веса. Полярность здесь важнее силы
// сигнала: «не любит» — это законный вход для «промолчать», а молчание в этом
// мире обязательный исход, а не отказ службы.
func triggersFromFacts(facts []archive.IdentityFact) []narod.Topic {
	out := make([]narod.Topic, 0, len(facts))
	for _, f := range facts {
		w := f.Score
		if w <= 0 {
			w = 0.5
		}
		switch f.Polarity {
		case archive.PolarityDislikes:
			w = -w
		case archive.PolarityMentions:
			// Тема просто всплывала в речи — это интерес, но слабый: считать
			// её увлечением значит выдумать человеку хобби.
			w /= 2
		}
		name := topicNames[f.Topic]
		if name == "" {
			name = f.Topic
		}
		out = append(out, narod.Topic{Name: name, Weight: round2(w)})
	}
	// Сильные с обоих концов: и любимое, и постылое. Отбрасывается середина —
	// темы, к которым человек равнодушен, а таких у говорливого большинство.
	sort.Slice(out, func(i, j int) bool {
		if math.Abs(out[i].Weight) != math.Abs(out[j].Weight) {
			return math.Abs(out[i].Weight) > math.Abs(out[j].Weight)
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > maxTriggers {
		out = out[:maxTriggers]
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// relationsFromArchive переносит ТОН как стартовую симпатию.
//
// Именно тон, а не тип отношений из разметки модели: типов в архиве размечено
// сто восемь пар на шестьдесят шесть тысяч, и «дружбы» там больше, чем ссор
// (conflict — ноль строк). Тон же есть у всех пар и знак имеет честный: он
// снят со смайлов и скобок, то есть с того, чем люди и правда обозначают
// отношение.
func relationsFromArchive(rels []archive.RelationRow) []narod.SeedEdge {
	out := make([]narod.SeedEdge, 0, len(rels))
	for _, r := range rels {
		if r.Tone == 0 {
			continue
		}
		out = append(out, narod.SeedEdge{
			Actor:    slugOfIdentity(r.To),
			Sympathy: round2(r.Tone * 10), // шкала мира [-10..10]
			Note:     r.Label,
		})
	}
	return out
}

// diceFromShape — стартовые вероятности прихода и потолки разговорчивости.
//
// ВЕРОЯТНОСТИ замерить по архиву нельзя: «сколько заметок человек прочёл и не
// ответил» не знает никто, включая его самого. Поэтому берём базу из брифа
// (четверо-шестеро из десяти не приходят) и лишь чуть двигаем её тем, живёт
// человек в разговоре или пишет мимо.
//
// А вот ПОТОЛКИ считаются прямо, и путать эти два случая дорого. Пока потолок
// стоял константой, у ДВ в карточке было 5 реплик на тред против настоящих
// 44–53, кубик глох после пятой, и калибровка ловила 6 % его настоящих ответов
// (замер 28.08.2026, первый же бесплатный прогон реплея). Отсюда правило:
// величина, которую видно в архиве, берётся из архива, даже когда соседняя
// величина рядом взята с потолка.
//
// Потолок на тред — p90 замера, а не максимум: максимум у говорливого автора
// это один скандал на всю историю, и мерить им обычный вечер значит разрешить
// жителю такой скандал каждый раз.
// Вероятности ответа здесь — ЗАПАСНОЙ путь с 28.08.2026: настоящие берутся из
// замера card.Rate, а эти работают там, где корзина замера пуста. Цену выдумки
// тот замер и назвал: ReplyOther = 0.15 против настоящих 0.4–3.7 % — промах в
// 20–40 раз, и он один давал 71 приход мимо на тред в 298 реплик. ReplyMention =
// 0.7 против настоящих 39–91 % попало почти точно. Урок записан здесь, потому
// что править эти числа будут отсюда: на глаз угадывается то, что человек делает
// часто, и не угадывается то, что редко, — а поведение в людном треде состоит
// как раз из редкого. ComeToNote замером не покрыт и покрыт быть не может: в
// архиве видно, в какие треды человек пришёл, и не видно, какие пролистал.
func diceFromShape(sh archive.VoiceShape, load archive.Dist) narod.DiceParams {
	d := narod.DiceParams{
		ComeToNote: 0.35, ReplyMention: 0.7, ReplyOther: 0.15,
		MaxPerThread: 3, MaxPerDay: 8,
	}
	if sh.AddressPrefix > 0.5 {
		// Человек, который в половине реплик обращается по имени, живёт в
		// разговоре, а не в монологе.
		d.ReplyOther = 0.25
	}
	if load.P90 > 0 {
		d.MaxPerThread = load.P90
	}
	if load.Median > 0 {
		// За сутки человек бывает в нескольких тредах: один людный плюс пара
		// обычных. Суточного замера у нас нет, но потолок обязан быть ВЫШЕ
		// тредового — иначе он молча заменяет его собой, и правило про тред
		// перестаёт что-либо значить. Ровно это и вышло на первой редакции
		// (3 × медиану = 9 против тредовых 11), и поймал это тест, а не прогон.
		d.MaxPerDay = d.MaxPerThread + 2*load.Median
	}
	return d
}

// slugOfIdentity — ключ актора из архивного идентификатора: p1527 → p1527.
// Идентификатор архива уже годится в slug, но проверить стоит: у нативных и
// восстановленных полос он другой.
func slugOfIdentity(identity string) string {
	s := strings.ToLower(strings.TrimSpace(identity))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// parseAge — возраст из анкеты. В архиве это текст («52 года»), потому что
// сайт отдаёт его словами.
func parseAge(s string) int {
	var digits strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
			continue
		}
		if digits.Len() > 0 {
			break
		}
	}
	n, err := strconv.Atoi(digits.String())
	if err != nil || n < 14 || n > 100 {
		return 0
	}
	return n
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

// rateFromArchive переносит кривую отклика в карточку. Числа переносятся как
// есть, включая пустые корзины: по ним видно, где замера не хватило, а
// выброшенная пустая корзина выглядела бы как её отсутствие в природе.
func rateFromArchive(r archive.ReplyRate) narod.ReplyRate {
	out := narod.ReplyRate{Threads: r.Threads}
	for _, b := range r.Buckets {
		out.Buckets = append(out.Buckets, narod.RateBucket{
			Upto: b.Upto, Chances: b.Chances, Answers: b.Answers,
			ToHimChances: b.ToHimChances, ToHimAnswers: b.ToHimAnswers,
		})
	}
	return out
}
