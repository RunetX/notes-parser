package archive

// Латентность: через сколько человек отзывается.
//
// Замер заведён под эмуляцию сообщества (эпик «народ»): без него все жители
// сбегаются в тред за одну минуту после заметки, и это первое, что выдаёт
// машину — раньше самого текста. Данные для замера лежали в архиве с самого
// начала (время реплики плюс настоящее дерево ответов), их просто никто не
// спрашивал.
//
// Меры две, и они про разное: «пришёл в тред» — это про заметку, увиденную в
// ленте (человек её ещё и прочитать должен), «ответил на реплику» — про
// разговор, где собеседник ждёт. У одного и того же человека они расходятся на
// порядок, поэтому одной средней задержки не хватает.

import (
	"context"
	"fmt"
	"time"
)

// Границы разумного. Отрицательная задержка — расхождение часов или строка,
// восстановленная из чужого зеркала; больше суток — это уже не отклик, а
// возвращение к старому разговору, и в квантилях оно только задирает край.
const (
	latencyMinSec = 0
	latencyMaxSec = 24 * 3600
)

// VoiceLatency — распределения задержек в секундах.
type VoiceLatency struct {
	ToThread Dist `json:"to_thread_sec"` // от публикации заметки до ПЕРВОЙ своей реплики в ней
	ToReply  Dist `json:"to_reply_sec"`  // от чужой реплики до ответа на неё
}

// MineLatency замеряет задержки личности (все её анкеты).
//
// Ответ считается по НАСТОЯЩЕМУ дереву (comment_reply), а не по parent_id:
// последний указывает на корень ветки и адресата угадывает лишь в трети случаев
// (см. шапку addressee.go), а задержка «от корня ветки» — это задержка от
// чужого разговора, к которому человек не имел отношения.
func (s *Store) MineLatency(ctx context.Context, accIDs []int64) (VoiceLatency, error) {
	var out VoiceLatency
	if len(accIDs) == 0 {
		return out, fmt.Errorf("latency: не задана ни одна анкета")
	}
	in := intList(accIDs)

	toReply, err := s.latencyPairs(ctx, `
		SELECT p.published_at, c.published_at
		  FROM comments c
		  JOIN comment_reply r ON r.comment_id = c.id
		  JOIN comments p      ON p.id = r.reply_to
		 WHERE c.author_id IN (`+in+`)
		   AND c.published_at IS NOT NULL AND p.published_at IS NOT NULL`)
	if err != nil {
		return out, err
	}

	// Первая реплика в треде: MIN по времени, а не по id. Полосы
	// идентификаторов у площадки разные, а время сравнимо всегда.
	toThread, err := s.latencyPairs(ctx, `
		SELECT n.published_at, f.first_at
		  FROM (SELECT note_id, MIN(published_at) AS first_at
		          FROM comments
		         WHERE author_id IN (`+in+`) AND published_at IS NOT NULL
		         GROUP BY note_id) f
		  JOIN notes n ON n.id = f.note_id
		 WHERE n.published_at IS NOT NULL`)
	if err != nil {
		return out, err
	}

	out.ToReply, out.ToThread = distOf(toReply), distOf(toThread)
	return out, nil
}

// latencyPairs читает пары «когда сказали — когда отозвался» и переводит их в
// секунды. Арифметика в Go, а не в julianday: формат времени в архиве наш
// (RFC3339), и разбирать его лучше тем же кодом, что его писал.
func (s *Store) latencyPairs(ctx context.Context, query string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var before, after string
		if err := rows.Scan(&before, &after); err != nil {
			return nil, err
		}
		if sec, ok := latencySec(before, after); ok {
			out = append(out, sec)
		}
	}
	return out, rows.Err()
}

// latencySec — чистое ядро замера: сколько секунд между двумя отметками и
// годится ли это в статистику.
func latencySec(before, after string) (int, bool) {
	b, err := time.Parse(time.RFC3339, before)
	if err != nil {
		return 0, false
	}
	a, err := time.Parse(time.RFC3339, after)
	if err != nil {
		return 0, false
	}
	sec := int(a.Sub(b).Seconds())
	if sec < latencyMinSec || sec > latencyMaxSec {
		return 0, false
	}
	return sec, true
}
