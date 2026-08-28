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
	"html"
	"sort"
	"strconv"
	"strings"
	"time"
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

// NewDist — квантильная сводка по набору чисел. Считает её distOf; наружу
// вынесено ради реплея, который меряет форму сгенерированного разговора теми же
// квантилями, что архив меряет настоящий. Второй способ свести числа к сводке
// разошёлся бы с первым молча, а сравнивают их между собой.
func NewDist(xs []int) Dist { return distOf(xs) }

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

	// РИТМ фразы — самое сильное из найденного по живым замерам. Среднее слов в
	// предложении у автора и у машинного текста совпадает, а текст всё равно
	// мёртвый: модель гонит ровный поток предложений одной длины, человек мешает
	// рубленые с длинными. У Монаха sd 5.9 против 2.3 у черновиков, ≤3 слов —
	// 18% против 5%. Мера — разброс и доли краёв, не среднее.
	SentWords  Dist    `json:"sent_words"` // длина предложения в словах
	SentWordSD float64 `json:"sent_word_sd"`
	ShortSents float64 `json:"short_sents"` // доля предложений ≤3 слов
	LongSents  float64 `json:"long_sents"`  // доля предложений ≥18 слов

	// Person — местоимения на 100 слов: «я»/«ты»/«мы»/«вы». Кому автор говорит —
	// признак не менее личный, чем словарь: Монах обращается на «ты» (0.9), а
	// черновики рассказывали о людях в третьем лице (0.1).
	Person map[string]float64 `json:"person"`

	EndsQuestion float64 `json:"ends_question"` // доля текстов, кончающихся вопросом
	HasQuote     float64 `json:"has_quote"`     // доля текстов с кавычками или тире-диалогом
	HasDigits    float64 `json:"has_digits"`    // доля текстов с цифрами

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
	n := 6
	switch med := medianInt(lens); {
	case med == 0:
		return 6
	case med < 150:
		n = 24
	case med < 400:
		n = 12
	}
	// Малый корпус: манеру взять больше негде, и лучше показать модели ощутимую
	// его часть, чем беречь тексты под полосу. Держим полосе хотя бы половину.
	if len(corpus) <= 60 && n < 12 {
		n = 12
	}
	if n > len(corpus)/2 {
		n = len(corpus) / 2
	}
	if n < 1 {
		n = 1
	}
	return n
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
