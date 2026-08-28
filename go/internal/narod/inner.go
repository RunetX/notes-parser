package narod

// Внутренние события: то, что случилось с жителем ВНЕ площадки.
//
// Без них персонаж — существо, у которого нет ничего, кроме реакций на чужие
// заметки. Он не устал, не съездил к матери, у него не сломалась стиральная
// машина; всё, что с ним происходит, происходит в треде. Такого выдаёт не язык
// и не длина реплики, а именно это: человек приходит в разговор ОТКУДА-ТО, и
// оттуда же берётся половина того, что он говорит.
//
// Устроено оно так, чтобы стоить дёшево и не врать:
//
//   - БРОСОК НА ДЕНЬ, а не поток. Бернулли с вероятностью 1/InnerMeanDays,
//     ключом (зерно, житель, день). Два следствия: за день с человеком случается
//     не больше одного рассказываемого события — это и правда так, — и бросок на
//     заданный день всегда один и тот же, как у кубика прихода. Прогон,
//     повторённый завтра, даст ту же жизнь.
//   - ТЕКСТ ПИШЕТ МОДЕЛЬ, но только «что случилось». Ни настроения, ни выводов,
//     ни того, как это скажется на репликах: настроение — дело самой реплики, а
//     событие с приклеенным к нему выводом житель пересказал бы этим выводом.
//   - ПРО ПЛОЩАДКУ НЕЛЬЗЯ. Внутреннее событие — это жизнь СНАРУЖИ; «прочитал
//     заметку и подумал» превращает механизм в самого себя. Запрет стоит в коде
//     проверкой, а не только просьбой в промпте.
//   - НЕ ПОВТОРЯТЬСЯ. Последние события уходят в промпт списком «это уже было»:
//     без него у модели каждый второй день ломается кран.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// InnerMeanDays — раз во сколько дней с жителем случается что-то, о чём он
// способен обмолвиться.
//
// Пять — число НЕ ЗАМЕРЕННОЕ, и притворяться иначе нельзя: в архиве видно, что
// человек написал, и не видно, что с ним было. Взято оно от обратного, по
// единственному наблюдаемому следствию: событие живёт в памяти жителя, пока его
// не вытеснят следующие (memoryInner = 3), то есть при пятидневном шаге у него
// всегда есть что-то из последних двух недель — примерно та глубина, на которую
// люди в тредах и ссылаются («на выходных», «неделю назад»). Чаще — и жизнь
// превращается в сериал, реже — и вспоминать нечего.
const InnerMeanDays = 5.0

// innerSalt разводит поток внутренних событий с потоком решений: без соли
// «что-то случилось» приходилось бы ровно на те же дни, что и «прийти в тред».
const innerSalt = 0xD1B54A32D192ED03

// InnerRunes — потолок рассказа. Событие — одна фраза, а не история: в реплике
// оно всё равно всплывёт вскользь.
const InnerRunes = 200

// InnerHappened — случилось ли что-то с жителем в этот день. Формула, денег не
// стоит; текст спрашивают отдельно и только для дней, где выпало «да».
func InnerHappened(seed uint64, actorID string, day time.Time) bool {
	y, m, d := day.Date()
	key := uint64(y)*10000 + uint64(m)*100 + uint64(d)
	rng := rand.New(rand.NewPCG(seed^innerSalt^hashString(actorID), key))
	return rng.Float64() < 1/InnerMeanDays
}

// InnerTick прокручивает дни [from, to) и записывает случившееся в журнал.
//
// Окно задаёт зовущий, и это не мелочь: в реплее между двумя архивными
// заметками проходят недели виртуального времени, и прокрутка их подряд стоила
// бы дороже самого разговора. Смысл же у события ровно один — быть свежим к
// моменту, когда житель заговорит, поэтому крутить надо последние дни перед
// заметкой, а не всё время с прошлой.
func InnerTick(ctx context.Context, w *World, gen JSONGenerator, card *Card,
	actorID string, seed uint64, from, to time.Time) ([]JournalEntry, error) {
	if w == nil || card == nil {
		return nil, fmt.Errorf("внутренние события: нет мира или карточки")
	}
	var out []JournalEntry
	for day := truncDay(from); day.Before(to); day = day.AddDate(0, 0, 1) {
		if !InnerHappened(seed, actorID, day) {
			continue
		}
		had, err := innerOnDay(ctx, w, actorID, day)
		if err != nil {
			return nil, err
		}
		if had {
			continue // прогон повторили — второй раз день не проживают
		}
		if gen == nil {
			continue // бесплатный прогон: бросок был, слов нет
		}
		text, err := askInner(ctx, gen, card, w, actorID, day)
		if err != nil {
			return nil, err
		}
		if text == "" {
			continue
		}
		e := JournalEntry{ActorID: actorID, At: day, Kind: JournalInner, Text: text}
		id, err := w.Remember(ctx, e)
		if err != nil {
			return nil, err
		}
		e.ID = id
		out = append(out, e)
	}
	return out, nil
}

