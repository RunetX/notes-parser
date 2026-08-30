package main

// narod speech — ЧЕМ наполнена реплика: свой случай против мнения.
//
// Стенд под жалобу владельца 30.08.2026 («реплики какие-то абстрактные,
// большинство людей так не общается»). Механику письма калибровка выправила ещё
// в августе — длина, ритм, поверхность сошлись с донорскими, — а содержание
// никто не мерил, и разговор о нём шёл на глаз.
//
// Команда СРАВНИВАЕТ и только сравнивает: одиночная доля невода не значит
// ничего (см. шапку internal/speech), поэтому колонок всегда несколько — корпус,
// доноры, наши. Ни в сеть, ни в Postgres она не ходит: архив читается, наши
// реплики подаются файлом, — значит гонять её можно сколько угодно и рядом с
// работающим демоном.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"lovegw/internal/archive"
	"lovegw/internal/speech"
)

type speechOpts struct {
	dbPath string
	corpus int    // сколько реплик корпуса взять в норму (0 — не брать)
	actors string // доноры через запятую: u<id>,p<id>
	recent int    // сколько последних реплик донора мерить
	files  string // файлы с нашими репликами через запятую
	notes  string // живые треды архива через запятую
}

// speechColumn — один портрет с именем.
type speechColumn struct {
	name  string
	marks speech.Marks
}

func narodSpeech(ctx context.Context, o speechOpts) error {
	var cols []speechColumn

	// Наши реплики идут ПЕРВЫМИ, потому что вопрос задают про них: корпус и
	// доноры здесь — мерка, а не предмет.
	for _, path := range splitList(o.files) {
		texts, err := readSpeechFile(path)
		if err != nil {
			return err
		}
		cols = append(cols, speechColumn{name: shortName(path), marks: speech.Measure(texts)})
	}

	if o.corpus > 0 || o.actors != "" || o.notes != "" {
		st, err := archive.Open(ctx, o.dbPath)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, token := range splitList(o.actors) {
			m, err := st.SpeechOf(ctx, token, o.recent)
			if err != nil {
				return err
			}
			cols = append(cols, speechColumn{name: token, marks: m})
		}
		for _, n := range splitList(o.notes) {
			id, err := strconv.ParseInt(n, 10, 64)
			if err != nil {
				return fmt.Errorf("narod speech: -note %q: %w", n, err)
			}
			m, err := st.SpeechNote(ctx, id)
			if err != nil {
				return err
			}
			cols = append(cols, speechColumn{name: "тред " + n, marks: m})
		}
		if o.corpus > 0 {
			m, err := st.SpeechCorpus(ctx, o.corpus)
			if err != nil {
				return err
			}
			cols = append(cols, speechColumn{name: "корпус", marks: m})
		}
	}

	if len(cols) < 2 {
		return fmt.Errorf("narod speech: нужно хотя бы два портрета — " +
			"доля невода в одиночку не значит ничего (-file, -actor, -corpus)")
	}
	fmt.Print(speechTable(cols))
	return nil
}

// speechTable печатает портреты столбцами.
func speechTable(cols []speechColumn) string {
	var b strings.Builder
	b.WriteString(pad("", 30))
	for _, c := range cols {
		b.WriteString(padLeft(c.name, 14))
	}
	b.WriteString("\n")
	row := func(title string, get func(speech.Marks) float64) {
		b.WriteString(pad(title, 30))
		for _, c := range cols {
			b.WriteString(padLeft(fmt.Sprintf("%.1f %%", 100*get(c.marks)), 14))
		}
		b.WriteString("\n")
	}
	b.WriteString(pad("реплик в замере", 30))
	for _, c := range cols {
		b.WriteString(padLeft(fmt.Sprintf("%d", c.marks.Texts), 14))
	}
	b.WriteString("\n")
	row("свой случай (я + прошедшее)", func(m speech.Marks) float64 { return m.OwnStory })
	row("привязка ко времени", func(m speech.Marks) float64 { return m.TimeMark })
	row("числа", func(m speech.Marks) float64 { return m.Digits })
	row("обобщение (все, всегда)", func(m speech.Marks) float64 { return m.General })
	row("поучение (надо, должен)", func(m speech.Marks) float64 { return m.Advice })
	return b.String()
}

// readSpeechFile читает реплики.
//
// JSON-массив строк, потому что реплика содержит переводы строк, и «одна на
// строку» разорвала бы половину из них. Выгрузка с площадки делается ровно в
// этом виде: `select json_agg(body) from comments …`.
func readSpeechFile(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var texts []string
	if err := json.Unmarshal(raw, &texts); err != nil {
		return nil, fmt.Errorf("%s: ожидался JSON-массив строк: %w", path, err)
	}
	if len(texts) == 0 {
		return nil, fmt.Errorf("%s: реплик нет", path)
	}
	return texts, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func shortName(path string) string {
	i := strings.LastIndexAny(path, `/\`)
	name := path[i+1:]
	return strings.TrimSuffix(name, ".json")
}

func pad(s string, n int) string {
	r := []rune(s)
	if len(r) >= n {
		return string(r[:n])
	}
	return s + strings.Repeat(" ", n-len(r))
}

func padLeft(s string, n int) string {
	r := []rune(s)
	if len(r) >= n {
		return string(r[:n])
	}
	return strings.Repeat(" ", n-len(r)) + s
}
