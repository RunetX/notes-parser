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
- Варианты должны различаться СОДЕРЖАНИЕМ, а не косметикой.`

const voiceSystemNote = voiceSystemBase + `

Пишешь ЗАМЕТКУ — самостоятельный пост в ленте: своя мысль, без обращения к
кому-либо и без ответа на чужой текст.`

const voiceSystemComment = voiceSystemBase + `

Пишешь КОММЕНТАРИЙ в живую ветку. Отвечаешь той реплике, что помечена
«← отвечаем на эту». Обращение «Ник,» в начале НЕ пиши — его подставит сам
инструмент. Комментарий короткий: держись медианы длины из измерений.`

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
		return nil, fmt.Errorf("voice: для комментария нужен контекст ветки (-reply-to)")
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
	member := s.identityMembers(ctx, card.Identity)
	if member == nil {
		return nil, fmt.Errorf("voice: %s не резолвится в анкеты", card.Identity)
	}
	accIDs := make([]int64, 0, len(member))
	for id := range member {
		accIDs = append(accIDs, id)
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
	if why := validateDraft(d.Text, j.card, j.req, j.kind, d.Copy); why != "" {
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
		raw, err := gen.GenerateJSON(ctx, systemFor(j.req.Mode), prompt, voiceSchema)
		if err != nil {
			return fmt.Errorf("раунд %d: %w", r, err)
		}
		var reply voiceReply
		if err := json.Unmarshal(raw, &reply); err != nil {
			return fmt.Errorf("раунд %d: разбор ответа модели: %w", r, err)
		}
		if len(reply.Drafts) == 0 {
			return fmt.Errorf("раунд %d: модель не вернула ни одного варианта", r)
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
	if miss := missingVocab(d.Rendered, card, 5); len(miss) > 0 {
		out = append(out, "не использованы характерные слова автора: "+strings.Join(miss, ", "))
	}
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

	if len(card.Samples) > 0 {
		b.WriteString("\n=== ОБРАЗЦЫ АВТОРА (манера — да, содержание и обороты — нет) ===\n")
		for i, sm := range card.Samples {
			fmt.Fprintf(&b, "--- образец %d (%d рун) ---\n%s\n", i+1, sm.Runes, sm.Text)
		}
	}
	if feedback != "" {
		b.WriteString(feedback)
	}
	return b.String()
}

func writeThreadBlock(b *strings.Builder, th *VoiceThread) {
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

func systemFor(mode string) string {
	if mode == VoiceModeComment {
		return voiceSystemComment
	}
	return voiceSystemNote
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
		return fmt.Sprintf("НЕ ПРИНЯТ: лучший черновик узнаётся лучше %.0f%% реальных текстов, порог %.0f%%",
			run.Best.Quantile*100, acceptOf(req)*100)
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
