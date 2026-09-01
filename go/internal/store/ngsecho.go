package store

// Опознанное эхо: записи НГС, которые зеркало НЕ несёт никуда, потому что это
// наши же слова, ушедшие туда из Зазеркалья (пакет platngs).
//
// Почему это память зеркала, а не площадки. Дубль, который надо погасить, у
// каждой ушедшей строки НЕ ОДИН: реплика с сайта идёт и на площадку (второй
// строкой в тот же тред), и в Telegram, и в MAX. Гасить их порознь в трёх местах
// значило бы завести три правила, которые однажды разойдутся, — а есть одна
// точка, через которую эхо проходит раньше всех остальных: приём зеркала. Не
// попав в lovegw.db, оно не попадёт уже никуда, включая сверку площадки: та
// сравнивает SQLite с Postgres, и обе стороны недосчитаются одной и той же
// строки, то есть сойдутся.

import (
	"context"
	"fmt"
	"time"
)

// Виды эха — те же, что у очереди выноса.
const (
	EchoNote    = "note"
	EchoComment = "comment"
)

// MarkNGSEcho запоминает, что запись сайта опознана как своя. Идемпотентна:
// такт зеркала видит одну и ту же реплику страницы много раз подряд.
//
// noteID у комментария — заметка, в которой он лежит; у заметки пусто.
func (s *Store) MarkNGSEcho(ctx context.Context, kind, siteID, noteID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO ngs_echo (kind, site_id, note_id, seen_at)
		VALUES (?, ?, ?, ?)`, kind, siteID, noteID, at.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("отметка эха %s %s: %w", kind, siteID, err)
	}
	return nil
}

// NGSEchoNotes дописывает в набор известных заметок те, что опознаны своими.
// Спрашивается раз на обход ленты, вместе со списком известных: лента держит
// заметку сутками, а решать про неё надо на каждом обходе.
//
// Именно ДОПИСЫВАЕТ в готовую карту, а не отдаёт свою: у зеркала один вопрос —
// «про эту запись уже всё решено», — и разводить его на два набора значило бы
// завести второе место, где о втором забудут.
func (s *Store) NGSEchoNotes(ctx context.Context, into map[string]bool) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT site_id FROM ngs_echo WHERE kind = ?`, EchoNote)
	if err != nil {
		return fmt.Errorf("список эха заметок: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("список эха заметок: %w", err)
		}
		into[id] = true
	}
	return rows.Err()
}

// NGSEchoComments дописывает в набор известных реплик заметки те, что опознаны
// своими — по тому же доводу, что и NGSEchoNotes.
func (s *Store) NGSEchoComments(ctx context.Context, noteID string, into map[int64]bool) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT site_id FROM ngs_echo WHERE kind = ? AND note_id = ?`, EchoComment, noteID)
	if err != nil {
		return fmt.Errorf("эхо в заметке %s: %w", noteID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("эхо в заметке %s: %w", noteID, err)
		}
		into[id] = true
	}
	return rows.Err()
}
