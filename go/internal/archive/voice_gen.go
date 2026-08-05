package archive

// «Голос» здесь — авторская манера письма, не голосовое сообщение (ср.
// tgx.VoiceHandler — то про ASR).
//
// Генерация текста в манере участника и ЗАМКНУТЫЙ ЦИКЛ: черновик тут же
// прогоняется через собственную атрибуцию архива и читается не сам по себе, а
// против эталонной полосы — рангов настоящих текстов того же человека.
//
// Онлайн-модель подаётся интерфейсом: пакет archive не знает ни про какого
// провайдера (тот же шов, что JSONGenerator в digest). Пути публикации здесь нет
// и быть не должно — см. VoiceWarning.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"lovegw/internal/love"
)

// JSONGenerator — онлайн-LLM, отвечающая строго по JSON-схеме (реализация —
// llm.Client).
type JSONGenerator interface {
	GenerateJSON(ctx context.Context, system, prompt string, schema map[string]any) ([]byte, error)
}

// Режимы генерации.
const (
	VoiceModeNote    = "note"
	VoiceModeComment = "comment"
)

// VoiceRequest — параметры прогона.
type VoiceRequest struct {
	Mode   string // note | comment
	Topic  string // тема заметки (note)
	Thread *VoiceThread

	Drafts  int     // черновиков за запрос
	Rounds  int     // запросов максимум (1 — без обратной связи)
	Accept  float64 // порог квантиля полосы
	MaxCopy float64 // потолок пересечения с образцами

	LexWeight      float64
	ActiveDays     int
	MinAuthorNotes int
	Control        string // вторая личность для контроля
	Model          string // для марки артефакта
}

// VoiceDraft — черновик и всё, что о нём известно.
type VoiceDraft struct {
	Text     string  `json:"text"`               // тело, как вернула модель
	Rendered string  `json:"rendered,omitempty"` // то, что появилось бы на сайте (с обращением)
	Idea     string  `json:"idea,omitempty"`
	Rejected string  `json:"rejected,omitempty"` // непустое — черновик выбыл, причина
	Copy     float64 `json:"copy"`               // пересечение с образцами

	// VocabRate — слов из списка характерных на 100 слов. Против VoiceCard.VocabRate
	// (нормы самого автора) это прямая мера набивки.
	VocabRate float64 `json:"vocab_rate"`

	Score        VoiceScore  `json:"score"`
	Quantile     float64     `json:"quantile"` // доля реальных текстов, узнанных ХУЖЕ
	Control      *VoiceScore `json:"control,omitempty"`
	ControlQuant float64     `json:"control_quantile,omitempty"`
	Diff         []string    `json:"diff,omitempty"`
}

// VoiceRound — один запрос к модели.
type VoiceRound struct {
	N      int          `json:"n"`
	Prompt string       `json:"prompt"`
	Drafts []VoiceDraft `json:"drafts"`
}

// VoiceRun — журнал прогона целиком.
type VoiceRun struct {
	Stamp    VoiceStamp `json:"stamp"`
	Identity string     `json:"identity"`
	Label    string     `json:"label"`
	Mode     string     `json:"mode"`
	Genre    string     `json:"genre"`
	Topic    string     `json:"topic,omitempty"`

	Card    *VoiceCard   `json:"card"`
	Thread  *VoiceThread `json:"thread,omitempty"`
	Band    VoiceBand    `json:"band"`
	Control *VoiceBand   `json:"control_band,omitempty"`

	Rounds   []VoiceRound `json:"rounds"`
	Best     *VoiceDraft  `json:"best,omitempty"`
	Accepted bool         `json:"accepted"`
	Verdict  string       `json:"verdict"`

	// Aborted — прогон оборвался на позднем раунде, но результаты предыдущих
	// сохранены. Молча терять это нельзя: неполный цикл читается иначе.
	Aborted string `json:"aborted,omitempty"`
}

const voiceSystemBase = `Ты — исследовательский инструмент проверки устойчивости атрибуции авторства.
Задача: написать текст в МАНЕРЕ участника сайта по механическим измерениям его
письма. Результат никуда не публикуется — он уходит в атрибутор того же архива,
чтобы измерить, узнаёт ли тот автора в машинном тексте.

Жёсткие правила:
- Никаких новых фактов о человеке: ни имён, ни городов, ни родни, ни событий
  биографии, которых нет в задании.
- Не копируй образцы: ни оборотов длиннее трёх слов, ни пересказа. Манера — да,
  содержание — новое.
- Измерения это ЦЕЛИ, а не пожелания: длина в рунах, число предложений, слов в
  предложении, пунктуация, регистр, абзацы.
- Разметку сайта воспроизводи как есть ([b]…[/b], :::код:::) и ровно с той
  частотой, что в измерениях. Markdown (**, ##, ---, обратные кавычки, списки
  дефисом) запрещён.
- Пиши обычным текстом, без HTML и без экранирования.
- Не приглаживай регистр. Образцы показывают, как человек пишет на самом деле:
  если он груб, коряв, нарочно пишет с ошибками и рубит фразу — воспроизводи это.
  Гладкий литературный текст с симметричной шуткой в конце — самый частый провал
  имитации: он не похож ни на кого конкретного.
- РИТМ важнее среднего. Ровный поток предложений одной длины — главная примета
  машинного текста, даже когда среднее совпало. Мешай рубленые фразы в 1–3 слова
  с длинными на 20+ слов ровно в той пропорции, что в измерениях.
- Повтори КОМПОЗИЦИЮ образцов, не только слова: как автор входит в текст (со
  сцены, с наблюдения, с тезиса), как разворачивает и чем заканчивает. Строй
  «мысль — пример — вывод» в каждом абзаце выдаёт машину.
- Варианты должны различаться СОДЕРЖАНИЕМ, а не косметикой.`

