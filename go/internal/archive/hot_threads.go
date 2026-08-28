package archive

// Подбор БУРЛЯЩИХ тредов — тех, где разговор был, а не соседство высказываний.
//
// Заведено 28.08.2026 по решению владельца: «самые бурные дискуссии как правило
// основываются на конфликте с текстом или на конфликте между участниками; добрый
// тон нам малоинтересен». И первый же платный прогон вакуума это подтвердил с
// другой стороны: летописец за восемь разговоров не нашёл ни одной ссоры, а
// раздражение не сдвинулось ни разу — потому что ссориться было не над чем.
// Мирный тред не проверяет ни граф отношений, ни память: у эмуляции сообщества
// нечего мерить там, где сообщество вежливо согласилось.
//
// МЕРА ЖАРА — ПЕРЕПАЛКА, а не объём. Тред на четыреста реплик, где каждый
// высказался заметке и разошёлся, — это не разговор, это очередь у микрофона;
// тред на сорок, где двое двадцать раз ответили друг другу, — разговор.
// Считается это настоящими рёбрами `comment_reply` (мобильное дерево сайта), а
// не догадкой по обращению: догадка ошибается примерно вполовину, и жар,
// посчитанный по ней, был бы наполовину выдуман.
//
// Двусторонность обязательна. Пара, где А ответил Б десять раз, а Б ему ни
// разу, — это не спор, это монолог с адресатом; пара, где ответили ОБА, — самое
// близкое к «сцепились», что вообще видно из архива, не читая текста.
//
// Чего эта мера НЕ ловит и знать об этом обязан всякий, кто её читает:
// «конфликт с текстом» — скрытую провокацию самой заметки. Он живёт в словах, а
// не в структуре, и вытащить его отсюда нечем. Но у провокации есть СЛЕДСТВИЕ,
// и оно как раз структурное: люди начинают отвечать друг другу. Поэтому мера
// ловит обе причины по их общему следу — и потому же она про тред, а не про
// заметку.

import (
	"context"
	"fmt"
)

// HotPick — тред вместе с тем, насколько в нём бурлило.
type HotPick struct {
	ThreadPick
	// Duels — пар, обменявшихся репликами В ОБЕ стороны.
	Duels int
	// Longest — самая длинная такая пара: сколько реплик между двумя.
	Longest int
	// Edges — сколько реплик треда вообще имеют настоящее ребро. Стоит рядом с
	// жаром потому, что без него жар нечитаем: ноль дуэлей у треда без рёбер
	// значит «не смотрели», а у треда с тремя сотнями рёбер — «не спорили», и
	// это совсем разные вещи.
	Edges int
}

// Heat — перепалок на сотню реплик. ПЛОТНОСТЬ, а не число, и это правлено сразу
// после первого прогона подбора: по абсолютному числу наверх встали простыни на
// две-четыре тысячи реплик (482 перепалки при 3888 репликах — 12 на сотню), а
// владелец, показав образец, назвал признак иначе — «ёмкий и определённо с
// конфликтами». Он прав арифметически: в длинном треде перепалок много просто
// потому, что в нём много всего, и отбор по их числу отбирает длину.
func (p HotPick) Heat() float64 {
	if p.Total == 0 {
		return 0
	}
	return float64(p.Duels) * 100 / float64(p.Total)
}

// hotMinTotal — ниже этого тред в подбор не идёт вовсе.
//
// Не ради статистики, а потому, что у плотности маленький знаменатель: в треде
// из шести реплик одна перепалка даёт 17 на сотню и обходит любой настоящий
// спор. Тридцать — примерно та величина, ниже которой дюжине жителей просто
// негде разговориться.
const hotMinTotal = 30

// PickHotThreads — треды донорского состава, отсортированные по перепалке.
//
// Порядок здесь УБЫВАЮЩИЙ, и это сознательный отход от равномерности, которой
// живёт PickCalibrationThreads. Там равномерность обязательна: мерилась полнота
// прихода, и верхушка списка подменяла её потолком реплик. Здесь мерится
// другое — двигается ли граф отношений, — и «взять самые бурные» это не перекос
// выборки, а её ПРЕДМЕТ: спрашивается, справится ли эмуляция со ссорой, а не
// как часто ссоры попадаются.
func (s *Store) PickHotThreads(ctx context.Context, userIDs []int64, minSaid, limit int) ([]HotPick, error) {
	if len(userIDs) == 0 {
		return nil, fmt.Errorf("не заданы анкеты донора")
	}
	ids := intList(userIDs)
	rows, err := s.db.QueryContext(ctx, `
		WITH mine AS (
		    SELECT DISTINCT note_id FROM comments WHERE author_id IN (`+ids+`)
		),
		said AS (
		    SELECT c.note_id,
		           SUM(CASE WHEN c.author_id IN (`+ids+`) THEN 1 ELSE 0 END) said,
		           COUNT(*) total
		      FROM comments c
		     WHERE c.note_id IN (SELECT note_id FROM mine)
		     GROUP BY c.note_id
		    HAVING said >= ? AND total >= ?
		),
		edge AS (
		    SELECT c.note_id,
		           MIN(c.author_id, p.author_id) a,
		           MAX(c.author_id, p.author_id) b,
		           c.author_id src
		      FROM comment_reply r
		      JOIN comments c ON c.id = r.comment_id
		      JOIN comments p ON p.id = r.reply_to
		     WHERE c.note_id IN (SELECT note_id FROM said)
		       AND c.author_id <> p.author_id
		),
		pair AS (
		    SELECT note_id, a, b, COUNT(*) n, COUNT(DISTINCT src) dirs
		      FROM edge GROUP BY note_id, a, b
		)
		SELECT s.note_id, s.said, s.total,
		       COALESCE(SUM(CASE WHEN p.dirs = 2 THEN 1 ELSE 0 END), 0) duels,
		       COALESCE(MAX(CASE WHEN p.dirs = 2 THEN p.n ELSE 0 END), 0) longest,
		       COALESCE(SUM(p.n), 0) edges
		  FROM said s LEFT JOIN pair p ON p.note_id = s.note_id
		 GROUP BY s.note_id, s.said, s.total
		 ORDER BY CAST(duels AS REAL) / s.total DESC, duels DESC, s.note_id`, minSaid, hotMinTotal)
	if err != nil {
		return nil, fmt.Errorf("подбор бурлящих тредов: %w", err)
	}
	defer rows.Close()

	var out []HotPick
	for rows.Next() {
		var p HotPick
		if err := rows.Scan(&p.NoteID, &p.Said, &p.Total,
			&p.Duels, &p.Longest, &p.Edges); err != nil {
			return nil, err
		}
		out = append(out, p)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}
