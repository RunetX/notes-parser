package archive

// Портрет СОДЕРЖАНИЯ по архиву: чем наполнены реплики живых людей.
//
// Меры считает листовой `speech` (он же меряет наши), здесь только выборка: из
// какого корпуса брать тексты и чьи. Разделение то же, что у нормы ошибок
// (BuildCorpusNorm) и по той же причине — «как пишет этот человек» без «как
// пишут все» не значит ничего.

import (
	"context"
	"fmt"

	"lovegw/internal/speech"
)

// SpeechCorpus — портрет содержания по выборке корпуса: как говорят ВСЕ.
//
// Комментарии, а не заметки, — по доводу BuildCorpusNorm: реплика это то, что
// пишут в разговоре, а заметку на сайте писали с оглядкой, её видно в ленте
// целиком. Выборка идёт с конца (последние по номеру): нам нужна нынешняя
// манера, а не средняя за тринадцать лет, — в 2013-м и сайт, и разговор были
// другими.
// CorpusTexts — те же реплики, что берёт SpeechCorpus, но СЫРЫМИ.
//
// Отдельным методом, а не полем результата: своду (speech.Sweep) нужны сами
// слова, а портрету содержания — только доли, и таскать сотню тысяч строк ради
// пяти чисел незачем. Выборка та же и по тому же доводу — с конца, нам нужна
// нынешняя манера, а не средняя за тринадцать лет.
func (s *Store) CorpusTexts(ctx context.Context, sample int) ([]string, error) {
	if sample <= 0 {
		sample = 100000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT text FROM comments WHERE text != '' ORDER BY id DESC LIMIT ?`, sample)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var texts []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		texts = append(texts, t)
	}
	return texts, rows.Err()
}

func (s *Store) SpeechCorpus(ctx context.Context, sample int) (speech.Marks, error) {
	if sample <= 0 {
		sample = 100000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT text FROM comments WHERE text != '' ORDER BY id DESC LIMIT ?`, sample)
	if err != nil {
		return speech.Marks{}, err
	}
	defer rows.Close()
	var texts []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return speech.Marks{}, err
		}
		texts = append(texts, t)
	}
	if err := rows.Err(); err != nil {
		return speech.Marks{}, err
	}
	return speech.Measure(texts), nil
}

// SpeechOf — портрет содержания одной личности (token: p<id>|u<id>|user_id).
//
// Личности, а не анкеты: у человека здесь по три-пять анкет, и разговор его
// размазан по ним — тот же resolver, что у карты письма (voiceTarget), иначе
// «донор говорит случаем в 20 % реплик» считалось бы по последней его анкете.
func (s *Store) SpeechOf(ctx context.Context, token string, limit int) (speech.Marks, error) {
	_, accIDs, err := s.voiceTarget(ctx, token, false)
	if err != nil {
		return speech.Marks{}, err
	}
	texts, err := s.voiceTexts(ctx, accIDs, "comments", limit)
	if err != nil {
		return speech.Marks{}, err
	}
	if len(texts) == 0 {
		return speech.Marks{}, fmt.Errorf("портрет содержания: у %s нет реплик в архиве", token)
	}
	bodies := make([]string, 0, len(texts))
	for _, t := range texts {
		bodies = append(bodies, t.text)
	}
	return speech.Measure(bodies), nil
}

// SpeechNote — портрет содержания ОДНОГО живого треда.
//
// Самая честная мерка для песочницы: тот же разговор, а не средняя температура
// корпуса. Именно так ловился уход разговора в гараж — наш тред сравнивали с
// живым тредом на ту же тему, — и мера содержания заводится с той же дорогой.
func (s *Store) SpeechNote(ctx context.Context, noteID int64) (speech.Marks, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT text FROM comments WHERE note_id = ? AND text != '' ORDER BY id`, noteID)
	if err != nil {
		return speech.Marks{}, err
	}
	defer rows.Close()
	var texts []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return speech.Marks{}, err
		}
		texts = append(texts, t)
	}
	if err := rows.Err(); err != nil {
		return speech.Marks{}, err
	}
	if len(texts) == 0 {
		return speech.Marks{}, fmt.Errorf("портрет содержания: у заметки %d нет реплик в архиве", noteID)
	}
	return speech.Measure(texts), nil
}
