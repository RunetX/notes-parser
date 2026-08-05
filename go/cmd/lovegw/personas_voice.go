package main

// «Голос» здесь — авторская манера письма, не голосовое сообщение (ср.
// tgx.VoiceHandler — то про ASR).
//
// personas voice: инструмент ИССЛЕДОВАТЕЛЬСКИЙ. Он строит читаемую стилевую
// карту участника и (в режимах note/comment) генерирует по ней текст, который
// тут же прогоняется через собственную атрибуцию архива. Пути публикации здесь
// нет и быть не должно: сгенерированное — имитация письма живого частного
// человека. Отсутствие публикации закреплено тестом на список импортов
// (personas_voice_test.go), а не обещанием в этом комментарии.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lovegw/internal/archive"
	"lovegw/internal/config"
)

type voiceOpts struct {
	cfgPath  string
	outDir   string
	genre    string
	genreSet bool
	recent   int
	samples  int
	band     int
	seed     int64
	topWords int

	topic   string
	replyTo int64
	noteID  int64
	solo    bool
	drafts  int
	rounds  int
	accept  float64
	maxCopy float64
	control string

	lexWeight      float64
	activeDays     int
	minAuthorNotes int
}

// personasVoice — под-действия card|note|comment. Первый позиционный аргумент —
// под-действие, второй — личность (p<id>|u<id>|user_id).
func personasVoice(ctx context.Context, ar *archive.Store, args []string, o voiceOpts) error {
	if len(args) < 2 {
		return fmt.Errorf("personas voice: нужно под-действие и личность: card|note|comment <p<id>|u<id>|user_id>")
	}
	sub, token := args[0], args[1]
	switch sub {
	case "card":
		return voiceCard(ctx, ar, token, o)
	case "note":
		return voiceGenerate(ctx, ar, token, archive.VoiceModeNote, o)
	case "comment":
		return voiceGenerate(ctx, ar, token, archive.VoiceModeComment, o)
	default:
		return fmt.Errorf("personas voice: неизвестное под-действие %q (card|note|comment)", sub)
	}
}

// voiceThread — контекст для комментария: ответ в ветке (-reply-to) или реплика
// первого уровня к заметке (-note). Анкеты личности передаются как «свои», чтобы
// её собственные реплики в этом же треде попали в промпт помеченными.
func voiceThread(ctx context.Context, ar *archive.Store, card *archive.VoiceCard, o voiceOpts) (*archive.VoiceThread, error) {
	ids := make([]int64, 0, len(card.Accounts))
	for _, a := range card.Accounts {
		ids = append(ids, a.ID)
	}
	if o.replyTo != 0 {
		return ar.LoadVoiceThread(ctx, o.replyTo, ids, voiceBranchLimit)
	}
	return ar.LoadVoiceNoteThread(ctx, o.noteID, ids, voiceBranchLimit)
}

