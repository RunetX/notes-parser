package archive

// «Голос» здесь — авторская манера письма, не голосовое сообщение (ср.
// tgx.VoiceHandler — то про ASR).
//
// Стилевая карта: читаемый портрет манеры автора, собранный ИЗМЕРЕНИЯМИ по
// сырому тексту. Нужна потому, что style_profiles/lexis_profiles — хешированные
// необратимые векторы: по ним нельзя ни сказать, как звучит человек, ни
// проверить профиль глазами. Карта детерминирована: та же БД и тот же seed дают
// ровно ту же карту, включая выбор образцов.

import (
	"context"
	"fmt"
	"hash/fnv"
	"html"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"lovegw/internal/love"
)

// siteTZ — время сайта (Новосибирск): published_at хранится в UTC, а ритм
// человека читается только в его поясе. Отдельной константой, а не импортом
// digest: пакету архива нечего знать про дайджест.
const siteTZ = "Asia/Novosibirsk"

// VoiceWarning — марка машинного артефакта. Инструмент имитирует письмо живого
// частного человека; артефакт исследовательский и публикации не подлежит.
const VoiceWarning = "Машинная имитация письма реального частного человека. " +
	"Исследовательский артефакт: не публиковать, не отправлять в мессенджеры, не выкладывать на сайт."

// VoiceStamp ставится КОНСТРУКТОРОМ, а не вызывающим: пути, порождающего
// артефакт без марки, в пакете нет.
type VoiceStamp struct {
	Tool         string `json:"tool"`
	Machine      bool   `json:"machine_generated"`
	DoNotPublish bool   `json:"do_not_publish"`
	Genre        string `json:"genre"`
	Model        string `json:"model,omitempty"`
	CreatedAt    string `json:"created_at"`
	Warning      string `json:"warning"`
}

func newVoiceStamp(genre string, now time.Time) VoiceStamp {
	return VoiceStamp{
		Tool: "lovegw personas voice", Machine: true, DoNotPublish: true,
		Genre: genre, CreatedAt: fmtTime(now), Warning: VoiceWarning,
	}
}

// Dist — квантильная сводка. Медиана и края важнее среднего: распределения длин
// у авторов длиннохвостые, среднее уводит одна простыня.
type Dist struct {
	N      int     `json:"n"`
	Mean   float64 `json:"mean"`
	P10    int     `json:"p10"`
	P25    int     `json:"p25"`
	Median int     `json:"median"`
	P75    int     `json:"p75"`
	P90    int     `json:"p90"`
	Max    int     `json:"max"`
}

// VoiceCount — «сколько раз встретилось» для топ-списков.
type VoiceCount struct {
	Text  string  `json:"text"`
	N     int     `json:"n"`
	Share float64 `json:"share"` // доля текстов, где встречается
}