const voiceSystemNote = voiceSystemBase + `

Пишешь ЗАМЕТКУ — самостоятельный пост в ленте: своя мысль, без обращения к
кому-либо и без ответа на чужой текст.`

const voiceSystemComment = voiceSystemBase + `

Пишешь КОММЕНТАРИЙ в живую ветку. Отвечаешь той реплике, что помечена
«← отвечаем на эту». Обращение «Ник,» в начале НЕ пиши — его подставит сам
инструмент. Комментарий короткий: держись медианы длины из измерений.`

const voiceSystemCommentTop = voiceSystemBase + `

Пишешь КОММЕНТАРИЙ первого уровня — реплику к самой заметке, а не ответ
кому-то из уже написавших. Обращений по нику в начале не пиши вовсе: адресата
здесь нет. Комментарий короткий: держись медианы длины из измерений.`

// voiceSchema — structured outputs Claude API. Ограничений массива (minItems/
// maxItems) здесь быть не должно: API их не поддерживает и отвечает 400
// («For 'array' type, property 'maxItems' is not supported»). Python/TS SDK
// молча вырезают такие ключи, Go отправляет как есть. Число вариантов задаётся
// текстом промпта и проверяется на нашей стороне.
var voiceSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"drafts": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{"type": "string"},
					"idea": map[string]any{"type": "string"},
				},
				"required":             []string{"text", "idea"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"drafts"},
	"additionalProperties": false,
}

type voiceReply struct {
	Drafts []struct {
		Text string `json:"text"`
		Idea string `json:"idea"`
	} `json:"drafts"`
}

// GenerateVoice гоняет цикл «черновики → атрибуция → дифф → повтор» и возвращает
// журнал прогона. Ошибка — только если прогон невозможен (нет профиля, модель не
// ответила); «ни один черновик не принят» — это нормальный результат с вердиктом,
// а не сбой: на машинном тексте атрибуция заведомо слаба.
func (s *Store) GenerateVoice(ctx context.Context, gen JSONGenerator, card *VoiceCard, req VoiceRequest, now time.Time) (*VoiceRun, error) {
	if req.Mode != VoiceModeNote && req.Mode != VoiceModeComment {
		return nil, fmt.Errorf("voice: неизвестный режим %q (note|comment)", req.Mode)
	}
	if req.Mode == VoiceModeComment && req.Thread == nil {
		return nil, fmt.Errorf("voice: для комментария нужен контекст (-reply-to или -note)")
	}
	kind := shapeKind(req.Mode)

	run := &VoiceRun{
		Stamp: newVoiceStamp(card.Genre, now), Identity: card.Identity, Label: card.Label,
		Mode: req.Mode, Genre: card.Genre, Topic: req.Topic, Card: card, Thread: req.Thread,
	}
	run.Stamp.Model = req.Model

	v, err := s.newVoiceScorer(ctx, card.Genre, req.LexWeight, req.ActiveDays, req.MinAuthorNotes)
	if err != nil {
		return nil, err
	}
	// Анкеты берём У КАРТЫ, а не через identityMembers(identity): в solo-режиме
	// карта — одна анкета, и ранг обязан считаться по ней же, иначе полоса меряет
	// лучшую из склеенных (у кластера из 11 анкет медиана ранга вырождается в 1).
	if len(card.Accounts) == 0 {
		return nil, fmt.Errorf("voice: %s не резолвится в анкеты", card.Identity)
	}
	member := make(map[int64]bool, len(card.Accounts))
	accIDs := make([]int64, 0, len(card.Accounts))
	for _, a := range card.Accounts {
		member[a.ID] = true
		accIDs = append(accIDs, a.ID)
	}
	pn, err := s.profileNgrams(ctx, accIDs, card.Genre)
	if err != nil {
		return nil, err
	}
	if run.Band, err = s.BuildVoiceBand(ctx, card.Identity, kind, card.HeldOut(), v, member, pn); err != nil {
		return nil, err
	}

	var ctlMember map[int64]bool
	var ctlIdentity string
	if req.Control != "" {
		if ctlIdentity, ctlMember, err = s.controlBand(ctx, req, v, run); err != nil {
			return nil, err
		}
	}

	samples := make([]string, 0, len(card.Samples))
	for _, sm := range card.Samples {
		samples = append(samples, sm.Text)
	}
	j := &voiceJudge{
		v: v, card: card, req: req, kind: kind, samples: samples,
		member: member, ctlMember: ctlMember, ctlIdentity: ctlIdentity, run: run,
	}
	if err := j.runRounds(ctx, gen); err != nil {
		return nil, err
	}

	if err := s.fillVoiceNames(ctx, run); err != nil {
		return nil, err
	}
	run.Verdict = voiceVerdict(run, req)
	return run, nil
}

