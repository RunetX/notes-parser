package archive

// Повторный заход: с какой вероятностью человек, уже говоривший в треде, ЗАЙДЁТ
// В САМУ ЗАМЕТКУ ещё раз, а не ответит кому-то.
//
// Отдельно от reply_rate.go намеренно, хотя единица возможности у них общая:
// там вопрос «ответишь ли ты ЭТОМУ», здесь — «начнёшь ли ты новую ветку». Это
// разные поступки, и мерить их одной долей значило бы получить среднее между
// разговором и монологом.
//
// Замер заведён 29.08.2026 по итогам прогона с поднятым потолком реплик, и
// повод у него точный. Тред у нас выходил вчетверо короче оригинального при
// БОЛЬШЕМ разрастании (0,90 против 0,82), и вся разница сидела в КОРНЯХ —
// репликах, пришедших в заметку, а не в ответ кому-то: у нас 7 на заметку, в
// оригинале 58. Размер треда есть корни, делённые на (1 − разрастание), и обе
// оценки сошлись с замером число в число, то есть причина была ровно одна.
//
// А причин у самих корней две, и вот вторая — та, что чинится здесь. В кубике
// «прийти в заметку» бросалось РОВНО ОДИН РАЗ за её жизнь: не пришёл при
// публикации — не придёшь уже никогда, разве что ответом. При таком устройстве
// тридцать жителей не дадут больше двадцати девяти корней ни при какой
// вероятности. Замер по десяти конфликтным тредам:
//
//	корней всего                583
//	разных людей в корнях       393  (23–60 на заметку)
//	корней на человека         1,48
//	ПОВТОРНЫХ заходов           40 % всех корней
//
// То есть двое из пяти корней — это человек, который в треде уже говорил и
// вернулся начать новую ветку. В позднем треде (313040, 2026 год) повторных
// было 100 против 53 первых, то есть больше половины.
//
// Мерить ПЕРВЫЙ заход этот замер не берётся и не может: в архиве видно, в какую
// заметку человек пришёл, и не видно, какую пролистал, — знаменателя у первого
// захода нет. У повторного он есть: человек уже в треде, и каждая чужая реплика
// после его первого слова — это возможность, которой он либо воспользовался,
// либо нет. Тот же знаменатель, что у отклика, и та же единица.

import (
	"context"
	"fmt"
	"time"
)

// RootRate — кривая повторного захода.
type RootRate struct {
	Threads int          `json:"threads"`
	Buckets []RateBucket `json:"buckets,omitempty"` // по позиции в треде
	Tempo   []RateBucket `json:"tempo,omitempty"`   // по накалу треда

	// Firsts/Repeats — корней первых и повторных в замеренных тредах. В кубик не
	// идут: это свидетельство о самом замере, по которому видно, стоит ли он
	// чего-нибудь. Доля повторных у человека, зашедшего всюду по разу, — ноль, и
	// без этих двух чисел ноль читался бы как «не любит начинать ветки».
	Firsts  int `json:"firsts"`
	Repeats int `json:"repeats"`
}

// Rate — вероятность повторного захода на очередной чужой реплике в позиции pos.
// Второе значение — был ли это ЗАМЕР: пустая корзина возвращает 0, false.
func (r RootRate) Rate(pos int) (float64, bool) { return rateIn(r.Buckets, pos, false) }

// TempoRate — то же при накале n (реплик за последние tempoWindow).
func (r RootRate) TempoRate(n int) (float64, bool) { return rateIn(r.Tempo, n, false) }

// MineRootRate считает кривую повторного захода по последним maxThreads тредам,
// где человек участвовал. Треды берутся ПОСЛЕДНИЕ по тому же доводу, что у
// MineReplyRate: манера меняется за годы.
func (s *Store) MineRootRate(ctx context.Context, accIDs []int64, maxThreads int) (RootRate, error) {
	var out RootRate
	if len(accIDs) == 0 {
		return out, fmt.Errorf("повторный заход: не задана ни одна анкета")
	}
	in := intList(accIDs)
	notes, err := s.rateThreads(ctx, in, maxThreads)
	if err != nil {
		return out, err
	}
	if len(notes) == 0 {
		return out, nil
	}
	ids := intList(notes)
	// Корень — реплика без ребра ответа. Спрашиваем МНОЖЕСТВОМ, как и в замере
	// отклика: коррелированный EXISTS на десятках тысяч строк не укладывается и
	// в десять минут.
	replied, err := s.idSet(ctx, `
		SELECT r.comment_id FROM comment_reply r
		  JOIN comments c ON c.id = r.comment_id
		 WHERE c.note_id IN (`+ids+`)`)
	if err != nil {
		return out, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT note_id, id, author_id IN (`+in+`), published_at
		  FROM comments
		 WHERE note_id IN (`+ids+`) AND published_at IS NOT NULL
		 ORDER BY note_id, published_at, id`)
	if err != nil {
		return out, fmt.Errorf("замер повторного захода: %w", err)
	}
	defer rows.Close()

	out.Buckets = make([]RateBucket, len(rateBuckets))
	for i, up := range rateBuckets {
		out.Buckets[i].Upto = up
	}
	out.Tempo = make([]RateBucket, len(tempoBuckets))
	for i, up := range tempoBuckets {
		out.Tempo[i].Upto = up
	}

	// Тред копится целиком: возможность засчитывается ЗАДНИМ ЧИСЛОМ — по чужой
	// реплике видно, что человек пришёл, только когда встретится его следующая.
	type step struct {
		own, root bool
		pos, temp int
	}
	var (
		note   int64
		steps  []step
		pos    int
		recent []time.Time
	)
	flush := func() {
		if len(steps) == 0 {
			return
		}
		out.Threads++
		spoke := false
		for i, st := range steps {
			if st.own {
				if st.root {
					if spoke {
						out.Repeats++
					} else {
						out.Firsts++
					}
				}
				spoke = true
				continue
			}
			if !spoke {
				// До первого своего слова возможности нет: это ПЕРВЫЙ заход, а у
				// него нет знаменателя (см. шапку).
				continue
			}
			// Успех — свой КОРЕНЬ до следующей чужой реплики. Возможностью служит
			// та же единица, что у отклика: очередная чужая реплика треда.
			came := false
			for j := i + 1; j < len(steps) && steps[j].own; j++ {
				if steps[j].root {
					came = true
					break
				}
			}
			countChance(&out.Buckets[bucketOf(st.pos)], false, came)
			countChance(&out.Tempo[bucketOfInt(st.temp, tempoBuckets)], false, came)
		}
		steps = steps[:0]
	}
	for rows.Next() {
		var noteID, id int64
		var own bool
		var at string
		if err := rows.Scan(&noteID, &id, &own, &at); err != nil {
			return out, err
		}
		if noteID != note {
			flush()
			note, pos = noteID, 0
			recent = recent[:0]
		}
		pos++
		now, err := parseArchiveTime(at)
		if err != nil {
			continue
		}
		edge := now.Add(-tempoWindow)
		cut := 0
		for cut < len(recent) && recent[cut].Before(edge) {
			cut++
		}
		recent = recent[cut:]
		temp := len(recent)
		recent = append(recent, now)
		steps = append(steps, step{own: own, root: !replied[id], pos: pos, temp: temp})
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	flush()
	return out, nil
}