// VoiceShape — МЕХАНИКА письма в одном жанре. Всё измеряется, ничего не
// оценивается: это цели для генерации и материал для диффа черновика.
type VoiceShape struct {
	Kind      string `json:"kind"`       // notes | comments
	Texts     int    `json:"texts"`      // сколько текстов легло в замер
	TotalHave int    `json:"total_have"` // сколько всего есть в архиве (замер мог быть урезан -recent)
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`

	Runes            Dist    `json:"runes"`
	Sentences        Dist    `json:"sentences"`
	Paragraphs       Dist    `json:"paragraphs"`
	WordsPerSentence float64 `json:"words_per_sentence"`
	WordRunes        float64 `json:"word_runes"`

	// Punct — знаков на 1000 рун: сравнимо между текстами разной длины.
	Punct map[string]float64 `json:"punct"`
	// ParenRuns — скобочная подпись: доля текстов с серией ")" длиной ровно
	// 1/2/3/4+. Держится ОТДЕЛЬНО от Punct намеренно: «)» и «))» — разные люди,
	// а частота на 1000 рун эту разницу размывает.
	ParenRuns map[string]float64 `json:"paren_runs"`
	SadParens float64            `json:"sad_parens"` // доля текстов с «((» и длиннее

	NoFinalPunct float64 `json:"no_final_punct"`
	AllLower     float64 `json:"all_lower"`
	StartsLower  float64 `json:"starts_lower"`
	AllCapsWords float64 `json:"all_caps_words"`
	YoRate       float64 `json:"yo_rate"`

	Markup     map[string]float64 `json:"markup"`
	SmileyRate float64            `json:"smiley_rate"` // :::код::: на текст
	TopSmileys []VoiceCount       `json:"top_smileys,omitempty"`
	EmojiRate  float64            `json:"emoji_rate"` // 0 — эмодзи в генерации запрещены
	URLRate    float64            `json:"url_rate"`

	AddressPrefix float64      `json:"address_prefix,omitempty"` // comments: доля реплик «Ник, …»
	TopOpenings   []VoiceCount `json:"top_openings,omitempty"`
}

// VoiceRhythm — когда человек пишет. В генерацию НЕ идёт (у черновика нет даты):
// это для отчёта и для человека, выбирающего правдоподобный час вручную.
type VoiceRhythm struct {
	TZ       string  `json:"tz"`
	Hours    [24]int `json:"hours"`
	Weekdays [7]int  `json:"weekdays"`
	Peak     []int   `json:"peak"` // часы выше среднего
}

// VoiceWord — характерное слово: tf·idf по СОХРАНЁННОМУ IDF жанра.
//
// Про надёжность веса. IDF берётся у КОРЗИНЫ хэша, а не у слова, и корзину делят
// многие слова (у плодовитого автора 4096 корзин на десятки тысяч слов — делят
// практически все). Но вес корзины ведёт себя как НИЖНЯЯ оценка редкости слова:
// любое частое слово в корзине тянет IDF вниз. Значит ВЫСОКИЙ tf·idf надёжен
// (частого соседа в корзине нет), а низкий не означает ничего. Поэтому список
// не фильтруется по Collision — фильтр по нему просто опустошал бы выдачу.
// Collision — сколько слов САМОГО автора делят корзину (>1); глобальные соседи
// не видны в принципе.
type VoiceWord struct {
	Word      string  `json:"word"`
	Count     int     `json:"count"`
	Docs      int     `json:"docs"` // в скольких разных текстах встречается
	IDF       float64 `json:"idf"`
	TFIDF     float64 `json:"tfidf"`
	Collision bool    `json:"collision,omitempty"`
}

// VoiceSample — образец, уходящий в промпт. У реплики образцом служит ПАРА
// «на что отвечает → что ответил»: короткая реплика сама по себе не показывает
// манеру, потому что вся манера в том, как человек цепляется за чужие слова.
type VoiceSample struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	At    string `json:"at,omitempty"`
	Runes int    `json:"runes"` // до усечения
	Text  string `json:"text"`

	Context       string `json:"context,omitempty"`        // текст, на который отвечает
	ContextAuthor string `json:"context_author,omitempty"` // и чей он
}

// VoiceAccount — анкета личности и надёжность её эталона.
type VoiceAccount struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Ngrams int    `json:"ngrams"` // объём стиль-профиля жанра; 0 — профиля нет
}

// VoiceCardParams — параметры сборки карты.
type VoiceCardParams struct {
	Genre    string // GenreNotes | GenreAll: слой атрибуции и источник IDF
	Kind     string // РОД текста, который пишем: notes | comments ("" — по жанру)
	Solo     bool   // только указанная анкета, без склейки личности
	Recent   int    // последних текстов в замер на жанр (0 — все)
	Samples  int    // образцов в промпт; 0 — БЕЗ дословных текстов автора; <0 — по роду
	Band     int    // held-out текстов под эталонную полосу
	Seed     int64
	TopWords int
	MaxRunes int // усечение образца
}

// VoiceCardDefaults — дефолты сборки. Recent не 0 потому, что idx_comments_author
// не покрывает text: у тяжёлого автора десятки тысяч комментариев читаются с
// диска построчно.
func VoiceCardDefaults() VoiceCardParams {
	return VoiceCardParams{
		Genre: GenreNotes, Recent: 2000, Samples: 6, Band: 30,
		Seed: 1, TopWords: 40, MaxRunes: 3000,
	}
}

// VoiceCard — детерминированная карта письма личности.
type VoiceCard struct {
	Stamp    VoiceStamp     `json:"stamp"`
	Identity string         `json:"identity"`
	Label    string         `json:"label"`
	Accounts []VoiceAccount `json:"accounts"`

	Genre     string      `json:"genre"`
	Notes     VoiceShape  `json:"notes"`
	Comments  VoiceShape  `json:"comments"`
	Rhythm    VoiceRhythm `json:"rhythm"`
	Vocab     []VoiceWord `json:"vocab"`
	VocabNote string      `json:"vocab_note,omitempty"` // почему словарь пуст
	VocabRate float64     `json:"vocab_rate"`           // слов из Vocab на 100 слов у САМОГО автора

	Samples []VoiceSample `json:"samples"`
	HeldIDs []int64       `json:"held_ids"` // отложены под полосу, в промпт НЕ идут
	Seed    int64         `json:"seed"`

	words map[string]int // полный словарь автора — для диффа «слов, которых у него нет»
	held  []voiceText    // тексты полосы (в JSON не выносим: это чужие тексты целиком)
}

// AuthorWords — словарь автора (слово → сколько раз). Нужен диффу черновика:
// «слов, которых у автора нет ни разу» — самый действенный пункт обратной связи.
func (c *VoiceCard) AuthorWords() map[string]int { return c.words }

// HeldOut — отложенные тексты для эталонной полосы.
func (c *VoiceCard) HeldOut() []voiceText { return c.held }

// voiceText — сырой текст корпуса личности.
type voiceText struct {
	id     int64
	author int64
	kind   string // notes | comments
	text   string
	pub    string
}

// --- сборка ---

// BuildVoiceCard собирает карту письма личности (token: p<id>|u<id>|user_id).
func (s *Store) BuildVoiceCard(ctx context.Context, token string, p VoiceCardParams, now time.Time) (*VoiceCard, error) {
	if !ValidGenre(p.Genre) {
		return nil, fmt.Errorf("voice: неизвестный жанр %q (all|notes)", p.Genre)
	}
	identity, accIDs, err := s.voiceTarget(ctx, token, p.Solo)
	if err != nil {
		return nil, err
	}

	notes, err := s.voiceTexts(ctx, accIDs, "notes", p.Recent)
	if err != nil {
		return nil, err
	}
	comments, err := s.voiceTexts(ctx, accIDs, "comments", p.Recent)
	if err != nil {
		return nil, err
	}
	haveNotes, haveComments, err := s.voiceTotals(ctx, accIDs)
	if err != nil {
		return nil, err
	}

	nicks, err := s.siteNicks(ctx)
	if err != nil {
		return nil, err
	}
	card := &VoiceCard{
		Stamp: newVoiceStamp(p.Genre, now), Identity: identity, Genre: p.Genre, Seed: p.Seed,
		Notes:    measureShape(notes, "notes", nicks),
		Comments: measureShape(comments, "comments", nicks),
		Rhythm:   measureRhythm(append(append([]voiceText{}, notes...), comments...)),
	}
	card.Notes.TotalHave, card.Comments.TotalHave = haveNotes, haveComments
	if card.Label, err = s.identityLabel(ctx, identity); err != nil {
		return nil, err
	}
	if card.Accounts, err = s.voiceAccounts(ctx, accIDs, p.Genre); err != nil {
		return nil, err
	}
	// В solo-режиме ярлыка личности нет (identity — сама анкета), берём её ник.
	if card.Label == "" && len(card.Accounts) > 0 {
		card.Label = card.Accounts[0].Name
	}

	corpus := voiceCorpus(p, notes, comments)
	if len(corpus) == 0 {
		return nil, fmt.Errorf("voice: у %s нет текстов жанра %s (род %q)", identity, p.Genre, p.Kind)
	}
	if p.Samples < 0 {
		p.Samples = voiceAutoSamples(corpus)
	}

	if card.Vocab, card.words, card.VocabNote, err = s.distinctiveWords(ctx, corpus, p.Genre, p.TopWords); err != nil {
		return nil, err
	}

	sample, held := voiceSplit(corpus, p.Samples, p.Band, p.Seed)
	card.held = held
	for _, t := range held {
		card.HeldIDs = append(card.HeldIDs, t.id)
	}
	card.Samples = voiceSamples(sample, p.MaxRunes)
	if err := s.fillSampleContexts(ctx, card.Samples); err != nil {
		return nil, err
	}
	card.VocabRate = vocabRate(corpus, card.Vocab)
	return card, nil
}

func voiceSamples(sample []voiceText, maxRunes int) []VoiceSample {
	out := make([]VoiceSample, 0, len(sample))
	for _, t := range sample {
		out = append(out, VoiceSample{
			ID: t.id, Kind: t.kind, At: shortDateStr(t.pub),
			Runes: len([]rune(t.text)), Text: excerpt(t.text, maxRunes),
		})
	}
	return out
}

// fillSampleContexts дотягивает к образцам-репликам то, на что они отвечают.
// Цель ответа берётся из слоя comment_reply (настоящая цель с мобильной версии) и
// только при его отсутствии — из parent_id, который указывает на корень ВЕТКИ, а
// не на адресата. Ни того, ни другого нет — значит реплика первого уровня, и
// отвечает она самой заметке.
func (s *Store) fillSampleContexts(ctx context.Context, samples []VoiceSample) error {
	ids := make([]int64, 0, len(samples))
	for _, sm := range samples {
		if sm.Kind == "comments" {
			ids = append(ids, sm.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	got, err := s.sampleContexts(ctx, ids)
	if err != nil {
		return err
	}
	for i := range samples {
		if c, ok := got[samples[i].ID]; ok && c.text != "" {
			samples[i].Context = excerpt(c.text, voiceContextRunes)
			samples[i].ContextAuthor = c.author
		}
	}
	return nil
}

type voiceCtx struct{ text, author string }

func (s *Store) sampleContexts(ctx context.Context, ids []int64) (map[int64]voiceCtx, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id,
		       COALESCE(p.text,''), COALESCE(pu.name,''),
		       COALESCE(n.text,''), COALESCE(nu.name,'')
		FROM comments c
		LEFT JOIN comment_reply r ON r.comment_id = c.id
		LEFT JOIN comments p ON p.id = COALESCE(r.reply_to, NULLIF(c.parent_id,0))
		LEFT JOIN users pu ON pu.id = p.author_id
		LEFT JOIN notes n ON n.id = c.note_id
		LEFT JOIN users nu ON nu.id = n.author_id
		WHERE c.id IN (`+intList(ids)+`)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	got := map[int64]voiceCtx{}
	for rows.Next() {
		var id int64
		var pText, pAuthor, nText, nAuthor string
		if err := rows.Scan(&id, &pText, &pAuthor, &nText, &nAuthor); err != nil {
			return nil, err
		}
		got[id] = pickSampleContext(pText, pAuthor, nText, nAuthor)
	}
	return got, rows.Err()
}

func pickSampleContext(pText, pAuthor, nText, nAuthor string) voiceCtx {
	if pText == "" {
		author := nAuthor + " (заметка)"
		if nAuthor == "" {
			author = "аноним (заметка)"
		}
		return voiceCtx{html.UnescapeString(nText), author}
	}
	if pAuthor == "" {
		pAuthor = "аноним"
	}
	return voiceCtx{html.UnescapeString(pText), pAuthor}
}

// voiceContextRunes — потолок контекста образца. Реплика короткая, и длинный
// контекст перевесил бы в промпте сам образец.
const voiceContextRunes = 300

// voiceTarget — кого имитируем: личность целиком или ОДНУ анкету.
//
// Склейка личности для имитации манеры не всегда то, что нужно. У кластера из
// одиннадцати анкет 2010–2026 годов (реальный случай) карта усредняет манеру
// пятнадцати лет: пять образцов из шести пришли из 2011-го, хотя имитировать
// просили нынешнюю анкету. Solo берёт ровно указанную анкету — тогда «профиль
// X» значит профиль X, а не всё, что с ним склеено.
func (s *Store) voiceTarget(ctx context.Context, token string, solo bool) (string, []int64, error) {
	if solo {
		id, err := parseAccountToken(token)
		if err != nil {
			return "", nil, err
		}
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, id).Scan(&n); err != nil {
			return "", nil, err
		}
		if n == 0 {
			return "", nil, fmt.Errorf("voice: анкета %d не найдена", id)
		}
		return fmt.Sprintf("u%d", id), []int64{id}, nil
	}
	identity, err := s.canonIdentity(ctx, token)
	if err != nil {
		return "", nil, err
	}
	accIDs, err := s.identityAccountIDs(ctx, identity)
	if err != nil {
		return "", nil, err
	}
	if len(accIDs) == 0 {
		return "", nil, fmt.Errorf("voice: у %s нет анкет", identity)
	}
	return identity, accIDs, nil
}

// parseAccountToken — u<id>|<id>. Личность p<id> в solo-режиме не годится: там
// анкет несколько, и выбирать за пользователя нельзя.
func parseAccountToken(token string) (int64, error) {
	t := strings.TrimSpace(token)
	if strings.HasPrefix(t, "p") {
		return 0, fmt.Errorf("voice -solo: нужна анкета u<id>, а не личность %q", token)
	}
	t = strings.TrimPrefix(t, "u")
	id, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("voice -solo: некорректный id анкеты %q", token)
	}
	return id, nil
}

// voiceCorpus — корпус образцов, словаря и полосы. Он обязан совпадать с РОДОМ
// текста, который пишем, а не со слоем атрибуции: у режима комментария эталон
// жанра all, и по нему в промпт комментария попадали заметки (у Гадёныша —
// 378-рунная заметка среди шести образцов), а словарь возглавляли формулы его
// заметочного формата («клуб пятничных неудачников объявляется открытым»).
// Пустой Kind — прежнее поведение по жанру, для `voice card`.
func voiceCorpus(p VoiceCardParams, notes, comments []voiceText) []voiceText {
	switch p.Kind {
	case "comments":
		return comments
	case "notes":
		return notes
	}
	if p.Genre == GenreAll {
		return append(append([]voiceText{}, notes...), comments...)
	}
	return notes
}

// voiceAutoSamples — сколько образцов брать, когда число не задано. Манера
// коротких текстов из шести примеров не читается: на медиане 87 рун это ~500 рун
// на весь промпт. Считаем по медиане корпуса, а не по роду: у автора длинных
// комментариев не должно быть двадцати четырёх образцов зря.
func voiceAutoSamples(corpus []voiceText) int {
	lens := make([]int, 0, len(corpus))
	for _, t := range corpus {
		lens = append(lens, len([]rune(t.text)))
	}
	switch med := medianInt(lens); {
	case med == 0:
		return 6
	case med < 150:
		return 24
	case med < 400:
		return 12
	default:
		return 6
	}
}

// voiceTexts — последние limit текстов жанра по анкетам личности. Свежие первыми:
// манера дрейфует, и недавняя ближе к тому, что человек написал бы сейчас.
func (s *Store) voiceTexts(ctx context.Context, accIDs []int64, kind string, limit int) ([]voiceText, error) {
	q := `SELECT id, author_id, text, COALESCE(published_at,'') FROM notes
	      WHERE author_id IN (` + intList(accIDs) + `) AND text != '' ORDER BY id DESC`
	if kind == "comments" {
		q = `SELECT id, author_id, text, COALESCE(published_at,'') FROM comments
		     WHERE author_id IN (` + intList(accIDs) + `) AND text != '' ORDER BY id DESC`
	}
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []voiceText
	for rows.Next() {
		t := voiceText{kind: kind}
		if err := rows.Scan(&t.id, &t.author, &t.text, &t.pub); err != nil {
			return nil, err
		}
		// Часть корпуса несёт неразобранные HTML-сущности (&quot; в 6 тыс. заметок,
		// плюс &amp;/&nbsp;): парсер брал .Text() у уже экранированного места. Без
		// раскодировки forEachWord видит слово «quot», и карта уверяет, что автор
		// начинает заметки с «quot». Атрибуции это не касается — она работает по
		// сырому тексту профилей.
		t.text = html.UnescapeString(t.text)
		out = append(out, t)
	}
	return out, rows.Err()
}

// voiceTotals — честный знаменатель: сколько текстов есть всего (замер мог быть
// урезан -recent, и это не должно быть невидимым).
func (s *Store) voiceTotals(ctx context.Context, accIDs []int64) (notes, comments int, err error) {
	in := intList(accIDs)
	err = s.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM notes    WHERE author_id IN (`+in+`) AND text != ''),
		        (SELECT COUNT(*) FROM comments WHERE author_id IN (`+in+`) AND text != '')`).
		Scan(&notes, &comments)
	return notes, comments, err
}