// controlBand строит полосу второй личности: если контроль получает те же числа,
// значит атрибутор на машинном тексте людей не различает и вердикт цикла
// отбрасывается целиком.
func (s *Store) controlBand(ctx context.Context, req VoiceRequest, v *voiceScorer, run *VoiceRun) (string, map[int64]bool, error) {
	identity, err := s.canonIdentity(ctx, req.Control)
	if err != nil {
		return "", nil, fmt.Errorf("контроль: %w", err)
	}
	member := s.identityMembers(ctx, identity)
	if member == nil {
		return "", nil, fmt.Errorf("контроль %s не резолвится в анкеты", req.Control)
	}
	p := VoiceCardDefaults()
	p.Genre, p.Samples = run.Genre, 0 // контролю образцы не нужны, только held-out
	card, err := s.BuildVoiceCard(ctx, identity, p, time.Unix(0, 0).UTC())
	if err != nil {
		return "", nil, fmt.Errorf("контроль: %w", err)
	}
	ids := make([]int64, 0, len(member))
	for id := range member {
		ids = append(ids, id)
	}
	pn, err := s.profileNgrams(ctx, ids, run.Genre)
	if err != nil {
		return "", nil, err
	}
	band, err := s.BuildVoiceBand(ctx, identity, shapeKind(run.Mode), card.HeldOut(), v, member, pn)
	if err != nil {
		return "", nil, err
	}
	run.Control = &band
	return identity, member, nil
}

// voiceJudge — всё, что нужно, чтобы оценить черновик: слой профилей, карта,
// образцы и обе личности (цель и контроль). Отдельной структурой, потому что
// иначе у оценщика получается десяток параметров.
type voiceJudge struct {
	v           *voiceScorer
	card        *VoiceCard
	req         VoiceRequest
	kind        string
	samples     []string
	member      map[int64]bool
	ctlMember   map[int64]bool
	ctlIdentity string
	run         *VoiceRun
}

// judge валидирует черновик, собирает то, что появилось бы на сайте, и меряет.
func (j *voiceJudge) judge(text, idea string) VoiceDraft {
	d := VoiceDraft{Text: strings.TrimSpace(text), Idea: idea}
	d.Copy = CopyOverlap(d.Text, j.samples)
	rate, hits := VocabUse(d.Text, j.card.Vocab)
	d.VocabRate = rate
	if why := validateDraft(d.Text, j.card, j.req, j.kind, d.Copy); why != "" {
		d.Rejected = why
		return d
	}
	// Набивка характерных слов — не стилистическая придирка, а обход самой меры:
	// такой черновик поднимает лексический косинус, не воспроизведя манеру.
	// Отбраковываем ДО скоринга, иначе цикл обратной связи учится набивать.
	if why := vocabStuffing(rate, j.card.VocabRate, hits); why != "" {
		d.Rejected = why
		return d
	}
	d.Rendered = renderDraft(d.Text, j.card, j.req)
	d.Score = j.v.score(d.Rendered, j.member, j.card.Identity)
	if d.Score.Rank == 0 {
		d.Rejected = "атрибутор не смог отскорить текст (слишком короткий или нет профиля)"
		return d
	}
	d.Quantile = BandQuantile(j.run.Band, d.Score.Rank)
	if j.ctlMember != nil {
		cs := j.v.score(d.Rendered, j.ctlMember, j.ctlIdentity)
		d.Control = &cs
		if j.run.Control != nil {
			d.ControlQuant = BandQuantile(*j.run.Control, cs.Rank)
		}
	}
	d.Diff = draftDiff(d, j.card, j.kind, j.run.Band)
	return d
}

// runRounds — цикл «запрос → черновики → оценка → дифф → повтор».
func (j *voiceJudge) runRounds(ctx context.Context, gen JSONGenerator) error {
	var feedback string
	for r := 1; r <= maxRounds(j.req.Rounds); r++ {
		prompt := buildVoicePrompt(j.card, j.req, j.kind, feedback)
		reply, err := askDrafts(ctx, gen, j.req, prompt)
		// Сбой ПОЗДНЕГО раунда не должен уносить уже сделанное: прогон дорогой
		// (полоса, слой профилей, предыдущие черновики), и «оборвался раунд 2»
		// — это результат с оговоркой, а не пустота. Сбой первого раунда —
		// по-прежнему ошибка: показывать нечего.
		if err != nil {
			if r == 1 || j.run.Best == nil {
				return fmt.Errorf("раунд %d: %w", r, err)
			}
			j.run.Aborted = fmt.Sprintf("раунд %d не состоялся: %v", r, err)
			return nil
		}

		round := VoiceRound{N: r, Prompt: prompt}
		for _, d := range reply.Drafts {
			round.Drafts = append(round.Drafts, j.judge(d.Text, d.Idea))
		}
		j.run.Rounds = append(j.run.Rounds, round)

		if best := pickBest(round.Drafts); best != nil && (j.run.Best == nil || best.Quantile > j.run.Best.Quantile) {
			j.run.Best = best
		}
		if j.run.Best != nil && j.run.Band.Usable && j.run.Best.Quantile >= acceptOf(j.req) {
			j.run.Accepted = true
			return nil
		}
		feedback = voiceFeedback(round.Drafts, j.card, j.kind)
	}
	return nil
}

// askDrafts — один запрос к модели с разбором и проверкой ответа.
func askDrafts(ctx context.Context, gen JSONGenerator, req VoiceRequest, prompt string) (*voiceReply, error) {
	raw, err := gen.GenerateJSON(ctx, systemFor(req.Mode, req.Thread), prompt, voiceSchema)
	if err != nil {
		return nil, err
	}
	var reply voiceReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("разбор ответа модели: %w", err)
	}
	if len(reply.Drafts) == 0 {
		return nil, fmt.Errorf("модель не вернула ни одного варианта")
	}
	return &reply, nil
}

