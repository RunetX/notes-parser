package archive

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"

	sqlite3 "modernc.org/sqlite"

	"lovegw/internal/love"
)

// Слой адресатов реплик.
//
// Зачем он нужен. Дерево комментариев на сайте двухуровневое: `parent_id`
// указывает на КОРЕНЬ ветки, а не на реплику, которой отвечают (проверено — на
// 8,2 млн ответов архива нет ни одного случая глубины ≥2, и древовидный вид
// сайта, который граббер и качает, ничего сверх этого не отдаёт). Настоящий
// адресат стоит префиксом в тексте — «Ник, текст», клиент подставляет его при
// ответе. Замер по каждому 53-му ответу: префикс есть у 93,6 % реплик, а автор
// корня ветки совпадает с адресатом лишь в 34,8 % — то есть граф по `parent_id`
// приписывает реплику не тому человеку в двух случаях из трёх.
//
// Что делает этот слой. Материализует адресата в comment_addressee тремя
// методами по убыванию надёжности:
//
//	reply          — точная цель ответа из мобильной версии сайта (слой
//	                 обогащения comment_reply). Не догадка, а то, что сайт
//	                 хранит сам; перекрывает всё остальное.
//	branch         — префикс совпал с единственным участником этой же ветки.
//	                 Ветка почти снимает омонимию ников (несколько кандидатов —
//	                 0,1 % случаев), поэтому метод точный; покрывает ~58 %.
//	history_branch — ник в момент реплики принадлежал участнику ветки по
//	                 nick_history (адресат сменил ник, users.name уже другое).
//
// Неразрешённые реплики в таблицу не пишутся: вьюхи падают на автора корня
// ветки через COALESCE, то есть до пересчёта и на непокрытом хвосте поведение
// остаётся прежним.
//
// nick_history восстанавливает историю ников тем же приёмом, что и досье:
// в ветке, открытой человеком, большинство обращений адресовано ему, поэтому
// доминирующий префикс месяца — его ник в этом месяце. Месяцы схлопываются в
// интервал [from_ym, to_ym]; ник, переходивший между людьми, даёт пересекающиеся
// интервалы и на резолве отсекается требованием единственного владельца.
//
// Был и четвёртый метод, history: владелец ника по nick_history БЕЗ проверки,
// что он вообще участвует в ветке. Он снят в v18 как неверный по построению.
// Сверка с точными парами сайта на 2020–2023: из 4907 его выводов 1472 (30 %) —
// ответы самому себе (там дерево и текст говорят о разном законно), а из
// оставшихся 3435 настоящих ответов другому человеку ошибочны ВСЕ 3435. Причина
// структурная: этап срабатывает только там, где поиск по ветке уже провалился, —
// а провалился он не из-за отсутствия человека (в 92,4 % ошибок настоящий
// адресат В ВЕТКЕ БЫЛ), а из-за имени. Метод шёл искать по всему сайту, то есть
// заведомо не там, и находил прежнего владельца ника вместо нынешнего:
// требование единственного владельца проверялось только внутри nick_history и
// никогда не сверялось с users.name. Сама nick_history этим не опорочена —
// history_branch стоит на ней же и ошибается в 0,3 % случаев; лечила именно
// проверка присутствия в ветке.

// Само правило «обращение — это префикс до первой запятой» живёт в
// love.AddressPrefix: им пользуется и живое зеркало, когда решает, на какое
// сообщение треда отвечать.

// udfErr — ошибка регистрации UDF. Регистрация обязана произойти до открытия
// первого соединения (драйвер раздаёт функции только тем, что открыты после),
// поэтому она в init, а ошибка всплывает в Open.
var udfErr error