// voiceGenerate — генерация с замкнутым циклом. Конфиг нужен только здесь: карта
// строится и без ключа к модели.
func voiceGenerate(ctx context.Context, ar *archive.Store, token, mode string, o voiceOpts) error {
	// Дешёвые проверки аргументов — до конфига и ключа: иначе на забытый
	// -reply-to инструмент жалуется на API-ключ, и причина не видна.
	if mode == archive.VoiceModeComment && o.replyTo == 0 && o.noteID == 0 {
		return fmt.Errorf("personas voice comment: нужен -reply-to <id комментария, которому отвечаем> " +
			"или -note <id заметки> для комментария первого уровня")
	}
	cfg, err := config.Load(o.cfgPath)
	if err != nil {
		return err
	}
	client, err := llmClient(cfg)
	if err != nil {
		return err
	}
	p := voiceParams(o, mode)
	card, err := ar.BuildVoiceCard(ctx, token, p, time.Now())
	if err != nil {
		return err
	}
	req := archive.VoiceRequest{
		Mode: mode, Topic: o.topic, Drafts: o.drafts, Rounds: o.rounds,
		Accept: o.accept, MaxCopy: o.maxCopy, LexWeight: o.lexWeight,
		ActiveDays: o.activeDays, MinAuthorNotes: o.minAuthorNotes,
		Control: o.control, Model: client.Model(),
	}
	if mode == archive.VoiceModeComment {
		if req.Thread, err = voiceThread(ctx, ar, card, o); err != nil {
			return err
		}
	}

	fmt.Fprintf(os.Stderr, "модель %s, черновиков %d, раундов до %d — запрос может занять минуты…\n",
		client.Model(), req.Drafts, req.Rounds)
	run, err := ar.GenerateVoice(ctx, client, card, req, time.Now())
	if err != nil {
		return err
	}

	dir := filepath.Join(o.outDir, "voice")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	stamp := time.Now().Format("20060102-150405")
	base := filepath.Join(dir, fmt.Sprintf("%s-%s-%s", run.Identity, mode, stamp))
	if err := writeJSONFile(base+".json", run); err != nil {
		return err
	}
	if run.Best != nil {
		if err := os.WriteFile(base+".txt", []byte(voiceDraftFile(run)), 0o644); err != nil {
			return err
		}
	}
	printVoiceRun(os.Stderr, run, req)
	fmt.Fprintf(os.Stderr, "\nжурнал:   %s.json\n", base)
	if run.Best != nil {
		fmt.Fprintf(os.Stderr, "черновик: %s.txt\n", base)
	}
	return nil
}

// voiceBranchLimit — сколько реплик ветки класть в контекст.
const voiceBranchLimit = 60

// voiceDraftFile — текстовый артефакт: предупреждение идёт ПЕРЕД текстом, чтобы
// его нельзя было не заметить, скопировав файл целиком.
func voiceDraftFile(run *archive.VoiceRun) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", archive.VoiceWarning)
	fmt.Fprintf(&b, "# Имитация манеры %s «%s», модель %s, %s\n",
		run.Identity, run.Label, run.Stamp.Model, run.Stamp.CreatedAt)
	fmt.Fprintf(&b, "# %s\n\n", run.Verdict)
	text := run.Best.Rendered
	if text == "" {
		text = run.Best.Text
	}
	b.WriteString(text)
	b.WriteString("\n")
	return b.String()
}

func printVoiceRun(w *os.File, run *archive.VoiceRun, req archive.VoiceRequest) {
	fmt.Fprintf(w, "\nvoice %s %s «%s» — эталон %s\n", run.Mode, run.Identity, run.Label, run.Genre)
	fmt.Fprintf(w, "  образцов %d (%s), характерных слов у автора %.2f на 100\n",
		len(run.Card.Samples), voiceKind(run.Mode), run.Card.VocabRate)
	printBand(w, "полоса цели", run.Band)
	if run.Control != nil {
		printBand(w, "полоса контроля "+run.Control.Identity, *run.Control)
	} else {
		fmt.Fprintln(w, "  ⚠ прогон без контрольной группы (-control): вердикт не интерпретируется —")
		fmt.Fprintln(w, "    неизвестно, различает ли атрибутор на машинном тексте людей вообще")
	}

	for _, r := range run.Rounds {
		fmt.Fprintf(w, "\nраунд %d\n", r.N)
		for i, d := range r.Drafts {
			printDraft(w, i+1, d)
		}
	}
	fmt.Fprintf(w, "\nВЕРДИКТ: %s\n", run.Verdict)
	if run.Control != nil && run.Best != nil && run.Best.ControlQuant >= run.Best.Quantile {
		fmt.Fprintln(w, "  ⚠ контроль не ниже цели — на машинном тексте атрибутор людей не различает,")
		fmt.Fprintln(w, "    вердикт цикла следует отбросить целиком")
	}
	fmt.Fprintf(w, "  %s\n", archive.VoiceWarning)
}

