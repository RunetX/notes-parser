package platform

// Пятничный вопрос: комментарий, под которым стоят варианты ответа.
//
// Рубрика площадки (эпик J). Вечером в пятницу под вступительной заметкой один
// за другим появляются вопросы из архива — «в каком году это написано»,
// «сколько реплик собрала заметка», «что ответили на эту реплику», — человек
// жмёт вариант, видит разгадку и ссылку на настоящий тред, а спорит о нём
// репликами тут же, в этом же дереве.
//
// ЗАЧЕМ ЭТО ЗДЕСЬ, А НЕ ОТДЕЛЬНОЙ СТРАНИЦЕЙ. Игра сбоку от площадки была бы
// игрой сбоку от площадки: сыграл и ушёл. Вопрос, заданный комментарием, попадает
// в тред — то есть туда, где люди и разговаривают, — и разгадка становится не
// концом, а поводом («а я помню этот тред»). Ради этого он и сделан НАДСТРОЙКОЙ
// над обычной репликой (см. миграцию 0029): дерево, модерация, живой добор,
// вынос в каналы и обезличивание достаются ему даром, а второго пути рендера в
// морде не заводится.
//
// ЧТО ПОКАЗЫВАЕТСЯ ДО ОТВЕТА — вопрос и варианты, и больше ничего. Ни счётчиков
// (иначе первый же десяток голосов подсказывает всем остальным), ни правильного,
// ни разгадки. Это правило держится ЗДЕСЬ, в `NoteQuiz`, а не в шаблоне: счётчик,
// уехавший в разметку «на всякий случай», однажды окажется виден в исходнике
// страницы, и рубрика кончится в тот же вечер.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Потолки вопроса. Числа авторские, замерить их пока нечем: рубрика новая.
const (
	// QuizMinOptions / QuizMaxOptions — сколько вариантов бывает у вопроса.
	// Два — это «да/нет», пять — предел строки на телефоне; за ними вопрос
	// перестаёт читаться с экрана, а не становится интереснее.
	QuizMinOptions = 2
	QuizMaxOptions = 5

	// QuizOptionRunes — потолок длины варианта. Вариант «что ответили дальше» —
	// это настоящая реплика из архива, а их медиана 64 знака; двести дают запас
	// на самые длинные, не превращая кнопку в абзац.
	QuizOptionRunes = 200

	// QuizRevealRunes — потолок разгадки. Она пишется одной-двумя фразами:
	// дата, число реплик, чем дело кончилось.
	QuizRevealRunes = 600
)

var (
	// ErrNotQuizMaster — вопрос задаёт площадка, а не участник.
	ErrNotQuizMaster = errors.New("задавать вопросы вправе только площадка")
	// ErrBadQuiz — вопрос собран неверно: мало вариантов, пустой вариант,
	// правильный за пределами списка.
	ErrBadQuiz = errors.New("вопрос собран неверно")
	// ErrNoQuiz — у этого комментария нет вариантов ответа.
	ErrNoQuiz = errors.New("это не вопрос")
)

// NewQuiz — варианты, которые публикуются ВМЕСТЕ с комментарием.
//
// Отдельного «создать вопрос» нет намеренно: он кладётся той же транзакцией,
// что и сама реплика (см. CreateComment), по общему правилу площадки —
// «опубликовано, но в очередь не попало» существовать не должно. Комментарий с
// вариантами, которых нет, это ровно такое состояние: вопрос без ответов.
type NewQuiz struct {
	Options    []string
	Right      int
	Reveal     string
	SourceNote int64
}

// QuizOption — вариант и сколько его выбрали. Votes заполнен только для того,
// кто уже ответил (см. NoteQuiz).
type QuizOption struct {
	Text  string
	Votes int
}

// Quiz — вопрос под комментарием, как его увидит страница.
type Quiz struct {
	CommentID  int64
	Options    []QuizOption
	Right      int
	Reveal     string
	SourceNote int64
	// MyChoice — что выбрал смотрящий; -1, если не отвечал (или не входил).
	MyChoice int
	Total    int
}

// Answered — ответил ли смотрящий. У гостя всегда false: ядро о куке не знает,
// и «ответил» для него решает морда (см. web/quiz.go).
func (q Quiz) Answered() bool { return q.MyChoice >= 0 }

