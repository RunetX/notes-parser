package platout

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"lovegw/internal/mirror"
	"lovegw/internal/platform"
	"lovegw/internal/store"
)

// --- заглушки ---------------------------------------------------------------

// source отдаёт заранее заготовленные строки, уважая курсор и предел: обход
// опирается на оба, и подделка, игнорирующая их, проверяла бы не то.
type source struct {
	notes    []platform.OutNote
	comments []platform.OutComment
	err      error
}

func (s *source) OutboundNotes(_ context.Context, afterID int64, limit int) ([]platform.OutNote, error) {
	if s.err != nil {
		return nil, s.err
	}
	var out []platform.OutNote
	for _, n := range s.notes {
		if n.ID > afterID && len(out) < limit {
			out = append(out, n)
		}
	}
	return out, nil
}

func (s *source) OutboundComments(_ context.Context, afterID int64, limit int) ([]platform.OutComment, error) {
	if s.err != nil {
		return nil, s.err
	}
	var out []platform.OutComment
	for _, c := range s.comments {
		if c.ID > afterID && len(out) < limit {
			out = append(out, c)
		}
	}
	return out, nil
}

type call struct {
	kind     string // note | comment | thread
	noteID   string
	comID    int64
	thread   string
	replyTo  string
	text     string
	textHTML string
	source   string
	author   string
	link     string
	hasImage bool
}

type sink struct {
	name  string
	calls []call
	next  int64
	fail  error // отказ на КАЖДОЙ отправке
}

func (s *sink) Name() string {
	if s.name == "" {
		return store.MessengerTelegram
	}
	return s.name
}

func (s *sink) id() string { s.next++; return "m" + strconv.FormatInt(s.next, 10) }

func (s *sink) PostNote(_ context.Context, n store.Note, avatar []byte) (string, error) {
	if s.fail != nil {
		return "", s.fail
	}
	s.calls = append(s.calls, call{kind: "note", noteID: n.ID, text: n.Text,
		textHTML: n.TextHTML, source: n.SourceURL,
		author: n.AuthorName, link: n.AuthorID, hasImage: len(avatar) > 0})
	return s.id(), nil
}

func (s *sink) PostComment(_ context.Context, n store.Note, threadID, replyToID string,
	c store.Comment, avatar []byte) (string, error) {
	if s.fail != nil {
		return "", s.fail
	}
	s.calls = append(s.calls, call{kind: "comment", noteID: n.ID, comID: c.ID, thread: threadID,
		replyTo: replyToID, text: c.Text, textHTML: c.TextHTML,
		author: c.AuthorName, link: c.AuthorLink,
		hasImage: len(avatar) > 0})
	return s.id(), nil
}

func (s *sink) PostNoteImage(_ context.Context, threadID, imageURL string, image []byte) (string, error) {
	if s.fail != nil {
		return "", s.fail
	}
	s.calls = append(s.calls, call{kind: "image", thread: threadID, source: imageURL,
		hasImage: len(image) > 0})
	return s.id(), nil
}

// threadSink — приёмник, открывающий тред сам (так устроен MAX).
type threadSink struct{ sink }

