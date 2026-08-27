// Пакет digest — еженедельный редакторский выпуск по заметкам и комментариям
// живой БД: расчёт рубрик, черновик для полуручной LLM-редактуры и публикация
// в каналы мессенджеров.
//
// В выпуск идёт только публичная активность сайта; персон-аналитика архива
// (атрибуция анонимов, пол, identity_facts) сюда не выносится.
//
// Известные погрешности данных (приняты осознанно):
//   - комментарии, появившиеся на сайте после архивации заметки, в живую БД
//     не попадают и в метриках не участвуют;
//   - у комментариев без published_at время берётся из created_at — момента
//     вставки (отстаёт до интервала опроса; бэкфиллы pull -full дают старым
//     комментариям свежий created_at);
//   - история «новых лиц» и рекордов ограничена глубиной живой БД.
package digest

import (
	"context"
	"fmt"
	"time"
)

// Пороги и константы рубрик. Значения — редакционные эвристики.
const (
	minRecordComments = 10               // тред короче — не претендует на рекорд
	minRecordPeakHour = 5                // пик-час ниже — не рекорд
	minDisputeSize    = 5                // комментариев, чтобы считаться «спором»
	quoteMinRunes     = 80               // границы длины «цитаты недели»
	quoteMaxRunes     = 600              //
	quoteReplyWindow  = 2 * time.Hour    // окно «ответов» после цитаты
	pingPongGap       = 10 * time.Minute // зазор реплик «пинг-понга»
	returneeGap       = 28 * 24 * time.Hour
	stillAliveWindow  = 48 * time.Hour
	topDisputes       = 3
	topQuotes         = 10
	topPersons        = 5
	topStillAlive     = 3
)

// Stats — сводные числа окна.
type Stats struct {
	Notes      int // заметок появилось в окне
	Comments   int // комментариев в окне
	Commenters int // уникальных анкет среди комментаторов окна
}

// NoteStat — обсуждение одной заметки в границах окна.
type NoteStat struct {
	Note       Note
	Comments   int
	Commenters int       // уникальных author_link (безанкетные не считаются)
	FirstAt    time.Time // первый и последний комментарий окна
	LastAt     time.Time
	PeakHour   time.Time // начало самого плотного календарного часа (UTC)
	PeakHourN  int
	PingPong   int     // пар соседних реплик разных авторов с зазором <= pingPongGap
	Heat       float64 // эвристика накала (шорт-лист «спора недели»)
}

// Quote — кандидат «цитаты недели».
type Quote struct {
	Comment      Comment
	RepliesAfter int // комментариев других авторов в треде за quoteReplyWindow после
}

// Person — участник для рубрики «новые лица / возвращение недели».
//
// Адреса анкеты у него нет и не заводится: имя в выпуске стоит текстом.
// Ссылкой на анкету НГС оно было до 27.08.2026 — ссылок на НГС проект не
// ставит нигде (решение владельца), а своей страницы участника у площадки нет.
type Person struct {
	Name       string
	Notes      int       // заметок в окне
	Comments   int       // комментариев в окне
	PrevSeenAt time.Time // последняя активность до окна; zero — новичок
}

// Record — сравнительный рекорд недели, готовая формулировка.
type Record struct {
	Text   string
	NoteID string // заметка-виновница; "" — рекорд без конкретного треда
}

// NoteBrief — строка списка заметок недели (материалы «тем недели»).
type NoteBrief struct {
	Note     Note
	Comments int // комментариев за окно своей недели
}

