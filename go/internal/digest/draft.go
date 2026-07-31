package digest

// Черновик выпуска и материалы для полуручной LLM-редактуры.
//
// Формат черновика (digest-<week>.draft.txt):
//   - строки, начинающиеся с «#», — служебные, в публикацию не идут;
//   - секции (рубрики) разделены строкой «---», абзацы внутри — пустой строкой;
//   - разрешённое HTML-подмножество: <b>, <i>, <a href="…">;
//   - маркер {note:ID|текст} при публикации становится ссылкой на тред
//     заметки в конкретном мессенджере (messenger-agnostic);
//   - плейсхолдеры <!-- LLM:… --> админ заменяет текстом из materials.md;
//     публикация с незаполненными плейсхолдерами возможна только «насухо»:
//     такие секции выбрасываются целиком.

import (
	"errors"
	"fmt"
	"html"
	"io"
	"regexp"
	"strings"
	"time"

	"lovegw/internal/store"
)

// Плейсхолдеры LLM-рубрик.
const (
	llmWeekSummary = "<!-- LLM:week-summary -->"
	llmDispute     = "<!-- LLM:dispute -->"
	llmQuote       = "<!-- LLM:quote -->"
	llmTopics      = "<!-- LLM:topics -->"
	llmMark        = "<!-- LLM:"
)

var (
	noteMarkerRe = regexp.MustCompile(`\{note:([^|}]+)\|([^}]*)\}`)
	htmlTagRe    = regexp.MustCompile(`</?([a-zA-Z0-9]+)[^<>]*>`)
)

// Draft — распарсенный черновик: секции → абзацы (HTML с маркерами).
type Draft struct {
	Sections [][]string
	Dropped  int // секций выброшено из-за незаполненных плейсхолдеров (-force)
}

type draftSection struct {
	hint     []string // подсказки, попадут в файл строками с «#»
	elements []string
}

