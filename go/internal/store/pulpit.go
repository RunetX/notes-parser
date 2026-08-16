package store

// Методы амвона (пакет pulpit): очередь своих реплик под новыми заметками,
// решения по ответам на ответы и рантайм-флаги (таблица settings, миграция v9).
//
// Дубль комментария на сайте необратим — отозвать реплику нечем, — поэтому
// однократность держится не транзакциями, а CAS-переходами состояния (как
// TryMarkReplyProcessed в bridge.go): занять заметку может ровно один вызов
// (INSERT OR IGNORE), а перевод queued → posting пишется ДО отправки. Строку,
// застрявшую в posting, не переотправляют никогда: её судьбу решает
// верификация треда.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Состояния строки амвона (общие для реплик под заметками и ответов).
const (
	PulpitQueued    = "queued"    // заметка занята, реплика ещё не отправлена
	PulpitPosting   = "posting"   // отправка началась; переотправка запрещена
	PulpitPosted    = "posted"    // сайт принял POST, реплику в треде ещё не нашли
	PulpitConfirmed = "confirmed" // реплика найдена в треде (известен comment_id)
	PulpitMissing   = "missing"   // тред прочитан, реплики в нём нет
	PulpitVanished  = "vanished"  // заметка исчезла с сайта — снесли её, а не нас
	PulpitSkipped   = "skipped"   // сознательно не пишем (холодный старт, свежесть, потолок, монетка)
	PulpitFailed    = "failed"    // не сгенерировали/некому отправить
)

// Ключи рантайм-флагов амвона.
const (
	FlagPulpitEnabled   = "pulpit.enabled"
	FlagPulpitOffReason = "pulpit.off_reason"
	FlagPulpitOffAt     = "pulpit.off_at"
	FlagPulpitOffBy     = "pulpit.off_by"
)

// PulpitComment — своя реплика под заметкой: одна строка на заметку.
type PulpitComment struct {
	NoteID    string
	State     string
	Reason    string
	Form      string
	Text      string
	CommentID int64 // 0 — реплика ещё не найдена в треде
	SeenAt    time.Time
	PostedAt  time.Time
	CheckedAt time.Time
	Checks    int
	CreatedAt time.Time
}

// PulpitReply — решение по чужой реплике: отвечать или нет.
type PulpitReply struct {
	ReplyToID int64
	NoteID    string
	AuthorID  string
	State     string
	Reason    string
	Text      string
	CommentID int64
	DecidedAt time.Time
	PostedAt  time.Time
}

const pulpitColumns = `note_id, state, reason, form, text, comment_id,
       seen_at, posted_at, checked_at, checks, created_at`

const pulpitReplyColumns = `reply_to_id, note_id, author_id, state, reason, text,
       comment_id, decided_at, posted_at`

// TryClaimPulpitNote занимает заметку под реплику. false — её уже занял другой
// вход (свой обход ленты и колбэк зеркала идут параллельно и сходятся здесь).
func (s *Store) TryClaimPulpitNote(ctx context.Context, noteID, state, reason string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO pulpit_comments (note_id, state, reason, seen_at, created_at)
		VALUES (?, ?, ?, ?, ?)`, noteID, state, reason, fmtTime(now), fmtTime(now))
	if err != nil {
		return false, fmt.Errorf("занять заметку %s под реплику: %w", noteID, err)
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// TryStartPulpitPost переводит queued → posting вместе с текстом реплики: это и
// есть точка невозврата, дальше повторной отправки не будет никогда. false —
// строка уже не в queued (параллельный цикл или чужая горутина).
func (s *Store) TryStartPulpitPost(ctx context.Context, noteID, form, text string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE pulpit_comments SET state = ?, form = ?, text = ?, posted_at = ?
		WHERE note_id = ? AND state = ?`,
		PulpitPosting, form, text, fmtTime(now), noteID, PulpitQueued)
	if err != nil {
		return false, fmt.Errorf("реплика под заметкой %s: %w", noteID, err)
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// CASPulpitState переводит строку из одного состояния в другое. false —
// состояние уже другое: переход не выполнен.
func (s *Store) CASPulpitState(ctx context.Context, noteID, from, to, reason string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE pulpit_comments SET state = ?, reason = ? WHERE note_id = ? AND state = ?`,
		to, reason, noteID, from)
	if err != nil {
		return false, fmt.Errorf("реплика под заметкой %s: %w", noteID, err)
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// SetPulpitState выставляет состояние безусловно (итог верификации).
func (s *Store) SetPulpitState(ctx context.Context, noteID, state, reason string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE pulpit_comments SET state = ?, reason = ?, checked_at = ? WHERE note_id = ?`,
		state, reason, fmtTime(now), noteID)
	if err != nil {
		return fmt.Errorf("реплика под заметкой %s: %w", noteID, err)
	}
	return nil
}