func (s *threadSink) StartThread(_ context.Context, n store.Note, _ string) (string, error) {
	s.calls = append(s.calls, call{kind: "thread", noteID: n.ID})
	return "t" + n.ID, nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func counts(c []call, kind string) int {
	n := 0
	for _, x := range c {
		if x.kind == kind {
			n++
		}
	}
	return n
}

// --- тесты ------------------------------------------------------------------

// Заметка площадки уходит в канал один раз: второй проход её не повторяет.
// Дедуп держится на message_targets, а не на памяти обхода, поэтому проверяем
// именно запись — она переживёт рестарт демона, а курсор нет.
func TestNotePostedOnce(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	src := &source{notes: []platform.OutNote{{
		ID: platform.NativeIDBase, AuthorID: 1443311, AuthorNick: "Dr. David Livesey",
		Body: "Купи ей нормального кофе.", PublishedAt: time.Now(),
	}}}
	sk := &sink{}
	svc := New(src, st, nil, []mirror.Sink{sk}, "https://t3h.ru", quiet())

	if _, err := svc.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got := counts(sk.calls, "note"); got != 1 {
		t.Fatalf("постов заметки: %d, ждали 1", got)
	}
	msg, _, found, err := st.Target(ctx, store.MessengerTelegram, store.TargetNotePost,
		strconv.FormatInt(platform.NativeIDBase, 10))
	if err != nil || !found || msg == "" {
		t.Fatalf("пост не зафиксирован: msg=%q found=%v err=%v", msg, found, err)
	}

	if _, err := svc.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got := counts(sk.calls, "note"); got != 1 {
		t.Errorf("повторный проход отправил ещё раз: всего %d", got)
	}
}

// Ссылка на страницу площадки едет ОТДЕЛЬНЫМ полем, а не припиской к тексту.
// Без неё у заметки, написанной здесь, нет вообще ни одного адреса: на НГС
// такой страницы нет. А отдельным полем — потому что длинное тело композер
// режет под предел сообщения, и ссылку в конце текста обрезка съела бы первой.
func TestNoteCarriesPlatformLink(t *testing.T) {
	st := newStore(t)
	src := &source{notes: []platform.OutNote{{
		ID: platform.NativeIDBase, AuthorID: 294456, AuthorNick: "А.С.Сенин 60",
		Body: "давайте", PublishedAt: time.Now(),
	}}}
	sk := &sink{}
	svc := New(src, st, nil, []mirror.Sink{sk}, "https://t3h.ru/", quiet())
	if _, err := svc.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sk.calls[0].source; got != "https://t3h.ru/n/100000000000" {
		t.Errorf("ссылка на страницу площадки: %q", got)
	}
	if got := sk.calls[0].text; got != "давайте" {
		t.Errorf("тело поста изменено: %q", got)
	}
	if sk.calls[0].link != "294456" {
		t.Errorf("ссылка на анкету автора: %q", sk.calls[0].link)
	}
}

// Знаки НГС в написанном ЗДЕСЬ приезжают приёмнику РАЗМЕЧЕННЫМИ. Композер
// экранирует всё, что ему дали сырым, поэтому без этого превращения заметка
// выходила в канал скобками — «[b]Хотелки[/b]».
func TestNativeMarkupReachesSink(t *testing.T) {
	st := newStore(t)
	src := &source{
		notes: []platform.OutNote{{
			ID: platform.NativeIDBase, AuthorID: 1, AuthorNick: "Паноптикум",
			Body: "[b]Хотелки[/b] & прочее", PublishedAt: time.Now(),
		}},
		comments: []platform.OutComment{{
			ID: platform.NativeIDBase, NoteID: platform.NativeIDBase, AuthorID: 2,
			AuthorNick: "Ответчик", Body: "[i]да[/i]", PublishedAt: time.Now(),
		}},
	}
	sk := &sink{}
	svc := New(src, st, nil, []mirror.Sink{sk}, "https://t3h.ru", quiet())
	if err := st.SetTarget(context.Background(), sk.Name(), store.TargetNoteThread,
		strconv.FormatInt(platform.NativeIDBase, 10), "", "thread1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sk.calls[0].textHTML; got != "<b>Хотелки</b> &amp; прочее" {
		t.Errorf("разметка заметки: %q", got)
	}
	com := sk.calls[len(sk.calls)-1]
	if com.kind != "comment" || com.textHTML != "<i>да</i>" {
		t.Errorf("разметка комментария: %+v", com)
	}
}

// У анонимной заметки автора не показывают вовсе. Проверяем именно то, что
// приёмнику НЕ ДАЛИ номера анкеты: композер поста делает из него ссылку, и одна
// такая ссылка деанонимизирует автора навсегда.
func TestAnonymousNoteHasNoAuthorRef(t *testing.T) {
	st := newStore(t)
	src := &source{notes: []platform.OutNote{{
		ID: platform.NativeIDBase, Anonymous: true, AuthorNick: platform.AnonNick,
		Body: "тайна", PublishedAt: time.Now(),
	}}}
	sk := &sink{}
	svc := New(src, st, nil, []mirror.Sink{sk}, "", quiet())
	if _, err := svc.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sk.calls[0].link != "" {
		t.Errorf("у анонимной заметки уехал номер анкеты: %q", sk.calls[0].link)
	}
	if sk.calls[0].author != "Аноним" {
		t.Errorf("имя автора анонимки: %q", sk.calls[0].author)
	}
}

// Комментарий площадки под заметкой НГС уходит в УЖЕ СУЩЕСТВУЮЩИЙ тред этой
// заметки, а адресатом ему служит зеркальный комментарий. Это и есть выигрыш от
// общей таблицы: одна и та же message_targets отвечает и про НГС, и про нас.
func TestCommentJoinsMirroredThread(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	tg := store.MessengerTelegram
	if err := st.SetTarget(ctx, tg, store.TargetNotePost, "313028", "500", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTarget(ctx, tg, store.TargetNoteThread, "313028", "", "900"); err != nil {
		t.Fatal(err)
	}
	// Зеркальная реплика, на которую отвечают с площадки.
	if err := st.SetTarget(ctx, tg, store.TargetComment, "63207290", "901", ""); err != nil {
		t.Fatal(err)
	}

	src := &source{comments: []platform.OutComment{{
		ID: platform.NativeIDBase + 7, NoteID: 313028, AuthorID: 1472546,
		AuthorNick: "Паноптикум", Body: "Кому делать, зачем делать?",
		ReplyToID: 63207290, PublishedAt: time.Now(),
	}}}
	sk := &sink{}
	svc := New(src, st, nil, []mirror.Sink{sk}, "https://t3h.ru", quiet())
	if _, err := svc.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sk.calls) != 1 {
		t.Fatalf("отправок: %d", len(sk.calls))
	}
	c := sk.calls[0]
	if c.thread != "900" {
		t.Errorf("тред: %q, ждали 900", c.thread)
	}
	if c.replyTo != "901" {
		t.Errorf("адресат: %q, ждали 901 (сообщение зеркальной реплики)", c.replyTo)
	}
	if c.link != "https://love.ngs.ru/profile/1472546/" {
		t.Errorf("ссылка на анкету: %q", c.link)
	}
}

// Реплика на реплику площадки: адресат находится там же, в message_targets, —
// его записал предыдущий такт этого же обхода.
func TestNativeReplyResolvesNativeAddressee(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	tg := store.MessengerTelegram
	for _, k := range []struct{ kind, ref, msg, thread string }{
		{store.TargetNotePost, "313028", "500", ""},
		{store.TargetNoteThread, "313028", "", "900"},
	} {
		if err := st.SetTarget(ctx, tg, k.kind, k.ref, k.msg, k.thread); err != nil {
			t.Fatal(err)
		}
	}
	first := platform.NativeIDBase + 4
	second := platform.NativeIDBase + 7
	src := &source{comments: []platform.OutComment{
		{ID: first, NoteID: 313028, AuthorNick: "А.С.Сенин 60", Body: "давайте", PublishedAt: time.Now()},
		{ID: second, NoteID: 313028, AuthorNick: "Паноптикум", Body: "Кому делать?",
			ReplyToID: first, PublishedAt: time.Now()},
	}}
	sk := &sink{}
	svc := New(src, st, nil, []mirror.Sink{sk}, "https://t3h.ru", quiet())
	if _, err := svc.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sk.calls) != 2 {
		t.Fatalf("отправок: %d", len(sk.calls))
	}
	if sk.calls[1].replyTo != "m1" {
		t.Errorf("адресат: %q, ждали m1 — сообщение первой реплики", sk.calls[1].replyTo)
	}
}

