// Пакет acct — сервисные аккаунты сайта: технические сессии для обходов под
// авторизацией и редких ручных действий (второй профиль как резерв доступа).
//
// Почему отдельная база, а не строка в боевой таблице sessions: та таблица
// принадлежит людям, вошедшим через ботов, и её читают все подряд без разбора
// мессенджера — store.TalksOwners берёт ВСЕ валидные сессии, так что служебная
// строка немедленно попала бы в обход личных сообщений и начала гасить
// непрочитанное на сайте. Плюс отдельный файл можно принести на рабочую машину
// без боевой БД, где лежат живые куки пользователей бота.
//
// Куки хранятся тем же форматом, что и в sessions (love.CookiesToJSON), и под
// тем же шифрованием (пакет secret) — строка остаётся взаимозаменяемой, если
// однажды понадобится настоящая многопрофильность в самом боте.
package acct

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"lovegw/internal/secret"

	_ "modernc.org/sqlite"
)

// ErrNotFound — аккаунта с таким именем нет.
var ErrNotFound = errors.New("аккаунт не найден")

const schemaSQL = `
CREATE TABLE accounts (
    name        TEXT PRIMARY KEY,          -- короткое имя: reserve, main
    cookies     TEXT NOT NULL,             -- куки сессии (шифротекст, если задан ключ)
    valid       INTEGER NOT NULL DEFAULT 1,
    profile_id  TEXT NOT NULL DEFAULT '',  -- анкета на сайте
    passport_id TEXT NOT NULL DEFAULT '',  -- сквозной номер НГС
    nick        TEXT NOT NULL DEFAULT '',  -- ник на момент последней проверки
    note        TEXT NOT NULL DEFAULT '',  -- зачем этот аккаунт
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,             -- когда последний раз входили
    last_ok_at  TEXT                       -- когда сессия последний раз сработала
);
`

// Account — сервисный аккаунт без кук: всё, что можно показать человеку.
type Account struct {
	Name       string
	ProfileID  string
	PassportID string
	Nick       string
	Note       string
	Valid      bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastOKAt   time.Time // нулевое — ни разу не проверяли
}

// Title — как аккаунт показывается в логе и на экране: имя и кто это на сайте.
func (a Account) Title() string {
	switch {
	case a.Nick != "" && a.ProfileID != "":
		return fmt.Sprintf("%s (%s, u%s)", a.Name, a.Nick, a.ProfileID)
	case a.Nick != "":
		return fmt.Sprintf("%s (%s)", a.Name, a.Nick)
	default:
		return a.Name
	}
}

type Store struct {
	db  *sql.DB
	key secret.Key
}

// Option — необязательная настройка хранилища.
type Option func(*Store)

// WithSecret включает шифрование кук на диске.
func WithSecret(k secret.Key) Option {
	return func(s *Store) { s.key = k }
}

