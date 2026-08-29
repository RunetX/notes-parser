package archive

// Первый заход: с какой вероятностью человек, БЫВШИЙ на сайте, зайдёт в новую
// заметку сам — начнёт в ней разговор, а не ответит кому-то.
//
// Величина эта считалась неизмеримой, и в кубике стояло придуманное число
// (`DiceParams.ComeToNote` = 0.35). Довод звучал так: в архиве видно, в какую
// заметку человек пришёл, и не видно, какую он пролистал, — то есть у события
// нет знаменателя. Довод неверен, и это выяснилось 29.08.2026, когда объём
// треда стал целью: знаменатель есть, если спросить иначе. ПРИСУТСТВИЕ человека
// на сайте доказывается его же репликой — где угодно, хоть в чужой заметке, — а
// значит «был на сайте в тот день и в эту заметку не зашёл» есть наблюдаемый
// промах, а не пустота.
//
// Замер по десяти конфликтным тредам: из 121 человека, бывшего на сайте в те же
// сутки, в заметку заходит 32 % (от 19 % до 59 % по заметкам). Придуманное 0.35
// попало почти точно — и это стоит записать рядом с обратным случаем: «влезть в
// чужой разговор = 0.15» был промахом в 20–40 раз. Угадывается частое.
//
// Отсюда же следует, ГДЕ на самом деле лежал разрыв «наш тред вчетверо короче»:
// не в этой вероятности, а в ЧИСЛЕ присутствующих. Сорок семь зашедших живых —
// это 32 % от ста двадцати одного, и тридцать жителей при той же готовности дают
// девять. Ручки, которая заменила бы людей, здесь нет.
//
// Мера эта ЛИЧНАЯ, как и разговорчивость: один заходит в каждую вторую заметку,
// другой в одну из двадцати. Поэтому и меряется по человеку, а не берётся общей
// константой — состав из «захожих» зажигает тред меньшим числом жителей, и это
// тот же рычаг, которым уже отобраны доноры по готовности влезть в чужой
// разговор.
//
// Сутки берутся по UTC, а не по новосибирскому поясу: сдвиг границы дня двигает
// и числитель, и знаменатель, а точность здесь нужна не до часа — вопрос стоит
// «был ли человек в эти дни на сайте вообще».

import (
	"context"
	"fmt"
)

// ComeRate — готовность зайти в новую заметку.
type ComeRate struct {
	// Days — по скольким СВОИМ дням снято. Ноль означает, что мерить было не на
	// чем, и такую меру нельзя подставлять в кубик.
	Days int `json:"days"`
	// Chances — заметок, вышедших в дни, когда человек был на сайте.
	Chances int `json:"chances"`
	// Came — в скольких из них он оказался КОРНЕМ (пришёл сам).
	Came int `json:"came"`

	// LiveChances/LiveCame — то же самое, но только по заметкам, У КОТОРЫХ РАЗГОВОР
	// СОСТОЯЛСЯ (не меньше liveThread реплик).
	//
	// Две меры, а не одна, потому что знаменатель обязан совпадать с тем, к чему
	// меру прикладывают. По ВСЕМ заметкам дня человек заходит в 14 %, но в
	// знаменателе там и мёртвые — те, где не написал никто и никогда; по живым он
	// заходит в 35 %. Разница ровно в доле живых заметок, и числа сходятся.
	//
	// Прикладывается это к заметке, которую пишет владелец или сам житель, — то
	// есть к той, ради которой всё и затевалось. Брать сюда среднее по всем
	// заметкам НГС значило бы заранее назначить большинству наших заметок судьбу
	// мёртвых, а замер по живым не выдумка: 35 % — настоящая доля зашедших в
	// заметку, у которой разговор получился.
	LiveChances int `json:"live_chances"`
	LiveCame    int `json:"live_came"`
}

// comeMinChances — ниже этого замером не считается. Порог тот же, что у отклика,
// и по той же причине: доля по трём случаям — это отсутствие данных.
const comeMinChances = 30

// Rate — личная доля заметок своего дня, в которые человек зашёл. Второе
// значение — был ли это замер. В кубик она идёт НЕ НАПРЯМУЮ (см. narod.ComeRate):
// мерилась она там, где в день выходило от восьми до двадцати восьми заметок, а
// у нашей площадки заметка в день одна, и внимание ни с чем не делится.
func (c ComeRate) Rate() (float64, bool) {
	if c.LiveChances < comeMinChances {
		return 0, false
	}
	return float64(c.LiveCame) / float64(c.LiveChances), true
}