// vocabStuffing — черновик набит характерными словами против нормы автора?
// Потолок относительный (норма × VocabStuffFactor) плюс абсолютный порог, чтобы
// автор с нормой 0,3 на 100 слов не отбраковывался за одно уместное слово в
// короткой реплике.
func vocabStuffing(draft, author float64, hits int) string {
	if author <= 0 || draft <= vocabStuffFloor || hits < vocabStuffMinHits {
		return ""
	}
	if draft > author*VocabStuffFactor {
		return fmt.Sprintf("набивка характерных слов: %.1f на 100 слов против %.2f у автора "+
			"(потолок ×%.0f)", draft, author, VocabStuffFactor)
	}
	return ""
}

// VocabStuffFactor — во сколько раз черновику позволено превысить норму автора по
// характерным словам. Три — с запасом на короткий текст, где одно слово даёт
// большую долю.
const VocabStuffFactor = 3.0

// vocabStuffFloor — ниже этой доли считать набивкой нечего.
const vocabStuffFloor = 3.0

// vocabStuffMinHits — и меньше трёх попаданий тоже: на реплике в 20 слов одно
// уместное слово даёт 5 на 100, а это не набивка.
const vocabStuffMinHits = 3

// validateDraft — проверки ДО скоринга. Брак не «поправляем», а отбраковываем:
// принять испорченный черновик значит замерить не то.
func validateDraft(text string, card *VoiceCard, req VoiceRequest, kind string, copyShare float64) string {
	if text == "" {
		return "пустой текст"
	}
	sh := shapeOf(card, kind)
	r := []rune(text)
	if sh.Runes.P10 > 0 {
		lo, hi := int(float64(sh.Runes.P10)*0.6), int(float64(sh.Runes.P90)*1.6)
		if len(r) < lo || len(r) > hi {
			return fmt.Sprintf("длина %d рун вне диапазона автора (%d–%d)", len(r), lo, hi)
		}
	}
	if md := markdownHit(text); md != "" {
		return "markdown в тексте (" + md + "), у автора такого нет"
	}
	if sh.EmojiRate == 0 && hasEmoji(r) {
		return "эмодзи, которых у автора нет ни разу"
	}
	if copyShare > maxCopyOf(req) {
		return fmt.Sprintf("пересечение с образцами %.0f%% > %.0f%%", copyShare*100, maxCopyOf(req)*100)
	}
	// Обращение подставляет инструмент — так ник нельзя выдумать.
	if req.Mode == VoiceModeComment && req.Thread != nil && req.Thread.AddresseeNick != "" {
		if p := love.AddressPrefix(text); p != "" && p == strings.ToLower(req.Thread.AddresseeNick) {
			return "обращение «Ник,» в теле — его подставляет инструмент"
		}
	}
	return ""
}

func markdownHit(text string) string {
	for _, m := range []string{"**", "##", "```", "~~"} {
		if strings.Contains(text, m) {
			return m
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "- ") {
			return "список дефисом"
		}
	}
	return ""
}

// renderDraft — то, что появилось бы на сайте. Обращение собирает инструмент, и
// скорится именно эта форма: префикс входит в измеряемый атрибутором сигнал.
func renderDraft(text string, card *VoiceCard, req VoiceRequest) string {
	if req.Mode != VoiceModeComment || req.Thread == nil {
		return text
	}
	if req.Thread.ReplyToID == 0 || req.Thread.AddresseeNick == "" {
		return text
	}
	if card.Comments.AddressPrefix < 0.5 {
		return text
	}
	return req.Thread.AddresseeNick + ", " + text
}

// draftDiff — измеримые расхождения черновика с автором. Только то, что выше
// допуска: короткий и действенный список бьёт длинный и полный.
func draftDiff(d VoiceDraft, card *VoiceCard, kind string, band VoiceBand) []string {
	sh := shapeOf(card, kind)
	got := measureShape([]voiceText{{text: d.Rendered, kind: kind}}, kind, nil)
	var out []string

	if off := quantileOff(len([]rune(d.Rendered)), sh.Runes); off != "" {
		out = append(out, "длина "+off)
	}
	if sh.Sentences.Median > 0 && got.Sentences.Median > 0 {
		if rel := relDiff(float64(got.Sentences.Median), float64(sh.Sentences.Median)); rel > 0.25 {
			out = append(out, fmt.Sprintf("предложений %d, у автора медиана %d",
				got.Sentences.Median, sh.Sentences.Median))
		}
	}
	out = append(out, rhythmDiff(got, sh)...)
	for _, k := range []string{",", ".", "!", "?", "…", "—"} {
		want, have := sh.Punct[k], got.Punct[k]
		if want < 1 && have < 1 {
			continue
		}
		if relDiff(have, want) > 0.4 {
			out = append(out, fmt.Sprintf("«%s» %.0f на 1000 рун, у автора %.0f", k, have, want))
		}
	}
	if sh.ParenRuns["2"] >= 0.2 && got.ParenRuns["2"] == 0 {
		out = append(out, fmt.Sprintf("ни разу «))», а у автора в %s текстов", pct(sh.ParenRuns["2"])))
	}
	if sh.NoFinalPunct >= 0.4 && got.NoFinalPunct == 0 {
		out = append(out, fmt.Sprintf("точка в конце стоит, а автор её не ставит в %s текстов", pct(sh.NoFinalPunct)))
	}
	// Прежняя формулировка была «не использованы характерные слова автора: …» —
	// то есть цикл прямо велел набивать список, а это и есть обход меры
	// (styleZ падал, lexZ рос). Теперь дифф двусторонний и говорит про НОРМУ.
	out = append(out, vocabDiff(d, card)...)
	if alien, total := alienWords(d.Rendered, card, 5); len(alien) > 0 {
		out = append(out, fmt.Sprintf("слов, которых у автора нет ни разу (%d из %d): %s",
			total, countWords(d.Rendered), strings.Join(alien, ", ")))
	}
	if band.Usable && d.Score.Rank > 0 {
		out = append(out, fmt.Sprintf("АТРИБУЦИЯ: автор на месте %d из %d; настоящие тексты автора занимают %d–%d, "+
			"черновик узнаётся лучше %.0f%% из них; первым атрибутор назвал %s",
			d.Score.Rank, d.Score.Of, band.Min, band.Max, d.Quantile*100, topLabel(d.Score)))
	}
	return out
}

