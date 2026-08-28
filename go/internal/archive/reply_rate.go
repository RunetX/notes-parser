package archive

// Отклик: с какой вероятностью человек отвечает на очередную реплику треда.
//
// Отдельно от decay.go намеренно: там ФОРМА РАЗГОВОРА вообще (когда тред
// умирает), здесь — поведение ОДНОГО человека внутри живого треда. Величины
// разные, и складывать их в один файл значило бы предлагать читателю искать, про
// кого очередная функция.
//
// Замер заведён под эмуляцию сообщества, и заведён по итогам прогона
// 28.08.2026, который показал, ЧЕГО кубику не хватает. Базовые вероятности были
// взяты с потолка одним числом на все случаи (0.15 «влезть в чужой разговор»), и
// на живых тредах это дало картину: в треде на 26 реплик кубик приходил 2 раза в
// точку и 4 мимо, а в треде на 298 реплик — 1 в точку и 71 мимо. Он одинаково
// рвётся в беседу на двадцатой реплике и на трёхсотой.
//
// Величина эта ПРЯМО СЧИТАЕТСЯ по архиву, как и разговорчивость (см. шапку
// MineThreadLoad): у каждой чужой реплики в треде, где человек участвовал, видно,
// ответил он на неё или нет — настоящим ребром `comment_reply`, а не догадкой.
//
// Меры две и разделены они не для красоты: «к нему обратились» и «мимо него
// говорят» отличаются на порядок, и одна средняя вероятность между ними не
// значит ничего. Разделение — то же самое, что у задержек (ToReply против
// ToThread), и по той же причине.
//
// Позиция в треде взята мерой затухания вместо времени, потому что решает
// именно она: человек уходит из разговора, когда тот разросся, а не когда
// постарел. Границы корзин — степенные: разница между 5-й и 15-й репликой
// огромна, между 205-й и 215-й нет никакой.

import (
	"context"
	"fmt"
	"time"
)

// rateBuckets — верхние границы корзин по позиции реплики в треде.
var rateBuckets = []int{10, 25, 50, 100, 250, 1 << 30}

// RateBucket — отклик в одной корзине позиций.
type RateBucket struct {
	Upto int `json:"upto"` // верхняя граница позиции

	// Chances/Answers — сколько раз человек МОГ ответить и сколько ответил.
	// Хранятся оба, а не готовая доля: по числу возможностей видно, стоит ли
	// корзина хоть чего-то, а доля сама по себе этого не говорит.
	Chances int `json:"chances"`
	Answers int `json:"answers"`

	// То же для реплик, обращённых К НЕМУ.
	ToHimChances int `json:"to_him_chances"`
	ToHimAnswers int `json:"to_him_answers"`
}

// ReplyRate — кривая отклика целиком.
type ReplyRate struct {
	Threads int          `json:"threads"` // по скольким тредам снято
	Buckets []RateBucket `json:"buckets,omitempty"`

	// Familiar — та же мера, но по ЗНАКОМСТВУ с говорящим: сколько раз человек
	// уже отвечал ему раньше. Ею проверяется, вправе ли граф отношений двигать
	// решение и насколько: без замера множитель за «своего» был бы ровно такой
	// же выдумкой, как «влезть в чужой разговор = 0.15».
	Familiar []RateBucket `json:"familiar,omitempty"`

	// Tempo — та же мера по НАКАЛУ треда: сколько реплик прилетело за последние
	// tempoWindow минут ПЕРЕД этой возможностью.
	//
	// Заведена 28.08.2026 по итогам второго платного прогона, и повод у неё
	// точный. Отбор конфликтных тредов доказал, что дело не в заметках: на
	// десяти самых плотных по перепалкам (28–30 на сотню реплик, включая
	// показанный владельцем образец) раздражение в графе не сдвинулось ни разу,
	// а пауза между нашими репликами вышла 18 минут против двух в оригинале.
	//
	// Причина в том, что позиция и знакомство описывают ГДЕ и С КЕМ, но не
	// КОГДА. Ссора — явление темповое, и заводить её постоянной вероятностью
	// нельзя: двенадцать жителей с неизменной долей в проценты дают восемь
	// одиночных высказываний рядом, сколько заметку ни выбирай.
	//
	// А ВОТ КАК ИМЕННО темп решает — замер сказал ровно наоборот тому, что
	// ожидалось, и это записано здесь, потому что ошибка была бы невидима.
	// Ожидалось «чем гуще реплики, тем охотнее все влезают». Настоящее (трое
	// доноров, 400 тредов каждый):
	//
	//	накал за 10 мин:      0      1      3      8     20    20+
	//	мимо, Полынь-Трава  3,13%  2,30%  2,37%  1,68%  1,40%  0,92%
	//	к нему, Полынь-Трава  17%    38%    37%    46%    46%    50%
	//	мимо, Елена-Милена  1,56%  2,69%  2,13%  2,24%  1,66%  1,42%
	//	к нему, Елена-Милена  38%    50%    65%    66%    66%    66%
	//
	// В шумном треде человек влезает в ЧУЖОЙ разговор ВТРОЕ РЕЖЕ (3,1 % → 0,9 %),
	// и это понятно: реплик много, все не прочтёшь. Зато отвечает ТОМУ, КТО
	// обратился к нему, вдвое надёжнее (17 % → 50 %, 38 % → 66 %).
	//
	// То есть горячий тред — это не «все со всеми», а НЕСКОЛЬКО ПАРАЛЛЕЛЬНЫХ
	// ДИАЛОГОВ: каждый держится своего собеседника и почти не отвлекается на
	// остальных. И перепалка выходит отсюда сама — не из общего гвалта, а из
	// того, что зацепившаяся пара держится всё крепче: я ответил тебе, ты почти
	// наверняка ответишь мне, и дальше по кругу.
	//
	// Мера, как и все прочие здесь, СЧИТАЕТСЯ, а не назначается: у каждой
	// возможности видно и время, и сколько было сказано перед ней. Тем она и
	// поймала перевёрнутую догадку — на глаз угадывается частое и не угадывается
	// редкое, и это уже второй раз на одном и том же кубике.
	Tempo []RateBucket `json:"tempo,omitempty"`
}