// identityLabel — ярлык личности из лёгкой v_identity. Намеренно НЕ через
// Portrait/v_persona_activity: та вью считается ~9 с на личность на живом архиве.
func (s *Store) identityLabel(ctx context.Context, identity string) (string, error) {
	var label string
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(label),'') FROM v_identity WHERE identity = ?`, identity).Scan(&label)
	return label, err
}

// voiceAccounts — анкеты личности с объёмом стиль-профиля жанра (надёжность
// эталона и знаменатель контаминации эталонной полосы).
func (s *Store) voiceAccounts(ctx context.Context, accIDs []int64, genre string) ([]VoiceAccount, error) {
	names, err := s.namesByIDs(ctx, accIDs)
	if err != nil {
		return nil, err
	}
	ngrams := map[int64]int{}
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, ngrams FROM style_profiles WHERE genre = ? AND user_id IN (`+intList(accIDs)+`)`, genre)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		ngrams[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]VoiceAccount, 0, len(accIDs))
	for _, id := range accIDs {
		out = append(out, VoiceAccount{ID: id, Name: names[id], Ngrams: ngrams[id]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ngrams > out[j].Ngrams })
	return out, nil
}

// --- измерения ---

// shapeAcc — накопитель замера: копит сырые счётчики по текстам, доли считает
// один раз в finish. Разнесено намеренно — единой функцией это нечитаемо.
type shapeAcc struct {
	runes, sentences, paragraphs []int

	totalRunes, totalWords, totalWordRunes, totalSentences, totalSmileys int
	capsWords                                                            int

	// счётчики ТЕКСТОВ с признаком (не вхождений)
	noFinal, allLower, startsLower, yo, emoji, urls, sadParens, addressed int

	punct       map[rune]int
	parenHas    map[string]int
	markup      map[string]int
	smileys     map[string]int
	smileyTexts map[string]int
	openings    map[string]int
}