// AllRate — то же по ВСЕМ заметкам дня, включая мёртвые. В кубик не идёт, но
// стоит в карточке: по паре чисел видно, насколько редка живая заметка вообще.
func (c ComeRate) AllRate() (float64, bool) {
	if c.Chances < comeMinChances {
		return 0, false
	}
	return float64(c.Came) / float64(c.Chances), true
}

// MineComeRate считает готовность зайти в новую заметку по последним maxDays
// дням, когда человек писал хоть что-нибудь.
//
// Дни берутся ПОСЛЕДНИЕ по тому же доводу, что треды у отклика: за тринадцать
// лет человек успевает стать другим, а хвост утянул бы меру к тому, каким он был
// когда-то.
func (s *Store) MineComeRate(ctx context.Context, accIDs []int64, maxDays int) (ComeRate, error) {
	var out ComeRate
	if len(accIDs) == 0 {
		return out, fmt.Errorf("первый заход: не задана ни одна анкета")
	}
	in := intList(accIDs)
	// Дни присутствия. Собственная реплика — доказательство того, что человек в
	// этот день на сайте был; обратное неверно (читал и молчал), и потому мера
	// выходит ВЕРХНЕЙ оценкой готовности: дни, когда он заходил и ничего не
	// написал, в знаменатель не попадают.
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT date(published_at) FROM comments
		 WHERE author_id IN (`+in+`) AND published_at IS NOT NULL
		 ORDER BY 1 DESC LIMIT ?`, maxDays)
	if err != nil {
		return out, fmt.Errorf("замер первого захода, дни: %w", err)
	}
	var days []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return out, err
		}
		days = append(days, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(days) == 0 {
		return out, nil
	}
	out.Days = len(days)
	marks := placeholders(len(days))
	args := make([]any, len(days))
	for i, d := range days {
		args[i] = d
	}

	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM notes
		 WHERE published_at IS NOT NULL AND date(published_at) IN (`+marks+`)`, args...).Scan(&out.Chances); err != nil {
		return out, fmt.Errorf("замер первого захода, заметки: %w", err)
	}
	// Корень — реплика без ребра ответа. Своя заметка не считается заходом: под
	// своей человек появляется, отвечая пришедшим, и кубик её тоже пропускает.
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT c.note_id)
		  FROM comments c
		  JOIN notes n ON n.id = c.note_id
		  LEFT JOIN comment_reply r ON r.comment_id = c.id
		 WHERE c.author_id IN (`+in+`) AND r.comment_id IS NULL
		   AND n.author_id NOT IN (`+in+`)
		   AND n.published_at IS NOT NULL AND date(n.published_at) IN (`+marks+`)`, args...).Scan(&out.Came); err != nil {
		return out, fmt.Errorf("замер первого захода, приходы: %w", err)
	}

	// То же по ЖИВЫМ заметкам — тем, где разговор состоялся. Знаменатель обязан
	// совпадать с тем, к чему меру прикладывают: мёртвая заметка не выбор
	// человека, а её собственное свойство, и держать её в знаменателе значило бы
	// назначить нашей заметке чужую судьбу заранее.
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM notes n
		 WHERE n.published_at IS NOT NULL AND date(n.published_at) IN (`+marks+`)
		   AND (SELECT COUNT(*) FROM comments c WHERE c.note_id = n.id) >= `+liveThreadSQL,
		args...).Scan(&out.LiveChances); err != nil {
		return out, fmt.Errorf("замер первого захода, живые заметки: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT c.note_id)
		  FROM comments c
		  JOIN notes n ON n.id = c.note_id
		  LEFT JOIN comment_reply r ON r.comment_id = c.id
		 WHERE c.author_id IN (`+in+`) AND r.comment_id IS NULL
		   AND n.author_id NOT IN (`+in+`)
		   AND n.published_at IS NOT NULL AND date(n.published_at) IN (`+marks+`)
		   AND (SELECT COUNT(*) FROM comments c2 WHERE c2.note_id = n.id) >= `+liveThreadSQL,
		args...).Scan(&out.LiveCame); err != nil {
		return out, fmt.Errorf("замер первого захода, приходы в живые: %w", err)
	}
	return out, nil
}

// liveThreadSQL — сколько реплик делает заметку «живой». Двадцать: ниже этого
// разговора не было вовсе, и промах «был на сайте, а не зашёл» там объясняется
// не человеком, а самой заметкой.
const liveThreadSQL = "20"
