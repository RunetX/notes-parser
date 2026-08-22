// Пакет platout — обратное направление зеркала: то, что люди пишут НА ПЛОЩАДКЕ,
// уходит в каналы мессенджеров.
//
// Зачем он появился. Зеркало несёт поток в одну сторону — НГС в Telegram, MAX и
// площадку, — и пока писали на НГС, этого хватало. С 17.08.2026 сайт отвечает
// 500 на любой POST комментария, писать там больше нельзя, и площадка стала
// единственным местом, где разговор продолжается. Без этого пакета она и канал
// расходятся: на странице идёт беседа, в канале тишина, а аудитория живёт в
// канале.
//
// Три решения, которые стоит понимать.
//
// ПРИЁМНИКИ ТЕ ЖЕ (mirror.Sink), и это не экономия кода, а требование вида:
// заметка площадки обязана выглядеть в канале ровно как заметка НГС — аватар
// подписью к фото, тот же заголовок, тот же тред. Иначе переезд читается как
// подмена. Сама площадка из списка исключается вызывающим — приёмник, который
// принял бы собственную запись обратно, замкнул бы петлю.
//
// УЧЁТ ОТПРАВЛЕННОГО ЛЕЖИТ В SQLITE (message_targets), а не в Postgres. Там уже
// записано, где в мессенджере находится КАЖДЫЙ зеркальный комментарий, — а
// нативная реплика сплошь и рядом отвечает зеркальной, и адресата надо найти
// одним запросом. Полосы идентификаторов не пересекаются, поэтому одна таблица
// честно отвечает на вопрос «где это сообщение» и про НГС, и про нас; заведи мы
// вторую, ответ пришлось бы искать в двух местах и сшивать.
//
// КУРСОР ЖИВЁТ В ПАМЯТИ и на старте всегда стоит в начале нативной полосы.
// Двигается он только по непрерывному началу законченного, поэтому заметка, у
// которой ещё не пойман тред обсуждения, задерживает свои комментарии, но не
// теряет их. Цена — лишний просмотр уже отправленного после рестарта, и она
// заплачена сознательно: пакетная проверка message_targets стоит одного
// запроса, а курсор в базе стоил бы миграции и второго источника правды.
package platout

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/chantext"
	"lovegw/internal/mirror"
	"lovegw/internal/platform"
	"lovegw/internal/store"
)

// Interval — период обхода. Комментарий на площадке должен появляться в канале
// быстро: разговор идёт в реальном времени, и минута задержки превращает ответ
// в реплику невпопад. Стоит такт двух индексных запросов к Postgres.
const Interval = 15 * time.Second

// readBudget — потолок ЧТЕНИЯ площадки за такт. Ограничивает он только запрос к
// Postgres, но не отправку: та идёт через лимитеры мессенджеров и законно
// длится минуты (Telegram в группе — сообщение раз в несколько секунд), поэтому
// общий срок на такт рвал бы пачку посередине.
//
// Нужен он ровно затем же, зачем бюджет у приёмника: занятая база не должна
// останавливать канал. Не прочитали — такт пустой, через 15 секунд следующий,
// курсор на месте.
const readBudget = 10 * time.Second

// batch — сколько строк забирать за такт. Ограничивает и объём запроса к
// Postgres, и размер пакетной проверки в SQLite.
const batch = 200

// threadGrace — сколько ждать тред обсуждения, прежде чем махнуть рукой.
//
// Ждать вообще нужно потому, что в Telegram корень треда ловится автофорвардом
// канала и приезжает через секунды после поста; сдаваться нужно потому, что
// бывает и «никогда»: заметка НГС, запощенная до подключения этого приёмника,
// треда в нём не получит уже никогда, а её комментарий на площадке написать
// могут. Без срока такой комментарий держал бы курсор вечно.
const threadGrace = time.Hour

// Source — что обход берёт у площадки. Интерфейс, а не *platform.Platform,
// ради тестов: подделать два чтения дешевле, чем поднимать Postgres.
type Source interface {
	OutboundNotes(ctx context.Context, afterID int64, limit int) ([]platform.OutNote, error)
	OutboundComments(ctx context.Context, afterID int64, limit int) ([]platform.OutComment, error)
}