// tempoWindow — за какое время считается накал.
//
// Десять минут: столько же держит vacuumBurst, и совпадение не случайно — там
// это окно, за которое разговор НЕ имеет права уложиться целиком, здесь окно,
// за которое видно, что он разогрелся. Обе величины про одно и то же чувство
// живого треда, и разводить их двумя числами значило бы завести два ответа на
// один вопрос.
const tempoWindow = 10 * time.Minute

// tempoBuckets — верхние границы по числу реплик в окне. Ноль отдельной
// корзиной: «в треде тихо» — это не «мало», а другое состояние, и мешать его с
// «одна реплика» нельзя.
var tempoBuckets = []int{0, 1, 3, 8, 20, 1 << 30}

// familiarBuckets — границы по числу прошлых ответов этому человеку. Первая
// корзина «ни разу» и есть встреча с незнакомым, дальше степенной шаг: разница
// между первым и пятым разговором велика, между сороковым и пятидесятым нет
// никакой.
var familiarBuckets = []int{0, 2, 5, 15, 50, 1 << 30}

// rateMinChances — ниже этого корзина не считается замером: доля по трём
// случаям — это не редкость события, а отсутствие данных, и подставлять её в
// кубик значило бы выдать шум за характер.
const rateMinChances = 30

// Rate — вероятность отклика на реплику в позиции pos. Второе значение — был ли
// это ЗАМЕР: пустая корзина возвращает 0, false, и звать её результатом нельзя.
func (r ReplyRate) Rate(pos int, toHim bool) (float64, bool) {
	return rateIn(r.Buckets, pos, toHim)
}

func rateIn(buckets []RateBucket, x int, toHim bool) (float64, bool) {
	for _, b := range buckets {
		if x > b.Upto {
			continue
		}
		chances, answers := b.Chances, b.Answers
		if toHim {
			chances, answers = b.ToHimChances, b.ToHimAnswers
		}
		if chances < rateMinChances {
			return 0, false
		}
		return float64(answers) / float64(chances), true
	}
	return 0, false
}

