package platform

// Запись: правила, общие для заметки и комментария.
//
// Главное правило площадки — УЧАСТНИК ТОЛЬКО ПИШЕТ. Правка и удаление живут у
// модератора, и единственное исключение — своя заметка первые десять минут и
// один раз. Причина не в экономии работы: тред это чужие ответы на твои слова,
// правка задним числом делает их бессмысленными, а удаление оставляет в ветке
// дыру. На НГС снос реплики — тоже действие МОДЕРАТОРА, и это часть той
// преемственности, ради которой всё затевается.
//
// «Убрать своё» у человека при этом есть, но рычаг другой — отзыв согласия на
// распространение (consent.go): он прячет ВСЕ публикации разом и немедленно.
// Это право субъекта по 152-ФЗ, а не редакторское удаление, и путать их нельзя:
// первое обязано работать самообслуживанием, второе — нет.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// EditWindow — сколько живёт окно правки своей заметки. Окно на опечатку, а не
// на смену позиции.
const EditWindow = 10 * time.Minute

// MaxNickRunes — потолок длины ника. На НГС длиннее не встречается, а подпись
// рисуется в узкой колонке автора.
const MaxNickRunes = 40

var (
	// ErrNotMember — писать может только вошедший участник. Тень (её завело
	// зеркало) автором у нас не считается: за ней никто не доказал владения.
	ErrNotMember = errors.New("публиковать может только участник")
	// ErrBanned — срок запрета ещё идёт.
	ErrBanned = errors.New("публикации запрещены до конца срока")
	// ErrHiddenAll — человек отозвал согласие на распространение. Публиковать
	// при отозванном согласии значило бы распространять то, на что согласия нет,
	// а спрятать новое сразу после публикации — издевательство над обоими.
	ErrHiddenAll = errors.New("ваши публикации скрыты: сначала верните согласие на распространение")
	// ErrRateLimited — слишком часто.
	ErrRateLimited = errors.New("слишком часто")
	// ErrThreadLocked — обсуждение закрыл НАШ модератор (а не отметка НГС).
	ErrThreadLocked = errors.New("обсуждение закрыто")
	// ErrEditWindowClosed — заметку править уже нельзя.
	ErrEditWindowClosed = errors.New("окно правки закрыто")
	// ErrNotYours — чужая запись.
	ErrNotYours = errors.New("это не ваша запись")
	// ErrBadNick — ник не годится.
	ErrBadNick = errors.New("ник не годится")
)

// Виды объектов очереди проверки. Строками, а не числами: очередь читают люди и
// SQL, и «note» понятнее нуля.
const (
	SubjectNote    = "note"
	SubjectComment = "comment"
)

// rateRule — не больше Max публикаций за Window.
type rateRule struct {
	Window time.Duration
	Max    int
}

// Пороги частоты. Меряются по НАТИВНЫМ публикациям автора: зеркальный след
// прошлых лет к тому, как часто человек пишет здесь, отношения не имеет.
//
// Числа взяты из плана эпика E и защищают от шторма, а не от разговорчивости:
// тридцать реплик в час — это вдвое больше, чем пишет самый быстрый комментатор
// зеркала в свой самый людный час.
var (
	noteRates    = []rateRule{{5 * time.Minute, 1}, {24 * time.Hour, 5}}
	commentRates = []rateRule{{10 * time.Second, 1}, {time.Hour, 30}}
)

const (
	notesRecentQuery    = `SELECT count(*) FROM notes    WHERE author_id = $1 AND id >= $2 AND published_at > $3`
	commentsRecentQuery = `SELECT count(*) FROM comments WHERE author_id = $1 AND id >= $2 AND published_at > $3`
)

// writeGuard — общая проверка «этому человеку сейчас можно публиковать».
//
// Читается ВНУТРИ той же транзакции, что и вставка: иначе между проверкой и
// записью успевает пройти бан или отзыв согласия, и запрет исполнится с
// задержкой в одну публикацию.
func writeGuard(ctx context.Context, q querier, userID int64) error {
	var (
		kind       Kind
		hideAll    bool
		banned     *time.Time
		anonymized *time.Time
	)
	err := q.QueryRow(ctx,
		`SELECT kind, hide_all, banned_until, anonymized_at FROM users WHERE id = $1`, userID).
		Scan(&kind, &hideAll, &banned, &anonymized)
	if err != nil {
		return fmt.Errorf("проверка автора %d: %w", userID, err)
	}
	switch {
	case anonymized != nil, kind != KindMember:
		return ErrNotMember
	case banned != nil && banned.After(time.Now()):
		return ErrBanned
	case hideAll:
		return ErrHiddenAll
	}
	return nil
}

// enforceRate проверяет пороги частоты по нативным публикациям автора.
func enforceRate(ctx context.Context, q querier, query string, authorID int64, now time.Time, rules []rateRule) error {
	for _, r := range rules {
		var n int
		if err := q.QueryRow(ctx, query, authorID, NativeIDBase, now.Add(-r.Window)).Scan(&n); err != nil {
			return fmt.Errorf("частота публикаций автора %d: %w", authorID, err)
		}
		if n >= r.Max {
			return ErrRateLimited
		}
	}
	return nil
}

// enqueueCheck ставит публикацию в очередь проверки. Той же транзакцией, что и
// сама публикация: «опубликовано, но в очередь не попало» — состояние, которого
// не должно быть вовсе.
//
// noteID хранится рядом, хотя у комментария он выводится из самой строки: по
// нему очередь строит ссылку на место в треде, а лезть за ней в comments на
// каждую строку показа незачем — заметка у комментария не меняется никогда.
//
// Повторная постановка (правка заметки в окне) СБРАСЫВАЕТ и решение человека:
// текст стал другим, и прежний вердикт относился не к нему. Обжалование при
// этом тоже снимается — обжаловать нечего, публикация снова видима.
func enqueueCheck(ctx context.Context, q querier, kind string, id, noteID, authorID int64) error {
	_, err := q.Exec(ctx, `
		INSERT INTO moderation_queue (subject_kind, subject_id, note_id, author_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (subject_kind, subject_id) DO UPDATE
		   SET queued_at = now(), checked_at = NULL, verdict = NULL, attempts = 0,
		       category = '', reason = '', quote = '', model = '', prompt_sha = NULL,
		       appealed_at = NULL, decided_at = NULL, decided_by = NULL, decision = NULL`,
		kind, id, nullID(noteID), nullID(authorID))
	return wrapf(err, "очередь проверки %s %d", kind, id)
}

// EditNote правит СВОЮ заметку в окне после публикации.
//
// Окно закрывают три вещи, и любая из них — насовсем: десять минут, первый
// комментарий и уже сделанная правка. Комментарий здесь важнее времени: текст,
// изменившийся под чужим ответом, выставляет ответившего дураком, и никакой
// таймер этого не исправляет. Однократность — из того же соображения: «поправить
// опечатку» это одно действие, а серия правок в окне есть та же смена позиции,
// только мелкими шагами.
//
// Правило целиком выражено состоянием строки, поэтому пережить рестарт и гонку
// ему нечем не помочь: edited_at и comment_count лежат в той же таблице и берутся
// под FOR UPDATE.
func (p *Platform) EditNote(ctx context.Context, userID, noteID int64, body string) error {
	body, err := cleanBody(body)
	if err != nil {
		return err
	}
	if userID == 0 {
		return ErrNotYours
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("правка заметки %d: %w", noteID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	var (
		author    *int64
		status    Status
		count     int
		published time.Time
		edited    *time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT author_id, status, comment_count, published_at, edited_at
		  FROM notes WHERE id = $1 FOR UPDATE`, noteID).
		Scan(&author, &status, &count, &published, &edited)
	if err != nil {
		return fmt.Errorf("правка заметки %d: %w", noteID, err)
	}
	// Автор проверяется по НАСТОЯЩЕМУ author_id, поэтому правило одинаково
	// работает и для анонимной заметки: она хранит своего автора.
	if idOf(author) != userID {
		return ErrNotYours
	}
	switch {
	case status != StatusVisible:
		return ErrNotFound
	case !IsNative(noteID):
		// Зеркальную заметку писали не у нас, и править её тут значило бы
		// расходиться с оригиналом молча.
		return ErrEditWindowClosed
	case count > 0, edited != nil, time.Since(published) >= EditWindow:
		return ErrEditWindowClosed
	}
	if _, err := tx.Exec(ctx,
		`UPDATE notes SET body = $2, edited_at = now() WHERE id = $1`, noteID, body); err != nil {
		return fmt.Errorf("правка заметки %d: %w", noteID, err)
	}
	// Текст стал другим — прежняя проверка к нему больше не относится.
	if err := enqueueCheck(ctx, tx, SubjectNote, noteID, noteID, userID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("правка заметки %d: %w", noteID, err)
	}
	return nil
}

// SetOwnNick — смена собственного ника участником.
//
// Отдельно от SetNick (та служебная, без проверок): здесь текст пришёл от
// человека, и вдобавок поднимается nick_custom — с этого момента вход по анкете
// НГС ник больше не переписывает. Без флага обещание «ник вы меняете сами» из
// текста согласия отменялось бы следующим же входом.
func (p *Platform) SetOwnNick(ctx context.Context, userID int64, nick string) error {
	nick, err := cleanNick(nick)
	if err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE users SET nick = $2, nick_custom = true
		 WHERE id = $1 AND kind = $3 AND anonymized_at IS NULL`, userID, nick, KindMember)
	if err != nil {
		return fmt.Errorf("смена ника %d: %w", userID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	return nil
}

// cleanNick приводит ник к виду, в котором его можно показать. Пробелы внутри
// разрешены (на НГС такие ники обычны), а вот управляющие знаки — нет: ими
// подпись ломается или подделывается под чужую.
func cleanNick(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "", fmt.Errorf("%w: пусто", ErrBadNick)
	}
	if utf8.RuneCountInString(s) > MaxNickRunes {
		return "", fmt.Errorf("%w: длиннее %d знаков", ErrBadNick, MaxNickRunes)
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", fmt.Errorf("%w: невидимые знаки", ErrBadNick)
		}
	}
	return s, nil
}