// innerOnDay — было ли уже записано событие этого дня. Однократность держится
// проверкой, а не ключом: журнал append-only и по устройству принимает всё, а
// сюда приходят с повторным прогоном той же серии.
func innerOnDay(ctx context.Context, w *World, actorID string, day time.Time) (bool, error) {
	var n int
	err := w.db.QueryRowContext(ctx, `
		SELECT count(*) FROM journal
		 WHERE actor_id = ? AND kind = ? AND at >= ? AND at < ?`,
		actorID, JournalInner, fmtTime(day), fmtTime(day.AddDate(0, 0, 1))).Scan(&n)
	return n > 0, err
}

func askInner(ctx context.Context, gen JSONGenerator, card *Card, w *World,
	actorID string, day time.Time) (string, error) {
	past, err := innerMemory(ctx, w, actorID)
	if err != nil {
		return "", err
	}
	raw, err := gen.GenerateJSON(ctx, innerSystem, innerPrompt(card, past, day), innerSchema)
	if err != nil {
		return "", fmt.Errorf("событие дня %s у %s: %w", day.Format("02.01.2006"), actorID, err)
	}
	var reply struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return "", fmt.Errorf("событие дня %s у %s: разбор ответа: %w",
			day.Format("02.01.2006"), actorID, err)
	}
	text := strings.TrimSpace(reply.Text)
	if text == "" {
		return "", nil
	}
	if r := []rune(text); len(r) > InnerRunes {
		text = strings.TrimSpace(string(r[:InnerRunes]))
	}
	if why := innerReject(text); why != "" {
		// Брак не переспрашиваем: день без события — рабочее состояние, а
		// вторая попытка стоит столько же, сколько первая, ради того, чего
		// могло и не быть.
		return "", nil
	}
	return text, nil
}

// innerForbidden — слова, по которым видно, что модель написала про площадку, а
// не про жизнь. Список короткий и закрытый — как AutoHideable: он про ПРАВО
// машины, а не про вкус, и правится diff'ом.
var innerForbidden = []string{
	"заметк", "коммент", "тред", "форум", "сайт", "площадк", "лент", "ветк", "ник ",
}

func innerReject(text string) string {
	low := strings.ToLower(text)
	for _, w := range innerForbidden {
		if strings.Contains(low, w) {
			return "про площадку, а не про жизнь: " + w
		}
	}
	return ""
}

const innerSystem = `Ты придумываешь один обычный день человека — то, что случилось с ним ВНЕ интернета.

Правила:
1. Одно событие, одна-две фразы, от первого лица, прошедшим временем. Не рассказ, а то, чем перекидываются между делом.
2. Обыденное сильнее яркого: сдали анализы, привезли шкаф не того цвета, соседи сверлили с семи утра, дочь получила двойку, наконец доехал до бассейна. Ни катастроф, ни озарений.
3. Про интернет, сайты, заметки и переписку не пиши ВООБЩЕ. Это про жизнь снаружи.
4. Никаких выводов, морали и настроения: только что произошло. Как человек к этому отнёсся — не твоё дело.
5. Не повторяй того, что уже было: если у него на этой неделе ломался кран, пусть в этот раз сломается что-нибудь другое или не сломается ничего.
6. Событие обязано подходить именно этому человеку — его возрасту, городу, работе и семье.`

func innerPrompt(card *Card, past string, day time.Time) string {
	var b strings.Builder
	b.WriteString("=== ЧЕЛОВЕК ===\n")
	p := card.Persona
	fmt.Fprintf(&b, "%s", p.Nick)
	if p.Age > 0 {
		fmt.Fprintf(&b, ", %d лет", p.Age)
	}
	for _, s := range []string{p.City, p.Job, p.Family} {
		if strings.TrimSpace(s) != "" {
			fmt.Fprintf(&b, ", %s", s)
		}
	}
	b.WriteString("\n")
	for _, f := range p.Facts {
		fmt.Fprintf(&b, "— %s\n", f)
	}
	if past != "" {
		b.WriteString("\n=== ЧТО С НИМ УЖЕ БЫЛО (не повторять) ===\n")
		b.WriteString(past)
	}
	fmt.Fprintf(&b, "\n=== ЗАДАНИЕ ===\nЧто с ним случилось %s (%s)?\n",
		day.Format("2 января"), weekdayRu(day.Weekday()))
	return b.String()
}

var innerSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"text"},
	"properties": map[string]any{
		"text": map[string]any{
			"type":        "string",
			"description": "что случилось: одна-две фразы от первого лица",
		},
	},
}

func truncDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func weekdayRu(d time.Weekday) string {
	names := [...]string{"воскресенье", "понедельник", "вторник", "среда",
		"четверг", "пятница", "суббота"}
	return names[int(d)%7]
}

// hashString — FNV-1a, чтобы у жителя был свой поток бросков. Криптографии
// здесь не нужно: ключ разводит потоки, а не защищает их.
func hashString(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}
