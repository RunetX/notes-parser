// Пакет platform — ядро собственной площадки «Заметки»: хранилище на Postgres,
// доменные типы и сервисный слой. HTTP живёт снаружи (internal/web, internal/api)
// и зовёт этот пакет напрямую, а не сам себя по loopback.
//
// ИНВАРИАНТ: platform не импортирует internal/archive — ни прямо, ни транзитивно
// (проверяется deps_test.go). Граница архитектурная и юридическая сразу: архив
// хранит персон-аналитику (имена, пол, альт-анкеты, интересы) — обработку данных
// третьих лиц без их согласия, которой на публичной площадке не место.
package platform

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// maxConns — потолок соединений пула. Сервер на старте — одно ядро, и
	// Postgres там настроен на max_connections = 20 при shared_buffers 512 МБ:
	// восемь рабочих соединений веб-морде хватает с запасом (страница треда
	// стоит четыре запроса), а лишние только отнимали бы память у кэша страниц.
	maxConns = 8
	// minConns — держим пару горячими: на одном ядре установка соединения с
	// разбором TLS заметна на фоне запроса, живущего две миллисекунды.
	minConns = 2

	maxConnLifetime = time.Hour
	maxConnIdleTime = 15 * time.Minute
	connectTimeout  = 10 * time.Second
)

// Platform — хранилище площадки. Пул потокобезопасен, экземпляр один на процесс.
type Platform struct {
	pool *pgxpool.Pool
}

// Open открывает пул и проверяет соединение. Миграции НЕ накатываются: это
// отдельное решение вызывающего (`lovegw platform migrate`), потому что схему
// меняет администратор в известный момент, а не любой стартующий контейнер.
func Open(ctx context.Context, dsn string) (*Platform, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("разбор DSN Postgres: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = minConns
	cfg.MaxConnLifetime = maxConnLifetime
	cfg.MaxConnIdleTime = maxConnIdleTime
	cfg.ConnConfig.ConnectTimeout = connectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("пул Postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("Postgres не отвечает: %w", err)
	}
	return &Platform{pool: pool}, nil
}

// Close закрывает пул. Повторный вызов безопасен.
func (p *Platform) Close() {
	if p != nil && p.pool != nil {
		p.pool.Close()
	}
}

// Pool — доступ к пулу для миграций и диагностики.
func (p *Platform) Pool() *pgxpool.Pool { return p.pool }

// Ping — база отвечает. Отдельный метод, а не Pool().Ping() у вызывающего:
// веб-морда знает о ядре ровно через интерфейс чтения (см. web.Store), и
// проверка здоровья не повод давать ей весь пул.
func (p *Platform) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// Migrate накатывает недостающие миграции схемы.
func (p *Platform) Migrate(ctx context.Context) error { return Migrate(ctx, p.pool) }

// idSet — выборка одной колонки bigint в множество. Общая для сверки с зеркалом:
// ей нужны именно множества id (что уже есть, чего не хватает), а не строки.
func (p *Platform) idSet(ctx context.Context, what, sql string, args ...any) (map[int64]bool, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	out := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// Version возвращает версию схемы в базе (0 — схемы ещё нет) и версию, на
// которую рассчитан бинарник. Расхождение — не ошибка сама по себе: его
// показывает doctor, а решает администратор.
func (p *Platform) Version(ctx context.Context) (inDB, wanted int, err error) {
	wanted, err = SchemaVersion()
	if err != nil {
		return 0, 0, err
	}
	// to_regclass возвращает NULL вместо ошибки, если таблицы нет: это рабочий
	// случай (пустая база), а не повод падать.
	var exists bool
	if err := p.pool.QueryRow(ctx,
		`SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, wanted, fmt.Errorf("проверка schema_migrations: %w", err)
	}
	if !exists {
		return 0, wanted, nil
	}
	if err := p.pool.QueryRow(ctx,
		`SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&inDB); err != nil {
		return 0, wanted, fmt.Errorf("чтение версии схемы: %w", err)
	}
	return inDB, wanted, nil
}