// rhythmDiff — расхождения ритма фразы и адресации. Самый действенный пункт
// обратной связи из найденных живыми замерами: у машинного текста верное среднее
// слов в предложении при вдвое меньшем разбросе, и текст от этого мёртвый.
func rhythmDiff(got, sh VoiceShape) []string {
	var out []string
	if sh.SentWordSD > 0 && got.SentWordSD > 0 && got.SentWordSD < sh.SentWordSD*0.7 {
		out = append(out, fmt.Sprintf("РИТМ РОВНЫЙ: разброс длины предложения %.1f против %.1f у автора — "+
			"добавь и рубленых фраз в 1–3 слова, и длинных на 20+", got.SentWordSD, sh.SentWordSD))
	}
	if sh.ShortSents >= 0.1 && got.ShortSents < sh.ShortSents*0.5 {
		out = append(out, fmt.Sprintf("рубленых предложений %s, у автора %s",
			pct(got.ShortSents), pct(sh.ShortSents)))
	}
	if sh.LongSents >= 0.08 && got.LongSents == 0 {
		out = append(out, fmt.Sprintf("ни одного длинного предложения (≥%d слов), у автора %s",
			longSentWords, pct(sh.LongSents)))
	}
	for _, p := range []string{"ты", "я", "вы"} {
		want, have := sh.Person[p], got.Person[p]
		if want >= 0.5 && have < want*0.4 {
			out = append(out, fmt.Sprintf("почти не говорит «%s»: %.1f на 100 слов против %.1f у автора",
				p, have, want))
		}
	}
	if sh.EndsQuestion >= 0.25 && got.EndsQuestion == 0 {
		out = append(out, fmt.Sprintf("не кончается вопросом, а автор так кончает %s текстов",
			pct(sh.EndsQuestion)))
	}
	return out
}

// vocabDiff — расхождение по характерным словам В ОБЕ СТОРОНЫ, считая нормой
// частоту самого автора. Просить «добавь характерных слов» без нормы значит
// заказывать набивку.
func vocabDiff(d VoiceDraft, card *VoiceCard) []string {
	if card.VocabRate <= 0 {
		return nil
	}
	switch {
	case d.VocabRate > card.VocabRate*1.5:
		return []string{fmt.Sprintf("характерных слов %.1f на 100 при норме автора %.2f — "+
			"их СЛИШКОМ много, набивка выдаёт подделку; оставь только те, что легли сами",
			d.VocabRate, card.VocabRate)}
	case d.VocabRate < card.VocabRate*0.5:
		miss := missingVocab(d.Rendered, card, 5)
		s := fmt.Sprintf("характерных слов %.1f на 100 при норме автора %.2f — речь звучит нейтрально",
			d.VocabRate, card.VocabRate)
		if len(miss) > 0 {
			s += "; из его привычных: " + strings.Join(miss, ", ")
		}
		return []string{s}
	}
	return nil
}

func topLabel(sc VoiceScore) string {
	if sc.TopName != "" {
		return fmt.Sprintf("«%s»(%d)", sc.TopName, sc.TopID)
	}
	return fmt.Sprintf("анкету %d", sc.TopID)
}

// missingVocab — характерные слова автора, которых в черновике нет.
func missingVocab(text string, card *VoiceCard, limit int) []string {
	used := map[string]bool{}
	forEachWord(text, func(w []rune) { used[string(w)] = true })
	var out []string
	for _, v := range card.Vocab {
		if v.Collision || used[v.Word] {
			continue
		}
		out = append(out, v.Word)
		if len(out) == limit {
			break
		}
	}
	if len(out) == 0 {
		for _, v := range card.Vocab {
			if used[v.Word] {
				continue
			}
			out = append(out, v.Word)
			if len(out) == limit {
				break
			}
		}
	}
	return out
}

// alienWords — слова черновика, которых у автора нет НИ РАЗУ. Самый действенный
// пункт обратной связи: полный словарь автора уже собран картой.
func alienWords(text string, card *VoiceCard, limit int) ([]string, int) {
	known := card.AuthorWords()
	if len(known) == 0 {
		return nil, 0
	}
	seen := map[string]bool{}
	var all []string
	forEachWord(text, func(w []rune) {
		s := string(w)
		if known[s] > 0 || seen[s] {
			return
		}
		seen[s] = true
		all = append(all, s)
	})
	sort.Slice(all, func(i, j int) bool { return len(all[i]) > len(all[j]) })
	total := len(all)
	if len(all) > limit {
		all = all[:limit]
	}
	return all, total
}

func countWords(text string) int {
	n := 0
	forEachWord(text, func([]rune) { n++ })
	return n
}