func printBand(w *os.File, title string, b archive.VoiceBand) {
	if !b.Usable {
		fmt.Fprintf(w, "  %s: НЕПРИГОДНА — %s\n", title, b.Why)
		return
	}
	fmt.Fprintf(w, "  %s: %d реальных текстов → ранги %d / %d / %d / %d / %d из %d\n",
		title, b.N, b.Min, b.P25, b.Median, b.P75, b.Max, b.Of)
	fmt.Fprintf(w, "    контаминация %.1f%% (столько профиля даёт один текст полосы; полоса оптимистична)\n",
		b.Contamination*100)
	if b.ShortTexts > 0 {
		fmt.Fprintf(w, "    из них %d короче порога объёма — их ранг шумит\n", b.ShortTexts)
	}
}

func printDraft(w *os.File, n int, d archive.VoiceDraft) {
	if d.Rejected != "" {
		fmt.Fprintf(w, "  #%d  отклонён: %s\n", n, d.Rejected)
		return
	}
	fmt.Fprintf(w, "  #%d  %d рун, 3-грамм %d | ранг %d из %d (лучше %.0f%% реальных) | топ-1 «%s»(%d)%s\n",
		n, d.Score.Runes, d.Score.Ngrams, d.Score.Rank, d.Score.Of, d.Quantile*100,
		d.Score.TopName, d.Score.TopID, selfMark(d.Score.Self))
	fmt.Fprintf(w, "      styleZ %.2f | lexZ %.2f | копирование %.0f%% | характерных слов %.1f на 100\n",
		d.Score.StyleZ, d.Score.LexZ, d.Copy*100, d.VocabRate)
	if d.Control != nil {
		// Квантили двух личностей несопоставимы напрямую: у одного автора его
		// собственные тексты узнаются медианно на 2-м месте, у другого на 400-м,
		// и одна и та же доля означает для них разное. Прямое сравнение рангов
		// честнее — если контроль ранжируется выше цели, черновик похож на него.
		mark := ""
		if d.Control.Rank > 0 && d.Control.Rank < d.Score.Rank {
			mark = "  ⚠ контроль выше цели — черновик ближе к нему"
		}
		fmt.Fprintf(w, "      контроль: ранг %d (лучше %.0f%% его реальных)%s\n",
			d.Control.Rank, d.ControlQuant*100, mark)
	}
}

func selfMark(self bool) string {
	if self {
		return " — узнан ✓"
	}
	return ""
}

// voiceGenre — жанр замера и эталона. Заметку атрибутируем note-эталоном
// (register-matched), комментарий — all: comments-слоя в архиве пока нет.
func voiceGenre(o voiceOpts, sub string) string {
	if o.genreSet {
		return o.genre
	}
	if sub == "comment" {
		return archive.GenreAll
	}
	return archive.GenreNotes
}

// voiceKind — РОД текста, который пишем. Отдельно от жанра эталона: у режима
// комментария эталон `all`, но учиться манере он обязан на комментариях.
func voiceKind(sub string) string {
	switch sub {
	case "note":
		return "notes"
	case "comment":
		return "comments"
	default:
		return "" // card — как раньше, по жанру
	}
}

func voiceParams(o voiceOpts, sub string) archive.VoiceCardParams {
	p := archive.VoiceCardDefaults()
	p.Genre = voiceGenre(o, sub)
	p.Kind = voiceKind(sub)
	p.Solo = o.solo
	if o.recent >= 0 {
		p.Recent = o.recent
	}
	// Флаг не задан — число образцов считает карта по медиане корпуса: шести
	// примеров на коротких репликах не хватает, чтобы манера вообще проявилась.
	p.Samples = -1
	if o.samples >= 0 {
		p.Samples = o.samples
	}
	if o.band >= 0 {
		p.Band = o.band
	}
	if o.topWords > 0 {
		p.TopWords = o.topWords
	}
	p.Seed = o.seed
	return p
}

