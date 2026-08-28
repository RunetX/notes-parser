package archive

// Кто с кем разговаривал: подбор доноров под СОСТАВ, а не поодиночке.
//
// Замер вакуумного реплея 28.08.2026 показал, зачем это нужно. Трое жителей
// дают 0,8 реплики на заметку и разговор между собой раз на сотню прогонов:
// влезть в чужой разговор человек готов в доле процента случаев, и полпроцента
// с двух соседей — это тишина. Разговорчивость доноров в архиве держалась не на
// их характере, а на людности треда.
//
// Отсюда правило подбора: житель добирается не «покрупнее корпусом», а ПО
// СОСЕДСТВУ — кто на самом деле сидел в тех же тредах и отвечал тем же людям.
// Так у будущей сцены есть и плотность, и настоящий оригинал для сравнения:
// суженный до состава тред имеет смысл ровно тогда, когда состав в нём
// действительно разговаривал.
//
// ЭПОХА здесь не украшение отчёта, а условие: донор, замолчавший в 2015-м, и
// донор, пришедший в 2021-м, не встретятся ни в одном треде, сколько их ни
// сажай рядом. Поэтому у каждого кандидата печатаются края его жизни на сайте.

import (
	"context"
	"fmt"
)

// CoParticipant — кандидат в жители: сосед по тредам.
type CoParticipant struct {
	UserID int64  `json:"user_id"`
	Nick   string `json:"nick"`
	Gender string `json:"gender,omitempty"` // пусто — неизвестен

	// Comments — весь корпус человека. Ниже нескольких сотен снимать слепок
	// бессмысленно: полоса голоса набирается пачками по 600 знаков, и корпус в
	// полтораста реплик не даст ни полосы, ни устойчивых чисел регистра.
	Comments int `json:"comments"`

	// Threads — в скольких тредах состава он говорил, Said — сколько там сказал.
	// Первое отвечает «часто ли он рядом», второе — «слышно ли его».
	Threads int `json:"threads"`
	Said    int `json:"said"`

	// Replies — сколько раз он и состав отвечали друг другу НАСТОЯЩИМ ребром,
	// в обе стороны. Главное число подбора: сосед по треду бывает случайным,
	// собеседник — нет.
	Replies int `json:"replies"`

	First string `json:"first"` // первая реплика на сайте
	Last  string `json:"last"`  // последняя
}

// MineCoParticipants — соседи состава по тредам, от самых разговорчивых с ним.
//
// Треды берутся ПОСЛЕДНИЕ (maxThreads): состав живёт в своей эпохе, и сосед,
// сидевший рядом десять лет назад, сегодня в те же треды не придёт.
func (s *Store) MineCoParticipants(ctx context.Context, withIDs []int64, maxThreads, minComments, limit int) ([]CoParticipant, error) {
	if len(withIDs) == 0 {
		return nil, fmt.Errorf("подбор соседей: не задан состав")
	}
	in := intList(withIDs)
	notes, err := s.rateThreads(ctx, in, maxThreads)
	if err != nil {
		return nil, err
	}
	if len(notes) == 0 {
		return nil, nil
	}
	ids := intList(notes)

	rows, err := s.db.QueryContext(ctx, `
		WITH near AS (
		  SELECT c.author_id AS uid, COUNT(DISTINCT c.note_id) threads, COUNT(*) said
		    FROM comments c
		   WHERE c.note_id IN (`+ids+`) AND c.author_id NOT IN (`+in+`)
		     AND c.published_at IS NOT NULL
		   GROUP BY c.author_id
		), talk AS (
		  SELECT uid, SUM(n) replies FROM (
		    SELECT a.author_id uid, COUNT(*) n
		      FROM comment_reply r
		      JOIN comments a ON a.id = r.comment_id
		      JOIN comments p ON p.id = r.reply_to
		     WHERE a.note_id IN (`+ids+`) AND p.author_id IN (`+in+`)
		       AND a.author_id NOT IN (`+in+`)
		     GROUP BY a.author_id
		    UNION ALL
		    SELECT p.author_id uid, COUNT(*) n
		      FROM comment_reply r
		      JOIN comments a ON a.id = r.comment_id
		      JOIN comments p ON p.id = r.reply_to
		     WHERE a.note_id IN (`+ids+`) AND a.author_id IN (`+in+`)
		       AND p.author_id NOT IN (`+in+`)
		     GROUP BY p.author_id
		  ) GROUP BY uid
		), corpus AS (
		  SELECT author_id uid, COUNT(*) total,
		         MIN(published_at) first, MAX(published_at) last
		    FROM comments
		   WHERE author_id IN (SELECT uid FROM near) AND published_at IS NOT NULL
		   GROUP BY author_id
		)
		SELECT n.uid, coalesce(u.name, ''), coalesce(u.gender, ''),
		       coalesce(c.total, 0), n.threads, n.said, coalesce(t.replies, 0),
		       coalesce(c.first, ''), coalesce(c.last, '')
		  FROM near n
		  LEFT JOIN talk   t ON t.uid = n.uid
		  LEFT JOIN corpus c ON c.uid = n.uid
		  LEFT JOIN users  u ON u.id  = n.uid
		 WHERE coalesce(c.total, 0) >= ?
		 ORDER BY coalesce(t.replies, 0) DESC, n.said DESC, n.uid
		 LIMIT ?`, minComments, limit)
	if err != nil {
		return nil, fmt.Errorf("подбор соседей: %w", err)
	}
	defer rows.Close()

	var out []CoParticipant
	for rows.Next() {
		var p CoParticipant
		if err := rows.Scan(&p.UserID, &p.Nick, &p.Gender, &p.Comments,
			&p.Threads, &p.Said, &p.Replies, &p.First, &p.Last); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
