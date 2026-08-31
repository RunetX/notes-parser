package narod

// ЧТО ДОЕЗЖАЕТ ДО КУБИКА В БОЮ.
//
// Все тесты здесь написаны по одному дефекту (31.08.2026, разбор первого
// смежного треда): у боевой службы DecisionPoint собирался неполным, и четыре
// ЗАМЕРЕННЫХ множителя молчали — накал, знакомство, отношение и темы заметки.
// Формулы при этом были верны и покрыты тестами (decide_test, relation_test), а
// калибровочный харнесс те же поля заполнял, — поэтому расхождение было
// молчаливым по устройству: одна формула на два мира, разные входные данные, и
// по поведению разницу увидеть нечем.
//
// Отсюда правило, которое эти тесты и исполняют: у замеренной величины обязан
// быть тест НА ПУТИ ДАННЫХ, а не только на формуле. Третий раз подряд один и тот
// же урок — пол собеседника (gender_test), теперь остальные четыре.
//
// Приём тот же, что в gender_test: корзины карточки доводятся до предела, чтобы
// вопрос «доехали ли данные» не пришлось решать статистикой.

import (
	"context"
	"testing"
	"time"
)

// wiringNote — песочница, одна на все тесты файла.
var wiringNote = StageNote{ID: 100000000030, AuthorID: 1, AuthorNick: "Паноптикум",
	AuthorGender: "male", Body: "у соседа собака лает всю ночь"}

// wiringCard — карточка, у которой рычаг из теста доводится до предела, а
// остальные молчат: так виден ровно тот множитель, про который тест.
func wiringCard(slug string) *Card {
	c := testCard()
	c.ID = slug
	c.Dice.MaxPerThread = 1000
	// Базовая вероятность отклика средняя: рычагу есть куда двигать в обе
	// стороны, и ни край, ни потолок MaxChance его не съедают.
	c.Rate = ReplyRate{Buckets: []RateBucket{{Upto: 1 << 30, Chances: 1000, Answers: 300}}}
	return &c
}

// spread — тред с заданным шагом между репликами. Шаг решает всё: накал это
// «сколько прилетело за окно ПЕРЕД этой репликой», и при шаге больше окна он
// всегда ноль.
func spread(n int, step time.Duration) []StageReply {
	t0 := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	out := make([]StageReply, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, StageReply{
			ID: int64(100 + i), NoteID: wiringNote.ID, AuthorID: 7,
			AuthorNick: "Собеседник", Gender: "female", Body: "реплика",
			PublishedAt: t0.Add(time.Duration(i) * step),
		})
	}
	return out
}

