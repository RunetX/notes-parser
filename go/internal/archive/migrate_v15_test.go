package archive

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"
)

// TestMigrateV15PreservesProfiles — v15 (genre в PK) пересоздаёт таблицы
// профилей; существующие строки должны сохраниться как genre='all'. Стенд:
// вручную поднимаем БД в состоянии v14 (старая схема без genre) с данными,
// затем Open() накатывает v15 и мы проверяем, что данные на месте.
func TestMigrateV15PreservesProfiles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "archive.db")

	// Порядок миграций v1..v14 (без v15 — его накатит Open).
	pre := []string{
		schemaSQL, migrateV2SQL, migrateV3SQL, migrateV4SQL, migrateV5SQL,
		migrateV6SQL, migrateV7SQL, migrateV8SQL, migrateV9SQL, migrateV10SQL,
		migrateV11SQL, migrateV12SQL, migrateV13SQL, migrateV14SQL,
	}
	dsn := "file:" + filepath.ToSlash(path) + "?" + url.Values{
		"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)"},
	}.Encode()
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	for i, m := range pre {
		if _, err := raw.ExecContext(ctx, m); err != nil {
			raw.Close()
			t.Fatalf("pre-миграция v%d: %v", i+1, err)
		}
	}
	// Данные в СТАРОЙ форме (без колонки genre).
	vec := encodeVec([]float32{1, 0, 0, 0})
	idf := encodeVec([]float32{0.5, 0.5, 0.5, 0.5})
	stmts := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO users (id, name, first_seen, last_seen) VALUES (1, 'A', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, nil},
		{`INSERT INTO style_profiles (user_id, ngrams, dims, vec, built_at) VALUES (1, 42, 4, ?, '2026-01-01T00:00:00Z')`, []any{vec}},
		{`INSERT INTO lexis_meta (id, dims, docs, idf, built_at) VALUES (1, 4, 1, ?, '2026-01-01T00:00:00Z')`, []any{idf}},
		{`INSERT INTO lexis_profiles (user_id, tokens, dims, vec, built_at) VALUES (1, 10, 4, ?, '2026-01-01T00:00:00Z')`, []any{vec}},
		{`PRAGMA user_version = 14`, nil},
	}
	for _, s := range stmts {
		if _, err := raw.ExecContext(ctx, s.q, s.args...); err != nil {
			raw.Close()
			t.Fatalf("подготовка данных (%s): %v", s.q, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// Open накатывает v15.
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (миграция v15): %v", err)
	}
	defer s.Close()

	var ver int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver < 15 {
		t.Fatalf("user_version после Open: got %d, want ≥15", ver)
	}

	// Стиль-профиль перенесён как genre='all' с сохранённым ngrams.
	var ng int
	if err := s.db.QueryRowContext(ctx,
		`SELECT ngrams FROM style_profiles WHERE user_id = 1 AND genre = ?`, GenreAll).Scan(&ng); err != nil {
		t.Fatalf("style_profiles(all) не перенесён: %v", err)
	}
	if ng != 42 {
		t.Errorf("ngrams не сохранён: got %d, want 42", ng)
	}
	// В genre='notes' ничего не должно появиться.
	notesIDs, _, err := s.loadStyleProfiles(ctx, GenreNotes)
	if err != nil {
		t.Fatal(err)
	}
	if len(notesIDs) != 0 {
		t.Errorf("в notes-слой просочились строки при миграции: %v", notesIDs)
	}

	// Лексический слой и meta перенесены как genre='all'.
	lIDs, _, _, dims, err := s.loadLexisProfiles(ctx, GenreAll)
	if err != nil {
		t.Fatal(err)
	}
	if dims != 4 || !hasID(lIDs, 1) {
		t.Errorf("lexis(all) не перенесён: dims=%d ids=%v", dims, lIDs)
	}
	var tokens int
	if err := s.db.QueryRowContext(ctx,
		`SELECT tokens FROM lexis_profiles WHERE user_id = 1 AND genre = ?`, GenreAll).Scan(&tokens); err != nil {
		t.Fatalf("lexis_profiles(all) не перенесён: %v", err)
	}
	if tokens != 10 {
		t.Errorf("tokens не сохранён: got %d, want 10", tokens)
	}
}

// TestMigrationAtomic — миграция накатывается атомарно: если один из стейтментов
// падает, эффекты предыдущих откатываются (иначе разрушительная миграция вроде
// v15 могла бы застрять полу-выполненной и навсегда сломать Open).
func TestMigrationAtomic(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	// Второй стейтмент падает (нет такой таблицы) → первый (CREATE) не должен
	// остаться.
	bad := `CREATE TABLE mig_atomic (x INTEGER);
INSERT INTO nonexistent_table_zzz (x) VALUES (1);`
	if err := s.applyMigration(ctx, bad, 999); err == nil {
		t.Fatal("ожидалась ошибка миграции")
	}

	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='mig_atomic'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("миграция не откатилась: таблица mig_atomic осталась (%d)", n)
	}
	// user_version не должен продвинуться от неудавшейся миграции.
	var ver int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver == 999 {
		t.Errorf("user_version продвинулся при неудачной миграции: %d", ver)
	}
}
