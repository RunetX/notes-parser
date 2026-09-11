// Пакет platfriday — отбор материала для пятничной рубрики (эпик J).
//
// Рубрика устроена так: вечером в пятницу площадка публикует вступительную
// заметку, а под ней, по одному, появляются вопросы из архива — «в каком году
// это написано», «сколько реплик собрала заметка», «что ответили на эту
// реплику». Показывает их `platform/quiz.go`, задаёт служба, а ЧТО именно
// спросить — решает этот пакет.
//
// ОТДЕЛЬНЫМ ПАКЕТОМ по той же причине, что platdigest и platsink: ядру площадки
// незачем знать про еженедельную рубрику. Правило «platform не импортирует
// archive» здесь ни при чём и соблюдается даром — архив НЕ НУЖЕН: раскатка
// 18.08.2026 перенесла в Postgres все 117 тысяч заметок и 10,7 млн реплик
// вместе с настоящими рёбрами ответов, так что материал лежит в той же базе, к
// которой ходит морда. Файла `archive.db` на хосте площадки нет и не будет.
//
// ВЫБОР ДЕТЕРМИНИРОВАН НЕДЕЛЕЙ. Тот же выпуск при повторном прогоне — не
// красота, а условие работы: служба падает и поднимается, команда `friday draft`
// показывает владельцу черновик, а опубликоваться должно ровно то, что он видел.
// Приём тот же, что у жребия портрета по слугу жителя и у ключа-дня в
// `morning_notes`.
package platfriday

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lovegw/internal/platform"
	"lovegw/internal/profanity"
)

// intro — вступительная заметка рубрики.
//
// Текст, а не шаблон с подстановками: он читается людьми раз в неделю и должен
// быть написан, а не собран. Числа в нём — про сам архив, и меняются они раз в
// год; соврут они не раньше, чем устареет всё остальное.
//
// Разметка и смайл — ЗНАКАМИ НГС, и форма смайла тильдой не случайна: замер
// 07.09.2026 говорит, что формы разошлись по месту — в заметках писали
// «~flowers~», в комментариях «:::popcorn:::». Площадка разбирает обе, но писать
// в заметке надо так, как здесь писали люди.
const intro = `[b]Пятница.[/b] Клуб пятничных неудачников собирается в этом разделе с февраля 2011 года — мы на него не претендуем, просто садимся за соседний столик. ~popcorn~

Под площадкой лежит весь архив заметок: 117 736 записей и 10,8 миллиона реплик, самая ранняя написана 26 сентября 2004 года. Листать это можно, но до середины ленты идти пять с лишним тысяч страниц, и туда не доходит никто. Обидно: там люди.

Поэтому — пятничные вопросы. Весь вечер в этом треде будут появляться комментарии с вариантами ответа. В каждом настоящий кусок настоящего разговора, а угадать надо: в каком году это сказано, сколько реплик собралось под заметкой, что ответили на реплику дальше. Нажали вариант — сразу видно разгадку и ссылку на тот самый тред.

Имён мы не называем и обращения из реплик убираем: вопрос про слова, а не про людей.

Нажимать может кто угодно, даже не входя. В общий счёт идут ответы участников, по одному с человека, иначе проценты ничего не значат.

А спорить, как всегда, репликами. Угадали с первой — расскажите, по чему поняли. Не угадали — расскажите, чем ваш вариант был лучше правильного. Иногда так и есть.`

// Пороги отбора. Числа авторские: замерить «интересность» нечем, а вот
// последствия каждого видно сразу.
const (
	// minThread — короткий тред не даёт ни вопроса про число реплик, ни выбора
	// обманок: в нём попросту нет запасных реплик той же темы.
	minThread = 40
	// maxThread — у заметки на тысячу реплик «сколько собрала» становится
	// вопросом про рекорд, а не про чутьё.
	maxThread = 600
	// minBody / maxBody — заметка должна читаться с экрана целиком: короткая не
	// даёт примет эпохи, длинную никто не дочитает до вариантов.
	minBody = 150
	maxBody = 900
	// weekSpan — сколько дней вокруг сегодняшнего числа считать «той же
	// неделей». Семь: рубрика недельная, и материал берётся из того же времени
	// года — сентябрь про осень и школу, январь про праздники.
	weekSpan = 7
	// minYearsAgo — насколько старой обязана быть заметка. Два года: свежую
	// помнят, и вопрос превращается в проверку памяти о позавчерашнем.
	minYearsAgo = 2
	// pickPool — сколько кандидатов тянуть из базы, прежде чем выбирать. Запрос
	// один на неделю, поэтому пул большой: чем он шире, тем меньше шанс, что
	// фильтры оставят пусто.
	pickPool = 400
)

// ErrNoMaterial — из архива не удалось собрать ни одного вопроса. Не паника:
// у рубрики бывает пустая неделя, и служба обязана промолчать, а не упасть.
var ErrNoMaterial = errors.New("нет материала для вопросов")

// Question — готовый вопрос: текст реплики, варианты и разгадка.
type Question struct {
	// Kind — чем вопрос спрашивает; идёт в лог и в черновик, на страницу не
	// попадает (там за него говорит сам текст).
	Kind string
	// Body — тело реплики, которая задаёт вопрос: он сам и материал под ним.
	Body string
	Quiz platform.NewQuiz
}

// Issue — выпуск недели: вступительная заметка и вопросы к ней.
type Issue struct {
	// Week — метка недели («2026-W37»), она же ключ идемпотентности выпуска.
	Week      string
	Intro     string
	Questions []Question
}

// Source — откуда берётся материал. Интерфейсом, чтобы отбор проверялся на
// заготовке в тестовом Postgres, а не только на боевом архиве.
type Source interface {
	// Candidates — зеркальные заметки того же времени года с живым тредом.
	Candidates(ctx context.Context, day time.Time, limit int) ([]Note, error)
	// ReplyPairs — пары «реплика → настоящий ответ» внутри треда плюс запасные
	// реплики того же треда для обманок.
	ReplyPairs(ctx context.Context, noteID int64) ([]Pair, []string, error)
}

// Note — заметка-кандидат.
type Note struct {
	ID        int64
	Body      string
	Published time.Time
	Comments  int
}

// Pair — реплика и настоящий ответ на неё.
type Pair struct {
	SeedID int64
	Seed   string
	Answer string
}

// PG — Source поверх Postgres площадки.
type PG struct{ pool *pgxpool.Pool }

// NewPG — источник поверх готового пула.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// candidatesQuery — заметки того же времени года, зеркальные и с живым тредом.
//
// ЗЕРКАЛЬНЫЕ (id < NativeIDBase) — не из вкусовщины: рубрика показывает архив
// НГС, то есть разговоры, которых на площадке иначе никто не откроет. Своя
// заметка позапрошлой недели такого свойства не имеет, а объявление площадки в
// вопросе выглядело бы саморекламой.
//
// Окно дат считается по НОВОСИБИРСКОМУ времени: сайт жил по нему, и «пятница»
// в архиве — это его пятница, а не UTC.
//
// ПОРЯДОК — хеш от номера и НЕДЕЛИ, а не «сначала свежие», и это замер, а не
// вкусовщина. Пул у сегодняшнего окна — 1127 заметок, ровно размазанных по
// 2014–2024 (по сотне на год), а `ORDER BY n.id DESC LIMIT 400` отрезал от него
// ровно пять последних лет: первый черновик на боевом архиве дал пять вопросов
// из шести из сентября 2021-го. Рубрика при этом обещает архив, а вопрос «в
// каком году» с ответом «опять недавно» перестаёт быть вопросом.
//
// Случайности (`random()`) здесь быть не может: выпуск обязан быть
// детерминирован неделей — владелец смотрит черновик заранее, а опубликоваться
// должно ровно то, что он видел. Хеш даёт ту же перетасовку, но повторимую, и
// сдвигается сам собой на следующей неделе.
const candidatesQuery = `
	SELECT n.id, n.body, n.published_at, n.comment_count
	  FROM notes n
	 WHERE n.id < $1
	   AND n.status = 0
	   AND NOT n.stage
	   AND n.synth_of IS NULL
	   AND n.comment_count BETWEEN $2 AND $3
	   AND char_length(n.body) BETWEEN $4 AND $5
	   AND n.published_at < $6
	   AND abs(
	         (extract(doy FROM n.published_at AT TIME ZONE 'Asia/Novosibirsk')
	          - extract(doy FROM $7::timestamptz AT TIME ZONE 'Asia/Novosibirsk') + 182)::int % 365 - 182
	       ) <= $8
	 ORDER BY md5(n.id::text || $9)
	 LIMIT $10`