// spoke — сколько раз житель решил заговорить, пройдя тред один раз.
func spoke(t *testing.T, card *Card, thread []StageReply, setup func(*World)) int {
	t.Helper()
	ctx := context.Background()
	stage := &fakeStage{notes: []StageNote{wiringNote}, thread: thread}
	svc, w := testService(t, stage)
	svc.players = []Player{{Card: card, UserID: 42}}
	if err := w.UpsertActor(ctx, Actor{ID: card.ID, Kind: ActorPersona,
		PlatformUserID: 42, Nick: "Житель"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(w)
	}
	if err := svc.scanThread(ctx, wiringNote.ID); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range thread {
		d, err := w.DiceOf(ctx, card.ID, eventKey(eventReply, c.ID))
		if err != nil {
			continue // монетки на эту реплику не бросали вовсе
		}
		if d.Verdict == DiceCome {
			n++
		}
	}
	return n
}

// peerInWorld — анкета собеседника и ребро к нему: ровно то, что копится от
// треда к треду и чего боевая точка решения не спрашивала.
func peerInWorld(t *testing.T, w *World, src string, d EdgeDelta) {
	t.Helper()
	ctx := context.Background()
	if err := w.UpsertActor(ctx, Actor{ID: "peer", Kind: ActorPersona,
		PlatformUserID: 7, Nick: "Собеседник"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	d.Src, d.Dst = src, "peer"
	if _, err := w.Nudge(ctx, d, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// НАКАЛ ТРЕДА доезжает до кубика.
//
// Замер говорит, что горячий тред РАЗВОДИТ людей по парам: в чужой разговор
// влезают втрое реже. Здесь тот же перекос доведён до предела, а разница между
// двумя прогонами — только в ЧАСТОТЕ реплик: тот же тред, те же тексты, тот же
// собеседник. Без подачи Tempo оба прогона попадают в одну корзину и дают
// одинаковый результат — на прежнем коде тест падает.
func TestКубикВидитНакалТреда(t *testing.T) {
	tempo := []RateBucket{
		{Upto: 2, Chances: 1000, Answers: 990}, // тихо — говорим почти всегда
		{Upto: 1 << 30, Chances: 1000, Answers: 1},
	}
	hotCard := wiringCard("test-tempo")
	hotCard.Rate.Tempo = tempo
	coldCard := wiringCard("test-tempo")
	coldCard.Rate.Tempo = tempo

	// Шаг в двадцать раз меньше окна: к концу треда в окне лежит весь тред.
	// Шага «вдвое меньше окна» не хватает — в десятиминутное окно попадают две
	// реплики, то есть та же корзина, что и у тишины.
	hot := spoke(t, hotCard, spread(20, TempoWindow/20), nil)
	// Шаг вдвое БОЛЬШЕ окна: перед каждой репликой окно пусто.
	cold := spoke(t, coldCard, spread(20, 2*TempoWindow), nil)
	if hot >= cold {
		t.Errorf("накал мимо кубика: в горячем треде %d приходов, в тихом %d — "+
			"а корзина накала велит наоборот", hot, cold)
	}
}

// ЗНАКОМСТВО доезжает до кубика.
//
// Замер: знакомство поднимает готовность влезть в чужой разговор втрое-впятеро.
// Величина копится в графе мира (Edge.Familiarity), и до 31.08.2026 боевая
// служба её не подавала — то есть рычаг не «молчал нейтрально», а ГАСИЛ: без
// данных всегда бралась корзина «незнакомый».
func TestЗнакомствоДоезжаетДоКубика(t *testing.T) {
	familiar := []RateBucket{
		{Upto: 0, Chances: 1000, Answers: 1}, // незнакомому почти не отвечаем
		{Upto: 1 << 30, Chances: 1000, Answers: 990},
	}
	strangerCard := wiringCard("test-familiar")
	strangerCard.Rate.Familiar = familiar
	friendCard := wiringCard("test-familiar")
	friendCard.Rate.Familiar = familiar

	thread := spread(20, 2*TempoWindow)
	strangers := spoke(t, strangerCard, thread, nil)
	friends := spoke(t, friendCard, thread, func(w *World) {
		peerInWorld(t, w, "test-familiar", EdgeDelta{Familiarity: 20})
	})
	if friends <= strangers {
		t.Errorf("знакомство мимо кубика: знакомому %d приходов, незнакомому %d",
			friends, strangers)
	}
}

// ОТНОШЕНИЕ доезжает до кубика.
//
// relationLift развернули 29.08.2026 нарочно: до этого кубик при отрицательном
// тоне влезал в разговор неприятного человека РЕЖЕ, то есть гасил ссоры сам, и
// мир выходил рафинированным при любом промпте. Разворот в бою не работал ни
// разу — Tone не подавался, и рычаг возвращал единицу.
//
// Размах рычага заимствован у ЗАМЕРА знакомства (FamiliarSpan), поэтому без
// корзин знакомства он не даётся вовсе: это часть контракта, а не подготовка.
func TestОтношениеДоезжаетДоКубика(t *testing.T) {
	// Размах — это best/avg по корзинам знакомства, поэтому на двух корзинах он
	// не превышает двойки, а двойка тонет в разбросе двадцати бросков. Отсюда
	// шесть корзин: пять тихих и одна громкая дают размах в пять с половиной.
	familiar := []RateBucket{
		{Upto: 0, Chances: 1000, Answers: 20},
		{Upto: 2, Chances: 1000, Answers: 20},
		{Upto: 5, Chances: 1000, Answers: 20},
		{Upto: 10, Chances: 1000, Answers: 20},
		{Upto: 20, Chances: 1000, Answers: 20},
		{Upto: 1 << 30, Chances: 1000, Answers: 990},
	}
	card := func() *Card {
		c := wiringCard("test-tone")
		// Базовая вероятность высокая: знакомство у незнакомца гасит её в
		// девять раз, и без запаса рычагу отношения нечего было бы поднимать.
		c.Rate.Buckets = []RateBucket{{Upto: 1 << 30, Chances: 1000, Answers: 900}}
		c.Rate.Familiar = familiar
		return c
	}
	calmCard, angryCard := card(), card()

	thread := spread(20, 2*TempoWindow)
	calm := spoke(t, calmCard, thread, nil)
	hostile := spoke(t, angryCard, thread, func(w *World) {
		peerInWorld(t, w, "test-tone", EdgeDelta{Irritation: EdgeScale})
	})
	if hostile <= calm {
		t.Errorf("отношение мимо кубика: к неприятному %d приходов, к безразличному %d — "+
			"а рычаг развёрнут так, что вражда тянет сильнее", hostile, calm)
	}
}

// ТЕМЫ ЗАМЕТКИ доезжают до кубика.
//
// Перекос по темам — замер (archive.MineTopicLift): во сколько раз чаще человек
// заходил в заметку на эту тему, чем такие заметки встречались в его время. Без
// него кубик про заметку не знает НИЧЕГО, кроме того, что она новая, и «кто
// придёт именно сюда» выходит жребием. Разбор живёт в архиве, ядру народа архив
// недоступен, поэтому подаётся он замыканием — и вот оно-то и проверяется.
func TestТемыЗаметкиДоезжаютДоКубика(t *testing.T) {
	ctx := context.Background()
	come := func(topics func(string) []string) bool {
		card := wiringCard("test-topics")
		// Базовая готовность зайти — один процент, перекос по теме — стократный.
		card.Come = ComeRate{Days: 100, Chances: 1000, Came: 10,
			LiveChances: 1000, LiveCame: 10}
		card.Triggers = []Topic{{Key: "dogs", Name: "собаки", Lift: 100}}
		svc, w := testService(t, &fakeStage{notes: []StageNote{wiringNote}})
		svc.players = []Player{{Card: card, UserID: 42}}
		if topics != nil {
			svc.SetTopics(topics)
		}
		if err := w.UpsertActor(ctx, Actor{ID: card.ID, Kind: ActorPersona,
			PlatformUserID: 42, Nick: "Житель"}, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := svc.scanNotes(ctx); err != nil {
			t.Fatal(err)
		}
		d, err := w.DiceOf(ctx, card.ID, eventKey(eventNote, wiringNote.ID))
		if err != nil {
			t.Fatalf("монетку на заметку не бросали вовсе: %v", err)
		}
		return d.Verdict == DiceCome
	}
	if !come(func(string) []string { return []string{"dogs"} }) {
		t.Error("тема заметки мимо кубика: житель не пришёл в заметку про своё")
	}
	if come(nil) {
		t.Error("приход случился и без разбора тем — значит рычаг проверить нечем, " +
			"базовая вероятность слишком высока")
	}
}

// ОКНО НАКАЛА одно на бой и на калибровку — своего числа здесь не заводится.
// Равенство его замеру (archive.TempoWindow) держит тест в narodsim: только тот
// пакет видит оба мира.
func TestОкноНакалаИСчётНакала(t *testing.T) {
	if TempoWindow != 10*time.Minute {
		t.Errorf("окно накала %v, а замер снят по десятиминутному", TempoWindow)
	}
	// Перед пятой репликой в окне лежат четыре предыдущие.
	if got := tempoAt(spread(6, time.Minute), 4, TempoWindow); got != 4 {
		t.Errorf("накал перед пятой репликой %d, ожидалось 4", got)
	}
	// В разреженном треде окно пусто.
	if got := tempoAt(spread(6, 2*TempoWindow), 4, TempoWindow); got != 0 {
		t.Errorf("в разреженном треде накал %d, ожидался ноль", got)
	}
	// Первая реплика треда накала не имеет по определению.
	if got := tempoAt(spread(6, time.Minute), 0, TempoWindow); got != 0 {
		t.Errorf("у первой реплики накал %d, ожидался ноль", got)
	}
}

// ---------------------------------------------------- монетка и потолок

// МОНЕТКА НЕ СГОРАЕТ НА ПОТОЛКЕ.
//
// Бросок ключуется СОБЫТИЕМ и делается ровно один раз — иначе пятнадцать
// процентов, спрошенные десять раз за десять тактов, превращаются в восемьдесят.
// Но потолок до 31.08.2026 спрашивался только на ИСПОЛНЕНИИ плана, и связка
// выходила такая: житель бросил монетку, выиграл, завёл намерение — а на
// исполнении упёрся в потолок, намерение убили, и второй раз эта чужая реплика
// ему не выпадет НИКОГДА. Терял на этом больше всех разговорчивый: у живых 40 %
// корней — повторный заход, а повторно заходит тот, кто уже наговорил.
//
// Проверяется поведением, а не устройством: наговорившийся житель проходит тред
// молча и НЕ оставляет бросков; освободилось окно — те же самые реплики
// достаются ему целыми. На прежнем коде тест падает первой же проверкой.
func TestПотолокНеСжигаетМонетку(t *testing.T) {
	ctx := context.Background()
	card := wiringCard("full")
	// Кубик у него почти всегда «прийти»: без этого «бросков нет» ничего не
	// доказывало бы — их могло не быть и по невезению.
	card.Rate = ReplyRate{Buckets: []RateBucket{{Upto: 1 << 30, Chances: 100, Answers: 99}}}
	thread := spread(6, time.Minute)

	stage := &fakeStage{notes: []StageNote{wiringNote}, thread: thread}
	svc, w := testService(t, stage)
	svc.cfg.PerPersonaHour = 3
	svc.players = []Player{{Card: card, UserID: 42}}
	if err := w.UpsertActor(ctx, Actor{ID: card.ID, Kind: ActorPersona,
		PlatformUserID: 42, Nick: "Житель"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Он уже сказал столько, сколько ему положено в час.
	for i := 0; i < 3; i++ {
		if _, err := w.Remember(ctx, JournalEntry{
			ActorID: card.ID, At: svc.clock.Now(), Kind: JournalComment,
			NoteID: wiringNote.ID, Text: "сказанное раньше",
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := svc.scanThread(ctx, wiringNote.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range thread {
		if _, err := w.DiceOf(ctx, card.ID, eventKey(eventReply, c.ID)); err == nil {
			t.Fatalf("наговорившийся житель потратил монетку на реплику %d", c.ID)
		}
	}

	// Окно освободилось — те же реплики достаются ему целыми, а не сгоревшими.
	if _, err := w.db.ExecContext(ctx, `DELETE FROM journal WHERE actor_id = ?`, card.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.scanThread(ctx, wiringNote.ID); err != nil {
		t.Fatal(err)
	}
	rolled := 0
	for _, c := range thread {
		if _, err := w.DiceOf(ctx, card.ID, eventKey(eventReply, c.ID)); err == nil {
			rolled++
		}
	}
	if rolled != len(thread) {
		t.Errorf("после освобождения окна брошено %d монеток из %d — часть возможностей сгорела",
			rolled, len(thread))
	}
}

// В ПОТОЛОК СЧИТАЮТСЯ И НАМЕРЕНИЯ, а не только сказанное.
//
// Между монеткой и репликой лежит замеренная задержка в минуты, и всё это время
// житель для журнала молчит. Считай потолок по одному журналу — и в людном
// треде житель наберёт десяток намерений при потолке в три: шесть станут
// репликами, остальные умрут на исполнении, потратив монетки. Здесь журнал пуст
// вовсе, а намерения уже есть, и правильный ответ — молчать.
func TestНамеренияСчитаютсяВПотолок(t *testing.T) {
	ctx := context.Background()
	card := wiringCard("planner")
	card.Rate = ReplyRate{Buckets: []RateBucket{{Upto: 1 << 30, Chances: 100, Answers: 99}}}
	thread := spread(4, time.Minute)

	stage := &fakeStage{notes: []StageNote{wiringNote}, thread: thread}
	svc, w := testService(t, stage)
	svc.cfg.PerPersonaHour = 2
	svc.players = []Player{{Card: card, UserID: 42}}
	if err := w.UpsertActor(ctx, Actor{ID: card.ID, Kind: ActorPersona,
		PlatformUserID: 42, Nick: "Житель"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Ни одной реплики в журнале — только намерения, ещё не исполненные.
	for i := 0; i < 2; i++ {
		if _, _, err := w.PlanReply(ctx, Plan{
			ActorID: card.ID, EventID: eventKey(eventReply, int64(900+i)),
			NoteID: wiringNote.ID, DueAt: svc.clock.Now().Add(time.Hour),
		}, svc.clock.Now()); err != nil {
			t.Fatal(err)
		}
	}

	if err := svc.scanThread(ctx, wiringNote.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range thread {
		if _, err := w.DiceOf(ctx, card.ID, eventKey(eventReply, c.ID)); err == nil {
			t.Fatalf("житель с полным набором намерений всё равно бросил монетку на %d", c.ID)
		}
	}
}