// ConfirmPulpitComment фиксирует найденную в треде свою реплику: её id — якорь
// и для ответов на ответы, и для проверки «не удалили ли».
func (s *Store) ConfirmPulpitComment(ctx context.Context, noteID string, commentID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE pulpit_comments
		SET state = ?, reason = '', comment_id = ?, checked_at = ?
		WHERE note_id = ?`, PulpitConfirmed, commentID, fmtTime(now), noteID)
	if err != nil {
		return fmt.Errorf("подтверждение реплики под заметкой %s: %w", noteID, err)
	}
	return nil
}

// BumpPulpitCheck отмечает неудачную проверку треда и возвращает их число.
// Считаются только успешно прочитанные страницы: сбой загрузки сюда не доходит.
func (s *Store) BumpPulpitCheck(ctx context.Context, noteID string, now time.Time) (int, error) {
	var checks int
	err := s.db.QueryRowContext(ctx, `
		UPDATE pulpit_comments SET checks = checks + 1, checked_at = ?
		WHERE note_id = ? RETURNING checks`, fmtTime(now), noteID).Scan(&checks)
	if err != nil {
		return 0, fmt.Errorf("проверка реплики под заметкой %s: %w", noteID, err)
	}
	return checks, nil
}

// PulpitNote возвращает строку амвона по заметке. ErrNotFound — заметку не брали.
func (s *Store) PulpitNote(ctx context.Context, noteID string) (PulpitComment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+pulpitColumns+` FROM pulpit_comments WHERE note_id = ?`, noteID)
	if err != nil {
		return PulpitComment{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return PulpitComment{}, err
		}
		return PulpitComment{}, fmt.Errorf("реплика под заметкой %s: %w", noteID, ErrNotFound)
	}
	return scanPulpit(rows)
}

// PulpitByState возвращает строки в перечисленных состояниях, от старых к новым.
func (s *Store) PulpitByState(ctx context.Context, states ...string) ([]PulpitComment, error) {
	if len(states) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(states))
	holders := ""
	for i, st := range states {
		if i > 0 {
			holders += ","
		}
		holders += "?"
		args = append(args, st)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+pulpitColumns+` FROM pulpit_comments
		WHERE state IN (`+holders+`) ORDER BY seen_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectPulpit(rows)
}

// PulpitRecent возвращает последние строки амвона (от новых к старым) — вход
// предохранителя и истории форм. Полоса считается из БД, а не из счётчика в
// памяти: краш-луп не должен сбрасывать предохранитель.
func (s *Store) PulpitRecent(ctx context.Context, limit int) ([]PulpitComment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+pulpitColumns+` FROM pulpit_comments ORDER BY seen_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectPulpit(rows)
}

// PulpitSentSince — сколько реплик реально ушло на сайт с момента since
// (включая непроверенные и потерянные: POST уже состоялся). Это суточный
// предохранитель от «сайт выкатил в ленту архив», а не троттлинг.
func (s *Store) PulpitSentSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pulpit_comments
		WHERE posted_at IS NOT NULL AND posted_at > ?`, fmtTime(since)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("суточный счёт реплик: %w", err)
	}
	return n, nil
}

// PulpitConfirmedSince — подтверждённые реплики свежее since: за ответами на
// ответы ходим только к ним.
func (s *Store) PulpitConfirmedSince(ctx context.Context, since time.Time) ([]PulpitComment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+pulpitColumns+` FROM pulpit_comments
		WHERE state = ? AND posted_at > ? ORDER BY posted_at DESC`,
		PulpitConfirmed, fmtTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectPulpit(rows)
}