func init() {
	// ru_lower — регистронезависимое сравнение ников: встроенный lower() в
	// SQLite приводит регистр только у ASCII, а ники сплошь кириллические.
	if err := sqlite3.RegisterDeterministicScalarFunction("ru_lower", 1,
		func(_ *sqlite3.FunctionContext, args []driver.Value) (driver.Value, error) {
			s, _ := args[0].(string)
			return strings.ToLower(s), nil
		}); err != nil {
		udfErr = fmt.Errorf("регистрация ru_lower: %w", err)
		return
	}
	// addr_prefix — обращение из начала реплики; NULL, если обращения нет.
	if err := sqlite3.RegisterDeterministicScalarFunction("addr_prefix", 1,
		func(_ *sqlite3.FunctionContext, args []driver.Value) (driver.Value, error) {
			s, _ := args[0].(string)
			if p := love.AddressPrefix(s); p != "" {
				return p, nil
			}
			return nil, nil
		}); err != nil {
		udfErr = fmt.Errorf("регистрация addr_prefix: %w", err)
	}
}

// migrateV16SQL — слой адресатов: comment_addressee (кому реально адресована
// реплика) и nick_history (кто каким ником звался в каком месяце). Вьюхи
// социального графа переводятся с автора корня ветки на адресата; COALESCE
// оставляет прежнее поведение там, где адресат не разрешён, поэтому миграция
// безопасна до пересчёта — она создаёт пустые таблицы, а не переписывает данные.
const migrateV16SQL = `
CREATE TABLE nick_history (
    nick    TEXT    NOT NULL,             -- ник в нижнем регистре
    user_id INTEGER NOT NULL REFERENCES users(id),
    from_ym TEXT    NOT NULL,             -- 'YYYY-MM', первый месяц владения
    to_ym   TEXT    NOT NULL,             -- последний месяц владения
    hits    INTEGER NOT NULL,             -- обращений, на которых держится вывод
    PRIMARY KEY (nick, user_id, from_ym)
);
CREATE INDEX idx_nick_history_user ON nick_history(user_id);

CREATE TABLE comment_addressee (
    comment_id   INTEGER PRIMARY KEY REFERENCES comments(id),
    addressee_id INTEGER NOT NULL REFERENCES users(id),
    method       TEXT    NOT NULL,        -- branch | history_branch | history
    confidence   REAL    NOT NULL
);
CREATE INDEX idx_comment_addressee_to ON comment_addressee(addressee_id);

DROP VIEW IF EXISTS v_reply_edges;
CREATE VIEW v_reply_edges AS
SELECT c.author_id AS from_id, ca.name AS from_name,
       COALESCE(a.addressee_id, p.author_id) AS to_id, pa.name AS to_name,
       COUNT(*) AS replies
FROM comments c
JOIN comments p ON p.id = c.parent_id
LEFT JOIN comment_addressee a ON a.comment_id = c.id
JOIN users ca ON ca.id = c.author_id
JOIN users pa ON pa.id = COALESCE(a.addressee_id, p.author_id)
WHERE c.parent_id != 0
GROUP BY c.author_id, COALESCE(a.addressee_id, p.author_id);

DROP VIEW IF EXISTS v_persona_reply_edges;
CREATE VIEW v_persona_reply_edges AS
SELECT fi.identity AS from_identity, ti.identity AS to_identity, COUNT(*) AS replies
FROM comments c
JOIN comments pc ON pc.id = c.parent_id
LEFT JOIN comment_addressee a ON a.comment_id = c.id
JOIN v_identity fi ON fi.user_id = c.author_id
JOIN v_identity ti ON ti.user_id = COALESCE(a.addressee_id, pc.author_id)
WHERE c.parent_id != 0
GROUP BY fi.identity, ti.identity;
`

// migrateV18SQL — снятие метода history и колонки confidence.
//
// Строки метода удаляются здесь, а не оставляются до ближайшего пересчёта:
// BuildAddressees идёт часами по всему архиву, а до него граф молча продолжал бы
// разносить заведомо ложные рёбра. Обоснование самого снятия — в шапке файла.
//
// confidence уходит следом как мнимая гарантия: значение было чистой функцией
// method (1.0/1.0/0.9) и не читалось НИ ОДНИМ потребителем — ни вьюхами графа,
// ни diag/ensemble/relations, ни extract.py в навыке досье. То есть слой
// выглядел взвешивающим свои выводы, не взвешивая ничего; пусть лучше
// надёжность метода читается из его имени, чем из поля, которое все игнорируют.
// DROP COLUMN, а не пересоздание таблицы: RENAME TO переписал бы ссылки на
// таблицу внутри v_reply_edges/v_persona_reply_edges на временное имя.
const migrateV18SQL = `
DELETE FROM comment_addressee WHERE method = 'history';
ALTER TABLE comment_addressee DROP COLUMN confidence;
`