// Пока тред заметки не пойман, комментарий ЖДЁТ, а не теряется: курсор не
// перешагивает через неотправленное. Как только корень появился — уходит.
func TestCommentWaitsForThread(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	tg := store.MessengerTelegram
	if err := st.SetTarget(ctx, tg, store.TargetNotePost, "313028", "500", ""); err != nil {
		t.Fatal(err)
	}
	src := &source{comments: []platform.OutComment{{
		ID: platform.NativeIDBase, NoteID: 313028, AuthorNick: "ЧЕРЕПАХА",
		Body: "Тишина, так тишина))))", PublishedAt: time.Now(),
	}}}
	sk := &sink{}
	svc := New(src, st, nil, []mirror.Sink{sk}, "https://t3h.ru", quiet())

	if _, err := svc.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sk.calls) != 0 {
		t.Fatalf("без треда ушло %d сообщений", len(sk.calls))
	}
	if err := st.SetTarget(ctx, tg, store.TargetNoteThread, "313028", "", "900"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sk.calls) != 1 {
		t.Fatalf("после появления треда ушло %d сообщений, ждали 1", len(sk.calls))
	}
}

// Заметка, которой в этом канале нет вовсе (запощена до подключения приёмника),
// треда не получит уже никогда. Такой комментарий держал бы курсор вечно,
// поэтому по истечении срока он пропускается — и следующие уходят.
func TestStaleCommentGivesUpAndLetsNextThrough(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	tg := store.MessengerTelegram
	if err := st.SetTarget(ctx, tg, store.TargetNotePost, "312000", "400", ""); err != nil {
		t.Fatal(err)
	}
	for _, k := range []struct{ kind, ref, msg, thread string }{
		{store.TargetNotePost, "313028", "500", ""},
		{store.TargetNoteThread, "313028", "", "900"},
	} {
		if err := st.SetTarget(ctx, tg, k.kind, k.ref, k.msg, k.thread); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * threadGrace)
	src := &source{comments: []platform.OutComment{
		{ID: platform.NativeIDBase, NoteID: 312000, AuthorNick: "кто-то", Body: "в пустоту", PublishedAt: old},
		{ID: platform.NativeIDBase + 1, NoteID: 313028, AuthorNick: "Паноптикум", Body: "а тут тред есть",
			PublishedAt: time.Now()},
	}}
	sk := &sink{}
	svc := New(src, st, nil, []mirror.Sink{sk}, "https://t3h.ru", quiet())
	if _, err := svc.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sk.calls) != 1 || sk.calls[0].comID != platform.NativeIDBase+1 {
		t.Fatalf("отправлено %d: %+v", len(sk.calls), sk.calls)
	}
	// Курсор перешагнул безнадёжный комментарий: второй проход не повторяет
	// работу и не зависает на нём.
	if svc.commentAt != platform.NativeIDBase+1 {
		t.Errorf("курсор комментариев: %d, ждали %d", svc.commentAt, platform.NativeIDBase+1)
	}
}

