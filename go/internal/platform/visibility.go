package platform

// Проход по ВСЕМ публикациям одного человека — и почему он не помещается в
// обработчик формы.
//
// Отзыв согласия на распространение (ст. 10.1) исполняется записью статусов, а
// не проверкой на чтении: иначе он стоил бы соединения с users на каждой
// странице ленты, и однажды его убрали бы оттуда «для скорости», тихо сломав
// исполнение закона. Это решение зафиксировано у feedQuery и остаётся в силе.
//
// Цена у него измерена. 18.08.2026, боевой Postgres площадки (comments — 10,77
// млн строк, 5,4 ГБ, 1 vCPU): один проход по строкам автора с 138 тыс. реплик
// стоит 53 секунды. Индекс comments_author находит их мгновенно, но статус
// лежит в куче — это 138 тыс. случайных чтений мимо кэша. У пула веб-морды
// statement_timeout 5 секунд, и это не перестраховка, а доля Postgres, которую
// вправе занять посторонний.
//
// Отсюда устройство: форма делает столько, сколько успевает в свой бюджет, и
// поднимает флаг users.visibility_dirty; остальное доводит фоновая служба
// демона (internal/platmod). Флаг, а не таблица заданий, — потому что состояние
// «статусы разошлись с рубильником» полностью выводится из самой базы, и
// очередь заданий была бы вторым источником правды, умеющим разойтись с первым.
// Флаг самозалечивается: служба гасит его, лишь когда проход не нашёл НИ ОДНОЙ
// строки.

import (
	"context"
	"fmt"
	"time"
)

const (
	// visibilityChunk — сколько комментариев двигаем за один запрос. Две тысячи
	// случайных чтений кучи — это доли секунды даже на холодном кэше, то есть
	// порция заведомо укладывается в срок запроса, каким бы плотным ни был
	// наплыв.
	visibilityChunk = 2000
	// webVisibilityBudget — сколько времени на проход отпущено ФОРМЕ. Меньше
	// бюджета запроса (8 с) и меньше statement_timeout пула морды (5 с): на
	// остальное человеку ещё надо нарисовать страницу.
	webVisibilityBudget = 2500 * time.Millisecond
	// jobVisibilityBudget — то же для фоновой службы. Ей спешить некуда, но
	// держать соединение демона часами тоже незачем: не доделала — доделает на
	// следующем такте, флаг никуда не денется.
	jobVisibilityBudget = 30 * time.Second
)

// sweepComments двигает ПОРЦИЮ комментариев автора между статусами и правит
// денормализованные счётчики затронутых заметок.
//
// Одним запросом, потому что счётчик обязан меняться вместе со статусами: два
// запроса дали бы окно, в котором под заметкой стоит «Комментарии 42», а видно
// сорок. Разницей, а не пересчётом, — пересчёт брал бы count(*) в КАЖДОМ
// треде, где человек когда-либо отвечал (у участника с 14 тыс. таких тредов это
// минуты на таблице в 10,7 млн строк; 18.08.2026 трое не смогли пройти экран
// согласий вовсе).
//
// Число сдвинутых строк считается по RETURNING, а не по самой таблице: снимок у
// всех частей запроса общий, и count(*) по comments увидел бы статусы ДО сдвига.
func sweepComments(ctx context.Context, q querier, userID int64, from, to Status, limit int) (int, error) {
	delta := -1
	if to == StatusVisible {
		delta = 1
	}
	var moved int
	err := q.QueryRow(ctx, `
		WITH picked AS (
		    SELECT id FROM comments
		     WHERE author_id = $1 AND status = $3
		     ORDER BY id
		     LIMIT $5
		), moved AS (
		    UPDATE comments c SET status = $2
		      FROM picked p WHERE c.id = p.id
		    RETURNING c.note_id
		), touched AS (
		    SELECT note_id, count(*) AS n FROM moved GROUP BY note_id
		), bumped AS (
		    -- Изменяющий CTE выполняется всегда и целиком, даже если основной
		    -- запрос его не читает, — поэтому счётчики правятся, а наружу идёт
		    -- только число сдвинутых комментариев.
		    UPDATE notes n
		       SET comment_count = greatest(0, n.comment_count + $4::int * t.n)
		      FROM touched t
		     WHERE n.id = t.note_id
		)
		SELECT count(*) FROM moved`,
		userID, to, from, delta, limit).Scan(&moved)
	if err != nil {
		return 0, fmt.Errorf("сдвиг видимости комментариев %d: %w", userID, err)
	}
	return moved, nil
}