// Фрагменты для запросов социального графа. Правило «кому адресована реплика»
// должно жить в одном месте: точный адресат из слоя, иначе — автор корня ветки
// (прежнее поведение, оно же покрывает реплики без обращения).
//
// Требуют алиасов: c — реплика, pc — корень её ветки, a — слой адресатов.
const (
	// sqlAddresseeJoin — присоединение корня ветки и слоя адресатов к реплике c.
	sqlAddresseeJoin = `JOIN comments pc ON pc.id = c.parent_id
		LEFT JOIN comment_addressee a ON a.comment_id = c.id`
	// sqlAddressee — id адресата реплики c.
	sqlAddressee = `COALESCE(a.addressee_id, pc.author_id)`
)

// sqlInboundReplies — реплики, адресованные анкетам из списка in: сначала по
// индексу слоя (адресат мог отвечать и в чужой ветке), затем — неразрешённый
// хвост в собственных ветках этих анкет. UNION ALL вместо одного условия с
// COALESCE намеренно: так обе половины идут по индексам, а не полным сканом
// 10,7 млн комментариев. Список in подставляется в запрос дважды.
//
// cols — список колонок, где {ADDR} заменяется на выражение адресата (оно в
// половинах разное), а алиас c — на реплику.
func sqlInboundReplies(in, cols string) string {
	exact := strings.ReplaceAll(cols, "{ADDR}", "a.addressee_id")
	fallback := strings.ReplaceAll(cols, "{ADDR}", "pc.author_id")
	return `SELECT ` + exact + `
		FROM comment_addressee a
		JOIN comments c ON c.id = a.comment_id
		WHERE a.addressee_id IN (` + in + `)
		UNION ALL
		SELECT ` + fallback + `
		FROM comments c
		JOIN comments pc ON pc.id = c.parent_id
		WHERE c.parent_id IN (SELECT id FROM comments WHERE author_id IN (` + in + `))
		  AND NOT EXISTS (SELECT 1 FROM comment_addressee a WHERE a.comment_id = c.id)`
}

// AddresseeStats — итог пересчёта слоя адресатов.
type AddresseeStats struct {
	Replies       int // ответов в архиве (parent_id != 0)
	WithPrefix    int // из них с обращением «Ник, …»
	Reply         int // разрешено по дереву мобильной версии (точно, без догадок)
	Branch        int // разрешено по участнику ветки
	HistoryBranch int // разрешено по истории ников, владелец в ветке
	Nicks         int // строк в nick_history
}

// Resolved — сколько реплик получили точного адресата.
func (s AddresseeStats) Resolved() int {
	return s.Reply + s.Branch + s.HistoryBranch
}