// voiceCard строит стилевую карту и кладёт её в <out>/voice/. LLM и конфиг здесь
// не нужны: карта — самостоятельный исследовательский артефакт.
func voiceCard(ctx context.Context, ar *archive.Store, token string, o voiceOpts) error {
	p := voiceParams(o, "card")
	card, err := ar.BuildVoiceCard(ctx, token, p, time.Now())
	if err != nil {
		return err
	}

	dir := filepath.Join(o.outDir, "voice")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, card.Identity+"-card.json")
	if err := writeJSONFile(path, card); err != nil {
		return err
	}

	printVoiceCard(os.Stderr, card, p)
	fmt.Fprintf(os.Stderr, "\nкарта: %s\n", path)
	return nil
}

func printVoiceCard(w *os.File, card *archive.VoiceCard, p archive.VoiceCardParams) {
	name := card.Label
	if name == "" && len(card.Accounts) > 0 {
		name = card.Accounts[0].Name
	}
	fmt.Fprintf(w, "voice card %s «%s» (анкет %d)\n", card.Identity, name, len(card.Accounts))
	fmt.Fprintf(w, "  корпус: заметок %d (замер %d), комментариев %d (замер %d)\n",
		card.Notes.TotalHave, card.Notes.Texts, card.Comments.TotalHave, card.Comments.Texts)

	profiles := make([]string, 0, len(card.Accounts))
	for _, a := range card.Accounts {
		if a.Ngrams == 0 {
			profiles = append(profiles, fmt.Sprintf("%s(%d): профиля жанра нет", a.Name, a.ID))
			continue
		}
		profiles = append(profiles, fmt.Sprintf("%s(%d): %d 3-грамм", a.Name, a.ID, a.Ngrams))
	}
	fmt.Fprintf(w, "  эталон %s — %s\n", card.Genre, strings.Join(profiles, "; "))

	fmt.Fprintln(w)
	_ = archive.WriteVoiceBrief(w, card, cardKind(card))
	fmt.Fprintln(w)

	if len(card.Rhythm.Peak) > 0 {
		fmt.Fprintf(w, "Часы активности (%s, в генерацию НЕ идёт): %s\n",
			card.Rhythm.TZ, fmtHours(card.Rhythm.Peak))
	}
	fmt.Fprintf(w, "Образцов отобрано: %d; отложено под эталонную полосу: %d (seed %d)\n",
		len(card.Samples), len(card.HeldIDs), card.Seed)
	if p.Samples == 0 {
		fmt.Fprintln(w, "  -samples 0: дословные тексты автора никуда не отправляются")
	}
	if len(card.Vocab) > 0 {
		fmt.Fprintf(w, "  характерные слова: вес взят у корзины хэша, а не у слова (%s делят корзину\n",
			pctOf(collisionShare(card)))
		fmt.Fprintln(w, "    с другим словом автора). Вес корзины — нижняя оценка редкости: частое слово")
		fmt.Fprintln(w, "    в корзине тянет его вниз, поэтому верх списка надёжен, а низ ничего не значит.")
	}
	if card.Notes.Texts > 0 && card.Notes.Texts < card.Notes.TotalHave {
		fmt.Fprintf(w, "  ⚠ замер урезан до %d последних заметок (-recent)\n", card.Notes.Texts)
	}
	fmt.Fprintf(w, "\n%s\n", archive.VoiceWarning)
}

// cardKind — какой жанр показывать в брифе: у заметочного эталона — заметки.
func cardKind(card *archive.VoiceCard) string {
	if card.Genre == archive.GenreNotes || card.Comments.Texts == 0 {
		return "notes"
	}
	if card.Notes.Texts == 0 {
		return "comments"
	}
	return "notes"
}

// collisionShare — доля показанных слов, делящих корзину с другим словом автора.
func collisionShare(card *archive.VoiceCard) float64 {
	if len(card.Vocab) == 0 {
		return 0
	}
	n := 0
	for _, v := range card.Vocab {
		if v.Collision {
			n++
		}
	}
	return float64(n) / float64(len(card.Vocab))
}

func pctOf(x float64) string { return fmt.Sprintf("%.0f%%", x*100) }

func fmtHours(hours []int) string {
	parts := make([]string, 0, len(hours))
	for _, h := range hours {
		parts = append(parts, fmt.Sprintf("%02d", h))
	}
	return strings.Join(parts, " ")
}