// WriteDraft пишет черновик выпуска. Тексты сайта экранированы уже здесь.
func WriteDraft(w io.Writer, is *Issue) error {
	loc := is.Window.End.Location()
	var b strings.Builder
	fmt.Fprintf(&b, "# lovegw digest %s · окно %s — %s (%s)\n",
		is.Window.ID, is.Window.Start.In(loc).Format("02.01 15:04"),
		is.Window.End.In(loc).Format("02.01 15:04"), loc)
	b.WriteString("# Черновик выпуска. Строки с # — служебные, в публикацию не идут.\n")
	b.WriteString("# Секции разделены строкой ---, абзацы внутри — пустой строкой.\n")
	b.WriteString("# {note:ID|текст} станет ссылкой на тред заметки в каждом мессенджере.\n")
	if is.Editorial == nil {
		fmt.Fprintf(&b, "# Плейсхолдеры %s… --> заполните текстом из digest-%s.materials.md;\n",
			llmMark, is.Window.ID)
		b.WriteString("# публиковать с незаполненными можно только с -force (секции выпадут).\n")
	} else {
		b.WriteString("# LLM-рубрики заполнены автоматически — при желании поправьте текст и публикуйте.\n")
	}
	for i, sec := range draftSections(is) {
		if i > 0 {
			b.WriteString("---\n")
		}
		for _, h := range sec.hint {
			b.WriteString("# " + h + "\n")
		}
		b.WriteString(strings.Join(sec.elements, "\n\n"))
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// draftSections собирает рубрики черновика; пустые рубрики опускаются.
// LLM-рубрики берутся из is.Editorial, если он есть и поле заполнено, иначе
// в черновик встаёт плейсхолдер полуручного цикла.
func draftSections(is *Issue) []draftSection {
	loc := is.Window.End.Location()
	secs := []draftSection{{
		elements: []string{fmt.Sprintf(
			"<b>📰 Дайджест недели</b> · %s–%s\nЗаметок: %d · комментариев: %d · участников: %d",
			is.Window.Start.In(loc).Format("02.01"), is.Window.End.In(loc).Format("02.01"),
			is.Stats.Notes, is.Stats.Comments, is.Stats.Commenters)},
	}}
	secs = append(secs, editorialSection(is, func(ed *Editorial) string { return ed.WeekSummary },
		llmWeekSummary, "О чём была неделя: 2–3 предложения, промпт 1 в materials.", nil))
	if is.TopNote != nil {
		secs = append(secs, draftSection{
			elements: []string{"<b>📌 Заметка недели</b>", noteStatLine(*is.TopNote)},
		})
	}
	if len(is.Disputes) > 0 {
		secs = append(secs, editorialSection(is, func(ed *Editorial) string { return ed.Dispute },
			llmDispute, "Спор недели: 2–3 предложения о накале, промпт 2 в materials.",
			[]string{"<b>🔥 Спор недели</b>"}, noteStatLine(disputePick(is))))
	}
	if len(is.Quotes) > 0 {
		secs = append(secs, editorialSection(is, func(ed *Editorial) string { return ed.Quote },
			llmQuote, "Цитата недели: подводка + цитата, промпт 3 в materials.",
			[]string{"<b>💬 Цитата недели</b>"}))
	}
	if len(is.Newcomers) > 0 {
		secs = append(secs, draftSection{
			elements: []string{"<b>👋 Новые лица</b>", newcomerLines(is.Newcomers)},
		})
	}
	if len(is.Returnees) > 0 {
		secs = append(secs, draftSection{
			elements: []string{"<b>🔄 Возвращение недели</b>", returneeLines(is)},
		})
	}
	if len(is.ThisWeekNotes) > 0 {
		secs = append(secs, editorialSection(is, func(ed *Editorial) string { return ed.Topics },
			llmTopics, "Темы недели: 2–4 строки, промпт 4 в materials.",
			[]string{"<b>🧭 Темы недели</b>"}))
	}
	if len(is.Records) > 0 {
		lines := make([]string, 0, len(is.Records))
		for _, r := range is.Records {
			lines = append(lines, recordLine(r, is.TopNote))
		}
		secs = append(secs, draftSection{
			elements: []string{"<b>🏆 Рекорды</b>", strings.Join(lines, "\n")},
		})
	}
	if len(is.StillAlive) > 0 {
		secs = append(secs, draftSection{
			elements: []string{"<b>♨️ Ещё обсуждают</b>", aliveLines(is.StillAlive)},
		})
	}
	return secs
}

func newcomerLines(persons []Person) string {
	lines := make([]string, 0, len(persons))
	for _, p := range persons {
		lines = append(lines, personLink(p)+" — "+activityText(p)+".")
	}
	return strings.Join(lines, "\n")
}

func returneeLines(is *Issue) string {
	lines := make([]string, 0, len(is.Returnees))
	for _, p := range is.Returnees {
		weeks := int(is.Window.Start.Sub(p.PrevSeenAt).Hours() / (24 * 7))
		lines = append(lines, fmt.Sprintf("%s — снова здесь после %d %s тишины.",
			personLink(p), weeks, pluralRu(weeks, "недели", "недель", "недель")))
	}
	return strings.Join(lines, "\n")
}

func aliveLines(stats []NoteStat) string {
	lines := make([]string, 0, len(stats))
	for _, s := range stats {
		line := noteMarker(s.Note)
		if s.Comments > 0 {
			line += fmt.Sprintf(" — %s за неделю", nComments(s.Comments))
		} else {
			line += " — обсуждение продолжается"
		}
		lines = append(lines, line+".")
	}
	return strings.Join(lines, "\n")
}

// editorialSection — рубрика с LLM-текстом: заполненное поле Editorial
// встаёт в черновик как готовый текст, пустое — плейсхолдером с подсказкой
// полуручного цикла. header и trailing — элементы вокруг текста (заголовок
// рубрики, строка-маркер кандидата).
func editorialSection(is *Issue, get func(*Editorial) string, placeholder, hint string, header []string, trailing ...string) draftSection {
	text := placeholder
	hints := []string{hint}
	if is.Editorial != nil {
		if t := get(is.Editorial); t != "" {
			text, hints = t, nil
		}
	}
	elements := append(append([]string{}, header...), text)
	return draftSection{hint: hints, elements: append(elements, trailing...)}
}

// noteStatLine — строка заметки с метриками обсуждения.
func noteStatLine(s NoteStat) string {
	return noteMarker(s.Note) + " — " + statText(s)
}

// statText — «87 комментариев от 19 участников за 4 дня, пик — 21 за час.»
func statText(s NoteStat) string {
	var b strings.Builder
	b.WriteString(nComments(s.Comments))
	if s.Commenters > 0 {
		fmt.Fprintf(&b, " от %d %s", s.Commenters,
			pluralRu(s.Commenters, "участника", "участников", "участников"))
	}
	if d := durText(s.LastAt.Sub(s.FirstAt)); d != "" {
		b.WriteString(" " + d)
	}
	if s.PeakHourN >= minRecordPeakHour {
		fmt.Fprintf(&b, ", пик — %d за час", s.PeakHourN)
	}
	b.WriteString(".")
	return b.String()
}

func recordLine(r Record, top *NoteStat) string {
	// Тред заметки недели уже виден в своей рубрике — второй раз не линкуем.
	if r.NoteID != "" && (top == nil || r.NoteID != top.Note.ID) {
		return r.Text + " ({note:" + r.NoteID + "|тред})"
	}
	return r.Text
}

func personLink(p Person) string {
	return fmt.Sprintf(`<a href="%s">%s</a>`, p.ProfileURL, html.EscapeString(p.Name))
}

func activityText(p Person) string {
	notes := ""
	if p.Notes > 0 {
		notes = fmt.Sprintf("%d %s", p.Notes, pluralRu(p.Notes, "заметка", "заметки", "заметок"))
	}
	switch {
	case notes != "" && p.Comments > 0:
		return notes + " и " + nComments(p.Comments)
	case notes != "":
		return notes
	default:
		return nComments(p.Comments)
	}
}

// durText — «за 4 дня» / «за 6 часов»; короче двух часов — пусто.
func durText(d time.Duration) string {
	if days := int(d.Hours() / 24); days >= 2 {
		return fmt.Sprintf("за %d %s", days, pluralRu(days, "день", "дня", "дней"))
	}
	if hours := int(d.Hours()); hours >= 2 {
		return fmt.Sprintf("за %d %s", hours, pluralRu(hours, "час", "часа", "часов"))
	}
	return ""
}

// noteMarker — messenger-agnostic ссылка на тред заметки.
func noteMarker(n store.Note) string {
	return "{note:" + n.ID + "|" + noteExcerpt(n, 60) + "}"
}

// noteExcerpt — «первые руны заметки…» для текста ссылки: схлопнутые пробелы,
// без символов разметки маркера, HTML экранирован.
func noteExcerpt(n store.Note, maxRunes int) string {
	text := strings.Map(func(c rune) rune {
		if c == '{' || c == '}' || c == '|' {
			return -1
		}
		return c
	}, n.Text)
	return "«" + html.EscapeString(collapse(text, maxRunes)) + "»"
}

// ParseDraft читает черновик: снимает служебные строки, режет на секции и
// абзацы, валидирует HTML-подмножество и маркеры. strict=true — ошибка при
// незаполненных плейсхолдерах LLM; strict=false — такие секции выбрасываются
// (публикация «насухо», -force).
func ParseDraft(r io.Reader, strict bool) (Draft, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Draft{}, err
	}
	sections := splitSections(string(data))

	var d Draft
	var errs []string
	for si, sec := range sections {
		if ph := sectionPlaceholder(sec); ph != "" {
			if strict {
				errs = append(errs, fmt.Sprintf("секция %d: не заполнен плейсхолдер %s", si+1, ph))
			}
			d.Dropped++
			continue
		}
		for _, el := range sec {
			if err := validateElement(el); err != nil {
				errs = append(errs, fmt.Sprintf("секция %d: %v", si+1, err))
			}
		}
		d.Sections = append(d.Sections, sec)
	}
	if len(errs) > 0 {
		return Draft{}, errors.New(strings.Join(errs, "; "))
	}
	if len(d.Sections) == 0 {
		return Draft{}, errors.New("в черновике не осталось ни одной секции")
	}
	return d, nil
}

// splitSections режет текст черновика на секции и абзацы, отбрасывая
// служебные #-строки.
func splitSections(text string) [][]string {
	var sections [][]string
	var section []string
	var para []string
	flushPara := func() {
		if len(para) > 0 {
			section = append(section, strings.Join(para, "\n"))
			para = nil
		}
	}
	flushSection := func() {
		flushPara()
		if len(section) > 0 {
			sections = append(sections, section)
			section = nil
		}
	}
	for _, ln := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(ln, "#"):
			continue
		case strings.TrimSpace(ln) == "---":
			flushSection()
		case strings.TrimSpace(ln) == "":
			flushPara()
		default:
			para = append(para, strings.TrimRight(ln, " \t"))
		}
	}
	flushSection()
	return sections
}

