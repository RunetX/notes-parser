package archive

// Тема заметки: во сколько раз чаще человек в такую заходит.
//
// Замер вакуума 28.08.2026 показал, чего кубику не хватает после отклика. Состав
// заговоривших сходился с настоящим на 24 % — ровно столько даёт СЛУЧАЙ (пятеро
// наших и четверо настоящих из двенадцати). То есть ЧАСТОТУ прихода кубик
// воспроизводит, а ВЫБОР — «кто придёт именно в эту заметку» — нет, и не может:
// про заметку он не знает ничего, кроме того, что она новая.
//
// ЧЕГО ЗДЕСЬ НЕ ИЗМЕРИТЬ, и притворяться не станем: настоящая вероятность
// «прийти в заметку» неизмерима в принципе — в архиве видно, куда человек
// пришёл, и не видно, что он пролистал. Поэтому меряется не она, а ПЕРЕКОС:
// доля темы среди заметок, где он говорил, против доли той же темы среди всех
// заметок его времени. Перекос втрое значит, что про машины он заходит втрое
// охотнее среднего, — и это ровно то число, которое нужно множителем.
//
// Окно берётся ЕГО: заметки до его прихода и после ухода он не пролистывал, и
// считать их в знаменателе значило бы мерить не его вкус, а смену тем на сайте.

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
)

// TopicLift — перекос по одной теме.
type TopicLift struct {
	Key   string `json:"key"`
	Title string `json:"title"`

	// Notes/His — заметок этой темы в окне и из них тех, где он говорил.
	// Хранятся оба, а не готовая доля: по числам видно, стоит ли тема чего-то.
	Notes int `json:"notes"`
	His   int `json:"his"`

	Base  float64 `json:"base"`  // доля темы среди всех заметок окна
	Share float64 `json:"share"` // доля темы среди «его» заметок
	Lift  float64 `json:"lift"`  // Share / Base
}

// topicLiftMinNotes — ниже этого тема не считается замеренной.
//
// Тридцать заметок темы в окне и пять своих: перекос, посчитанный по двум
// заметкам, — это не редкий вкус, а отсутствие данных, и подставлять его
// множителем в кубик значит выдать шум за характер. Тот же порог и тот же довод,
// что у rateMinChances.
const (
	topicLiftMinNotes = 30
	topicLiftMinHis   = 5
)

// MineTopicLift считает перекос по темам за окно жизни человека на сайте.
func (s *Store) MineTopicLift(ctx context.Context, accIDs []int64, topics []TopicLexicon, maxNotes int) ([]TopicLift, error) {
	if len(accIDs) == 0 {
		return nil, fmt.Errorf("перекос по темам: не задана ни одна анкета")
	}
	res, err := compileTopics(topics)
	if err != nil {
		return nil, err
	}
	in := intList(accIDs)
	from, to, err := s.activeWindow(ctx, in)
	if err != nil || from == "" {
		return nil, err
	}
	his, err := s.idSet(ctx, `
		SELECT DISTINCT note_id FROM comments
		 WHERE author_id IN (`+in+`) AND published_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, text FROM notes
		 WHERE published_at BETWEEN ? AND ?
		 ORDER BY published_at DESC
		 LIMIT ?`, from, to, maxNotes)
	if err != nil {
		return nil, fmt.Errorf("перекос по темам: %w", err)
	}
	defer rows.Close()

	all := make([]int, len(res))
	mine := make([]int, len(res))
	total, totalHis := 0, 0
	for rows.Next() {
		var (
			id   int64
			text string
		)
		if err := rows.Scan(&id, &text); err != nil {
			return nil, err
		}
		total++
		if his[id] {
			totalHis++
		}
		for i, re := range res {
			if !re.MatchString(text) {
				continue
			}
			all[i]++
			if his[id] {
				mine[i]++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if total == 0 || totalHis == 0 {
		return nil, nil
	}

	out := make([]TopicLift, 0, len(res))
	for i, t := range topics {
		l := TopicLift{Key: t.Key, Title: t.Title, Notes: all[i], His: mine[i]}
		l.Base = float64(all[i]) / float64(total)
		l.Share = float64(mine[i]) / float64(totalHis)
		if l.Base > 0 {
			l.Lift = round2(l.Share / l.Base)
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Lift > out[j].Lift })
	return out, nil
}

// Measured — можно ли верить перекосу этой темы.
func (l TopicLift) Measured() bool {
	return l.Notes >= topicLiftMinNotes && l.His >= topicLiftMinHis && l.Lift > 0
}

// activeWindow — от первой до последней реплики человека.
func (s *Store) activeWindow(ctx context.Context, in string) (string, string, error) {
	var from, to sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT MIN(published_at), MAX(published_at) FROM comments
		 WHERE author_id IN (`+in+`) AND published_at IS NOT NULL`).Scan(&from, &to)
	if err != nil {
		return "", "", fmt.Errorf("окно активности: %w", err)
	}
	return from.String, to.String, nil
}

// compileTopics — регэкспы тем в том же порядке, что и сами темы.
func compileTopics(topics []TopicLexicon) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(topics))
	for _, t := range topics {
		re, err := t.compile()
		if err != nil {
			return nil, err
		}
		out = append(out, re)
	}
	return out, nil
}

// TopicsOf — темы одного текста: ключи тех лексиконов, что в нём сработали.
// Тем же разбором, что и замер, — второй рядом разошёлся бы с ним молча.
func TopicsOf(text string, topics []TopicLexicon) ([]string, error) {
	res, err := compileTopics(topics)
	if err != nil {
		return nil, err
	}
	var out []string
	for i, re := range res {
		if re.MatchString(text) {
			out = append(out, topics[i].Key)
		}
	}
	return out, nil
}