// Приёмник, умеющий открыть тред сам (MAX), делает это ДО поста в канал —
// иначе кнопка «Обсудить» на посте вести некуда.
func TestThreadStarterOpensThreadBeforePost(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	src := &source{notes: []platform.OutNote{{
		ID: platform.NativeIDBase, AuthorNick: "Паноптикум", Body: "пост", PublishedAt: time.Now(),
	}}}
	sk := &threadSink{sink{name: store.MessengerMax}}
	svc := New(src, st, nil, []mirror.Sink{sk}, "https://t3h.ru", quiet())
	if _, err := svc.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sk.calls) < 2 || sk.calls[0].kind != "thread" || sk.calls[1].kind != "note" {
		t.Fatalf("порядок вызовов: %+v", sk.calls)
	}
	_, thread, found, err := st.Target(ctx, store.MessengerMax, store.TargetNoteThread,
		strconv.FormatInt(platform.NativeIDBase, 10))
	if err != nil || !found || thread == "" {
		t.Fatalf("корень треда не зафиксирован: %q %v %v", thread, found, err)
	}
}

// Отказ одного приёмника не мешает другому и не теряет работу: на следующем
// проходе повтор уходит ТОЛЬКО отказавшему.
func TestFailedSinkRetriedAlone(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	src := &source{notes: []platform.OutNote{{
		ID: platform.NativeIDBase, AuthorNick: "Паноптикум", Body: "пост", PublishedAt: time.Now(),
	}}}
	good := &sink{name: store.MessengerTelegram}
	bad := &sink{name: store.MessengerMax, fail: errors.New("канал недоступен")}
	svc := New(src, st, nil, []mirror.Sink{good, bad}, "https://t3h.ru", quiet())

	if _, err := svc.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if counts(good.calls, "note") != 1 {
		t.Fatalf("живой приёмник не получил заметку: %+v", good.calls)
	}
	if svc.noteAt != platform.NativeIDBase-1 {
		t.Errorf("курсор ушёл вперёд при отказе: %d", svc.noteAt)
	}

	bad.fail = nil
	if _, err := svc.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if counts(good.calls, "note") != 1 {
		t.Errorf("живому приёмнику заметка ушла дважды: %+v", good.calls)
	}
	if counts(bad.calls, "note") != 1 {
		t.Errorf("отказавшему приёмнику повтор не дошёл: %+v", bad.calls)
	}
	if svc.noteAt != platform.NativeIDBase {
		t.Errorf("курсор не сдвинулся после успеха: %d", svc.noteAt)
	}
}

