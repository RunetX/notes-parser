package narod

// Состояние мира — слои 2–3 брифа: личная память персонажа, граф отношений и
// эпизоды-«счёты». Это и есть предмет эмуляции: реплики можно сгенерировать и
// без всего этого, но тогда получится генератор комментариев, а не сообщество.
//
// Хранилище — своя SQLite (см. шапку worldschema.sql о том, почему не общая
// база). Записи сквозные: отложенное намерение персонажа обязано пережить
// рестарт демона, иначе «отвечу через сорок минут» превращается в «не отвечу
// никогда».

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed worldschema.sql
var worldSchemaSQL string

// worldMigrations — вся история схемы мира: migrations[i] переводит её на
// версию i+1. Приём и его обоснование — из store.migrateList: каждая миграция
// идёт своей транзакцией вместе с записью user_version, поэтому обрыв на
// полпути откатывает и схему, и версию.
var worldMigrations = []string{
	worldSchemaSQL, // v1 — акторы, журнал, граф, эпизоды, кубики, планы, расход
}

// Сорта акторов. Персонажами мир не ограничен намеренно: живой участник и
// ручной персонаж владельца — такие же узлы графа, иначе персонажи не смогли бы
// ни помнить их, ни отвечать им тем же механизмом, что и друг другу.
const (
	ActorPersona = "persona"
	ActorHuman   = "human"
	ActorManual  = "manual"
)

// Состояния плана. Переход queued → posting пишется ДО генерации и отправки:
// строка, застрявшая в posting, не переотправляется никогда — судьбу её решает
// сверка с площадкой, а не догадка (приём store/pulpit.go).
const (
	PlanQueued  = "queued"
	PlanPosting = "posting"
	PlanDone    = "done"
	PlanDropped = "dropped"
)

// Исходы кубика.
const (
	DiceCome = "come"
	DiceSkip = "skip"
)

// World — состояние мира.
type World struct {
	db *sql.DB
}

// MemoryWorld — путь для мира, живущего только в памяти. Нужен реплею и
// тестам: калибровочный прогон не обязан оставлять файл.
const MemoryWorld = ":memory:"

// OpenWorld открывает (при необходимости создавая) базу мира и накатывает
// миграции.
func OpenWorld(ctx context.Context, path string) (*World, error) {
	dsn := "file:" + path + "?" + url.Values{
		"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)"},
	}.Encode()
	if path == MemoryWorld {
		// У памяти нет ни каталога, ни журнала: WAL на ней не значит ничего, а
		// общий кэш нужен, чтобы соединения видели одну и ту же базу.
		dsn = "file::memory:?cache=shared&_pragma=foreign_keys(1)"
	} else if dir := filepath.Dir(path); dir != "." && dir != "" {
		// 0700: в мире лежат тексты реплик и граф отношений — производные
		// архивных писем живых людей.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("открытие мира %s: %w", path, err)
	}
	// Одно соединение снимает вопрос конкурентной записи при нашей нагрузке
	// (десятки реплик в сутки), а для :memory: ещё и держит базу живой.
	db.SetMaxOpenConns(1)

	w := &World{db: db}
	if err := w.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return w, nil
}

func (w *World) Close() error { return w.db.Close() }

func (w *World) migrate(ctx context.Context) error {
	var version int
	if err := w.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("чтение user_version мира: %w", err)
	}
	for i, migration := range worldMigrations {
		target := i + 1
		if version >= target {
			continue
		}
		if err := w.applyMigration(ctx, migration, target); err != nil {
			return fmt.Errorf("миграция мира до v%d: %w", target, err)
		}
	}
	return nil
}

func (w *World) applyMigration(ctx context.Context, migration string, target int) error {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, migration); err != nil {
		return err
	}
	// PRAGMA user_version — часть заголовка базы и откатывается вместе с
	// транзакцией. Параметры PRAGMA не биндит; target — наш внутренний int.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", target)); err != nil {
		return err
	}
	return tx.Commit()
}

// WorldVersion — версия схемы открытой базы (для доктора и тестов).
func (w *World) WorldVersion(ctx context.Context) (int, error) {
	var v int
	err := w.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// Actor — участник мира.
type Actor struct {
	ID             string
	Kind           string
	PlatformUserID int64 // 0 — анкеты на площадке нет
	Nick           string
	CardPath       string
	CreatedAt      time.Time
}

// UpsertActor заводит или обновляет актора. Идемпотентна по id: `narod enroll`
// зовут повторно после правки карточки, и заводить второго жителя от этого
// нельзя — у первого уже есть отношения и память.
func (w *World) UpsertActor(ctx context.Context, a Actor, now time.Time) error {
	var pid any
	if a.PlatformUserID != 0 {
		pid = a.PlatformUserID
	}
	_, err := w.db.ExecContext(ctx, `
		INSERT INTO actors (id, kind, platform_user_id, nick, card_path, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		    kind = excluded.kind,
		    platform_user_id = coalesce(excluded.platform_user_id, actors.platform_user_id),
		    nick = excluded.nick,
		    card_path = excluded.card_path`,
		a.ID, a.Kind, pid, a.Nick, a.CardPath, fmtTime(now))
	if err != nil {
		return fmt.Errorf("актор %s: %w", a.ID, err)
	}
	return nil
}

// Actors — все участники мира, по id.
func (w *World) Actors(ctx context.Context) ([]Actor, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT id, kind, coalesce(platform_user_id, 0), nick, card_path, created_at
		  FROM actors ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Actor
	for rows.Next() {
		var a Actor
		var created string
		if err := rows.Scan(&a.ID, &a.Kind, &a.PlatformUserID, &a.Nick, &a.CardPath, &created); err != nil {
			return nil, err
		}
		a.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ActorByPlatformUser — актор по анкете площадки. Так узнаётся автор пришедшей
// реплики: живой человек заводится актором при первой же встрече, иначе к нему
// не может появиться отношения.
func (w *World) ActorByPlatformUser(ctx context.Context, userID int64) (Actor, bool, error) {
	var a Actor
	var created string
	err := w.db.QueryRowContext(ctx, `
		SELECT id, kind, coalesce(platform_user_id, 0), nick, card_path, created_at
		  FROM actors WHERE platform_user_id = ?`, userID).
		Scan(&a.ID, &a.Kind, &a.PlatformUserID, &a.Nick, &a.CardPath, &created)
	if err == sql.ErrNoRows {
		return Actor{}, false, nil
	}
	if err != nil {
		return Actor{}, false, err
	}
	a.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return a, true, nil
}

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }
