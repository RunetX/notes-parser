package platform

// Звонок живого канала против настоящего Postgres.
//
// Подделкой это не проверить в принципе: всё содержание правки — в поведении
// самого Postgres. Уведомление доставляется ПОСЛЕ фиксации транзакции (а не в
// момент NOTIFY), одинаковые внутри одной транзакции схлопываются, откат
// отменяет звонок вместе с записью. Заглушка подтвердила бы только то, что мы
// вызвали функцию.

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ringWait — сколько тест ждёт звонка. Секунда: обещание брифа — 300 мс на
// локальных событиях, и запас втрое отделяет поломку от медленной машины.
const ringWait = time.Second

// listenFor подписывается и возвращает канал, в который падают звонки. Это
// «второй клиент» из приёмки: он ничего не пишет и узнаёт о чужой публикации
// только звонком.
func listenFor(t *testing.T, p *Platform) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rings := make(chan struct{}, 16)
	up := make(chan struct{})
	var once sync.Once
	go func() {
		_ = p.ListenLive(ctx, func() { once.Do(func() { close(up) }) }, func() {
			select {
			case rings <- struct{}{}:
			default:
			}
		})
	}()
	select {
	case <-up:
	case <-time.After(ringWait):
		t.Fatal("подписка на живой канал не встала")
	}
	return rings
}

func expectRing(t *testing.T, rings <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-rings:
	case <-time.After(ringWait):
		t.Fatalf("%s: звонок не пришёл за %s — страница узнала бы об этом только тактом", what, ringWait)
	}
}

func expectSilence(t *testing.T, rings <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-rings:
		t.Fatalf("%s: пришёл звонок, которого быть не должно", what)
	case <-time.After(200 * time.Millisecond):
	}
}

// Приёмка брифа: один пишет реплику, другой узнаёт о ней НЕ ДОЖИДАЯСЬ такта.
// Ждём меньше секунды — такт-страховка втрое дольше, так что дойти сигнал мог
// только звонком.
func TestLiveRingOnNativeComment(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Паноптикум")
	noteID := mustNote(t, p, author, "тред")
	other := mustUser(t, p, "Пуша")

	// Подписываемся ПОСЛЕ заметки: она звонит и сама, а проверять надо звонок
	// реплики — то самое, чего ждёт человек, глядя в открытый тред.
	rings := listenFor(t, p)
	commentID := say(t, p, noteID, other, 0, "а вот и реплика")
	expectRing(t, rings, "реплика")

	// Звонок — это ЗВОНОК, а не данные: пришедший по нему хаб читает журнал
	// своим курсором и получает ровно тот факт, который и был записан.
	list, err := p.LiveSince(ctx, 0, 100)
	if err != nil {
		t.Fatalf("живая лента: %v", err)
	}
	var found bool
	for _, e := range list {
		if e.Kind == EventComment && e.CommentID == commentID {
			found = true
			if e.At.IsZero() {
				t.Error("у факта нет времени — замер задержки мерить нечем")
			}
		}
	}
	if !found {
		t.Fatalf("реплика %d не попала в живую ленту", commentID)
	}
}

// Раздача поводов звонит отдельно, и это не дубль: повод появляется позже
// факта, а колокольчику нужен именно он.
func TestLiveRingOnFanOut(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Паноптикум")
	noteID := mustNote(t, p, author, "тред")
	other := mustUser(t, p, "Пуша")
	say(t, p, noteID, other, 0, "ответ автору заметки")

	// Подписываемся ПОСЛЕ записи: так звонок может прийти только от раздачи.
	rings := listenFor(t, p)
	n, err := p.FanOut(ctx, 100)
	if err != nil {
		t.Fatalf("раздача поводов: %v", err)
	}
	if n == 0 {
		t.Fatal("раздавать было нечего — тест ничего не проверяет")
	}
	expectRing(t, rings, "раздача поводов")

	pokes, err := p.PokesSince(ctx, 0, 100)
	if err != nil {
		t.Fatalf("поводы: %v", err)
	}
	if len(pokes) == 0 {
		t.Fatal("поводов нет вовсе")
	}
	if pokes[0].At.IsZero() {
		t.Error("у повода нет времени факта — замер задержки мерить нечем")
	}
}

// Откат отменяет звонок вместе с записью. Это и есть причина, по которой NOTIFY
// стоит ТОЙ ЖЕ транзакцией: звонок о факте, которого не случилось, послал бы
// открытые страницы читать пустоту.
func TestLiveRingRollsBackWithTransaction(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	rings := listenFor(t, p)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("транзакция: %v", err)
	}
	ringLive(ctx, tx)
	expectSilence(t, rings, "до фиксации")
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("откат: %v", err)
	}
	expectSilence(t, rings, "после отката")
}