// Зеркальные строки исходящий обход не трогает: их в канал уже отнесло само
// зеркало. Граница — полоса идентификаторов, и держит её запрос к площадке;
// здесь стережём, что курсор стартует ровно на ней.
func TestCursorStartsAtNativeBand(t *testing.T) {
	svc := New(&source{}, newStore(t), nil, nil, "", quiet())
	if svc.noteAt != platform.NativeIDBase-1 || svc.commentAt != platform.NativeIDBase-1 {
		t.Errorf("курсоры: %d/%d, ждали %d", svc.noteAt, svc.commentAt, platform.NativeIDBase-1)
	}
}

// ---------------------------------------------------------------- иллюстрация

// fakeMedia — хранилище на диске: один файл под любой sha.
type fakeMedia struct{ path string }

func (m fakeMedia) FilePath([]byte, string) string { return m.path }

func mediaWithFile(t *testing.T) fakeMedia {
	t.Helper()
	p := filepath.Join(t.TempDir(), "shot.webp")
	if err := os.WriteFile(p, []byte("байты картинки"), 0o644); err != nil {
		t.Fatal(err)
	}
	return fakeMedia{path: p}
}

func noteWithShot(published time.Time) platform.OutNote {
	return platform.OutNote{
		ID: platform.NativeIDBase, AuthorID: 1443311, AuthorNick: "Рио",
		Body: "с картинкой", PublishedAt: published,
		ImageSHA: []byte{0xab, 0xcd}, ImageMIME: "image/webp",
	}
}

// Картинка идёт В ТРЕД и ПОСЛЕ поста: тред заводится постом, и до него нести
// иллюстрацию некуда.
func TestNoteImageGoesToThreadAfterThePost(t *testing.T) {
	st := newStore(t)
	src := &source{notes: []platform.OutNote{noteWithShot(time.Now())}}
	sk := &threadSink{}
	svc := New(src, st, mediaWithFile(t), []mirror.Sink{sk}, "https://t3h.ru", quiet())

	if _, err := svc.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range sk.calls {
		order = append(order, c.kind)
	}
	want := []string{"thread", "note", "image"}
	if len(order) != len(want) {
		t.Fatalf("вызовы: %v, ожидались %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("вызовы: %v, ожидались %v", order, want)
		}
	}
	img := sk.calls[2]
	if !img.hasImage {
		t.Error("байты картинки до приёмника не дошли")
	}
	if img.thread == "" {
		t.Error("картинка ушла не в тред")
	}
	if img.source != "https://t3h.ru/media/ab/abcd.webp" {
		t.Errorf("ссылка на картинку %q", img.source)
	}
}