// voiceFeedback — блок обратной связи следующего раунда.
func voiceFeedback(drafts []VoiceDraft, card *VoiceCard, kind string) string {
	var b strings.Builder
	b.WriteString("\n=== ЧТО НЕ СОШЛОСЬ В ПРОШЛЫЙ РАЗ ===\n")
	for i, d := range drafts {
		fmt.Fprintf(&b, "ВАРИАНТ %d", i+1)
		if d.Rejected != "" {
			fmt.Fprintf(&b, " — отклонён: %s\n", d.Rejected)
			continue
		}
		b.WriteString(":\n")
		for _, line := range d.Diff {
			fmt.Fprintf(&b, "  - %s\n", line)
		}
	}
	b.WriteString("Исправь ровно эти расхождения, содержание оставь новым.\n")
	return b.String()
}

// --- промпт ---

func buildVoicePrompt(card *VoiceCard, req VoiceRequest, kind, feedback string) string {
	var b strings.Builder
	_ = WriteVoiceBrief(&b, card, kind)

	if req.Mode == VoiceModeNote {
		b.WriteString("\n=== ЗАДАНИЕ ===\n")
		if strings.TrimSpace(req.Topic) != "" {
			fmt.Fprintf(&b, "Напиши ЗАМЕТКУ на тему: %s\n", strings.TrimSpace(req.Topic))
		} else {
			b.WriteString("Напиши ЗАМЕТКУ на любую из тем, которыми автор обычно занят.\n")
		}
	} else {
		writeThreadBlock(&b, req.Thread)
	}
	fmt.Fprintf(&b, "Вариантов: %d.\n", draftsOf(req))

	writeSamplesBlock(&b, card.Samples)
	if feedback != "" {
		b.WriteString(feedback)
	}
	return b.String()
}

// writeSamplesBlock — образцы автора. У реплики образец подаётся ПАРОЙ «на что
// отвечает → что ответил»: короткая реплика в отрыве от чужих слов не показывает
// манеру, а именно в цеплянии за чужие слова она и состоит.
func writeSamplesBlock(b *strings.Builder, samples []VoiceSample) {
	if len(samples) == 0 {
		return
	}
	b.WriteString("\n=== ОБРАЗЦЫ АВТОРА (манера — да, содержание и обороты — нет) ===\n")
	for i, sm := range samples {
		fmt.Fprintf(b, "--- образец %d (%d рун) ---\n", i+1, sm.Runes)
		if sm.Context != "" {
			fmt.Fprintf(b, "[%s]: %s\n→ ", sm.ContextAuthor, sm.Context)
		}
		fmt.Fprintf(b, "%s\n", sm.Text)
	}
}

func writeThreadBlock(b *strings.Builder, th *VoiceThread) {
	// Два случая: ответ в ветке (ReplyToID != 0) и комментарий первого уровня к
	// самой заметке. У второго нет ни адресата, ни помеченной реплики, а корневые
	// реплики других идут как «уже сказанное» — иначе модель пишет то, что в
	// треде уже прозвучало.
	if th.ReplyToID == 0 {
		b.WriteString("\n=== ЗАМЕТКА, К КОТОРОЙ ПИШЕШЬ ===\n")
		fmt.Fprintf(b, "Заметка «%s»:\n%s\n", th.NoteAuthor, th.NoteText)
		if len(th.Branch) > 0 {
			b.WriteString("\nУже сказанное в треде (не повторять, но можно спорить):\n")
			for _, m := range th.Branch {
				mark := ""
				if m.Self {
					mark = "   ← это реплика самого автора, чью манеру воспроизводим"
				}
				fmt.Fprintf(b, "[%s] %s%s\n", m.Author, m.Text, mark)
			}
		}
		b.WriteString("\n=== ЗАДАНИЕ ===\nНапиши КОММЕНТАРИЙ к самой заметке — " +
			"первого уровня, не ответ конкретной реплике. Обращения по нику не писать.\n")
		return
	}

	b.WriteString("\n=== ВЕТКА, В КОТОРУЮ ПИШЕШЬ ===\n")
	fmt.Fprintf(b, "Заметка «%s»:\n%s\n\n", th.NoteAuthor, th.NoteText)
	for _, m := range th.Branch {
		mark := ""
		if m.Target {
			mark = "   ← отвечаем на эту"
		} else if m.Self {
			mark = "   ← это реплика самого автора, чью манеру воспроизводим"
		}
		fmt.Fprintf(b, "[%s] %s%s\n", m.Author, m.Text, mark)
	}
	fmt.Fprintf(b, "\n=== ЗАДАНИЕ ===\nНапиши КОММЕНТАРИЙ — ответ помеченной реплике.\n")
	if th.AddresseeNick != "" {
		fmt.Fprintf(b, "Адресат — %s; обращение подставит инструмент, сам его не пиши.\n", th.AddresseeNick)
	}
}

// --- контекст ветки ---

// VoiceThread — куда именно уходит реплика. parent_id для этого НЕ используется:
// он указывает на корень ВЕТКИ, а не на адресата (совпадает лишь в 34,8 %, см.
// addressee.go), поэтому адресат берётся у той реплики, на которую отвечаем.
type VoiceThread struct {
	NoteID     int64  `json:"note_id"`
	NoteAuthor string `json:"note_author"`
	NoteText   string `json:"note_text"`
	RootID     int64  `json:"root_id"`

	ReplyToID     int64  `json:"reply_to_id"`
	AddresseeID   int64  `json:"addressee_id"`
	AddresseeNick string `json:"addressee_nick"`
	SelfInBranch  bool   `json:"self_in_branch"`

	Branch []VoiceThreadMsg `json:"branch"`
}

