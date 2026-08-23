// Пакет platform — ядро собственной площадки «Зазеркалье»: хранилище на Postgres,
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
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// minConns — держим пару горячими: на одном ядре установка соединения с
	// разбором TLS заметна на фоне запроса, живущего две миллисекунды.
	minConns = 2

	maxConnLifetime = time.Hour
	maxConnIdleTime = 15 * time.Minute
	connectTimeout  = 10 * time.Second
)

// Opts — под какую роль открывается пул. Роли различаются не из аккуратности:
// хост один и ядро одно, поэтому доля Postgres, которую вправе занять
// ПОСТОРОННИЙ, задаётся ровно здесь. Веб-морда — единственное, до чего он
// дотягивается; демон и разовые команды приходят с той же машины.
type Opts struct {
	// MaxConns — потолок соединений, он же потолок ОДНОВРЕМЕННЫХ запросов от
	// этого процесса. Сумма по всем ролям обязана оставаться ниже
	// max_connections (20 в postgresql.conf, минус три служебных).
	MaxConns int32
	// StatementTimeout — потолок одного запроса на стороне СЕРВЕРА. Контекста
	// для этого мало, и это не перестраховка: на отмену pgx выставляет
	// соединению сетевой срок и рвёт его с КЛИЕНТСКОЙ стороны
	// (DeadlineContextWatcherHandler), а backend продолжает считать запрос,
	// пока не упрётся в чтение сокета — client_connection_check_interval по
	// умолчанию выключен. Отменённый снаружи тяжёлый SELECT без этого потолка
	// продолжает жечь единственное ядро, то есть делает ровно то, чем душат.
	// Ноль — без потолка: так открываются миграции, импорт архива и сверка.
	StatementTimeout time.Duration
}

// WebOpts — пул веб-морды. Четыре соединения при странице в четыре запроса —
// это до четырёх одновременных страниц, вчетверо больше нынешнего пика; потолок
// в пять секунд при ленте 86 мс и треде 96 мс оставляет полсотни крат запаса.
// Оба числа выбраны так, чтобы наплыв снаружи упирался в них РАНЬШЕ, чем в
// процессорное время, которого ждёт зеркало.
func WebOpts() Opts { return Opts{MaxConns: 4, StatementTimeout: 5 * time.Second} }

// DaemonOpts — пул демона: приём зеркала, сверка, исходящий обход. Потолка на
// запрос нет намеренно — сверка гоняет агрегат по 10,7 млн комментариев, и
// срок, подобранный под страницу, убивал бы её каждые пять минут. Демон
// защищён иначе: бюджетом на КАЖДЫЙ вызов у себя (platsink, platout, bridge),
// потому что там известно, чего ждать.
func DaemonOpts() Opts { return Opts{MaxConns: 6} }

// ToolOpts — пул разовых команд: миграции, импорт архива, ручная сверка,
// doctor. Они идут в известный момент и под присмотром администратора.
func ToolOpts() Opts { return Opts{MaxConns: 6} }

// Platform — хранилище площадки. Пул потокобезопасен, экземпляр один на процесс.
type Platform struct {
	pool *pgxpool.Pool
}

// Open открывает пул разовой команды. Миграции НЕ накатываются: это отдельное
// решение вызывающего (`lovegw platform migrate`), потому что схему меняет
// администратор в известный момент, а не любой стартующий контейнер.
func Open(ctx context.Context, dsn string) (*Platform, error) {
	return OpenWith(ctx, dsn, ToolOpts())
}

// OpenWith открывает пул под названную роль (см. Opts).
func OpenWith(ctx context.Context, dsn string, o Opts) (*Platform, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("разбор DSN Postgres: %w", err)
	}
	cfg.MaxConns = o.MaxConns
	cfg.MinConns = minConns
	cfg.MaxConnLifetime = maxConnLifetime
	cfg.MaxConnIdleTime = maxConnIdleTime
	cfg.ConnConfig.ConnectTimeout = connectTimeout
	if o.StatementTimeout > 0 {
		// Параметром соединения, а не `SET` после подключения: так он стоит на
		// КАЖДОМ соединении пула с первого же запроса, включая те, что пул
		// заведёт под нагрузкой, — то есть ровно тогда, когда потолок и нужен.
		cfg.ConnConfig.RuntimeParams["statement_timeout"] =
			strconv.FormatInt(o.StatementTimeout.Milliseconds(), 10)
	}

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