func newShapeAcc() *shapeAcc {
	return &shapeAcc{
		punct: map[rune]int{}, parenHas: map[string]int{}, markup: map[string]int{},
		smileys: map[string]int{}, smileyTexts: map[string]int{}, openings: map[string]int{},
	}
}

// addLengths — длины: руны, предложения, абзацы, слова.
func (a *shapeAcc) addLengths(text string, r []rune) {
	a.runes = append(a.runes, len(r))
	a.totalRunes += len(r)
	sn := countSentences(text)
	a.sentences = append(a.sentences, sn)
	a.totalSentences += sn
	a.paragraphs = append(a.paragraphs, countParagraphs(text))
	forEachWord(text, func(w []rune) {
		a.totalWords++
		a.totalWordRunes += len(w)
	})
	a.capsWords += countCapsWords(text)
}

// addPunct — пунктуация и скобочная подпись.
func (a *shapeAcc) addPunct(r []rune) {
	for _, c := range r {
		if _, ok := punctKeys[c]; ok {
			a.punct[c]++
		}
	}
	for _, b := range runBuckets(r, ')') {
		a.parenHas[b]++
	}
	if maxRun(r, '(') >= 2 {
		a.sadParens++
	}
}

// addTraits — признаки текста целиком: регистр, финал, «ё», эмодзи, ссылки.
func (a *shapeAcc) addTraits(text string, r []rune) {
	if lacksFinalTerminator(r) {
		a.noFinal++
	}
	if isAllLower(r) {
		a.allLower++
	}
	if firstLetterLower(r) {
		a.startsLower++
	}
	if strings.ContainsAny(text, "ёЁ") {
		a.yo++
	}
	if hasEmoji(r) {
		a.emoji++
	}
	if strings.Contains(text, "http") {
		a.urls++
	}
}