// MediaSource — где лежат байты аватара. Может быть nil: тогда заметка и
// комментарий уходят без картинки, как уходит зеркальный комментарий без
// аватара.
type MediaSource interface {
	FilePath(sha []byte, mime string) string
}

// Stats — что обход сделал за проход.
type Stats struct {
	Notes    int
	Comments int
}

// Empty — делать было нечего (обычное состояние).
func (s Stats) Empty() bool { return s.Notes+s.Comments == 0 }

// Service — сам обход.
type Service struct {
	src     Source
	st      *store.Store
	media   MediaSource
	sinks   []mirror.Sink
	baseURL string
	log     *slog.Logger

	// Курсоры непрерывного начала отправленного. Общие на все приёмники:
	// отставший приёмник держит курсор, а ушедшие вперёд отсеиваются пакетной
	// проверкой message_targets — она и так делается на каждом такте.
	noteAt    int64
	commentAt int64

	// quiet — про какие заметки уже сказано «треда нет»: без этого лог
	// заполнялся бы одной строкой раз в пятнадцать секунд.
	quiet map[string]bool
}

// New создаёт обход. sinks — приёмники БЕЗ самой площадки; baseURL — адрес
// площадки для ссылки под постом заметки (пусто — ссылки не будет).
func New(src Source, st *store.Store, media MediaSource, sinks []mirror.Sink, baseURL string, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		src: src, st: st, media: media, sinks: sinks,
		baseURL: strings.TrimSuffix(baseURL, "/"),
		log:     log,
		noteAt:  platform.NativeIDBase - 1, commentAt: platform.NativeIDBase - 1,
		quiet: map[string]bool{},
	}
}

// Run крутит обход до отмены контекста.
func (s *Service) Run(ctx context.Context) error {
	if len(s.sinks) == 0 {
		s.log.Info("исходящий обход площадки не запущен: приёмников нет")
		return nil
	}
	names := make([]string, 0, len(s.sinks))
	for _, sink := range s.sinks {
		names = append(names, sink.Name())
	}
	s.log.Info("исходящий обход площадки запущен", "every", Interval, "sinks", strings.Join(names, ","))

	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			st, err := s.Once(ctx)
			if err != nil {
				s.log.Error("исходящий обход площадки", "err", err)
				continue
			}
			if !st.Empty() {
				s.log.Info("площадка отдана в каналы", "notes", st.Notes, "comments", st.Comments)
			}
		}
	}
}

// Once — один проход: сперва заметки, потом комментарии. Порядок обязателен —
// комментарий уходит в тред своей заметки, а тред заводится постом.
func (s *Service) Once(ctx context.Context) (Stats, error) {
	var st Stats
	n, err := s.sendNotes(ctx)
	st.Notes = n
	if err != nil {
		return st, err
	}
	c, err := s.sendComments(ctx)
	st.Comments = c
	return st, err
}

// outboundNotes и outboundComments — чтения площадки под сроком (см. readBudget).
func (s *Service) outboundNotes(ctx context.Context) ([]platform.OutNote, error) {
	ctx, cancel := context.WithTimeout(ctx, readBudget)
	defer cancel()
	return s.src.OutboundNotes(ctx, s.noteAt, batch)
}

func (s *Service) outboundComments(ctx context.Context) ([]platform.OutComment, error) {
	ctx, cancel := context.WithTimeout(ctx, readBudget)
	defer cancel()
	return s.src.OutboundComments(ctx, s.commentAt, batch)
}