// setOwnVisibility прячет или возвращает публикации человека, сколько успеет за
// budget. Возвращает true, если работа доведена до конца.
//
// Заметки двигаются целиком и без порций: их у самого плодовитого автора
// площадки тысячи, а не сотни тысяч, и идут они по своему индексу
// (notes_author_all — он полный именно затем, чтобы скрытая строка из счёта не
// выпадала).
//
// Скрытое МОДЕРАЦИЕЙ (StatusHiddenMod) не трогается ни в ту, ни в другую
// сторону: возврат согласия не отменяет решения модератора, а отзыв не должен
// присваивать модераторскому скрытию чужую причину.
func setOwnVisibility(ctx context.Context, q querier, userID int64, to Status, budget time.Duration) (bool, error) {
	from := StatusVisible
	if to == StatusVisible {
		from = StatusHiddenOwner
	}
	if _, err := q.Exec(ctx,
		`UPDATE notes SET status = $2 WHERE author_id = $1 AND status = $3`, userID, to, from); err != nil {
		return false, fmt.Errorf("сдвиг видимости заметок %d: %w", userID, err)
	}
	deadline := time.Now().Add(budget)
	for {
		n, err := sweepComments(ctx, q, userID, from, to, visibilityChunk)
		if err != nil {
			return false, err
		}
		if n < visibilityChunk {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
	}
}

// markVisibilityDirty поднимает или гасит флаг «проход не доведён до конца».
func markVisibilityDirty(ctx context.Context, q querier, userID int64, dirty bool) error {
	_, err := q.Exec(ctx, `
		UPDATE users SET visibility_dirty = $2
		 WHERE id = $1 AND visibility_dirty <> $2`, userID, dirty)
	return wrapf(err, "отметка незавершённого прохода %d", userID)
}

// DirtyVisibility — люди, у которых проход не доведён до конца. Спрашивает
// фоновая служба; список короткий по построению — это не очередь заданий, а
// список расхождений.
func (p *Platform) DirtyVisibility(ctx context.Context, limit int) ([]int64, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id FROM users WHERE visibility_dirty ORDER BY id LIMIT $1`, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("незавершённые проходы: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("незавершённые проходы: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SettleVisibility доводит отложенный проход. Возвращает true, когда доводить
// больше нечего.
//
// Направление берётся из САМОГО рубильника (users.hide_all), а не из параметра:
// пока проход шёл, человек мог вернуть согласие, и продолжать прятать было бы
// исполнением уже отменённого распоряжения.
//
// Транзакции здесь нет намеренно: порции идут своими запросами, и каждая
// оставляет базу в согласованном виде (статусы и счётчики двигаются вместе).
// Одна транзакция на десятки секунд держала бы строки заметок под замком ровно
// в тех тредах, где сейчас разговаривают.
func (p *Platform) SettleVisibility(ctx context.Context, userID int64) (bool, error) {
	var hideAll bool
	if err := p.pool.QueryRow(ctx,
		`SELECT hide_all FROM users WHERE id = $1`, userID).Scan(&hideAll); err != nil {
		return false, wrapf(err, "доводка видимости %d", userID)
	}
	to := StatusVisible
	if hideAll {
		to = StatusHiddenOwner
	}
	done, err := setOwnVisibility(ctx, p.pool, userID, to, jobVisibilityBudget)
	if err != nil {
		return false, err
	}
	if !done {
		return false, nil
	}
	return true, markVisibilityDirty(ctx, p.pool, userID, false)
}