// addMarkup — разметка сайта и смайлы: часть голоса, не мусор (normalizeStyle их
// тоже сохраняет, то есть они входят в измеряемый атрибутором сигнал).
func (a *shapeAcc) addMarkup(text string) {
	for _, tag := range markupTags {
		if strings.Contains(text, tag) {
			a.markup[tag]++
		}
	}
	seen := map[string]bool{}
	for _, m := range smileyRe.FindAllStringSubmatch(text, -1) {
		a.smileys[m[1]]++
		a.totalSmileys++
		if !seen[m[1]] {
			seen[m[1]] = true
			a.smileyTexts[m[1]]++
		}
	}
}

// addOpening — обращение и зачин. Зачин считается по телу БЕЗ обращения: иначе
// «чем начинает» вырождается в список ников собеседников.
//
// Префикс сверяется с реальными никами (nicks). Без сверки love.AddressPrefix
// считает обращением ЛЮБУЮ раннюю запятую («всё как всегда, ничего нового»), а
// от этой доли зависит, подставлять ли ник в черновик, — завышать её нельзя.
// nicks == nil — сверка выключена (тогда доля читается как верхняя оценка).
func (a *shapeAcc) addOpening(text, kind string, nicks map[string]bool) {
	body := text
	if kind == "comments" {
		if pref := love.AddressPrefix(text); pref != "" && (nicks == nil || nicks[pref]) {
			a.addressed++
			// Режем по самой запятой: AddressPrefix отдаёт ник в нижнем регистре,
			// и TrimPrefix по нему не совпал бы с исходным «Аня,».
			if i := strings.IndexByte(body, ','); i >= 0 {
				body = strings.TrimSpace(body[i+1:])
			}
		}
	}
	if w := firstWord(body); w != "" {
		a.openings[w]++
	}
}

func (a *shapeAcc) finish(sh *VoiceShape, texts int) {
	n := float64(texts)
	sh.Runes, sh.Sentences, sh.Paragraphs = distOf(a.runes), distOf(a.sentences), distOf(a.paragraphs)
	if a.totalSentences > 0 {
		sh.WordsPerSentence = round2(float64(a.totalWords) / float64(a.totalSentences))
	}
	if a.totalWords > 0 {
		sh.WordRunes = round2(float64(a.totalWordRunes) / float64(a.totalWords))
		sh.AllCapsWords = round4(float64(a.capsWords) / float64(a.totalWords))
	}
	if a.totalRunes > 0 {
		for c, cnt := range a.punct {
			sh.Punct[string(c)] = round2(float64(cnt) * 1000 / float64(a.totalRunes))
		}
	}
	for b, cnt := range a.parenHas {
		sh.ParenRuns[b] = round4(float64(cnt) / n)
	}
	for tag, cnt := range a.markup {
		sh.Markup[tag] = round4(float64(cnt) / n)
	}
	sh.SadParens = round4(float64(a.sadParens) / n)
	sh.NoFinalPunct = round4(float64(a.noFinal) / n)
	sh.AllLower = round4(float64(a.allLower) / n)
	sh.StartsLower = round4(float64(a.startsLower) / n)
	sh.YoRate = round4(float64(a.yo) / n)
	sh.EmojiRate = round4(float64(a.emoji) / n)
	sh.URLRate = round4(float64(a.urls) / n)
	sh.SmileyRate = round2(float64(a.totalSmileys) / n)
	if sh.Kind == "comments" {
		sh.AddressPrefix = round4(float64(a.addressed) / n)
	}
	sh.TopSmileys = topCounts(a.smileys, a.smileyTexts, texts, 10)
	sh.TopOpenings = topCounts(a.openings, a.openings, texts, 8)
}

// measureShape меряет механику письма. nicks — множество реальных ников сайта
// (нижний регистр) для проверки обращений; nil — сверка выключена.
func measureShape(texts []voiceText, kind string, nicks map[string]bool) VoiceShape {
	sh := VoiceShape{
		Kind: kind, Texts: len(texts),
		Punct: map[string]float64{}, ParenRuns: map[string]float64{}, Markup: map[string]float64{},
	}
	if len(texts) == 0 {
		return sh
	}
	a := newShapeAcc()
	for _, t := range texts {
		r := []rune(t.text)
		a.addLengths(t.text, r)
		a.addPunct(r)
		a.addTraits(t.text, r)
		a.addMarkup(t.text)
		a.addOpening(t.text, kind, nicks)
	}
	a.finish(&sh, len(texts))
	sh.From, sh.To = spanOf(texts)
	return sh
}

// siteNicks — все ники сайта в нижнем регистре. Нужен для проверки обращений;
// ~22 тыс. строк, один запрос за сборку карты.
func (s *Store) siteNicks(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT name FROM users WHERE name != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[strings.ToLower(strings.TrimSpace(n))] = true
	}
	return out, rows.Err()
}

// punctKeys — знаки, чью частоту меряем (на 1000 рун).
var punctKeys = map[rune]struct{}{
	',': {}, '.': {}, '!': {}, '?': {}, '…': {}, '—': {}, '-': {},
	':': {}, ';': {}, '«': {}, '"': {}, '(': {}, ')': {},
}

var markupTags = []string{"[b]", "[i]", "[u]"}

// countSentences — грубое деление на предложения (терминаторы . ! ? … и перевод
// строки, серии схлопываются). Лингвистически неточно и не должно быть точным:
// число служит целью и диффом, а обе стороны меряются одной функцией.
func countSentences(s string) int {
	n, inTerm, seen := 0, false, false
	for _, c := range s {
		switch {
		case c == '.' || c == '!' || c == '?' || c == '…' || c == '\n':
			if !inTerm && seen {
				n++
			}
			inTerm = true
		case unicode.IsSpace(c):
			// пробел не закрывает и не открывает предложение
		default:
			inTerm = false
			seen = true
		}
	}
	if !inTerm && seen {
		n++
	}
	if n == 0 && seen {
		n = 1
	}
	return n
}

// countParagraphs — абзацы разделяются пустой строкой.
func countParagraphs(s string) int {
	n := 0
	for _, part := range strings.Split(s, "\n\n") {
		if strings.TrimSpace(part) != "" {
			n++
		}
	}
	if n == 0 {
		n = 1
	}
	return n
}