func sectionPlaceholder(sec []string) string {
	for _, el := range sec {
		if i := strings.Index(el, llmMark); i >= 0 {
			if end := strings.Index(el[i:], "-->"); end >= 0 {
				return el[i : i+end+len("-->")]
			}
			return llmMark + "…"
		}
	}
	return ""
}

// validateElement проверяет абзац: целые маркеры, только разрешённые парные
// теги, никаких неэкранированных < и > в тексте.
func validateElement(el string) error {
	if strings.Count(el, "{note:") != len(noteMarkerRe.FindAllString(el, -1)) {
		return errors.New("повреждён маркер {note:ID|текст}")
	}
	var stack []string
	for _, m := range htmlTagRe.FindAllString(el, -1) {
		name := strings.ToLower(htmlTagRe.FindStringSubmatch(m)[1])
		switch name {
		case "b", "i", "a":
		default:
			return fmt.Errorf("тег <%s> не поддерживается (можно <b>, <i>, <a>)", name)
		}
		if strings.HasPrefix(m, "</") {
			if len(stack) == 0 || stack[len(stack)-1] != name {
				return fmt.Errorf("непарный тег </%s>", name)
			}
			stack = stack[:len(stack)-1]
		} else {
			stack = append(stack, name)
		}
	}
	if len(stack) > 0 {
		return fmt.Errorf("незакрытый тег <%s>", stack[len(stack)-1])
	}
	if rest := htmlTagRe.ReplaceAllString(el, ""); strings.ContainsAny(rest, "<>") {
		return errors.New("символы < и > вне тегов нужно экранировать (&lt; и &gt;)")
	}
	return nil
}