type VoiceThreadMsg struct {
	ID       int64  `json:"id"`
	AuthorID int64  `json:"author_id"`
	Author   string `json:"author"`
	Text     string `json:"text"`
	At       string `json:"at,omitempty"`
	Target   bool   `json:"target,omitempty"`
	Self     bool   `json:"self,omitempty"`
}

// LoadVoiceNoteThread собирает контекст комментария ПЕРВОГО уровня — к самой
// заметке, а не в ветку. Адресата у такого комментария нет, поэтому обращение по
// нику не подставляется; корневые реплики других грузятся как «уже сказанное».
func (s *Store) LoadVoiceNoteThread(ctx context.Context, noteID int64, selfIDs []int64, limit int) (*VoiceThread, error) {
	note, ok, err := s.LoadNote(ctx, noteID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("voice: заметка %d не найдена в архиве (заведите её grab %d)", noteID, noteID)
	}
	th := &VoiceThread{NoteID: noteID, NoteText: excerpt(note.Text, 1500), NoteAuthor: "аноним"}
	if note.Author != nil {
		th.NoteAuthor = note.Author.Name
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.author_id, COALESCE(u.name,''), c.text, COALESCE(c.published_at,'')
		FROM comments c LEFT JOIN users u ON u.id = c.author_id
		WHERE c.note_id = ? AND c.parent_id = 0
		ORDER BY c.id LIMIT ?`, noteID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	self := map[int64]bool{}
	for _, id := range selfIDs {
		self[id] = true
	}
	for rows.Next() {
		var m VoiceThreadMsg
		var at string
		if err := rows.Scan(&m.ID, &m.AuthorID, &m.Author, &m.Text, &at); err != nil {
			return nil, err
		}
		m.At = shortDateStr(at)
		m.Text = excerpt(m.Text, 400)
		m.Self = self[m.AuthorID]
		if m.Self {
			th.SelfInBranch = true
		}
		th.Branch = append(th.Branch, m)
	}
	return th, rows.Err()
}

// LoadVoiceThread собирает контекст ответа на комментарий replyToID.
func (s *Store) LoadVoiceThread(ctx context.Context, replyToID int64, selfIDs []int64, limit int) (*VoiceThread, error) {
	var noteID, parentID, authorID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT note_id, parent_id, author_id FROM comments WHERE id = ?`, replyToID).
		Scan(&noteID, &parentID, &authorID)
	if err != nil {
		return nil, fmt.Errorf("voice: комментарий %d не найден в архиве: %w", replyToID, err)
	}
	th := &VoiceThread{NoteID: noteID, ReplyToID: replyToID, AddresseeID: authorID, RootID: parentID}
	if th.RootID == 0 {
		th.RootID = replyToID
	}

	note, ok, err := s.LoadNote(ctx, noteID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("voice: заметка %d не найдена в архиве", noteID)
	}
	th.NoteText = excerpt(note.Text, 1500)
	th.NoteAuthor = "аноним"
	if note.Author != nil {
		th.NoteAuthor = note.Author.Name
	}

	// Ник адресата — ТЕКУЩИЙ (users.name): пишем «сейчас». Для реконструкции
	// задним числом понадобился бы nick_history — здесь он не нужен.
	names, err := s.namesByIDs(ctx, []int64{authorID})
	if err != nil {
		return nil, err
	}
	th.AddresseeNick = names[authorID]

	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.author_id, COALESCE(u.name,''), c.text, COALESCE(c.published_at,'')
		FROM comments c LEFT JOIN users u ON u.id = c.author_id
		WHERE c.note_id = ? AND (c.id = ? OR c.parent_id = ?)
		ORDER BY c.id LIMIT ?`, noteID, th.RootID, th.RootID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	self := map[int64]bool{}
	for _, id := range selfIDs {
		self[id] = true
	}
	for rows.Next() {
		var m VoiceThreadMsg
		var at string
		if err := rows.Scan(&m.ID, &m.AuthorID, &m.Author, &m.Text, &at); err != nil {
			return nil, err
		}
		m.At = shortDateStr(at)
		m.Text = excerpt(m.Text, 400)
		m.Target = m.ID == replyToID
		m.Self = self[m.AuthorID]
		if m.Self {
			th.SelfInBranch = true
		}
		th.Branch = append(th.Branch, m)
	}
	return th, rows.Err()
}

// --- утилиты прогона ---

func shapeKind(mode string) string {
	if mode == VoiceModeComment {
		return "comments"
	}
	return "notes"
}

func shapeOf(card *VoiceCard, kind string) VoiceShape {
	if kind == "comments" {
		return card.Comments
	}
	return card.Notes
}

// systemFor — системный промпт под режим. У комментария их два: ответ в ветке и
// комментарий первого уровня к заметке (там нет ни адресата, ни помеченной
// реплики, и правило про обращение звучит наоборот).
func systemFor(mode string, th *VoiceThread) string {
	if mode != VoiceModeComment {
		return voiceSystemNote
	}
	if th != nil && th.ReplyToID == 0 {
		return voiceSystemCommentTop
	}
	return voiceSystemComment
}

func draftsOf(req VoiceRequest) int {
	if req.Drafts <= 0 {
		return 3
	}
	return req.Drafts
}

// maxRounds — потолок раундов. Жёстко ограничен: раунды взбираются по хешированной
// метрике, и на длинной дистанции модель начинает набивать признаки вместо манеры.
func maxRounds(n int) int {
	if n <= 0 {
		return 2
	}
	if n > 3 {
		return 3
	}
	return n
}

func acceptOf(req VoiceRequest) float64 {
	if req.Accept <= 0 {
		return AcceptQuantile
	}
	return req.Accept
}

func maxCopyOf(req VoiceRequest) float64 {
	if req.MaxCopy <= 0 {
		return 0.30
	}
	return req.MaxCopy
}

func pickBest(drafts []VoiceDraft) *VoiceDraft {
	var best *VoiceDraft
	for i := range drafts {
		d := &drafts[i]
		if d.Rejected != "" || d.Score.Rank == 0 {
			continue
		}
		if best == nil || d.Quantile > best.Quantile ||
			(d.Quantile == best.Quantile && d.Score.Rank < best.Score.Rank) {
			best = d
		}
	}
	return best
}

// fillVoiceNames проставляет имена анкет во всех скорах одним запросом.
func (s *Store) fillVoiceNames(ctx context.Context, run *VoiceRun) error {
	need := map[int64]bool{}
	visit := func(sc *VoiceScore) {
		if sc == nil {
			return
		}
		need[sc.TopID], need[sc.BestID] = true, true
	}
	for i := range run.Rounds {
		for j := range run.Rounds[i].Drafts {
			visit(&run.Rounds[i].Drafts[j].Score)
			visit(run.Rounds[i].Drafts[j].Control)
		}
	}
	ids := make([]int64, 0, len(need))
	for id := range need {
		if id != 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	names, err := s.namesByIDs(ctx, ids)
	if err != nil {
		return err
	}
	set := func(sc *VoiceScore) {
		if sc == nil {
			return
		}
		sc.TopName, sc.BestName = names[sc.TopID], names[sc.BestID]
	}
	for i := range run.Rounds {
		for j := range run.Rounds[i].Drafts {
			set(&run.Rounds[i].Drafts[j].Score)
			set(run.Rounds[i].Drafts[j].Control)
		}
	}
	// run.Best указывает в срез раунда — переставим на обновлённую копию.
	if run.Best != nil {
		for i := range run.Rounds {
			for j := range run.Rounds[i].Drafts {
				if run.Rounds[i].Drafts[j].Text == run.Best.Text {
					run.Best = &run.Rounds[i].Drafts[j]
					return nil
				}
			}
		}
	}
	return nil
}

// voiceVerdict — читаемый итог. Отрицательный результат формулируется прямо: на
// машинном тексте атрибуция может не работать вовсе, и это тоже результат.
func voiceVerdict(run *VoiceRun, req VoiceRequest) string {
	if run.Aborted != "" {
		return voiceVerdictBody(run, req) + " [цикл неполон: " + run.Aborted + "]"
	}
	return voiceVerdictBody(run, req)
}

func voiceVerdictBody(run *VoiceRun, req VoiceRequest) string {
	switch {
	case !run.Band.Usable:
		return "ПОЛОСА НЕПРИГОДНА: " + run.Band.Why + " — ранги черновиков не с чем сравнивать"
	case run.Best == nil:
		return "НЕ ПРИНЯТ: ни один черновик не прошёл валидацию или скоринг"
	case run.Accepted:
		v := fmt.Sprintf("ПРИНЯТ: узнаётся лучше %.0f%% реальных текстов автора (порог %.0f%%)",
			run.Best.Quantile*100, acceptOf(req)*100)
		// Приёмку мог вытянуть один лексический сигнал: модель, получив список
		// характерных слов, набивает их в текст и поднимает tf-idf-косинус, не
		// воспроизведя манеру. Отрицательный StyleZ при положительном LexZ —
		// ровно этот случай, и молчать о нём нельзя: вердикт уходит в шапку
		// текстового артефакта.
		if run.Best.Score.StyleZ < 0 && run.Best.Score.LexZ > 0 {
			v += fmt.Sprintf("; НО принят лексикой, а не манерой (styleZ %.2f < 0 при lexZ %.2f) — "+
				"похоже на набивку характерных слов", run.Best.Score.StyleZ, run.Best.Score.LexZ)
		}
		return v
	default:
		v := fmt.Sprintf("НЕ ПРИНЯТ: лучший черновик узнаётся лучше %.0f%% реальных текстов, порог %.0f%%",
			run.Best.Quantile*100, acceptOf(req)*100)
		// У очень узнаваемого автора вся полоса стоит на первых местах, и «лучше
		// 25% реальных» требует ранга 1 — недостижимо для машинного текста.
		// Молчать об этом нельзя: попадание ВНУТРЬ диапазона полосы — сильный
		// результат, а вердикт по квантилю читается как провал.
		if r := run.Best.Score.Rank; r >= run.Band.Min && r <= run.Band.Max {
			v += fmt.Sprintf("; НО ранг %d лежит ВНУТРИ диапазона настоящих текстов автора (%d–%d): "+
				"полоса слишком тесная, чтобы квантиль что-то значил",
				r, run.Band.Min, run.Band.Max)
		}
		return v
	}
}

func relDiff(got, want float64) float64 {
	if want == 0 {
		if got == 0 {
			return 0
		}
		return 1
	}
	d := (got - want) / want
	if d < 0 {
		return -d
	}
	return d
}

// quantileOff — попала ли длина в межквартильный коридор автора.
func quantileOff(n int, d Dist) string {
	if d.N == 0 {
		return ""
	}
	switch {
	case n < d.P25:
		return fmt.Sprintf("%d рун — короче обычного (у автора %d–%d)", n, d.P25, d.P75)
	case n > d.P75:
		return fmt.Sprintf("%d рун — длиннее обычного (у автора %d–%d)", n, d.P25, d.P75)
	}
	return ""
}