// runBuckets — какие длины серий символа c встречаются в тексте («1","2","3","4+»).
func runBuckets(r []rune, c rune) []string {
	seen := map[string]bool{}
	run := 0
	flush := func() {
		if run == 0 {
			return
		}
		b := "4+"
		if run < 4 {
			b = fmt.Sprintf("%d", run)
		}
		seen[b] = true
		run = 0
	}
	for _, x := range r {
		if x == c {
			run++
			continue
		}
		flush()
	}
	flush()
	out := make([]string, 0, len(seen))
	for b := range seen {
		out = append(out, b)
	}
	return out
}

func maxRun(r []rune, c rune) int {
	best, run := 0, 0
	for _, x := range r {
		if x == c {
			run++
			if run > best {
				best = run
			}
			continue
		}
		run = 0
	}
	return best
}

// lacksFinalTerminator — текст не закрыт знаком конца предложения. Скобка-улыбка
// в конце («день))») считается НЕзакрытым: для генерации важно ровно это — ставит
// человек точку или обрывает фразу.
func lacksFinalTerminator(r []rune) bool {
	for i := len(r) - 1; i >= 0; i-- {
		if unicode.IsSpace(r[i]) {
			continue
		}
		switch r[i] {
		case '.', '!', '?', '…':
			return false
		}
		return true
	}
	return false
}

func isAllLower(r []rune) bool {
	letter := false
	for _, c := range r {
		if unicode.IsLetter(c) {
			letter = true
			if unicode.IsUpper(c) {
				return false
			}
		}
	}
	return letter
}

func firstLetterLower(r []rune) bool {
	for _, c := range r {
		if unicode.IsLetter(c) {
			return unicode.IsLower(c)
		}
	}
	return false
}

// hasEmoji — грубая проверка диапазонов. Без зависимости: нам нужно лишь знать,
// водятся ли эмодзи у автора вообще (корпус старше их, и модель насыпет своих).
func hasEmoji(r []rune) bool {
	for _, c := range r {
		switch {
		case c >= 0x1F300 && c <= 0x1FAFF,
			c >= 0x2600 && c <= 0x27BF,
			c == 0xFE0F,
			c >= 0x1F000 && c <= 0x1F0FF:
			return true
		}
	}
	return false
}

func countCapsWords(s string) int {
	n := 0
	forEachWord(s, func(w []rune) {
		upper, letters := 0, 0
		for _, c := range w {
			if unicode.IsLetter(c) {
				letters++
				if unicode.IsUpper(c) {
					upper++
				}
			}
		}
		if letters >= 2 && upper == letters {
			n++
		}
	})
	return n
}

// firstWord — первое слово текста в нижнем регистре (зачины автора).
func firstWord(s string) string {
	var out string
	forEachWord(s, func(w []rune) {
		if out == "" {
			out = strings.ToLower(string(w))
		}
	})
	return out
}

