package narod

// Летописец: что тред изменил в отношениях.
//
// Разговор кончился — и кто-то кого-то поддел, кто-то за кого-то вступился, а
// двое сцепились. Ни одно из этих слов не выводится из чисел: в тред приходит
// текст, а не разметка, и прочитать его может только тот, кто понимает слова.
// Отсюда единственное место эпика, где модель судит не о ТЕКСТЕ, а о ЛЮДЯХ.
//
// ГРАНИЦА ЕЁ ВЛАСТИ УЗКАЯ, и держится она кодом, а не просьбой в промпте:
//
//   - шкалы двигаются на ДЕЛЬТУ с потолком (MaxDelta), а не задаются целиком:
//     один разговор не вправе переписать отношения набело — иначе пара,
//     поссорившаяся в четверг, к пятнице теряет всё, что между ними было;
//   - ЗНАКОМСТВО модель не трогает вовсе. Это счётчик встреч, а счётчики
//     считают, а не оценивают: сколько раз двое ответили друг другу, видно из
//     треда точно, и спрашивать об этом значило бы менять точное на мнение;
//   - вид эпизода — из ЗАКРЫТОГО списка ядра (EpisodeKinds), как у
//     platform.AutoHideable: модель, которой позволено придумывать себе виды
//     отношений, через десяток тредов заведёт «взаимное уважение с оттенком
//     иронии», и сравнивать миры между собой станет нечем;
//   - имена разрешаются по УЧАСТНИКАМ треда, неизвестное отбрасывается с
//     причиной: эпизод — свидетельство, а свидетельство о том, кого в комнате
//     не было, не свидетельство;
//   - ссылки на реплики проверяются по самому треду: выдуманный номер — это
//     выдуманный повод, и всплывёт он через месяц как «а помнишь, как ты…» про
//     то, чего не случалось.
//
// Что остаётся модели: ЗНАК и ПОВОД. Ровно то, чего нет ни в одном замере.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// JSONGenerator — онлайн-LLM, отвечающая строго по JSON-схеме (тот же контракт,
// что у digest, pulpit и platmod; реализации — llm.Client и rullm.Client).
type JSONGenerator interface {
	GenerateJSON(ctx context.Context, system, prompt string, schema map[string]any) ([]byte, error)
}

// MaxDelta — насколько один тред вправе подвинуть шкалу.
//
// Двойка при шкале в десять: чтобы дойти от безразличия до вражды, надо
// поссориться пять раз. Это и есть смысл эмуляции — отношения складываются из
// разговоров, а не объявляются одним.
const MaxDelta = 2.0

// ChronicleReply — реплика треда так, как её видит летописец.
type ChronicleReply struct {
	ID      int64
	ActorID string // житель мира; пусто — посторонний (о таких летопись молчит)
	Nick    string
	Text    string
	ReplyTo int64
	Target  string // кому отвечал, актором
}

// ChronicleThread — разговор целиком.
type ChronicleThread struct {
	NoteID   int64
	NoteText string
	NoteBy   string // ник автора заметки
	Replies  []ChronicleReply
	At       time.Time // когда тред признан закрытым
}

// ChronicleResult — что летопись записала.
type ChronicleResult struct {
	Edges    []Edge    `json:"edges"`
	Episodes []Episode `json:"episodes"`
	// Dropped — что отвергнуто и почему. Не служебный шум: отвергнутое
	// показывает, куда модель тянет, — а тянет она всегда в одну сторону, и
	// увидеть это можно только списком.
	Dropped []string `json:"dropped,omitempty"`
	// Familiar — сколько пар подвинулось знакомством. Считает код, поэтому это
	// число есть и у бесплатного прогона.
	Familiar int `json:"familiar"`
	// Asked — ходили ли к модели. Умолчание здесь означало бы, что мир двигался,
	// а граф стоял: без модели знакомство копится, а симпатия — нет.
	Asked bool `json:"asked"`
}