// PulpitStats — сводка для ручки /pulpit: сколько реплик ушло всего и за сутки
// и какая была последней (её текст и якорь показываем админу).
func (s *Store) PulpitStats(ctx context.Context, since time.Time) (total, day int, last PulpitComment, err error) {
	if err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pulpit_comments WHERE posted_at IS NOT NULL`).Scan(&total); err != nil {
		return 0, 0, PulpitComment{}, err
	}
	if day, err = s.PulpitSentSince(ctx, since); err != nil {
		return 0, 0, PulpitComment{}, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+pulpitColumns+` FROM pulpit_comments
		WHERE posted_at IS NOT NULL ORDER BY posted_at DESC LIMIT 1`)
	if err != nil {
		return 0, 0, PulpitComment{}, err
	}
	defer rows.Close()
	if rows.Next() {
		if last, err = scanPulpit(rows); err != nil {
			return 0, 0, PulpitComment{}, err
		}
	}
	return total, day, last, rows.Err()
}

// TryDecideReply записывает решение по чужой реплике. false — решение по ней уже
// принято: монетка бросается ровно один раз (PK по reply_to_id).
func (s *Store) TryDecideReply(ctx context.Context, r PulpitReply) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO pulpit_replies
			(reply_to_id, note_id, author_id, state, reason, decided_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ReplyToID, r.NoteID, r.AuthorID, r.State, r.Reason,
		fmtTime(r.DecidedAt), fmtTime(r.DecidedAt))
	if err != nil {
		return false, fmt.Errorf("решение по реплике %d: %w", r.ReplyToID, err)
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// TryStartPulpitReply переводит ответ queued → posting вместе с текстом.
func (s *Store) TryStartPulpitReply(ctx context.Context, replyToID int64, text string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE pulpit_replies SET state = ?, text = ?, posted_at = ?
		WHERE reply_to_id = ? AND state = ?`,
		PulpitPosting, text, fmtTime(now), replyToID, PulpitQueued)
	if err != nil {
		return false, fmt.Errorf("ответ на реплику %d: %w", replyToID, err)
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// SetPulpitReplyState выставляет состояние ответа.
func (s *Store) SetPulpitReplyState(ctx context.Context, replyToID int64, state, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE pulpit_replies SET state = ?, reason = ? WHERE reply_to_id = ?`,
		state, reason, replyToID)
	if err != nil {
		return fmt.Errorf("ответ на реплику %d: %w", replyToID, err)
	}
	return nil
}

// ConfirmPulpitReply фиксирует найденный в треде свой ответ.
func (s *Store) ConfirmPulpitReply(ctx context.Context, replyToID, commentID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE pulpit_replies SET state = ?, comment_id = ? WHERE reply_to_id = ?`,
		PulpitConfirmed, commentID, replyToID)
	if err != nil {
		return fmt.Errorf("подтверждение ответа на реплику %d: %w", replyToID, err)
	}
	return nil
}