// sendNotes отправляет нативные заметки в каналы всех приёмников.
func (s *Service) sendNotes(ctx context.Context) (int, error) {
	notes, err := s.outboundNotes(ctx)
	if err != nil || len(notes) == 0 {
		return 0, err
	}
	refs := make([]string, len(notes))
	for i, n := range notes {
		refs[i] = strconv.FormatInt(n.ID, 10)
	}

	sent := 0
	done := allTrue(len(notes))
	for _, sink := range s.sinks {
		already, err := s.st.SentTargets(ctx, sink.Name(), store.TargetNotePost, refs)
		if err != nil {
			s.log.Error("чтение отправленных заметок", "sink", sink.Name(), "err", err)
			markFrom(done, 0)
			continue
		}
		for i, n := range notes {
			if already[refs[i]] {
				continue
			}
			if err := s.postNote(ctx, sink, n); err != nil {
				// Порядок важен так же, как у зеркала: не перескакиваем через
				// неотправленное, иначе в канале заметки поменяются местами.
				s.log.Warn("заметка площадки не отправлена", "note", n.ID, "sink", sink.Name(), "err", err)
				markFrom(done, i)
				break
			}
			s.log.Info("заметка площадки отправлена", "note", n.ID, "sink", sink.Name())
			sent++
		}
	}
	s.noteAt = advance(s.noteAt, done, func(i int) int64 { return notes[i].ID })
	return sent, nil
}

// postNote постит заметку в канал приёмника и фиксирует пост.
func (s *Service) postNote(ctx context.Context, sink mirror.Sink, n platform.OutNote) error {
	sn := s.noteRow(n)
	// Приёмник, открывающий тред сам (MAX), делает это ДО поста в канал —
	// тогда кнопка «Обсудить» ведёт сразу в ветку заметки. В Telegram корень
	// треда приносит автофорвард, и делать здесь нечего.
	s.openThread(ctx, sink, sn, "")

	msgID, err := sink.PostNote(ctx, sn, s.avatar(n.AvatarSHA, n.AvatarMIME))
	if err != nil {
		return err
	}
	return s.st.SetTarget(ctx, sink.Name(), store.TargetNotePost, sn.ID, msgID, "")
}

// sendComments отправляет нативные комментарии в треды их заметок.
func (s *Service) sendComments(ctx context.Context) (int, error) {
	cs, err := s.outboundComments(ctx)
	if err != nil || len(cs) == 0 {
		return 0, err
	}
	refs := make([]string, len(cs))
	for i, c := range cs {
		refs[i] = strconv.FormatInt(c.ID, 10)
	}

	sent := 0
	done := allTrue(len(cs))
	for _, sink := range s.sinks {
		already, err := s.st.SentTargets(ctx, sink.Name(), store.TargetComment, refs)
		if err != nil {
			s.log.Error("чтение отправленных комментариев", "sink", sink.Name(), "err", err)
			markFrom(done, 0)
			continue
		}
		threads := map[int64]string{} // тред заметки спрашиваем один раз на такт
		for i, c := range cs {
			if already[refs[i]] {
				continue
			}
			thread := s.threadFor(ctx, sink, c.NoteID, threads)
			if thread == "" {
				// Треда пока нет. Ждём — но не дольше срока: у заметки,
				// которой в этом канале нет вовсе, тред не появится никогда.
				if time.Since(c.PublishedAt) < threadGrace {
					done[i] = false
				} else {
					s.warnOnce(sink.Name(), c.NoteID,
						"комментарий площадки некуда отнести: треда заметки в этом канале нет")
				}
				continue
			}
			msgID, err := sink.PostComment(ctx, s.noteStub(c.NoteID), thread,
				s.replyTarget(ctx, sink, c.ReplyToID), s.commentRow(c), s.avatar(c.AvatarSHA, c.AvatarMIME))
			if err != nil {
				s.log.Warn("комментарий площадки не отправлен", "comment", c.ID, "sink", sink.Name(), "err", err)
				markFrom(done, i)
				break
			}
			if err := s.st.SetTarget(ctx, sink.Name(), store.TargetComment, refs[i], msgID, ""); err != nil {
				s.log.Error("фиксация комментария площадки", "comment", c.ID, "sink", sink.Name(), "err", err)
				markFrom(done, i)
				break
			}
			sent++
		}
	}
	s.commentAt = advance(s.commentAt, done, func(i int) int64 { return cs[i].ID })
	return sent, nil
}

// threadFor — корень треда заметки в этом приёмнике; "" — треда ещё (или уже)
// нет. Заметка, не запощенная в этот канал, треда не получит: у нативной пост
// поставит следующий такт, у зеркальной — не поставит никто.
func (s *Service) threadFor(ctx context.Context, sink mirror.Sink, noteID int64, cache map[int64]string) string {
	if t, ok := cache[noteID]; ok {
		return t
	}
	thread := s.lookupThread(ctx, sink, noteID)
	cache[noteID] = thread
	return thread
}