// ПЕРЕЕЗД ВЕТКИ ЗВОНИТ. Оплачено жалобой владельца 24.08.2026: обход мобильного
// дерева переставил ребро, в базе стало правильно, а на открытой странице ветка
// осталась там, куда её поставила догадка зеркала, — и увиделось это только
// перезагрузкой. Отметка moved_at и заголовок X-Fresh-Moved были на месте;
// не было повода за ними сходить, потому что сигналы рождались только из events,
// а переезд событием не является и становиться им не должен.
func TestLiveRingOnMovedBranch(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 313058, 175869, "Паноптикум")
	ingestComment(t, p, 63238869, 313058, 1, 0)
	ingestComment(t, p, 63238877, 313058, 1, 63238869)
	ingestComment(t, p, 63238879, 313058, 2, 63238877) // адресат УГАДАН

	rings := listenFor(t, p)
	// На самом деле отвечали корню треда.
	st, err := p.ApplyReplyTree(ctx, 313058, map[int64]int64{
		63238869: 0, 63238877: 63238869, 63238879: 63238869})
	if err != nil {
		t.Fatalf("дерево: %v", err)
	}
	if st.Edges != 1 {
		t.Fatalf("переставлено рёбер %d, ожидалось 1 — тест ничего не проверяет", st.Edges)
	}
	expectRing(t, rings, "переезд ветки")

	// И сигнал этот заметке адресован: хаб спрашивает про открытые треды.
	moved, err := p.MovedNotesSince(ctx, []int64{313058}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("переезды: %v", err)
	}
	if _, ok := moved[313058]; !ok {
		t.Fatal("переезд не виден через MovedNotesSince — сигнал некуда адресовать")
	}
}

// Проход, который ничего не сдвинул, молчит. Иначе обход ста заметок подряд
// (командной очередью добора истории) поднимал бы хаб на каждой из них зря.
func TestLiveSilentWhenNothingMoved(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 313058, 175869, "Паноптикум")
	ingestComment(t, p, 63238869, 313058, 1, 0)
	ingestComment(t, p, 63238877, 313058, 1, 63238869)
	tree := map[int64]int64{63238869: 0, 63238877: 63238869}
	if _, err := p.ApplyReplyTree(ctx, 313058, tree); err != nil {
		t.Fatalf("дерево: %v", err)
	}

	// Второй проход тем же деревом двигать уже нечего.
	rings := listenFor(t, p)
	st, err := p.ApplyReplyTree(ctx, 313058, tree)
	if err != nil {
		t.Fatalf("дерево: %v", err)
	}
	if st.Edges != 0 {
		t.Fatalf("второй проход переставил %d рёбер — заготовка не та", st.Edges)
	}
	expectSilence(t, rings, "проход без переездов")
}

// Про чужую заметку не спрашивают, а спросив — не получают: сигнал переезда
// адресный, и разослать его всем значило бы гнать на /fresh все открытые треды
// разом при каждом обходе.
func TestMovedNotesSinceIsScopedToAsked(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 313058, 175869, "Паноптикум")
	ingestComment(t, p, 63238869, 313058, 1, 0)
	ingestComment(t, p, 63238877, 313058, 1, 63238869)
	ingestComment(t, p, 63238879, 313058, 2, 63238877)
	if _, err := p.ApplyReplyTree(ctx, 313058, map[int64]int64{
		63238869: 0, 63238877: 63238869, 63238879: 63238869}); err != nil {
		t.Fatalf("дерево: %v", err)
	}

	moved, err := p.MovedNotesSince(ctx, []int64{312811}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("переезды: %v", err)
	}
	if len(moved) != 0 {
		t.Fatalf("спросили про 312811, а ответили про %v", moved)
	}
	if moved, err := p.MovedNotesSince(ctx, nil, time.Now().Add(-time.Hour)); err != nil || len(moved) != 0 {
		t.Fatalf("пустой список дал %v, %v — хаб без слушателей обязан молчать", moved, err)
	}
}

// Подписка НЕ отбирает строку у пула. Требование брифа и заодно самое дорогое
// место всей затеи: у морды четыре соединения на всю площадку, и пятая часть
// пула, ушедшая в вечное ожидание, отбирается у страниц, а через них — у
// зеркала на том же ядре.
//
// Проверяется делом, а не чтением кода: держим подписку и ЗАНИМАЕМ пул целиком.
// Возьмись подписка изнутри — четвёртая строка не выдалась бы.
func TestLiveListenStaysOutOfPool(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	// Пул именно веб-морды: у неё соединений меньше всех, и считать надо по ней.
	web, err := OpenWith(ctx, os.Getenv("LOVEGW_TEST_PG_DSN"), WebOpts())
	if err != nil {
		t.Fatalf("пул морды: %v", err)
	}
	defer web.Close()

	_ = listenFor(t, web)

	take, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var held []*pgxpool.Conn
	for i := range int(WebOpts().MaxConns) {
		c, err := web.pool.Acquire(take)
		if err != nil {
			t.Fatalf("подписка съела строку пула: соединение %d из %d не выдалось: %v",
				i+1, WebOpts().MaxConns, err)
		}
		held = append(held, c)
	}
	for _, c := range held {
		c.Release()
	}

	// И звонки при этом ходят: подписка жива, а не «не мешает, потому что
	// отвалилась».
	rings := listenFor(t, web)
	if _, err := p.pool.Exec(ctx, `SELECT pg_notify($1, '')`, LiveChannel); err != nil {
		t.Fatalf("звонок: %v", err)
	}
	expectRing(t, rings, "звонок при занятом пуле")
}

// Пачка одинаковых звонков в одной транзакции — один звонок: Postgres схлопывает
// их сам. Проверяется не ради экономии, а ради обещания «стоимость канала не
// растёт вместе с потоком»: перенос архива пишет события тысячами.
func TestLiveRingCollapsesWithinTransaction(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	rings := listenFor(t, p)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("транзакция: %v", err)
	}
	for range 50 {
		ringLive(ctx, tx)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("фиксация: %v", err)
	}
	expectRing(t, rings, "пачка звонков")
	expectSilence(t, rings, "остаток пачки")
}
