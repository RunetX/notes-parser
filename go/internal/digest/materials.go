package digest

// Материалы полуручного LLM-цикла: готовые промпты рубрик и сырьё к ним.
// Файл читают админ и LLM; это markdown, HTML здесь не экранируется.

import (
	"fmt"
	"io"
	"strings"
)

const materialsHowTo = `## Как пользоваться

1. Скопируйте промпт рубрики вместе с материалами под ним в LLM.
2. Ответ вставьте в черновик digest-<неделя>.draft.txt вместо
   соответствующего плейсхолдера <!-- LLM:… -->.
3. Проверка: lovegw digest preview; публикация: lovegw digest publish.

Общий тон всех рубрик: лёгкая ирония, тепло, без сарказма в адрес конкретных
людей. Запрещено: приписывать авторам намерения и диагнозы, делать выводы о
личностях, использовать что-либо сверх приведённых ниже текстов.
`

const promptWeekSummary = `Ниже — заметки этой и прошлой недели из раздела знакомств. Напиши 2–3
предложения «о чём была неделя» для дайджеста: что занимало людей и что
сдвинулось против прошлой недели. Тон — лёгкая ирония без сарказма; без
чисел, без имён и без выводов о конкретных людях.`

const promptDispute = `Ниже — кандидаты в «спор недели» с метриками и последними репликами. Выбери
самый накалённый тред и опиши его в 2–3 предложениях: из-за чего сыр-бор и
какая температура. Никого не высмеивать адресно, реплики дословно не
цитировать. В конце ответа укажи id выбранной заметки: в черновике под этой
рубрикой стоит маркер-ссылка {note:ID|…} — поправьте id, если выбран другой
тред.`

const promptQuote = `Ниже — шорт-лист заметных комментариев недели. Выбери один самый яркий и
напиши к нему подводку в одно предложение. Формат вставки в черновик:
подводка, затем цитата в кавычках и имя автора курсивом:
«…» — <i>Имя</i>.`

const promptTopics = `Сгруппируй заметки этой недели в 2–4 темы, по короткой строке на тему;
отметь, какие темы появились и какие исчезли по сравнению с прошлой неделей
(списки заметок — в материалах промпта 1). Лёгкая ирония, без чисел.`

// WriteMaterials пишет материалы выпуска для админа и LLM.
func WriteMaterials(w io.Writer, is *Issue) error {
	loc := is.Window.End.Location()
	var b strings.Builder
	fmt.Fprintf(&b, "# Материалы дайджеста %s\n\n", is.Window.ID)
	fmt.Fprintf(&b, "Окно: %s — %s (%s). Заметок: %d, комментариев: %d, участников: %d.\n\n",
		is.Window.Start.In(loc).Format("02.01.2006 15:04"),
		is.Window.End.In(loc).Format("02.01.2006 15:04"), loc,
		is.Stats.Notes, is.Stats.Comments, is.Stats.Commenters)
	b.WriteString(materialsHowTo + "\n")

	fmt.Fprintf(&b, "## Промпт 1 — «О чём была неделя» → %s\n\n%s\n\n", llmWeekSummary, promptWeekSummary)
	b.WriteString("Заметки этой недели:\n")
	writeNoteList(&b, is.ThisWeekNotes)
	b.WriteString("\nЗаметки прошлой недели:\n")
	writeNoteList(&b, is.PrevWeekNotes)

	fmt.Fprintf(&b, "\n## Промпт 2 — «Спор недели» → %s\n\n%s\n", llmDispute, promptDispute)
	if len(is.Disputes) == 0 {
		b.WriteString("\n(кандидатов на этой неделе нет — рубрику можно опустить)\n")
	}
	for _, s := range is.Disputes {
		fmt.Fprintf(&b, "\n### Кандидат: заметка %s — %s\n", s.Note.ID, statText(s))
		fmt.Fprintf(&b, "Текст заметки (%s): «%s»\n", s.Note.AuthorName, collapse(s.Note.Text, 300))
		b.WriteString("Последние реплики:\n")
		cs := is.CommentsByNote[s.Note.ID]
		if len(cs) > disputeTailComments {
			cs = cs[len(cs)-disputeTailComments:]
		}
		for _, c := range cs {
			fmt.Fprintf(&b, "- %s: «%s»\n", c.AuthorName, collapse(c.Text, 300))
		}
	}

	fmt.Fprintf(&b, "\n## Промпт 3 — «Цитата недели» → %s\n\n%s\n\n", llmQuote, promptQuote)
	if len(is.Quotes) == 0 {
		b.WriteString("(кандидатов на этой неделе нет — рубрику можно опустить)\n")
	}
	for i, q := range is.Quotes {
		replies := "без быстрых ответов"
		if q.RepliesAfter > 0 {
			replies = fmt.Sprintf("%d %s в следующие два часа", q.RepliesAfter,
				pluralRu(q.RepliesAfter, "ответ", "ответа", "ответов"))
		}
		fmt.Fprintf(&b, "%d. %s (%s), заметка %s: «%s»\n",
			i+1, q.Comment.AuthorName, replies, q.Comment.NoteID, collapse(q.Comment.Text, 600))
	}

	fmt.Fprintf(&b, "\n## Промпт 4 — «Темы недели» → %s\n\n%s\n", llmTopics, promptTopics)

	_, err := io.WriteString(w, b.String())
	return err
}

// disputeTailComments — сколько последних реплик кандидата класть в материалы.
const disputeTailComments = 20

func writeNoteList(b *strings.Builder, briefs []NoteBrief) {
	if len(briefs) == 0 {
		b.WriteString("- (заметок не было)\n")
		return
	}
	for _, nb := range briefs {
		fmt.Fprintf(b, "- [%s] %s — %s: «%s»\n",
			nb.Note.ID, nb.Note.AuthorName, nComments(nb.Comments), collapse(nb.Note.Text, 160))
	}
}

// collapse схлопывает пробелы и обрезает текст до maxRunes рун.
func collapse(text string, maxRunes int) string {
	t := strings.Join(strings.Fields(text), " ")
	if r := []rune(t); len(r) > maxRunes {
		t = string(r[:maxRunes]) + "…"
	}
	return t
}