// BuildAddressees пересчитывает nick_history и comment_addressee с нуля.
// Идемпотентна: обе таблицы очищаются перед заполнением. Тяжёлые промежуточные
// множества (участники веток, префиксы) живут во временных таблицах, а не в
// памяти процесса — на 10,7 млн комментариев разница принципиальная.
// progress получает читаемые отметки этапов; nil допустим.
func (s *Store) BuildAddressees(ctx context.Context, progress func(string)) (AddresseeStats, error) {
	var st AddresseeStats
	say := func(format string, args ...any) {
		if progress != nil {
			progress(fmt.Sprintf(format, args...))
		}
	}
	exec := func(what, query string, args ...any) error {
		if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	}

	say("подготовка: обращения из текстов реплик")
	// _pref — по строке на ответ с обращением: кто, кому (ник), в какой ветке,
	// в каком месяце. Джойн к корню ветки делается здесь один раз, чтобы
	// дальнейшие этапы обходились без обращения к comments.
	if err := exec("временная таблица обращений", `
		DROP TABLE IF EXISTS temp._pref;
		CREATE TEMP TABLE _pref (
			comment_id  INTEGER PRIMARY KEY,
			root        INTEGER NOT NULL,
			root_author INTEGER NOT NULL,
			author_id   INTEGER NOT NULL,
			nick        TEXT    NOT NULL,
			ym          TEXT    NOT NULL
		);
		INSERT INTO _pref
		SELECT c.id, c.parent_id, r.author_id, c.author_id,
		       addr_prefix(c.text), substr(COALESCE(c.published_at, ''), 1, 7)
		FROM comments c
		JOIN comments r ON r.id = c.parent_id
		WHERE c.parent_id != 0 AND addr_prefix(c.text) IS NOT NULL;
		CREATE INDEX temp._pref_root ON _pref(root);
		CREATE INDEX temp._pref_nick ON _pref(nick, ym);`); err != nil {
		return st, err
	}

	say("подготовка: участники веток и имена анкет")
	if err := exec("временная таблица участников", `
		DROP TABLE IF EXISTS temp._member;
		CREATE TEMP TABLE _member (
			root      INTEGER NOT NULL,
			author_id INTEGER NOT NULL,
			PRIMARY KEY (root, author_id)
		) WITHOUT ROWID;
		INSERT OR IGNORE INTO _member SELECT parent_id, author_id FROM comments WHERE parent_id != 0;
		INSERT OR IGNORE INTO _member SELECT id, author_id FROM comments WHERE parent_id = 0;

		DROP TABLE IF EXISTS temp._uname;
		CREATE TEMP TABLE _uname (user_id INTEGER PRIMARY KEY, nick TEXT NOT NULL);
		INSERT INTO _uname SELECT id, ru_lower(name) FROM users WHERE name != '';
		CREATE INDEX temp._uname_nick ON _uname(nick);`); err != nil {
		return st, err
	}

	if err := exec("очистка слоя", `DELETE FROM comment_addressee; DELETE FROM nick_history;`); err != nil {
		return st, err
	}

	say("этап 0/3: точные цели ответа с мобильной версии")
	// Пары, снятые слоем обогащения (comment_reply), — не догадка, а то, что
	// сайт хранит сам: они идут первыми и перекрывают все остальные методы
	// (дальше INSERT OR IGNORE и NOT EXISTS их не трогают). Обращение здесь не
	// требуется — точный адресат есть и у реплик без «Ник, …», которые
	// эвристике вообще недоступны. Само-адресация отсекается, как и в
	// остальных этапах: петля «сам себе» ничего не добавляет социальному графу,
	// а сырая пара остаётся в comment_reply.
	if err := exec("резолв по дереву мобильной", `
		INSERT OR IGNORE INTO comment_addressee(comment_id, addressee_id, method)
		SELECT r.comment_id, t.author_id, 'reply'
		FROM comment_reply r
		JOIN comments c ON c.id = r.comment_id
		JOIN comments t ON t.id = r.reply_to
		WHERE c.parent_id != 0 AND t.author_id != c.author_id;`); err != nil {
		return st, err
	}

	say("этап 1/3: адресат по участнику ветки")
	// Единственный участник ветки с таким текущим именем — и есть адресат.
	// HAVING COUNT(*) = 1 отсекает омонимов: лучше не разрешить, чем соврать.
	if err := exec("резолв по ветке", `
		INSERT OR IGNORE INTO comment_addressee(comment_id, addressee_id, method)
		SELECT p.comment_id, MIN(m.author_id), 'branch'
		FROM _pref p
		JOIN _member m ON m.root = p.root AND m.author_id != p.author_id
		JOIN _uname u ON u.user_id = m.author_id AND u.nick = p.nick
		GROUP BY p.comment_id
		HAVING COUNT(*) = 1;`); err != nil {
		return st, err
	}

	say("этап 2/3: история ников")
	// Доминирующее обращение месяца в собственных ветках человека — его ник в
	// этом месяце. Порог 35 % и минимум 3 обращения отсекают шум: в ветке
	// адресуются и третьим лицам. Месяцы схлопываются в интервал владения.
	if err := exec("история ников", `
		INSERT INTO nick_history(nick, user_id, from_ym, to_ym, hits)
		SELECT nick, user_id, MIN(ym), MAX(ym), SUM(hits)
		FROM (
			SELECT user_id, ym, nick, hits FROM (
				SELECT p.root_author AS user_id, p.ym AS ym, p.nick AS nick,
				       COUNT(*) AS hits,
				       SUM(COUNT(*)) OVER (PARTITION BY p.root_author, p.ym) AS total,
				       ROW_NUMBER() OVER (PARTITION BY p.root_author, p.ym ORDER BY COUNT(*) DESC, p.nick) AS rn
				FROM _pref p
				WHERE p.root_author != p.author_id AND p.ym != ''
				GROUP BY p.root_author, p.ym, p.nick
			)
			WHERE rn = 1 AND hits >= 3 AND hits * 100 >= total * 35
		)
		GROUP BY nick, user_id;`); err != nil {
		return st, err
	}

	say("этап 3/3: адресат по истории ников (участник ветки)")
	// Сменившие ник: users.name уже другое, но в момент реплики ник принадлежал
	// конкретной анкете. Присутствие владельца в ветке обязательно — оно же и
	// проверка на переиспользование ника. Без него метод выдаёт прежнего
	// владельца вместо нынешнего и ошибается практически всегда (см. шапку).
	if err := exec("резолв по истории (в ветке)", `
		INSERT OR IGNORE INTO comment_addressee(comment_id, addressee_id, method)
		SELECT p.comment_id, MIN(h.user_id), 'history_branch'
		FROM _pref p
		JOIN nick_history h ON h.nick = p.nick AND p.ym BETWEEN h.from_ym AND h.to_ym
		JOIN _member m ON m.root = p.root AND m.author_id = h.user_id
		WHERE h.user_id != p.author_id
		  AND NOT EXISTS (SELECT 1 FROM comment_addressee a WHERE a.comment_id = p.comment_id)
		GROUP BY p.comment_id
		HAVING COUNT(DISTINCT h.user_id) = 1;`); err != nil {
		return st, err
	}

	if err := exec("уборка временных таблиц", `
		DROP TABLE IF EXISTS temp._pref;
		DROP TABLE IF EXISTS temp._member;
		DROP TABLE IF EXISTS temp._uname;`); err != nil {
		return st, err
	}

	return s.addresseeStats(ctx)
}