// Chronicle читает закрывшийся тред и записывает, что он изменил.
//
// Генератор может быть nil — и это рабочий режим, а не заглушка: знакомство
// копится и без модели, потому что оно считается, и калибровка формы разговора
// остаётся даровой. Тогда двигается только оно, и отчёт говорит об этом прямо.
func Chronicle(ctx context.Context, w *World, gen JSONGenerator, th ChronicleThread) (*ChronicleResult, error) {
	if w == nil {
		return nil, fmt.Errorf("летопись: мира нет")
	}
	res := &ChronicleResult{}
	now := th.At
	if now.IsZero() {
		now = time.Now()
	}

	// Знакомство — первым и всегда: оно считается по самому треду, а значит не
	// зависит ни от модели, ни от её доступности.
	for _, m := range meetings(th) {
		e, err := w.Nudge(ctx, EdgeDelta{Src: m.src, Dst: m.dst, Familiarity: float64(m.n)}, now)
		if err != nil {
			return nil, err
		}
		res.Edges = append(res.Edges, e)
		res.Familiar++
	}
	if gen == nil {
		return res, nil
	}
	res.Asked = true

	byNick, spoke := castOf(th)
	if len(spoke) < 2 {
		// Разговора не было: один говорил, остальные молчали. Спрашивать модель
		// не о чем, и это не отказ, а отсутствие предмета.
		return res, nil
	}
	ids := idsOf(th)

	raw, err := gen.GenerateJSON(ctx, chronicleSystem, chroniclePrompt(th), chronicleSchema)
	if err != nil {
		return nil, fmt.Errorf("летопись заметки %d: %w", th.NoteID, err)
	}
	var reply chronicleReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("летопись заметки %d: разбор ответа: %w", th.NoteID, err)
	}

	for _, e := range reply.Edges {
		src, dst, why := resolvePair(byNick, spoke, e.Src, e.Dst)
		if why != "" {
			res.Dropped = append(res.Dropped, "ребро: "+why)
			continue
		}
		d := EdgeDelta{Src: src, Dst: dst,
			Sympathy: clampDelta(e.Sympathy), Irritation: clampDelta(e.Irritation)}
		if d.Sympathy == 0 && d.Irritation == 0 {
			continue // «ничего не изменилось» — законный и частый ответ
		}
		got, err := w.Nudge(ctx, d, now)
		if err != nil {
			return nil, err
		}
		res.Edges = append(res.Edges, got)
	}

	for _, ep := range reply.Episodes {
		src, dst, why := resolvePair(byNick, spoke, ep.Src, ep.Dst)
		if why != "" {
			res.Dropped = append(res.Dropped, "эпизод: "+why)
			continue
		}
		if !knownEpisodeKind(ep.Kind) || ep.Kind == EpisodeDigest {
			res.Dropped = append(res.Dropped,
				fmt.Sprintf("эпизод: вид %q не из списка %v", ep.Kind, EpisodeKinds))
			continue
		}
		refs, missing := keepKnown(ep.CommentIDs, ids)
		if len(missing) > 0 {
			res.Dropped = append(res.Dropped,
				fmt.Sprintf("эпизод %s→%s: реплик %v в треде нет", src, dst, missing))
			continue
		}
		if strings.TrimSpace(ep.Summary) == "" {
			res.Dropped = append(res.Dropped, fmt.Sprintf("эпизод %s→%s: без повода", src, dst))
			continue
		}
		e := Episode{Src: src, Dst: dst, At: now, Kind: ep.Kind,
			Summary: strings.TrimSpace(ep.Summary), CommentIDs: refs, NoteID: th.NoteID}
		id, err := w.AddEpisode(ctx, e)
		if err != nil {
			return nil, err
		}
		e.ID = id
		res.Episodes = append(res.Episodes, e)
		if err := w.CompactEpisodes(ctx, src, dst, now); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// meeting — «сколько раз в этом треде src ответил dst».
type meeting struct {
	src, dst string
	n        int
}

// meetings считает встречи по самому треду.
//
// Встреча — это ОТВЕТ, а не соседство в треде: люди, промолчавшие друг другу в
// тысячной ветке, не познакомились. Ребро направленное по той же причине, по
// которой направлена симпатия: ответил один, а не оба.
func meetings(th ChronicleThread) []meeting {
	n := map[string]int{}
	for _, c := range th.Replies {
		if c.ActorID == "" || c.Target == "" || c.ActorID == c.Target {
			continue
		}
		n[c.ActorID+"\x00"+c.Target]++
	}
	out := make([]meeting, 0, len(n))
	for k, v := range n {
		src, dst, _ := strings.Cut(k, "\x00")
		out = append(out, meeting{src: src, dst: dst, n: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].src != out[j].src {
			return out[i].src < out[j].src
		}
		return out[i].dst < out[j].dst
	})
	return out
}

// castOf — кто говорил в треде: имя (в нижнем регистре) → актор, и множество
// заговоривших. Модель называет людей никами, потому что никами они и названы в
// разговоре; идентификаторы жителей она носила бы вслепую и путала.
func castOf(th ChronicleThread) (map[string]string, map[string]bool) {
	byNick := map[string]string{}
	spoke := map[string]bool{}
	for _, c := range th.Replies {
		if c.ActorID == "" {
			continue
		}
		spoke[c.ActorID] = true
		byNick[strings.ToLower(strings.TrimSpace(c.Nick))] = c.ActorID
		byNick[strings.ToLower(c.ActorID)] = c.ActorID
	}
	return byNick, spoke
}

func idsOf(th ChronicleThread) map[int64]bool {
	out := make(map[int64]bool, len(th.Replies))
	for _, c := range th.Replies {
		out[c.ID] = true
	}
	return out
}

// resolvePair разрешает пару имён в акторов. Причина отказа идёт третьим
// значением: отказ обязан называть себя, иначе половина ответа модели пропадает
// молча.
func resolvePair(byNick map[string]string, spoke map[string]bool, a, b string) (string, string, string) {
	src, ok := byNick[strings.ToLower(strings.TrimSpace(a))]
	if !ok {
		return "", "", fmt.Sprintf("%q в треде не говорил", a)
	}
	dst, ok := byNick[strings.ToLower(strings.TrimSpace(b))]
	if !ok {
		return "", "", fmt.Sprintf("%q в треде не говорил", b)
	}
	if src == dst {
		return "", "", fmt.Sprintf("%q сам с собой", a)
	}
	if !spoke[src] || !spoke[dst] {
		return "", "", fmt.Sprintf("%s→%s: кто-то не говорил", src, dst)
	}
	return src, dst, ""
}

// keepKnown отделяет настоящие номера реплик от выдуманных.
func keepKnown(ids []int64, known map[int64]bool) ([]int64, []int64) {
	var ok, missing []int64
	for _, id := range ids {
		if known[id] {
			ok = append(ok, id)
			continue
		}
		missing = append(missing, id)
	}
	return ok, missing
}

func clampDelta(x float64) float64 {
	switch {
	case x > MaxDelta:
		return MaxDelta
	case x < -MaxDelta:
		return -MaxDelta
	}
	return x
}

// --- разговор с моделью ---

type chronicleReply struct {
	Edges []struct {
		Src        string  `json:"src"`
		Dst        string  `json:"dst"`
		Sympathy   float64 `json:"sympathy"`
		Irritation float64 `json:"irritation"`
		Why        string  `json:"why"`
	} `json:"edges"`
	Episodes []struct {
		Src        string  `json:"src"`
		Dst        string  `json:"dst"`
		Kind       string  `json:"kind"`
		Summary    string  `json:"summary"`
		CommentIDs []int64 `json:"comment_ids"`
	} `json:"episodes"`
}

const chronicleSystem = `Ты летописец сообщества, а не его участник. Реплик ты не пишешь.
Тебе дают разговор, который уже кончился, и один вопрос: что он изменил в отношениях между людьми.

Правила:
1. Говори только о тех, кто в этом разговоре ГОВОРИЛ. О молчавших сказать нечего.
2. Отношение направленное: «Аня стала лучше относиться к Боре» и «Боря к Ане» — разные строки.
3. Двигай шкалы НЕМНОГО. Один разговор редко меняет много: обычная величина — от 0 до 1, двойка это ссора или спасение. Пустой ответ («ничего не изменилось») — нормальный и частый.
4. Раздражение растёт от грубости и передёргивания, симпатия — от поддержки, общей шутки, согласия. Это разные шкалы, а не концы одной: людей, которые нравятся и бесят одновременно, полно.
5. Эпизод пиши только про то, что ПРОИЗОШЛО, и всегда со ссылками на номера реплик. Пересказ короткий, до 200 знаков, о СОБЫТИИ, а не оценка человека: «назвал её ответ отпиской» — да, «повёл себя некрасиво» — нет.
6. Не выдумывай ни имён, ни номеров реплик: и то и другое проверяется по разговору, выдуманное отбрасывается.
7. Разговор может не изменить ничего. Тогда оба списка пустые — это верный ответ, а не пропуск работы.`

func chroniclePrompt(th ChronicleThread) string {
	var b strings.Builder
	b.WriteString("=== РАЗГОВОР ===\n")
	fmt.Fprintf(&b, "Заметка «%s»:\n%s\n\n", th.NoteBy, strings.TrimSpace(th.NoteText))
	for _, c := range th.Replies {
		who := c.Nick
		if who == "" {
			who = "посторонний"
		}
		to := ""
		if c.Target != "" && c.ReplyTo != 0 {
			to = fmt.Sprintf(" → #%d", c.ReplyTo)
		}
		text := strings.TrimSpace(c.Text)
		if text == "" {
			// Реплика без слов — след бесплатного прогона. Пропускать её нельзя:
			// номера должны идти как в разговоре, иначе ссылки поедут.
			text = "(без текста)"
		}
		fmt.Fprintf(&b, "#%d [%s]%s: %s\n", c.ID, who, to, text)
	}
	b.WriteString("\n=== ЗАДАНИЕ ===\n")
	fmt.Fprintf(&b, "Виды эпизодов — только эти: %s.\n", strings.Join(EpisodeKinds, ", "))
	b.WriteString("Имена — ровно те, что стоят в квадратных скобках. Номера реплик — те, что после решётки.\n")
	return b.String()
}

var chronicleSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"edges", "episodes"},
	"properties": map[string]any{
		"edges": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"src", "dst", "sympathy", "irritation", "why"},
				"properties": map[string]any{
					"src":        map[string]any{"type": "string", "description": "чьё отношение изменилось"},
					"dst":        map[string]any{"type": "string", "description": "к кому"},
					"sympathy":   map[string]any{"type": "number", "description": "насколько подвинулась симпатия, от -2 до 2"},
					"irritation": map[string]any{"type": "number", "description": "насколько подвинулось раздражение, от -2 до 2"},
					"why":        map[string]any{"type": "string", "description": "коротко, из-за чего"},
				},
			},
		},
		"episodes": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"src", "dst", "kind", "summary", "comment_ids"},
				"properties": map[string]any{
					"src":     map[string]any{"type": "string"},
					"dst":     map[string]any{"type": "string"},
					"kind":    map[string]any{"type": "string", "enum": EpisodeKinds},
					"summary": map[string]any{"type": "string", "description": "что произошло, до 200 знаков"},
					"comment_ids": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "integer"},
					},
				},
			},
		},
	},
}