// MineReplyRate считает кривую отклика по последним maxThreads тредам, где
// человек участвовал.
//
// Берутся ПОСЛЕДНИЕ, а не все: манера отвечать меняется за годы, а хвост в
// тринадцать лет утянул бы кривую к тому, каким человек был когда-то. Тот же
// довод, по которому карточка снимается с последних реплик (-recent).
func (s *Store) MineReplyRate(ctx context.Context, accIDs []int64, maxThreads int) (ReplyRate, error) {
	var out ReplyRate
	if len(accIDs) == 0 {
		return out, fmt.Errorf("отклик: не задана ни одна анкета")
	}
	in := intList(accIDs)
	// Три запроса, а не один с подзапросами: спросить у каждой реплики «ответил
	// ли он на неё» коррелированным EXISTS значит сходить в базу по разу на
	// каждую из десятков тысяч, и на живом архиве замер не укладывался и в
	// десять минут. Здесь же вопрос задаётся МНОЖЕСТВАМИ — они целиком помещаются
	// в память, а позиция в треде считается в Go.
	notes, err := s.rateThreads(ctx, in, maxThreads)
	if err != nil {
		return out, err
	}
	if len(notes) == 0 {
		return out, nil
	}
	ids := intList(notes)
	answered, err := s.idSet(ctx, `
		SELECT r.reply_to FROM comment_reply r
		  JOIN comments a ON a.id = r.comment_id
		 WHERE a.author_id IN (`+in+`) AND a.note_id IN (`+ids+`)`)
	if err != nil {
		return out, err
	}
	toHim, err := s.idSet(ctx, `
		SELECT r.comment_id FROM comment_reply r
		  JOIN comments p ON p.id = r.reply_to
		 WHERE p.author_id IN (`+in+`) AND p.note_id IN (`+ids+`)`)
	if err != nil {
		return out, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT note_id, id, author_id, author_id IN (`+in+`), published_at
		  FROM comments
		 WHERE note_id IN (`+ids+`) AND published_at IS NOT NULL
		 ORDER BY note_id, published_at, id`)
	if err != nil {
		return out, fmt.Errorf("замер отклика: %w", err)
	}
	defer rows.Close()

	out.Buckets = make([]RateBucket, len(rateBuckets))
	for i, up := range rateBuckets {
		out.Buckets[i].Upto = up
	}
	out.Familiar = make([]RateBucket, len(familiarBuckets))
	for i, up := range familiarBuckets {
		out.Familiar[i].Upto = up
	}
	out.Tempo = make([]RateBucket, len(tempoBuckets))
	for i, up := range tempoBuckets {
		out.Tempo[i].Upto = up
	}
	// prior — сколько раз человек УЖЕ ответил этому собеседнику к моменту
	// очередной возможности. Считается на ходу, потому что вопрос временной:
	// «отвечает ли он знакомым чаще» — про то, что было ДО, а итоговое число
	// ответов знает и о том, что случилось после, и превращает замер в
	// тавтологию.
	prior := map[int64]int{}
	var note int64
	pos := 0
	// recent — времена последних реплик треда, скользящим окном: накал это
	// «сколько прилетело за последние tempoWindow ДО этой возможности», и
	// заглядывать вперёд нельзя — там уже видно, чем разговор кончился.
	var recent []time.Time
	for rows.Next() {
		var noteID, id, author int64
		var own bool
		var at string
		if err := rows.Scan(&noteID, &id, &author, &own, &at); err != nil {
			return out, err
		}
		if noteID != note {
			note, pos = noteID, 0
			recent = recent[:0]
			out.Threads++
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
		tempo := len(recent)
		recent = append(recent, now)
		// Своя реплика возможностью не считается: сам себе человек не отвечает,
		// и кубика на своей реплике тоже нет (см. RunSolo).
		if own {
			continue
		}
		countChance(&out.Buckets[bucketOf(pos)], toHim[id], answered[id])
		countChance(&out.Familiar[bucketOfInt(prior[author], familiarBuckets)], toHim[id], answered[id])
		countChance(&out.Tempo[bucketOfInt(tempo, tempoBuckets)], toHim[id], answered[id])
		if answered[id] {
			prior[author]++
		}
	}
	return out, rows.Err()
}

// countChance — одна возможность в корзину. Обращённые к человеку реплики считаются
// ОТДЕЛЬНО от прочих везде, где считаются вообще: они отличаются на порядок, и
// смешанная доля не описывает ни тех, ни других.
func countChance(b *RateBucket, toHim, answered bool) {
	if toHim {
		b.ToHimChances++
		if answered {
			b.ToHimAnswers++
		}
		return
	}
	b.Chances++
	if answered {
		b.Answers++
	}
}

// rateThreads — последние треды, где человек участвовал.
func (s *Store) rateThreads(ctx context.Context, in string, limit int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT note_id FROM comments
		 WHERE author_id IN (`+in+`) AND published_at IS NOT NULL
		 GROUP BY note_id
		 ORDER BY MAX(published_at) DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("замер отклика, треды: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) idSet(ctx context.Context, query string) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("замер отклика: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func bucketOf(pos int) int { return bucketOfInt(pos, rateBuckets) }

func bucketOfInt(x int, edges []int) int {
	for i, up := range edges {
		if x <= up {
			return i
		}
	}
	return len(edges) - 1
}

// FamiliarRate — вероятность отклика на реплику человека, которому уже отвечал
// prior раз. Второе значение — был ли это замер.
func (r ReplyRate) FamiliarRate(prior int, toHim bool) (float64, bool) {
	return rateIn(r.Familiar, prior, toHim)
}