// Open открывает (при необходимости создавая) базу аккаунтов.
// Каталог под 0700 — здесь куки технических сессий (см. Open в store).
func Open(ctx context.Context, path string, opts ...Option) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + filepath.ToSlash(path) + "?" + url.Values{
		"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)"},
	}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("открытие базы аккаунтов %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	for _, opt := range opts {
		opt(s)
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrate накатывает недостающие миграции; версия схемы — PRAGMA user_version
// (тот же порядок, что в store и modwatch).
func (s *Store) migrate(ctx context.Context) error {
	migrations := []string{
		schemaSQL, // v1 — базовая схема
	}
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("чтение user_version: %w", err)
	}
	for i, migration := range migrations {
		target := i + 1
		if version >= target {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migration); err != nil {
			tx.Rollback()
			return fmt.Errorf("миграция v%d: %w", target, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", target)); err != nil {
			tx.Rollback()
			return fmt.Errorf("миграция v%d: установка версии: %w", target, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Put сохраняет вход: куки и то, кем аккаунт оказался на сайте. Повторный вход
// тем же именем обновляет строку, сохраняя created_at и заметку, если новую не
// задали.
func (s *Store) Put(ctx context.Context, a Account, cookiesJSON string, now time.Time) error {
	stored, err := s.key.Seal(secret.AccountAAD(a.Name), cookiesJSON)
	if err != nil {
		return fmt.Errorf("аккаунт %s: %w", a.Name, err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO accounts (name, cookies, valid, profile_id, passport_id, nick, note, created_at, updated_at)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			cookies     = excluded.cookies,
			valid       = 1,
			profile_id  = excluded.profile_id,
			passport_id = excluded.passport_id,
			nick        = excluded.nick,
			note        = CASE WHEN excluded.note <> '' THEN excluded.note ELSE accounts.note END,
			updated_at  = excluded.updated_at`,
		a.Name, stored, a.ProfileID, a.PassportID, a.Nick, a.Note, fmtTime(now), fmtTime(now))
	if err != nil {
		return fmt.Errorf("аккаунт %s: %w", a.Name, err)
	}
	return nil
}

// Get возвращает аккаунт и куки его сессии. ErrNotFound — такого имени нет.
func (s *Store) Get(ctx context.Context, name string) (Account, string, error) {
	rows, err := s.db.QueryContext(ctx, selectAccounts+` WHERE name = ?`, name)
	if err != nil {
		return Account{}, "", err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Account{}, "", err
		}
		return Account{}, "", fmt.Errorf("%q: %w", name, ErrNotFound)
	}
	a, stored, err := scanAccount(rows)
	if err != nil {
		return Account{}, "", err
	}
	cookies, err := s.key.Open(secret.AccountAAD(a.Name), stored)
	if err != nil {
		return Account{}, "", fmt.Errorf("аккаунт %s: %w", a.Name, err)
	}
	return a, cookies, nil
}

// List возвращает все аккаунты (без кук), по имени.
func (s *Store) List(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, selectAccounts+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, _, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetValid помечает сессию (не)валидной; успех фиксируется в last_ok_at.
// Порядок тот же, что у сессий пользователей в store.SetSessionValid.
func (s *Store) SetValid(ctx context.Context, name string, valid bool, now time.Time) error {
	var res sql.Result
	var err error
	if valid {
		res, err = s.db.ExecContext(ctx,
			`UPDATE accounts SET valid = 1, last_ok_at = ? WHERE name = ?`, fmtTime(now), name)
	} else {
		res, err = s.db.ExecContext(ctx, `UPDATE accounts SET valid = 0 WHERE name = ?`, name)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%q: %w", name, ErrNotFound)
	}
	return nil
}

// SetIdentity обновляет, кем аккаунт видится сайту: ник на площадке меняют
// когда захотят, и проверка сессии — единственный момент, когда мы это видим.
func (s *Store) SetIdentity(ctx context.Context, name, profileID, passportID, nick string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE accounts SET profile_id = ?, passport_id = ?, nick = ? WHERE name = ?`,
		profileID, passportID, nick, name)
	return err
}

// Forget удаляет аккаунт вместе с куками. false — такого имени не было.
func (s *Store) Forget(ctx context.Context, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

const selectAccounts = `
	SELECT name, cookies, valid, profile_id, passport_id, nick, note, created_at, updated_at, last_ok_at
	FROM accounts`

func scanAccount(rows *sql.Rows) (Account, string, error) {
	var a Account
	var stored, created, updated string
	var lastOK sql.NullString
	var valid int
	if err := rows.Scan(&a.Name, &stored, &valid, &a.ProfileID, &a.PassportID,
		&a.Nick, &a.Note, &created, &updated, &lastOK); err != nil {
		return Account{}, "", err
	}
	a.Valid = valid == 1
	a.CreatedAt = parseTime(created)
	a.UpdatedAt = parseTime(updated)
	if lastOK.Valid {
		a.LastOKAt = parseTime(lastOK.String)
	}
	return a, stored, nil
}

// Время храним строкой в UTC — как в остальных базах проекта.
const timeLayout = "2006-01-02T15:04:05Z"

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) time.Time {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