// QuizAnswer — что нажали.
type QuizAnswer struct {
	UserID    int64
	NoteID    int64
	CommentID int64
	Choice    int
}

func (in *NewQuiz) clean() error {
	if in == nil {
		return ErrBadQuiz
	}
	if len(in.Options) < QuizMinOptions || len(in.Options) > QuizMaxOptions {
		return fmt.Errorf("%w: вариантов %d", ErrBadQuiz, len(in.Options))
	}
	for i, o := range in.Options {
		o = strings.TrimSpace(o)
		if o == "" {
			return fmt.Errorf("%w: вариант %d пуст", ErrBadQuiz, i+1)
		}
		if len([]rune(o)) > QuizOptionRunes {
			return fmt.Errorf("%w: вариант %d длиннее %d знаков", ErrBadQuiz, i+1, QuizOptionRunes)
		}
		in.Options[i] = o
	}
	if in.Right < 0 || in.Right >= len(in.Options) {
		return fmt.Errorf("%w: правильный вариант %d", ErrBadQuiz, in.Right)
	}
	in.Reveal = strings.TrimSpace(in.Reveal)
	if len([]rune(in.Reveal)) > QuizRevealRunes {
		return fmt.Errorf("%w: разгадка длиннее %d знаков", ErrBadQuiz, QuizRevealRunes)
	}
	return nil
}

// quizAskGuard — кто вправе задать вопрос.
//
// Площадка (KindService) или администратор, и это не осторожность ради
// осторожности. Вопрос с вариантами выглядит как голос самой площадки: под ним
// стоят кнопки, которых нет ни у кого другого, и ответ «правильный» объявляет
// она. Дай эту дверь участнику — и первым же вечером под чужой заметкой встанет
// вопрос «кто здесь дурак» с тремя вариантами, а снимать его придётся
// модерацией. Администратор в списке ради ручного вечера: рубрику заводят
// раньше, чем её служба.
func quizAskGuard(ctx context.Context, q querier, userID int64) error {
	var (
		kind Kind
		role Role
	)
	err := q.QueryRow(ctx, `SELECT kind, role FROM users WHERE id = $1`, userID).Scan(&kind, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("автор %d: %w", userID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("право задать вопрос: %w", err)
	}
	if kind != KindService && role < RoleAdmin {
		return ErrNotQuizMaster
	}
	return nil
}

// insertQuiz кладёт варианты рядом с уже вставленным комментарием. Зовётся
// изнутри CreateComment, той же транзакцией.
func insertQuiz(ctx context.Context, q querier, commentID, noteID int64, in *NewQuiz) error {
	_, err := q.Exec(ctx, `
		INSERT INTO quiz_questions (comment_id, note_id, options, right_idx, reveal, source_note)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		commentID, noteID, in.Options, in.Right, in.Reveal, nullID(in.SourceNote))
	if err != nil {
		return fmt.Errorf("вопрос к комментарию %d: %w", commentID, err)
	}
	return nil
}

// AnswerQuiz записывает ответ. Первый ответ окончателен.
//
// ПЕРЕГОЛОСОВАТЬ НЕЛЬЗЯ, и это отличает вопрос от реакции, у которой то же
// устройство: нажатие реакции меняет её и снимает, а здесь после разгадки
// правильный вариант известен — разреши смену, и через минуту его выбрали бы
// все, а проценты под вопросом перестали бы что-либо значить. Повторное нажатие
// поэтому не ошибка, а тишина: двойной клик и возврат по «назад» — обычное дело,
// ругаться на них незачем.
func (p *Platform) AnswerQuiz(ctx context.Context, in QuizAnswer) error {
	if in.UserID == 0 || in.CommentID == 0 {
		return errors.New("ответ без автора или без вопроса")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ответ на вопрос: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	// Те же ворота, что у реакции: тень не участник, забаненный молчит,
	// отозвавший согласие не оставляет новых следов в общем счёте.
	if err := writeGuard(ctx, tx, in.UserID); err != nil {
		return err
	}

	// Вопрос и его заметка проверяются ВМЕСТЕ — ответ приходит формой, а в
	// форме можно поменять что угодно, и голос уехал бы к чужому треду.
	var (
		noteID int64
		n      int
		status Status
		locked bool
	)
	err = tx.QueryRow(ctx, `
		SELECT q.note_id, array_length(q.options, 1), n.status, n.locked
		  FROM quiz_questions q
		  JOIN notes n ON n.id = q.note_id
		 WHERE q.comment_id = $1`, in.CommentID).Scan(&noteID, &n, &status, &locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("вопрос %d: %w", in.CommentID, ErrNoQuiz)
	}
	if err != nil {
		return fmt.Errorf("ответ на вопрос %d: %w", in.CommentID, err)
	}
	if in.NoteID != 0 && in.NoteID != noteID {
		return fmt.Errorf("вопрос %d: %w", in.CommentID, ErrNotFound)
	}
	if status != StatusVisible {
		return fmt.Errorf("заметка %d: %w", noteID, ErrNotFound)
	}
	// Замок треда закрывает и ответы: запертый разговор заперт целиком.
	if locked {
		return ErrThreadLocked
	}
	if in.Choice < 0 || in.Choice >= n {
		return fmt.Errorf("%w: выбран вариант %d из %d", ErrBadQuiz, in.Choice, n)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO quiz_votes (comment_id, note_id, user_id, choice)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT ON CONSTRAINT quiz_votes_one DO NOTHING`,
		in.CommentID, noteID, in.UserID, in.Choice); err != nil {
		return fmt.Errorf("ответ на вопрос %d: %w", in.CommentID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ответ на вопрос %d: %w", in.CommentID, err)
	}
	return nil
}

// NoteQuiz — вопросы заметки со счётом, ОДНИМ запросом на страницу.
//
// Ключ — id комментария, задавшего вопрос. Два запроса, а не по одному на
// реплику, — та же причина, по которой note_id денормализован в обеих таблицах
// (см. 0029): под вступительной заметкой к вечеру набирается тред, и запрос на
// каждую его строку положил бы страницу на том единственном ядре, где живёт
// зеркало.
//
// СЧЁТ ОТДАЁТСЯ ТОЛЬКО ОТВЕТИВШЕМУ. Не ответившему проценты не просто бесполезны
// — они подсказка: большинство обычно право, и вопрос превратился бы в вопрос
// «что думает большинство». Поэтому чужие голоса тут не прячутся шаблоном, а не
// выезжают из ядра вовсе.
func (p *Platform) NoteQuiz(ctx context.Context, viewerID, noteID int64) (map[int64]Quiz, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT comment_id, options, right_idx, reveal, coalesce(source_note, 0)
		  FROM quiz_questions WHERE note_id = $1`, noteID)
	if err != nil {
		return nil, fmt.Errorf("вопросы заметки %d: %w", noteID, err)
	}
	out := map[int64]Quiz{}
	for rows.Next() {
		var (
			q    Quiz
			opts []string
		)
		if err := rows.Scan(&q.CommentID, &opts, &q.Right, &q.Reveal, &q.SourceNote); err != nil {
			return nil, fmt.Errorf("вопросы заметки %d: %w", noteID, err)
		}
		q.MyChoice = -1
		q.Options = make([]QuizOption, len(opts))
		for i, o := range opts {
			q.Options[i] = QuizOption{Text: o}
		}
		out[q.CommentID] = q
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("вопросы заметки %d: %w", noteID, err)
	}
	if len(out) == 0 || viewerID == 0 {
		return out, nil
	}

	// Свои ответы — отдельным запросом и только они: чей это голос, дальше
	// ядра не уходит никогда (то же правило, что у реакций).
	mine, err := p.pool.Query(ctx,
		`SELECT comment_id, choice FROM quiz_votes WHERE note_id = $1 AND user_id = $2`,
		noteID, viewerID)
	if err != nil {
		return nil, fmt.Errorf("свои ответы в заметке %d: %w", noteID, err)
	}
	answered := map[int64]int{}
	for mine.Next() {
		var cid int64
		var choice int
		if err := mine.Scan(&cid, &choice); err != nil {
			return nil, fmt.Errorf("свои ответы в заметке %d: %w", noteID, err)
		}
		answered[cid] = choice
	}
	if err := mine.Err(); err != nil {
		return nil, fmt.Errorf("свои ответы в заметке %d: %w", noteID, err)
	}
	if len(answered) == 0 {
		return out, nil
	}

	tally, err := p.pool.Query(ctx, `
		SELECT comment_id, choice, count(*)
		  FROM quiz_votes WHERE note_id = $1
		 GROUP BY comment_id, choice`, noteID)
	if err != nil {
		return nil, fmt.Errorf("счёт ответов в заметке %d: %w", noteID, err)
	}
	counts := map[int64]map[int]int{}
	for tally.Next() {
		var cid int64
		var choice, n int
		if err := tally.Scan(&cid, &choice, &n); err != nil {
			return nil, fmt.Errorf("счёт ответов в заметке %d: %w", noteID, err)
		}
		if counts[cid] == nil {
			counts[cid] = map[int]int{}
		}
		counts[cid][choice] = n
	}
	if err := tally.Err(); err != nil {
		return nil, fmt.Errorf("счёт ответов в заметке %d: %w", noteID, err)
	}

	for cid, choice := range answered {
		q, ok := out[cid]
		if !ok {
			continue
		}
		q.MyChoice = choice
		for i := range q.Options {
			n := counts[cid][i]
			q.Options[i].Votes = n
			q.Total += n
		}
		out[cid] = q
	}
	return out, nil
}

// QuizStat — сводка вопроса для команд и отчётов: сколько ответили и сколько
// угадали. Наружу, на страницу, это не идёт — там счёт показывается долями по
// вариантам.
type QuizStat struct {
	CommentID  int64
	NoteID     int64
	Asked      time.Time
	Votes      int
	Right      int
	SourceNote int64
}

// QuizStats — сводка по всем вопросам заметки, для `platform doctor` и команды
// рубрики. Считается одним запросом.
func (p *Platform) QuizStats(ctx context.Context, noteID int64) ([]QuizStat, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT q.comment_id, q.note_id, q.created_at, coalesce(q.source_note, 0),
		       count(v.user_id),
		       count(v.user_id) FILTER (WHERE v.choice = q.right_idx)
		  FROM quiz_questions q
		  LEFT JOIN quiz_votes v ON v.comment_id = q.comment_id
		 WHERE q.note_id = $1
		 GROUP BY q.comment_id, q.note_id, q.created_at, q.source_note
		 ORDER BY q.created_at`, noteID)
	if err != nil {
		return nil, fmt.Errorf("сводка вопросов заметки %d: %w", noteID, err)
	}
	defer rows.Close()

	var out []QuizStat
	for rows.Next() {
		var s QuizStat
		if err := rows.Scan(&s.CommentID, &s.NoteID, &s.Asked, &s.SourceNote, &s.Votes, &s.Right); err != nil {
			return nil, fmt.Errorf("сводка вопросов заметки %d: %w", noteID, err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// FridayIssue — вступительная заметка недели. Ноль означает «этой недели ещё не
// было»: рабочее состояние, а не ошибка.
func (p *Platform) FridayIssue(ctx context.Context, week string) (int64, error) {
	var id int64
	err := p.pool.QueryRow(ctx, `SELECT note_id FROM friday_issues WHERE week = $1`, week).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("выпуск рубрики %s: %w", week, err)
	}
	return id, nil
}

// StartFridayIssue помечает заметку выпуском недели.
//
// Возвращает номер УЖЕ заведённой заметки, если неделя занята: ключ-неделя и
// есть однократность рубрики (см. миграцию 0029), поэтому повтор отвечает
// адресом, а не отказом, — спрашивали, где выпуск, а не разрешения.
func (p *Platform) StartFridayIssue(ctx context.Context, week string, noteID int64) (int64, error) {
	var got int64
	err := p.pool.QueryRow(ctx, `
		INSERT INTO friday_issues (week, note_id) VALUES ($1, $2)
		ON CONFLICT (week) DO UPDATE SET week = EXCLUDED.week
		RETURNING note_id`, week, noteID).Scan(&got)
	if err != nil {
		return 0, fmt.Errorf("выпуск рубрики %s: %w", week, err)
	}
	return got, nil
}

// FridayAsked — сколько вопросов уже стоит под заметкой выпуска. Считается по
// самим вопросам, а не счётчиком рядом: счётчик разошёлся бы с фактом на первом
// же откате транзакции.
func (p *Platform) FridayAsked(ctx context.Context, noteID int64) (int, error) {
	var n int
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM quiz_questions WHERE note_id = $1`, noteID).Scan(&n); err != nil {
		return 0, fmt.Errorf("вопросов под заметкой %d: %w", noteID, err)
	}
	return n, nil
}