// addresseeStats собирает итоговые счётчики слоя.
func (s *Store) addresseeStats(ctx context.Context) (AddresseeStats, error) {
	var st AddresseeStats
	row := s.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM comments WHERE parent_id != 0),
		       (SELECT COUNT(*) FROM comments WHERE parent_id != 0 AND addr_prefix(text) IS NOT NULL),
		       (SELECT COUNT(*) FROM comment_addressee WHERE method = 'reply'),
		       (SELECT COUNT(*) FROM comment_addressee WHERE method = 'branch'),
		       (SELECT COUNT(*) FROM comment_addressee WHERE method = 'history_branch'),
		       (SELECT COUNT(*) FROM nick_history)`)
	if err := row.Scan(&st.Replies, &st.WithPrefix, &st.Reply,
		&st.Branch, &st.HistoryBranch, &st.Nicks); err != nil {
		return st, fmt.Errorf("статистика слоя адресатов: %w", err)
	}
	return st, nil
}

// Coverage — доля ВСЕХ ответов архива, получивших точного адресата. Считается
// от всех, а не от ответов с обращением: метод reply разрешает и те реплики,
// где обращения нет вовсе, — от них знаменатель «с обращением» переполнялся бы.
func (s AddresseeStats) Coverage() float64 {
	if s.Replies == 0 {
		return 0
	}
	return float64(s.Resolved()) / float64(s.Replies)
}
