package store

// Методы утренней заметки (пакет morning, миграция v11): одна строка на сутки.
//
// Однократность держится ПЕРВИЧНЫМ КЛЮЧОМ по дню, а не проверкой «а не
// публиковали ли уже»: заметку заводит INSERT OR IGNORE, поэтому ни второй
// прогон планировщика, ни ручной догон, ни рестарт демона второй заметки в
// ленту не поставят. Строка заводится СРАЗУ в posting — то есть до отправки, —
// и назад из него не откатывается никогда: `love.Client.PostNote` возвращает
// одну лишь ошибку, без id, а значит «сайт принял и ответил сбоем» неотличимо
// от «не принял». Дубль в ленте убирается только модератором, пропуск — просто
// пропуск, и размен сделан в пользу пропуска.

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Состояния утренней заметки.
const (
	MorningPosting   = "posting"   // отправка началась; переотправки не будет
	MorningPosted    = "posted"    // сайт принял POST, в ленте заметку ещё не нашли
	MorningConfirmed = "confirmed" // заметка найдена в ленте (известен note_id)
	MorningMissing   = "missing"   // лента прочитана, заметки в ней нет
	MorningSkipped   = "skipped"   // сознательно не пишем (чужое «доброе утро», поздно, выключено)
	MorningFailed    = "failed"    // не сгенерировали/некому отправить
)

// MorningReasonSendFailed — причина у строки, чей POST не дошёл (5xx, обрыв).
// Живёт здесь, а не только в пакете morning, потому что по ней фильтрует
// предохранитель: не долетевшая заметка — не признак того, что нас забанили
// (17.08.2026 сайт отвечал 500 на всё подряд, и вина была не наша).
const MorningReasonSendFailed = "send_failed"

// Ключи рантайм-флагов утренней заметки (таблица settings, та же, что у амвона).
const (
	FlagMorningEnabled = "morning.enabled"
	// FlagNarodEnabled — тумблер народа (эпик «народ»). Стоит рядом с утренним,
	// потому что устроен так же: конфиг решает, есть ли служба вообще, а этот
	// флаг — работает ли она сейчас; отсутствие значит «включена».
	FlagNarodEnabled     = "narod.enabled"
	FlagMorningOffReason = "morning.off_reason"
	FlagMorningOffAt     = "morning.off_at"
	FlagMorningOffBy     = "morning.off_by"
)

// MorningNote — утренняя заметка одного дня.
type MorningNote struct {
	Day       string // YYYY-MM-DD в поясе слота
	State     string
	Reason    string
	Text      string
	Facts     string // поводы дня, как их видел прогон (JSON)
	NoteID    string // id на сайте, после верификации
	PostedAt  time.Time
	CheckedAt time.Time
	Checks    int
	CreatedAt time.Time
}

const morningColumns = `day, state, reason, text, facts, note_id,
	posted_at, checked_at, checks, created_at`

// TryStartMorning заводит строку дня сразу в posting и возвращает false, если
// день уже занят. Это и есть точка невозврата: вызывать её надо ДО отправки.
func (s *Store) TryStartMorning(ctx context.Context, day, text, facts string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO morning_notes (day, state, text, facts, posted_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		day, MorningPosting, text, facts, fmtTime(now), fmtTime(now))
	if err != nil {
		return false, fmt.Errorf("утренняя заметка %s: %w", day, err)
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// MarkMorning заводит день сразу в конечном состоянии (пропуск, отказ) — так
// пишутся дни, в которые мы сознательно промолчали. Тоже INSERT OR IGNORE:
// пропуск не должен затирать уже опубликованную заметку.
func (s *Store) MarkMorning(ctx context.Context, day, state, reason string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO morning_notes (day, state, reason, created_at)
		VALUES (?, ?, ?, ?)`, day, state, reason, fmtTime(now))
	if err != nil {
		return false, fmt.Errorf("утренняя заметка %s: %w", day, err)
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// SetMorningState переводит день в новое состояние с причиной.
func (s *Store) SetMorningState(ctx context.Context, day, state, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE morning_notes SET state = ?, reason = ? WHERE day = ?`, state, reason, day)
	if err != nil {
		return fmt.Errorf("состояние утренней заметки %s: %w", day, err)
	}
	return nil
}

// ConfirmMorning отмечает найденную в ленте заметку.
func (s *Store) ConfirmMorning(ctx context.Context, day, noteID string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE morning_notes
		SET state = ?, note_id = ?, checked_at = ?, checks = checks + 1
		WHERE day = ?`, MorningConfirmed, noteID, fmtTime(now), day)
	if err != nil {
		return fmt.Errorf("подтверждение утренней заметки %s: %w", day, err)
	}
	return nil
}

// BumpMorningCheck считает попытку найти заметку в ленте и возвращает их число.
func (s *Store) BumpMorningCheck(ctx context.Context, day string, now time.Time) (int, error) {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE morning_notes SET checked_at = ?, checks = checks + 1 WHERE day = ?`,
		fmtTime(now), day); err != nil {
		return 0, fmt.Errorf("проверка утренней заметки %s: %w", day, err)
	}
	var checks int
	err := s.db.QueryRowContext(ctx,
		`SELECT checks FROM morning_notes WHERE day = ?`, day).Scan(&checks)
	return checks, err
}

// MorningByDay возвращает строку дня; ErrNotFound — дня ещё не было.
func (s *Store) MorningByDay(ctx context.Context, day string) (MorningNote, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+morningColumns+` FROM morning_notes WHERE day = ?`, day)
	if err != nil {
		return MorningNote{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return MorningNote{}, err
		}
		return MorningNote{}, fmt.Errorf("утренняя заметка %s: %w", day, ErrNotFound)
	}
	return scanMorning(rows)
}

// MorningRecent — последние дни (от новых к старым): вход предохранителя и
// отчёта в ЛС. Полоса считается ИЗ БД, а не из счётчика в памяти: краш-луп не
// должен сбрасывать предохранитель.
func (s *Store) MorningRecent(ctx context.Context, limit int) ([]MorningNote, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+morningColumns+` FROM morning_notes ORDER BY day DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MorningNote
	for rows.Next() {
		n, err := scanMorning(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func scanMorning(rows *sql.Rows) (MorningNote, error) {
	var (
		n                 MorningNote
		posted, checkedAt sql.NullString
		createdAt         string
	)
	if err := rows.Scan(&n.Day, &n.State, &n.Reason, &n.Text, &n.Facts, &n.NoteID,
		&posted, &checkedAt, &n.Checks, &createdAt); err != nil {
		return MorningNote{}, err
	}
	var err error
	if n.CreatedAt, err = parseTime(createdAt); err != nil {
		return n, err
	}
	if posted.Valid {
		if n.PostedAt, err = parseTime(posted.String); err != nil {
			return n, err
		}
	}
	if checkedAt.Valid {
		if n.CheckedAt, err = parseTime(checkedAt.String); err != nil {
			return n, err
		}
	}
	return n, nil
}