// PulpitRepliesByNote — все решения по ответам в этой заметке.
func (s *Store) PulpitRepliesByNote(ctx context.Context, noteID string) ([]PulpitReply, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+pulpitReplyColumns+` FROM pulpit_replies WHERE note_id = ?
		ORDER BY reply_to_id`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PulpitReply
	for rows.Next() {
		r, err := scanPulpitReply(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PulpitReplySentSince — сколько ответов ушло на сайт с момента since.
func (s *Store) PulpitReplySentSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pulpit_replies
		WHERE posted_at IS NOT NULL AND posted_at > ?`, fmtTime(since)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("суточный счёт ответов: %w", err)
	}
	return n, nil
}

// OwnerComments возвращает тексты собственных комментариев владельца анкеты из
// живой БД — стилевой якорь генерации (манера, а не регистр). Отбор по ссылке
// на анкету: author_id в comments не хранится, а ссылка есть всегда.
func (s *Store) OwnerComments(ctx context.Context, profileID string, minRunes, maxRunes, limit int) ([]string, error) {
	if profileID == "" {
		return nil, nil
	}
	// LENGTH() у SQLite для TEXT считает знаки, а не байты, — порог в рунах
	// работает как задумано и на кириллице.
	rows, err := s.db.QueryContext(ctx, `
		SELECT text FROM comments
		WHERE author_link LIKE ? AND LENGTH(text) BETWEEN ? AND ?
		ORDER BY id DESC LIMIT ?`,
		"%/profile/"+profileID+"/%", minRunes, maxRunes, limit)
	if err != nil {
		return nil, fmt.Errorf("свои комментарии %s: %w", profileID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, err
		}
		out = append(out, text)
	}
	return out, rows.Err()
}

// SessionForProfile — самая свежая валидная сессия владельца анкеты. Ищем по
// site_profile_id, а не по мессенджеру: у одного человека бывает несколько
// строк сессий (вход в telegram и в MAX) на одну и ту же анкету сайта.
// ErrNotFound — валидной сессии этой анкеты нет.
func (s *Store) SessionForProfile(ctx context.Context, profileID string) (messenger string, userID int64, err error) {
	if profileID == "" {
		return "", 0, fmt.Errorf("сессия анкеты: id анкеты не задан")
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT messenger, user_id FROM sessions
		WHERE site_profile_id = ? AND valid = 1
		ORDER BY updated_at DESC LIMIT 1`, profileID).Scan(&messenger, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, fmt.Errorf("сессия анкеты %s: %w", profileID, ErrNotFound)
	}
	if err != nil {
		return "", 0, err
	}
	return messenger, userID, nil
}

// Flag читает рантайм-флаг. found == false — флага нет, решает значение по
// умолчанию вызывающего.
func (s *Store) Flag(ctx context.Context, key string) (value string, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("флаг %s: %w", key, err)
	}
	return value, true, nil
}

// SetFlag записывает рантайм-флаг. by — кто переключил («admin:<id>», «fuse»):
// в ручке видно, автоматика это была или человек. Секретам здесь не место.
func (s *Store) SetFlag(ctx context.Context, key, value, by string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at, updated_by) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value, updated_at = excluded.updated_at,
			updated_by = excluded.updated_by`,
		key, value, fmtTime(now), by)
	if err != nil {
		return fmt.Errorf("флаг %s: %w", key, err)
	}
	return nil
}

func collectPulpit(rows *sql.Rows) ([]PulpitComment, error) {
	var out []PulpitComment
	for rows.Next() {
		p, err := scanPulpit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanPulpit(rows *sql.Rows) (PulpitComment, error) {
	var p PulpitComment
	var commentID sql.NullInt64
	var postedAt, checkedAt sql.NullString
	var seenAt, createdAt string
	if err := rows.Scan(&p.NoteID, &p.State, &p.Reason, &p.Form, &p.Text, &commentID,
		&seenAt, &postedAt, &checkedAt, &p.Checks, &createdAt); err != nil {
		return p, err
	}
	p.CommentID = commentID.Int64
	var err error
	if p.SeenAt, err = parseTime(seenAt); err != nil {
		return p, err
	}
	if p.CreatedAt, err = parseTime(createdAt); err != nil {
		return p, err
	}
	if postedAt.Valid {
		if p.PostedAt, err = parseTime(postedAt.String); err != nil {
			return p, err
		}
	}
	if checkedAt.Valid {
		if p.CheckedAt, err = parseTime(checkedAt.String); err != nil {
			return p, err
		}
	}
	return p, nil
}

func scanPulpitReply(rows *sql.Rows) (PulpitReply, error) {
	var r PulpitReply
	var commentID sql.NullInt64
	var postedAt sql.NullString
	var decidedAt string
	if err := rows.Scan(&r.ReplyToID, &r.NoteID, &r.AuthorID, &r.State, &r.Reason,
		&r.Text, &commentID, &decidedAt, &postedAt); err != nil {
		return r, err
	}
	r.CommentID = commentID.Int64
	var err error
	if r.DecidedAt, err = parseTime(decidedAt); err != nil {
		return r, err
	}
	if postedAt.Valid {
		if r.PostedAt, err = parseTime(postedAt.String); err != nil {
			return r, err
		}
	}
	return r, nil
}