// Треда ещё нет (в Telegram его приносит автофорвард через секунды): заметка
// ДЕРЖИТ курсор, а следующий такт не повторяет пост, но доносит картинку.
func TestNoteImageWaitsForTheThread(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	src := &source{notes: []platform.OutNote{noteWithShot(time.Now())}}
	sk := &sink{}
	svc := New(src, st, mediaWithFile(t), []mirror.Sink{sk}, "https://t3h.ru", quiet())

	if _, err := svc.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got := counts(sk.calls, "note"); got != 1 {
		t.Fatalf("постов заметки %d, ждали 1", got)
	}
	if got := counts(sk.calls, "image"); got != 0 {
		t.Fatalf("картинка ушла без треда: %d", got)
	}

	// Тред появился — картинка доезжает, а пост не повторяется.
	ref := strconv.FormatInt(platform.NativeIDBase, 10)
	// Тред записывается ТРЕТЬИМ полем, а не идентификатором сообщения: так его
	// кладёт openThread, и так его читает lookupThread.
	if err := st.SetTarget(ctx, sk.Name(), store.TargetNoteThread, ref, "", "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got := counts(sk.calls, "note"); got != 1 {
		t.Errorf("заметка запощена %d раз — курсор не удержал повтор", got)
	}
	if got := counts(sk.calls, "image"); got != 1 {
		t.Errorf("картинок отправлено %d, ждали 1", got)
	}

	// И третий такт ничего не повторяет: отправленное записано в
	// message_targets, а не в память процесса.
	if _, err := svc.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got := counts(sk.calls, "image"); got != 1 {
		t.Errorf("картинка отправлена повторно: %d", got)
	}
}

// У заметки, которой в этом канале нет вовсе, треда не будет никогда — после
// срока ждать перестаём, иначе она держала бы курсор вечно.
func TestNoteImageGivesUpAfterGrace(t *testing.T) {
	st := newStore(t)
	src := &source{notes: []platform.OutNote{noteWithShot(time.Now().Add(-2 * threadGrace))}}
	sk := &sink{}
	svc := New(src, st, mediaWithFile(t), []mirror.Sink{sk}, "https://t3h.ru", quiet())

	if _, err := svc.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := counts(sk.calls, "image"); got != 0 {
		t.Fatalf("картинка ушла без треда: %d", got)
	}
	if svc.noteAt != platform.NativeIDBase {
		t.Fatalf("курсор на %d: заметку без треда ждать больше нечего", svc.noteAt)
	}
}

// Заметка без картинки ведёт себя ровно как прежде.
func TestNoteWithoutImageIsUnchanged(t *testing.T) {
	st := newStore(t)
	src := &source{notes: []platform.OutNote{{
		ID: platform.NativeIDBase, AuthorID: 1443311, AuthorNick: "Рио",
		Body: "без картинки", PublishedAt: time.Now(),
	}}}
	sk := &sink{}
	svc := New(src, st, mediaWithFile(t), []mirror.Sink{sk}, "https://t3h.ru", quiet())

	if _, err := svc.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := counts(sk.calls, "image"); got != 0 {
		t.Errorf("отправлено картинок: %d", got)
	}
	if svc.noteAt != platform.NativeIDBase {
		t.Errorf("курсор на %d", svc.noteAt)
	}
}

// Нет файла на диске — нести нечего, и ждать его неоткуда: хранилище наполняем
// мы сами. Заметка при этом уходит и курсор двигается.
func TestNoteImageWithoutFileIsSkipped(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	src := &source{notes: []platform.OutNote{noteWithShot(time.Now())}}
	sk := &threadSink{}
	svc := New(src, st, fakeMedia{path: filepath.Join(t.TempDir(), "нет-такого")},
		[]mirror.Sink{sk}, "https://t3h.ru", quiet())

	if _, err := svc.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got := counts(sk.calls, "image"); got != 0 {
		t.Errorf("отправлено картинок: %d", got)
	}
	if got := counts(sk.calls, "note"); got != 1 {
		t.Errorf("заметка не ушла: %d", got)
	}
	if svc.noteAt != platform.NativeIDBase {
		t.Errorf("курсор на %d", svc.noteAt)
	}
}