// Issue — рассчитанный выпуск.
type Issue struct {
	Window Window
	Stats  Stats

	TopNote    *NoteStat  // «заметка недели»; nil — комментариев в окне не было
	Disputes   []NoteStat // шорт-лист «спора недели» (без TopNote)
	Quotes     []Quote    // шорт-лист «цитаты недели»
	Newcomers  []Person   // появились впервые
	Returnees  []Person   // вернулись после молчания >= returneeGap
	Records    []Record
	StillAlive []NoteStat // обсуждение не утихло к выпуску (без TopNote);
	// рубрика осмысленна для текущего слота: считается по текущему
	// last_comment_at, а не по состоянию на конец ретро-окна

	ThisWeekNotes []NoteBrief // материалы «тем недели»
	PrevWeekNotes []NoteBrief

	CommentsByNote map[string][]Comment // комментарии окна по заметкам (для материалов LLM)

	// Editorial — тексты LLM-рубрик (GenerateEditorial); nil — черновик
	// пишется с плейсхолдерами под полуручной цикл.
	Editorial *Editorial
}

// Build считает выпуск по источнику: зеркалу НГС (SQLite) или площадке
// (Postgres). Что именно спрашивается у базы — в Source.
func Build(ctx context.Context, src Source, w Window) (*Issue, error) {
	comments, err := src.CommentsBetween(ctx, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("комментарии окна: %w", err)
	}
	byNote := groupByNote(comments)
	noteIDs := make([]string, 0, len(byNote))
	for id := range byNote {
		noteIDs = append(noteIDs, id)
	}
	heads, err := src.NotesByIDs(ctx, noteIDs)
	if err != nil {
		return nil, fmt.Errorf("шапки заметок: %w", err)
	}
	stats := buildNoteStats(byNote, heads)
	computeHeat(stats)

	is := &Issue{Window: w, CommentsByNote: byNote}
	is.TopNote = pickTopNote(stats)
	is.Disputes = pickDisputes(stats, is.TopNote)
	is.Quotes = pickQuotes(byNote)

	if err := fillPersons(ctx, src, is); err != nil {
		return nil, err
	}
	if err := fillRecords(ctx, src, is, comments); err != nil {
		return nil, err
	}
	if err := fillStillAlive(ctx, src, is, stats); err != nil {
		return nil, err
	}
	if err := fillWeekNotes(ctx, src, is); err != nil {
		return nil, err
	}

	is.Stats = Stats{
		Notes:      len(is.ThisWeekNotes),
		Comments:   len(comments),
		Commenters: distinctLinks(comments),
	}
	return is, nil
}

func fillStillAlive(ctx context.Context, src Source, is *Issue, stats []NoteStat) error {
	alive, err := src.ActiveNotesSince(ctx, is.Window.End.Add(-stillAliveWindow))
	if err != nil {
		return fmt.Errorf("живые заметки: %w", err)
	}
	byID := make(map[string]NoteStat, len(stats))
	for _, s := range stats {
		byID[s.Note.ID] = s
	}
	for _, n := range alive {
		if is.TopNote != nil && n.ID == is.TopNote.Note.ID {
			continue
		}
		if len(is.StillAlive) == topStillAlive {
			break
		}
		s, ok := byID[n.ID]
		if !ok {
			s = NoteStat{Note: n}
		}
		is.StillAlive = append(is.StillAlive, s)
	}
	return nil
}

func fillWeekNotes(ctx context.Context, src Source, is *Issue) error {
	w := is.Window
	this, err := src.NotesPublishedBetween(ctx, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("заметки окна: %w", err)
	}
	for _, n := range this {
		is.ThisWeekNotes = append(is.ThisWeekNotes,
			NoteBrief{Note: n, Comments: len(is.CommentsByNote[n.ID])})
	}

	prevStart := w.Start.AddDate(0, 0, -7)
	prev, err := src.NotesPublishedBetween(ctx, prevStart, w.Start)
	if err != nil {
		return fmt.Errorf("заметки прошлой недели: %w", err)
	}
	prevComments, err := src.CommentsBetween(ctx, prevStart, w.Start)
	if err != nil {
		return fmt.Errorf("комментарии прошлой недели: %w", err)
	}
	prevByNote := groupByNote(prevComments)
	for _, n := range prev {
		is.PrevWeekNotes = append(is.PrevWeekNotes,
			NoteBrief{Note: n, Comments: len(prevByNote[n.ID])})
	}
	return nil
}