func (s *Service) lookupThread(ctx context.Context, sink mirror.Sink, noteID int64) string {
	ref := strconv.FormatInt(noteID, 10)
	_, thread, found, err := s.st.Target(ctx, sink.Name(), store.TargetNoteThread, ref)
	if err != nil {
		s.log.Error("чтение треда заметки", "note", noteID, "sink", sink.Name(), "err", err)
		return ""
	}
	if found && thread != "" {
		return thread
	}
	postID, _, posted, err := s.st.Target(ctx, sink.Name(), store.TargetNotePost, ref)
	if err != nil {
		s.log.Error("чтение поста заметки", "note", noteID, "sink", sink.Name(), "err", err)
		return ""
	}
	if !posted || postID == "" {
		return ""
	}
	// Приёмник, умеющий открыть тред сам (MAX), — единственный, кому здесь есть
	// что сделать: в Telegram корень треда приносит автофорвард.
	return s.openThread(ctx, sink, s.noteStub(noteID), postID)
}

// openThread просит приёмник открыть тред и фиксирует корень. Все отказы
// мягкие: без треда заметка уйдёт в канал обычным путём, а комментарии подождут
// следующего такта.
func (s *Service) openThread(ctx context.Context, sink mirror.Sink, n store.Note, postMsgID string) string {
	ts, ok := sink.(mirror.ThreadStarter)
	if !ok {
		return ""
	}
	thread, err := ts.StartThread(ctx, n, postMsgID)
	if err != nil {
		s.log.Warn("тред заметки площадки не открыт", "note", n.ID, "sink", sink.Name(), "err", err)
		return ""
	}
	if err := s.st.SetTarget(ctx, sink.Name(), store.TargetNoteThread, n.ID, "", thread); err != nil {
		s.log.Error("фиксация треда заметки площадки", "note", n.ID, "sink", sink.Name(), "err", err)
		return ""
	}
	s.log.Info("тред заметки площадки открыт", "note", n.ID, "sink", sink.Name(), "thread", thread)
	return thread
}

// replyTarget — сообщение, на которое отвечает реплика. Один запрос отвечает и
// про зеркального адресата, и про нативного: полосы идентификаторов не
// пересекаются, а таблица одна. Пусто — адресата нет либо его сообщение до
// мессенджера не доехало: реплика уйдёт корню треда.
func (s *Service) replyTarget(ctx context.Context, sink mirror.Sink, replyToID int64) string {
	if replyToID == 0 {
		return ""
	}
	msgID, _, found, err := s.st.Target(ctx, sink.Name(), store.TargetComment,
		strconv.FormatInt(replyToID, 10))
	if err != nil {
		s.log.Error("поиск адресата реплики площадки", "reply_to", replyToID, "sink", sink.Name(), "err", err)
		return ""
	}
	if !found {
		return ""
	}
	return msgID
}

// noteRow собирает строку заметки в том виде, в каком её ждут приёмники.
//
// Две вещи здесь делаются ЗА приёмника, потому что знание о происхождении
// текста есть только тут.
//
// Первая — РАЗМЕТКА. На площадке текст хранится плоским, а начертания в нём —
// знаки НГС («[b]жирный[/b]»), те самые, что у переехавших в пальцах.
// Композер приёмника экранирует строку целиком и другого не умеет, поэтому без
// этого превращения в канале выходили скобки.
//
// Вторая — ССЫЛКА на страницу площадки. Она отдельным полем, а не припиской к
// тексту: у заметки, написанной здесь, страницы на НГС нет вовсе, а тело
// длиной с эту режется под предел сообщения — приписанную к нему ссылку
// обрезка съела бы первой, то есть ровно у тех заметок, где она нужнее всего.
func (s *Service) noteRow(n platform.OutNote) store.Note {
	return store.Note{
		ID:              strconv.FormatInt(n.ID, 10),
		AuthorID:        authorRef(n.AuthorID),
		AuthorName:      n.AuthorNick,
		Text:            n.Body,
		TextHTML:        chantext.FromSiteMarkup(n.Body),
		SourceURL:       s.noteURL(n.ID),
		AuthorAvatarURL: s.mediaURL(n.AvatarSHA, n.AvatarMIME),
		FirstSeenAt:     n.PublishedAt,
	}
}