// Candidates — заметки того же времени года прошлых лет.
func (s *PG) Candidates(ctx context.Context, day time.Time, limit int) ([]Note, error) {
	rows, err := s.pool.Query(ctx, candidatesQuery,
		platform.NativeIDBase, minThread, maxThread, minBody, maxBody,
		day.AddDate(-minYearsAgo, 0, 0), day, weekSpan, WeekOf(day), limit)
	if err != nil {
		return nil, fmt.Errorf("кандидаты рубрики: %w", err)
	}
	defer rows.Close()

	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Body, &n.Published, &n.Comments); err != nil {
			return nil, fmt.Errorf("кандидаты рубрики: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ReplyPairs — настоящие пары «реплика → ответ» одного треда и запасные реплики.
//
// Ребро берётся ТОЛЬКО настоящее (`reply_source`), а не угаданное зеркалом по
// обращению: угаданное верно примерно вполовину, и вопрос «что ответили дальше»
// с угаданным ответом — это вопрос без правильного варианта.
//
// Обманки возвращаются отдельным списком и БЕЗ тех реплик, что отвечают на ту же
// затравку: две настоящие реакции на одну и ту же фразу — это два правильных
// ответа, и человек, выбравший «неверный», был бы прав.
func (s *PG) ReplyPairs(ctx context.Context, noteID int64) ([]Pair, []string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.body, b.body
		  FROM comments b
		  JOIN comments a ON a.id = b.reply_to_id AND a.note_id = b.note_id
		 WHERE b.note_id = $1 AND b.status = 0 AND a.status = 0
		   AND b.reply_source = $2
		 LIMIT 200`, noteID, int(platform.ReplyMobileTree))
	if err != nil {
		return nil, nil, fmt.Errorf("пары треда %d: %w", noteID, err)
	}
	var pairs []Pair
	for rows.Next() {
		var p Pair
		if err := rows.Scan(&p.SeedID, &p.Seed, &p.Answer); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("пары треда %d: %w", noteID, err)
		}
		pairs = append(pairs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("пары треда %d: %w", noteID, err)
	}
	return pairs, nil, nil
}

// Decoys — запасные реплики треда, не отвечающие на указанную затравку.
func (s *PG) Decoys(ctx context.Context, noteID, seedID int64, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT body FROM comments
		 WHERE note_id = $1 AND status = 0
		   AND id <> $2
		   AND (reply_to_id IS NULL OR reply_to_id <> $2)
		 LIMIT $3`, noteID, seedID, limit)
	if err != nil {
		return nil, fmt.Errorf("обманки треда %d: %w", noteID, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, fmt.Errorf("обманки треда %d: %w", noteID, err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// yearRe — год, названный в самом тексте.
//
// Отдельное правило, и оно оплачено разбором материала 11.09.2026: половина
// пятничных заметок архива — УТРЕННИЕ, а они по устройству называют дату прямо
// («Доброе утро! Сегодня 20 сентября 2019, пятница. Погода в Новосибирске +24»).
// Вопрос «в каком году это написано» на таком тексте бесплатен.
var yearRe = regexp.MustCompile(`\b(19|20)\d{2}\b`)

// nickRe — слово с заглавной буквы не в начале фразы: чаще всего это чужой ник.
//
// Грубо по устройству, и цена ошибки несимметрична: лишний пропуск стоит одного
// кандидата из четырёхсот, а ник, уехавший в вопрос, — это имя живого человека,
// выставленное в игру. Тем же доводом живут strangeName и SelfRole у народа.
var nickRe = regexp.MustCompile(`[^.!?…]\s+[А-ЯЁ][а-яё]{2,}`)

// addressRe — обращение в НАЧАЛЕ реплики: «Звёздная, ага..», «Алюминиевый
// сквозняк, нет».
//
// Отдельным правилом, потому что nickRe ловит ник только в СЕРЕДИНЕ фразы, а у
// обращения место ровно первое — и это самая частая его форма: на НГС адресат
// живёт префиксом в теле, а сюда доезжают как раз те реплики, у которых приём
// зеркала его НЕ распознал (у распознанных префикс срезан и стоит ребром).
// Поймано первым же черновиком на боевом архиве 11.09.2026: три вопроса из
// шести назвали живых людей по имени — при том что вступительная заметка
// обещает обратное.
//
// Правило нарочно без списка исключений: «Да, конечно» и «Хорошо, подумаю»
// отвергаются вместе с никами. Список зачинов, который спас бы их, — это ровно
// тот список, который через полгода разойдётся с разговором; а цена ошибки
// несимметрична, как у nickRe, и кандидатов у рубрики четыреста.
var addressRe = regexp.MustCompile(`^\S+(\s+\S+)?,`)

// looksAddressed — похоже ли, что реплика начинается с обращения по нику.
func looksAddressed(s string) bool { return addressRe.MatchString(s) }

// latinRe — латиница посреди русского текста: ники вида «Lady in red», «ML».
var latinRe = regexp.MustCompile(`\b[A-Za-z]{2,}\b`)

// clean — годится ли текст, чтобы показать его в вопросе.
func clean(s string, minRunes, maxRunes int) (string, bool) {
	s = strings.Join(strings.Fields(s), " ")
	n := len([]rune(s))
	if n < minRunes || n > maxRunes {
		return "", false
	}
	if profanity.FindMat(s) != "" {
		return "", false
	}
	if strings.Contains(s, "http") || strings.Contains(s, "@") {
		return "", false
	}
	if latinRe.MatchString(s) || nickRe.MatchString(s) {
		return "", false
	}
	return s, true
}

// Build собирает выпуск недели. Детерминирован: одна и та же неделя даёт один и
// тот же набор вопросов.
func Build(ctx context.Context, src Source, day time.Time, want int) (Issue, error) {
	week := WeekOf(day)
	notes, err := src.Candidates(ctx, day, pickPool)
	if err != nil {
		return Issue{}, err
	}
	rnd := rand.New(rand.NewSource(seedOf(week))) //nolint:gosec // жребий рубрики, не крипта
	rnd.Shuffle(len(notes), func(i, j int) { notes[i], notes[j] = notes[j], notes[i] })

	issue := Issue{Week: week, Intro: intro}
	kinds := []string{KindYear, KindCount, KindReply}
	for _, n := range notes {
		if len(issue.Questions) >= want {
			break
		}
		// Виды ЧЕРЕДУЮТСЯ, но заметка пробуется всеми по кругу, начиная с
		// очередного. Без этого выпуск застревал: не собрался вид — и все
		// оставшиеся кандидаты меряются им же, а вечер выходит короче
		// задуманного. Поймано тестом 11.09.2026 (два вопроса при плане в три).
		start := len(issue.Questions) % len(kinds)
		for i := range kinds {
			q, err := question(ctx, src, kinds[(start+i)%len(kinds)], n, rnd)
			if err != nil || q == nil {
				continue
			}
			issue.Questions = append(issue.Questions, *q)
			break
		}
	}
	if len(issue.Questions) == 0 {
		return Issue{}, ErrNoMaterial
	}
	return issue, nil
}

// Виды вопросов.
const (
	KindYear  = "год"
	KindCount = "реплики"
	KindReply = "ответ"
)

func question(ctx context.Context, src Source, kind string, n Note, rnd *rand.Rand) (*Question, error) {
	switch kind {
	case KindYear:
		return yearQuestion(n, rnd), nil
	case KindCount:
		return countQuestion(n, rnd), nil
	default:
		return replyQuestion(ctx, src, n, rnd)
	}
}

// yearQuestion — «в каком году это написано».
func yearQuestion(n Note, rnd *rand.Rand) *Question {
	body, ok := clean(n.Body, minBody, maxBody)
	if !ok || yearRe.MatchString(body) {
		return nil
	}
	nsk := n.Published.In(nskLoc())
	year := nsk.Year()
	opts, right := numberOptions(year, []int{-5, -2, 2, 5}, rnd, func(v int) bool {
		return v >= 2014 && v <= time.Now().Year()
	})
	if len(opts) < 3 {
		return nil
	}
	return &Question{
		Kind: KindYear,
		Body: "В каком году это написано?\n\n" + body,
		Quiz: platform.NewQuiz{
			Options:    numsToStrings(opts),
			Right:      right,
			Reveal:     fmt.Sprintf("%s, %s.", dateWord(nsk), repliesWord(n.Comments)),
			SourceNote: n.ID,
		},
	}
}

// countQuestion — «сколько реплик она собрала».
func countQuestion(n Note, rnd *rand.Rand) *Question {
	body, ok := clean(n.Body, minBody, maxBody)
	if !ok {
		return nil
	}
	opts, right := numberOptions(n.Comments, []int{-n.Comments * 4 / 5, -n.Comments / 2, n.Comments / 2}, rnd,
		func(v int) bool { return v > 5 })
	if len(opts) < 3 {
		return nil
	}
	return &Question{
		Kind: KindCount,
		Body: "Сколько реплик собрала эта заметка?\n\n" + body,
		Quiz: platform.NewQuiz{
			Options:    numsToStrings(opts),
			Right:      right,
			Reveal:     fmt.Sprintf("%s, %s.", dateWord(n.Published.In(nskLoc())), repliesWord(n.Comments)),
			SourceNote: n.ID,
		},
	}
}

// replyQuestion — «что ответили на эту реплику».
//
// Самый содержательный вид и самый хрупкий: обманки обязаны быть из ТОГО ЖЕ
// треда (чужая выдаёт себя темой с первого слова) и при этом не отвечать на ту
// же реплику.
func replyQuestion(ctx context.Context, src Source, n Note, rnd *rand.Rand) (*Question, error) {
	pairs, _, err := src.ReplyPairs(ctx, n.ID)
	if err != nil {
		return nil, err
	}
	dec, ok := src.(interface {
		Decoys(ctx context.Context, noteID, seedID int64, limit int) ([]string, error)
	})
	if !ok {
		return nil, nil
	}
	for _, p := range pairs {
		seed, ok1 := clean(p.Seed, 50, 190)
		answer, ok2 := clean(p.Answer, 40, 190)
		if !ok1 || !ok2 {
			continue
		}
		// Обращение по нику — стоп для ОБЕИХ реплик пары: и та, что показана
		// затравкой, и та, что объявлена ответом, читаются целиком.
		if looksAddressed(seed) || looksAddressed(answer) {
			continue
		}
		raw, err := dec.Decoys(ctx, n.ID, p.SeedID, 60)
		if err != nil {
			return nil, err
		}
		var decoys []string
		for _, d := range raw {
			if c, ok := clean(d, 40, 190); ok && !looksAddressed(c) && c != answer && c != seed {
				decoys = append(decoys, c)
			}
			if len(decoys) == 2 {
				break
			}
		}
		if len(decoys) < 2 {
			continue
		}
		opts := []string{answer, decoys[0], decoys[1]}
		right := rnd.Intn(3)
		opts[0], opts[right] = opts[right], opts[0]
		return &Question{
			Kind: KindReply,
			Body: "Что ответили на эту реплику?\n\n«" + seed + "»",
			Quiz: platform.NewQuiz{
				Options:    opts,
				Right:      right,
				Reveal:     fmt.Sprintf("%s. Две другие — тоже настоящие реплики того треда, но сказаны в других его ветках.", dateWord(n.Published.In(nskLoc()))),
				SourceNote: n.ID,
			},
		}, nil
	}
	return nil, nil
}

// numberOptions — правильное число и ложные рядом с ним, вперемешку.
func numberOptions(right int, deltas []int, rnd *rand.Rand, ok func(int) bool) ([]int, int) {
	seen := map[int]bool{right: true}
	out := []int{right}
	for _, d := range deltas {
		v := right + d
		if d == 0 || seen[v] || !ok(v) {
			continue
		}
		seen[v] = true
		out = append(out, v)
		if len(out) == 3 {
			break
		}
	}
	if len(out) < 3 {
		return nil, 0
	}
	rnd.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	for i, v := range out {
		if v == right {
			return out, i
		}
	}
	return out, 0
}

func numsToStrings(v []int) []string {
	out := make([]string, len(v))
	for i, n := range v {
		out[i] = strconv.Itoa(n)
	}
	return out
}

// seedOf — жребий недели. FNV, а не время: выпуск обязан повторяться.
func seedOf(week string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(week))
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

// WeekOf — метка недели ISO, как у дайджеста («2026-W37»). Она же ключ
// идемпотентности выпуска.
func WeekOf(t time.Time) string {
	y, w := t.In(nskLoc()).ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

func nskLoc() *time.Location {
	loc, err := time.LoadLocation("Asia/Novosibirsk")
	if err != nil {
		return time.FixedZone("NSK", 7*3600)
	}
	return loc
}

var months = [...]string{"января", "февраля", "марта", "апреля", "мая", "июня",
	"июля", "августа", "сентября", "октября", "ноября", "декабря"}

func dateWord(t time.Time) string {
	return fmt.Sprintf("%d %s %d года", t.Day(), months[int(t.Month())-1], t.Year())
}

func repliesWord(n int) string {
	switch {
	case n%10 == 1 && n%100 != 11:
		return fmt.Sprintf("%d реплика", n)
	case n%10 >= 2 && n%10 <= 4 && (n%100 < 10 || n%100 >= 20):
		return fmt.Sprintf("%d реплики", n)
	default:
		return fmt.Sprintf("%d реплик", n)
	}
}
