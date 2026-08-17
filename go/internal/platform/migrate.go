package platform

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrateLockID — ключ advisory-блокировки на время миграций. Демон и веб живут
// в разных контейнерах одного образа и стартуют одновременно, поэтому накатывать
// схему они могут наперегонки; блокировка делает гонку невозможной, а не
// маловероятной. Число произвольное, но постоянное.
const migrateLockID int64 = 0x10E6_0000_0001

// migration — один шаг схемы. Version берётся из имени файла (0001_init.sql),
// то есть история миграций — это содержимое каталога, а не список в коде: забыть
// дописать строку в массив нельзя.
type migration struct {
	Version int
	Name    string
	SQL     string
}

// loadMigrations читает каталог migrations и возвращает шаги по возрастанию
// версии. Дыры и повторы версий — ошибка сборки схемы, а не «как-нибудь
// накатится»: молча пропущенный шаг обнаружится годы спустя на чужой машине.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("чтение каталога миграций: %w", err)
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("миграция %q: имя без номера (ожидается 0001_описание.sql)", e.Name())
		}
		version, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("миграция %q: номер не число: %w", e.Name(), err)
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("миграция %q: %w", e.Name(), err)
		}
		out = append(out, migration{Version: version, Name: e.Name(), SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("миграция %q: ожидалась версия %d, получена %d (дыра или повтор в нумерации)", m.Name, i+1, m.Version)
		}
	}
	return out, nil
}

// SchemaVersion — версия схемы, на которую рассчитан этот бинарник.
func SchemaVersion() (int, error) {
	ms, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	return len(ms), nil
}

// Migrate накатывает недостающие миграции.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	return migrateList(ctx, pool, ms)
}

// migrateList — тело Migrate, вынесено ради теста на обрыв посередине.
//
// Каждая миграция идёт СВОЕЙ транзакцией вместе с записью версии: падение на
// полпути (сеть, kill, отказ диска) откатывает и схему, и версию, поэтому
// следующий запуск начинает этот шаг заново, а не спотыкается о полусостояние.
// В Postgres DDL транзакционен, так что это честная гарантия, а не пожелание.
//
// Правило на будущее: шагу, которому нужен CREATE INDEX CONCURRENTLY (он вне
// транзакции), эта обёртка не годится — такой шаг обязан жить отдельным файлом
// и выполняться своим кодом, вне транзакции.
func migrateList(ctx context.Context, pool *pgxpool.Pool, ms []migration) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("соединение под миграции: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockID); err != nil {
		return fmt.Errorf("блокировка миграций: %w", err)
	}
	defer func() {
		// Отпускаем блокировку своим контекстом: ctx мог уже отмениться, а
		// держать её до разрыва соединения незачем.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLockID)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		    version    integer PRIMARY KEY,
		    name       text NOT NULL,
		    applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("таблица schema_migrations: %w", err)
	}

	var version int
	if err := conn.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("чтение версии схемы: %w", err)
	}

	for _, m := range ms {
		if version >= m.Version {
			continue
		}
		if err := applyMigration(ctx, conn.Conn(), m); err != nil {
			return fmt.Errorf("миграция v%d (%s): %w", m.Version, m.Name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, conn *pgx.Conn, m migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
		m.Version, m.Name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