// noteStub — заметка, от которой приёмнику нужен только id (тред, ссылки).
func (s *Service) noteStub(noteID int64) store.Note {
	return store.Note{ID: strconv.FormatInt(noteID, 10)}
}

// commentRow собирает строку комментария для приёмников.
//
// Возраст пуст: анкетных полей площадка не заводит вовсе. Обращения «Ник, » в
// теле тоже нет — оно хранится ребром, а в мессенджере его роль играет реплай
// на сообщение адресата, и цитату рисует сам мессенджер. Разметка разбирается
// по той же причине, что и у заметки: знаки НГС в теле остались бы скобками.
func (s *Service) commentRow(c platform.OutComment) store.Comment {
	return store.Comment{
		ID:          c.ID,
		NoteID:      strconv.FormatInt(c.NoteID, 10),
		AuthorName:  c.AuthorNick,
		AuthorLink:  authorLink(c.AuthorID),
		AvatarURL:   s.mediaURL(c.AvatarSHA, c.AvatarMIME),
		PublishedAt: c.PublishedAt,
		Text:        c.Body,
		TextHTML:    chantext.FromSiteMarkup(c.Body),
	}
}

// noteURL — адрес заметки на площадке; пусто, если адрес площадки не задан
// (тогда ссылки в посте не будет, как было до Ш5а).
func (s *Service) noteURL(id int64) string {
	if s.baseURL == "" {
		return ""
	}
	return s.baseURL + "/n/" + strconv.FormatInt(id, 10)
}

// mediaURL — абсолютная ссылка на аватар в нашем хранилище. Приёмники держат по
// ней кэш file_id, поэтому важна её устойчивость: имя файла есть sha
// содержимого, так что один и тот же аватар всегда даёт одну и ту же ссылку.
func (s *Service) mediaURL(sha []byte, mime string) string {
	path := platform.MediaURL(sha, mime)
	if path == "" || s.baseURL == "" {
		return path
	}
	return s.baseURL + path
}

// avatar — байты аватара с диска. Нет хранилища, нет файла — nil: заметка уйдёт
// текстом, комментарий без картинки. Это штатный путь, а не отказ.
func (s *Service) avatar(sha []byte, mime string) []byte {
	if s.media == nil || len(sha) == 0 {
		return nil
	}
	data, err := os.ReadFile(s.media.FilePath(sha, mime))
	if err != nil {
		return nil
	}
	return data
}

// warnOnce пишет предупреждение про заметку один раз на приёмник: такт у обхода
// пятнадцатисекундный, и без этого лог состоял бы из одной строки.
func (s *Service) warnOnce(sink string, noteID int64, msg string) {
	key := sink + "/" + strconv.FormatInt(noteID, 10)
	if s.quiet[key] {
		return
	}
	s.quiet[key] = true
	s.log.Warn(msg, "note", noteID, "sink", sink)
}

// authorRef — id автора для композера поста: он делает из него ссылку на анкету
// НГС. Пусто — имя без ссылки (аноним либо участник без анкеты).
func authorRef(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

// authorLink — ссылка на анкету НГС в шапке комментария; пусто — имя без ссылки.
func authorLink(id int64) string {
	if id == 0 {
		return ""
	}
	return fmt.Sprintf("https://love.ngs.ru/profile/%d/", id)
}

func allTrue(n int) []bool {
	done := make([]bool, n)
	for i := range done {
		done[i] = true
	}
	return done
}

// markFrom помечает незаконченным всё начиная с i: приёмник сорвался, и порядок
// требует повторить с этого места.
func markFrom(done []bool, i int) {
	for ; i < len(done); i++ {
		done[i] = false
	}
}

// advance двигает курсор по непрерывному началу законченного. Дальше первого
// незаконченного он не идёт намеренно: перескочив, обход забыл бы про него до
// перезапуска.
func advance(at int64, done []bool, id func(int) int64) int64 {
	for i, ok := range done {
		if !ok {
			break
		}
		at = id(i)
	}
	return at
}