func topCounts(counts, texts map[string]int, total, limit int) []VoiceCount {
	if len(counts) == 0 || total == 0 {
		return nil
	}
	out := make([]VoiceCount, 0, len(counts))
	for k, n := range counts {
		out = append(out, VoiceCount{Text: k, N: n, Share: round4(float64(texts[k]) / float64(total))})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Text < out[j].Text
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func spanOf(texts []voiceText) (string, string) {
	from, to := "", ""
	for _, t := range texts {
		if t.pub == "" {
			continue
		}
		if from == "" || t.pub < from {
			from = t.pub
		}
		if to == "" || t.pub > to {
			to = t.pub
		}
	}
	return shortDateStr(from), shortDateStr(to)
}

// measureRhythm — часы и дни недели в поясе сайта.
func measureRhythm(texts []voiceText) VoiceRhythm {
	rh := VoiceRhythm{TZ: siteTZ}
	loc, err := time.LoadLocation(siteTZ)
	if err != nil {
		loc = time.UTC
		rh.TZ = "UTC"
	}
	total := 0
	for _, t := range texts {
		if t.pub == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339, t.pub)
		if err != nil {
			continue
		}
		local := ts.In(loc)
		rh.Hours[local.Hour()]++
		rh.Weekdays[int(local.Weekday())]++
		total++
	}
	if total == 0 {
		return rh
	}
	avg := float64(total) / 24
	for h, n := range rh.Hours {
		if float64(n) > avg {
			rh.Peak = append(rh.Peak, h)
		}
	}
	return rh
}

// --- характерная лексика ---

// distinctiveWords ранжирует слова автора по tf·idf, беря IDF из УЖЕ построенного
// lexis_meta по корзине хэша слова. Так вес слова считается в том же пространстве,
// что и у атрибутора, и без прохода по всему корпусу (10,7 млн комментариев).
//
// Пределы, которые обязан печатать отчёт: корзины общие, поэтому редкая корзина
// может подарить вес частому слову; поймать мы умеем только ВНУТРИавторские
// коллизии (флаг Collision). forEachWord требует букв ≥2 — «и», «в», «не»
// невидимы (это пробел слоя funcwords, не наш).
func (s *Store) distinctiveWords(ctx context.Context, corpus []voiceText, genre string, top int) ([]VoiceWord, map[string]int, string, error) {
	counts, docs := wordCounts(corpus)
	idf, dims, err := s.loadLexisIDF(ctx, genre)
	if err != nil {
		return nil, counts, "", err
	}
	if dims == 0 || len(idf) == 0 {
		return nil, counts, "лексический слой жанра не построен (personas lexis build -genre " + genre + ")", nil
	}

	bucketWords := map[uint64]int{}
	for w := range counts {
		bucketWords[hashWordRunes([]rune(w))%uint64(dims)]++
	}
	words := make([]VoiceWord, 0, len(counts))
	for w, c := range counts {
		// Слово из одного текста — не привычка, а разовая тема: в список оно
		// попадает только высоким tf·idf и тянет генерацию в пересказ образца.
		if docs[w] < 2 && len(corpus) > 2 {
			continue
		}
		b := hashWordRunes([]rune(w)) % uint64(dims)
		wIDF := float64(idf[b])
		words = append(words, VoiceWord{
			Word: w, Count: c, Docs: docs[w], IDF: round2(wIDF),
			TFIDF:     round2((1 + math.Log(float64(c))) * wIDF),
			Collision: bucketWords[b] > 1,
		})
	}
	sort.Slice(words, func(i, j int) bool {
		if words[i].TFIDF != words[j].TFIDF {
			return words[i].TFIDF > words[j].TFIDF
		}
		return words[i].Word < words[j].Word
	})
	if len(words) > top {
		words = words[:top]
	}
	return words, counts, "", nil
}

// wordCounts — слова корпуса: сколько раз всего и в скольких разных текстах.
// Разметка сайта снимается: из «[color=red]» forEachWord достаёт «color» и «red»,
// и такие токены возглавляли список характерных слов, хотя это не слова автора, а
// теги. Частота разметки в промпт идёт отдельной строкой измерений.
func wordCounts(corpus []voiceText) (counts, docs map[string]int) {
	counts, docs = map[string]int{}, map[string]int{}
	for _, t := range corpus {
		seen := map[string]bool{}
		forEachWord(stripSiteMarkup(t.text), func(w []rune) {
			s := string(w)
			counts[s]++
			if !seen[s] {
				seen[s] = true
				docs[s]++
			}
		})
	}
	return counts, docs
}

// siteMarkupRe — теги сайта: [b]…[/b], [color=red], :::smile:::.
var siteMarkupRe = regexp.MustCompile(`\[[^\[\]\n]{1,40}\]|:::[^:\s]{1,40}:::`)

func stripSiteMarkup(text string) string { return siteMarkupRe.ReplaceAllString(text, " ") }

// vocabRate — слов из списка характерных на 100 слов текста. Норма автора против
// той же величины у черновика — прямая мера набивки: модель, получив список,
// склонна насыпать его вместо манеры (см. отрицательный styleZ при
// положительном lexZ в живых замерах).
func vocabRate(corpus []voiceText, vocab []VoiceWord) float64 {
	if len(vocab) == 0 {
		return 0
	}
	in := make(map[string]bool, len(vocab))
	for _, v := range vocab {
		in[v.Word] = true
	}
	var hits, total int
	for _, t := range corpus {
		forEachWord(stripSiteMarkup(t.text), func(w []rune) {
			total++
			if in[string(w)] {
				hits++
			}
		})
	}
	if total == 0 {
		return 0
	}
	return round2(100 * float64(hits) / float64(total))
}

// VocabUse — та же мера для одного текста (черновика) плюс абсолютное число
// попаданий. Без него короткая реплика отбраковывалась бы за одно уместное слово:
// на 20 словах это сразу 5 на 100.
func VocabUse(text string, vocab []VoiceWord) (rate float64, hits int) {
	if len(vocab) == 0 {
		return 0, 0
	}
	in := make(map[string]bool, len(vocab))
	for _, v := range vocab {
		in[v.Word] = true
	}
	var total int
	forEachWord(stripSiteMarkup(text), func(w []rune) {
		total++
		if in[string(w)] {
			hits++
		}
	})
	if total == 0 {
		return 0, hits
	}
	return round2(100 * float64(hits) / float64(total)), hits
}

// --- разрез корпуса ---

// voiceSplit детерминированно делит корпус на образцы для промпта и held-out
// полосу. Ключ текста — FNV(seed, id): один и тот же seed на той же БД даёт ту же
// выборку. Образцы берутся ПОСЛОЙНО по длине (корпус режется на samples корзин по
// рунам, из каждой — текст с минимальным ключом): иначе промпт уезжает в один
// регистр и «манера» получается не та.
//
// Пересечения образцов и полосы НЕТ по построению: полоса меряет, как атрибутор
// узнаёт тексты, КОТОРЫХ МОДЕЛЬ НЕ ВИДЕЛА.
func voiceSplit(texts []voiceText, samples, band int, seed int64) (sample, held []voiceText) {
	if len(texts) == 0 {
		return nil, nil
	}
	key := func(t voiceText) uint64 {
		h := fnv.New64a()
		fmt.Fprintf(h, "%d:%d", seed, t.id)
		return h.Sum64()
	}
	byLen := append([]voiceText{}, texts...)
	sort.Slice(byLen, func(i, j int) bool {
		li, lj := len([]rune(byLen[i].text)), len([]rune(byLen[j].text))
		if li != lj {
			return li < lj
		}
		return byLen[i].id < byLen[j].id
	})

	chosen := map[int64]bool{}
	for i := 0; i < samples && i < len(byLen); i++ {
		lo, hi := i*len(byLen)/samples, (i+1)*len(byLen)/samples
		if hi <= lo {
			hi = lo + 1
		}
		if hi > len(byLen) {
			hi = len(byLen)
		}
		best, bestKey := -1, uint64(0)
		for k := lo; k < hi; k++ {
			if chosen[byLen[k].id] {
				continue
			}
			if kk := key(byLen[k]); best < 0 || kk < bestKey {
				best, bestKey = k, kk
			}
		}
		if best >= 0 {
			chosen[byLen[best].id] = true
			sample = append(sample, byLen[best])
		}
	}

	rest := make([]voiceText, 0, len(texts))
	for _, t := range texts {
		if !chosen[t.id] {
			rest = append(rest, t)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return key(rest[i]) < key(rest[j]) })
	if band > 0 && band < len(rest) {
		rest = rest[:band]
	}
	return sample, rest
}

// --- утилиты ---

func distOf(xs []int) Dist {
	d := Dist{N: len(xs)}
	if len(xs) == 0 {
		return d
	}
	f := make([]float64, len(xs))
	sum := 0
	for i, x := range xs {
		f[i] = float64(x)
		sum += x
	}
	sort.Float64s(f)
	d.Mean = round2(float64(sum) / float64(len(xs)))
	d.P10, d.P25 = int(quantile(f, 0.10)+0.5), int(quantile(f, 0.25)+0.5)
	d.Median = int(quantile(f, 0.50) + 0.5)
	d.P75, d.P90 = int(quantile(f, 0.75)+0.5), int(quantile(f, 0.90)+0.5)
	d.Max = int(f[len(f)-1])
	return d
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }
func round4(x float64) float64 { return math.Round(x*10000) / 10000 }

// --- рендер карты ---

// WriteVoiceBrief печатает карту компактным русским блоком. Один и тот же текст
// идёт и человеку в отчёт, и в промпт модели: если он нечитаем человеком, то и
// модели он не помогает.
func WriteVoiceBrief(w io.Writer, c *VoiceCard, kind string) error {
	sh := c.Notes
	if kind == "comments" {
		sh = c.Comments
	}
	name := c.Label
	if name == "" && len(c.Accounts) > 0 {
		name = c.Accounts[0].Name
	}
	fmt.Fprintf(w, "=== КАРТА ПИСЬМА %s «%s» (жанр замера: %s) ===\n", c.Identity, name, sh.Kind)
	if sh.Texts == 0 {
		fmt.Fprintf(w, "текстов этого жанра нет\n")
		return nil
	}
	fmt.Fprintf(w, "Замерено текстов: %d из %d", sh.Texts, sh.TotalHave)
	if sh.From != "" {
		fmt.Fprintf(w, " (%s — %s)", sh.From, sh.To)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Длина: медиана %d рун (p10 %d, p90 %d, макс %d) — цель %d–%d.\n",
		sh.Runes.Median, sh.Runes.P10, sh.Runes.P90, sh.Runes.Max, sh.Runes.P25, sh.Runes.P75)
	fmt.Fprintf(w, "Предложений: медиана %d; слов в предложении %.1f; длина слова %.1f; абзацев медиана %d.\n",
		sh.Sentences.Median, sh.WordsPerSentence, sh.WordRunes, sh.Paragraphs.Median)
	fmt.Fprintf(w, "Пунктуация на 1000 рун: %s\n", fmtRates(sh.Punct, 8))
	fmt.Fprintf(w, "Скобки-улыбки: %s; грустных «((» в %s текстов.\n",
		fmtParens(sh.ParenRuns), pct(sh.SadParens))
	fmt.Fprintf(w, "Регистр: с маленькой начинает %s текстов; целиком строчными %s; КАПС %s слов.\n",
		pct(sh.StartsLower), pct(sh.AllLower), pct(sh.AllCapsWords))
	fmt.Fprintf(w, "Точку в конце не ставит в %s текстов. «ё» пишет в %s текстов.\n",
		pct(sh.NoFinalPunct), pct(sh.YoRate))
	if len(sh.Markup) > 0 {
		fmt.Fprintf(w, "Разметка сайта: %s\n", fmtRates(sh.Markup, 4))
	}
	// При 0.00 на текст показываем долю текстов: иначе строка «0.00 на текст
	// (чаще :::santaclaus:::)» читается как противоречие.
	switch {
	case len(sh.TopSmileys) == 0:
		fmt.Fprintln(w, "Смайлы сайта: не ставит.")
	case sh.SmileyRate < 0.01:
		fmt.Fprintf(w, "Смайлы сайта: почти не ставит (%s), из редких — %s.\n",
			pct(smileyTextShare(sh)), fmtSmileys(sh.TopSmileys, 3))
	default:
		fmt.Fprintf(w, "Смайлы сайта: %.2f на текст (чаще %s).\n",
			sh.SmileyRate, fmtSmileys(sh.TopSmileys, 5))
	}
	if sh.EmojiRate == 0 {
		fmt.Fprintln(w, "Эмодзи: не использует — запрещены.")
	} else {
		fmt.Fprintf(w, "Эмодзи: в %s текстов.\n", pct(sh.EmojiRate))
	}
	if sh.Kind == "comments" {
		fmt.Fprintf(w, "Обращение «Ник, …» в начале реплики: %s.\n", pct(sh.AddressPrefix))
	}
	if len(sh.TopOpenings) > 0 {
		ws := make([]string, 0, 6)
		for i, o := range sh.TopOpenings {
			if i == 6 {
				break
			}
			ws = append(ws, o.Text)
		}
		fmt.Fprintf(w, "Чем начинает: %s\n", strings.Join(ws, ", "))
	}
	if len(c.Vocab) > 0 {
		ws := make([]string, 0, len(c.Vocab))
		for _, v := range c.Vocab {
			ws = append(ws, v.Word)
		}
		// Список подаётся НЕ как цель: набивка характерных слов поднимает
		// лексический косинус, обрушивая стилевой (живые замеры: styleZ < 0 при
		// lexZ > 0). Поэтому рядом идёт норма самого автора и потолок.
		fmt.Fprintf(w, "Привычные слова автора (НЕ список обязательных — берётся только то, "+
			"что ложится само): %s\n", strings.Join(ws, ", "))
		fmt.Fprintf(w, "Сам автор употребляет их %.2f на 100 слов — держись этой нормы, "+
			"насыпать их больше значит выдать подделку.\n", c.VocabRate)
	} else if c.VocabNote != "" {
		fmt.Fprintf(w, "Характерные слова: %s\n", c.VocabNote)
	}
	return nil
}

// smileyTextShare — доля текстов хотя бы с одним смайлом (верхняя оценка: берём
// самый частый код).
func smileyTextShare(sh VoiceShape) float64 {
	if len(sh.TopSmileys) == 0 {
		return 0
	}
	return sh.TopSmileys[0].Share
}

func fmtSmileys(top []VoiceCount, limit int) string {
	codes := make([]string, 0, limit)
	for i, sm := range top {
		if i == limit {
			break
		}
		codes = append(codes, ":::"+sm.Text+":::")
	}
	return strings.Join(codes, " ")
}

func fmtRates(m map[string]float64, limit int) string {
	type kv struct {
		k string
		v float64
	}
	all := make([]kv, 0, len(m))
	for k, v := range m {
		if v > 0 {
			all = append(all, kv{k, v})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })
	if len(all) > limit {
		all = all[:limit]
	}
	parts := make([]string, 0, len(all))
	for _, p := range all {
		parts = append(parts, fmt.Sprintf("%s %g", p.k, p.v))
	}
	return strings.Join(parts, " | ")
}

func fmtParens(m map[string]float64) string {
	order := []string{"1", "2", "3", "4+"}
	parts := make([]string, 0, len(order))
	for _, b := range order {
		if v, ok := m[b]; ok && v > 0 {
			parts = append(parts, fmt.Sprintf("«%s» в %s", strings.Repeat(")", runLen(b)), pct(v)))
		}
	}
	if len(parts) == 0 {
		return "не ставит"
	}
	return strings.Join(parts, ", ")
}

func runLen(bucket string) int {
	if bucket == "4+" {
		return 4
	}
	return int(bucket[0] - '0')
}

func pct(x float64) string { return fmt.Sprintf("%.0f%%", x*100) }
