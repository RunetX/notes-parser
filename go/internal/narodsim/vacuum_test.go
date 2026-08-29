package narodsim

import (
	"context"
	"fmt"
	"testing"
	"time"

	"lovegw/internal/archive"
	"lovegw/internal/narod"
)

// vacCast — сцена из n жителей с одной и той же карточкой и своими зёрнами.
// Зёрна разные обязательно: с одним на всех жители бросают одну монетку и
// приходят строем, а это ровно то, что прогон и должен уметь показать.
func vacCast(n int, card narod.Card, seed uint64) []VacuumActor {
	out := make([]VacuumActor, 0, n)
	for i := range n {
		id := int64(100 + i)
		out = append(out, VacuumActor{
			UserID: id, Nick: fmt.Sprintf("житель%d", i),
			Decider: &CardDecider{Card: card, Seed: seed ^ uint64(i)*0x9E3779B1},
		})
	}
	return out
}

// Разговор растёт из одной заметки: чужих реплик в него не подаётся вовсе.
func TestVacuumGrowsFromNoteAlone(t *testing.T) {
	sc := soloScript()
	run, err := RunVacuum(context.Background(), sc, VacuumOpts{
		Actors:     vacCast(3, testCard(), 1),
		MaxReplies: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range run.Thread.Comments {
		for _, orig := range sc.Comments {
			if c.ID == orig.ID {
				t.Fatalf("в вакуум просочилась реплика оригинала %d", orig.ID)
			}
		}
		if c.AuthorID < 100 || c.AuthorID > 102 {
			t.Fatalf("реплика %d от постороннего %d", c.ID, c.AuthorID)
		}
	}
	// Оригинал остался тем, чем был: вакуум его читает, а не правит.
	if run.OrigReplies != len(sc.Comments) {
		t.Errorf("оригинал %d реплик, посчитано %d", len(sc.Comments), run.OrigReplies)
	}
	if run.Stopped == "" {
		t.Error("прогон не назвал, почему кончился")
	}
}

// Каждая сказанная реплика — точка решения ВСЕМ остальным, и никогда самому
// сказавшему.
func TestVacuumCascadeSkipsSpeaker(t *testing.T) {
	sc := soloScript()
	seen := map[int64][]int64{}
	actors := []VacuumActor{
		{UserID: 100, Nick: "а", Decider: &recDecider{id: 100, seen: seen, speakOn: 0}},
		{UserID: 101, Nick: "б", Decider: &recDecider{id: 101, seen: seen, speakOn: -1}},
	}
	run, err := RunVacuum(context.Background(), sc, VacuumOpts{Actors: actors, MaxReplies: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Thread.Comments) != 1 {
		t.Fatalf("реплик вышло %d, ожидалась одна", len(run.Thread.Comments))
	}
	first := run.Thread.Comments[0]
	for _, trig := range seen[100] {
		if trig == first.ID {
			t.Error("житель получил точку решения на собственную реплику")
		}
	}
	if len(seen[101]) != 2 { // заметка и чужая реплика
		t.Errorf("сосед решал %d раз, ожидалось два: %v", len(seen[101]), seen[101])
	}
}

// recDecider говорит один раз — на названном триггере — и запоминает, о чём его
// спрашивали.
type recDecider struct {
	id      int64
	seen    map[int64][]int64
	speakOn int64
	done    bool
}

func (d *recDecider) Decide(_ context.Context, p DecisionPoint) (Decision, error) {
	d.seen[d.id] = append(d.seen[d.id], p.TriggerID)
	if d.done || p.TriggerID != d.speakOn {
		return Decision{}, nil
	}
	d.done = true
	return Decision{Speak: true, After: time.Minute}, nil
}

// Потолок реплик на тред обязан считать НАМЕРЕННОЕ вместе со сказанным. Иначе
// он не ограничивает ничего: житель успевает наметить десяток ответов раньше,
// чем произнесёт первый, и все десять проходят проверку «сколько я уже сказал».
func TestVacuumCapCountsIntent(t *testing.T) {
	card := testCard()
	card.Dice = narod.DiceParams{ComeToNote: 1, ReplyMention: 1, ReplyOther: 1, MaxPerThread: 2}
	// Задержка длинная, чтобы намеченное копилось: пока житель молчит, каскад
	// успевает предложить ему ещё несколько поводов.
	card.Latency.ToThreadSec = narod.Dist{P10: 3600, Median: 3600, P90: 3600, Max: 3600}
	card.Latency.ToReplySec = narod.Dist{P10: 3600, Median: 3600, P90: 3600, Max: 3600}

	run, err := RunVacuum(context.Background(), sc0(), VacuumOpts{
		Actors: vacCast(3, card, 5), MaxReplies: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	for id, n := range run.Got.ByActor {
		if n > card.Dice.MaxPerThread {
			t.Errorf("житель %d сказал %d раз при потолке %d", id, n, card.Dice.MaxPerThread)
		}
	}
	if run.Got.Replies == 0 {
		t.Fatal("никто не заговорил — потолок проверять не на чем")
	}
}

// Оригинал сравнивается СУЖЕННЫМ до состава: чужие реплики не считаются ни
// числом, ни парами.
func TestVacuumShapeRestrictsToCast(t *testing.T) {
	t0 := time.Date(2016, 5, 12, 9, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	// Тред, где рядом с составом говорит посторонний: 2 и 9 в составе, 3 — нет.
	cs := []archive.ScriptComment{
		{ID: 1, AuthorID: 2, PublishedAt: at(1), ReplyTo: 0, TargetID: 1},
		{ID: 2, AuthorID: 9, PublishedAt: at(2), ReplyTo: 1, TargetID: 2},
		{ID: 3, AuthorID: 3, PublishedAt: at(3), ReplyTo: 2, TargetID: 9},
		{ID: 4, AuthorID: 9, PublishedAt: at(5), ReplyTo: 3, TargetID: 3},
		{ID: 5, AuthorID: 2, PublishedAt: at(6), ReplyTo: 4, TargetID: 9},
	}
	sh := shapeOf(cs, map[int64]bool{9: true, 2: true}, t0)
	if sh.Replies != 4 { // всё, кроме реплики постороннего
		t.Errorf("реплик состава %d, ожидалось 4", sh.Replies)
	}
	if len(sh.Spoke) != 2 {
		t.Errorf("заговорили %v, ожидались двое", sh.Spoke)
	}
	// Пары только внутри состава: ответ автору заметки и ответ постороннему не в
	// счёт — этих людей на сцене нет.
	if want := []string{"2>9", "9>2"}; fmt.Sprint(sh.Pairs) != fmt.Sprint(want) {
		t.Errorf("пары %v, ожидались %v", sh.Pairs, want)
	}
	// Цепочка, вышедшая за состав, начинается заново: 1→2 даёт глубину два, а
	// реплика 4 отвечала постороннему и потому снова первая.
	if sh.Depth != 2 {
		t.Errorf("глубина %d, ожидалась 2", sh.Depth)
	}
	if sh.SpanSec != 360 {
		t.Errorf("разговор шёл %d с, ожидалось 360", sh.SpanSec)
	}
}

// measuredCard — карточка с ЗАМЕРЕННЫМИ числами, а не с выдуманными.
//
// Обязательный тест про шторм нельзя ставить на testCard: у той задержка прихода
// в тред медианой ровно десять минут — то есть по построению половина приходов
// попадает внутрь окна, которое тест и проверяет. Числа здесь сняты с живого
// слепка ДВ (u498196, 28.08.2026): в тред человек приходит медианой через 43
// минуты, десятая доля быстрее восьми, а отвечает — медианой через шесть.
// Отклик тоже замерный: около процента на чужой разговор и половина на прямое
// обращение — то самое, что оказалось в 20–40 раз ниже придуманного.
func measuredCard() narod.Card {
	c := testCard()
	c.Latency = narod.LatencyDist{
		ToThreadSec: narod.Dist{P10: 508, Median: 2587, P90: 10882, Max: 60783},
		ToReplySec:  narod.Dist{P10: 116, Median: 385, P90: 2033, Max: 42852},
	}
	c.Dice.MaxPerThread = 11
	c.Rate = narod.ReplyRate{
		Threads: 300,
		Buckets: []narod.RateBucket{{
			Upto: 1 << 30, Chances: 10000, Answers: 100,
			ToHimChances: 1000, ToHimAnswers: 500,
		}},
	}
	return c
}

// Обязательный признак провала из брифа: разговор, целиком уместившийся в
// десять минут, выдаёт машину раньше любого текста. Проверяется он не на одном
// прогоне, а на пяти сотнях — единичный быстрый тред законен и у живых.
func TestVacuumIsNotABurst(t *testing.T) {
	const runs = 500
	burst, withSilent, alive := 0, 0, 0
	for seed := range uint64(runs) {
		run, err := RunVacuum(context.Background(), sc0(), VacuumOpts{
			Actors: vacCast(10, measuredCard(), seed+1), MaxReplies: 60,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(run.Got.Spoke) < 10 {
			withSilent++
		}
		if run.Got.Replies < 2 {
			continue
		}
		alive++
		if run.Got.BurstOnly() {
			burst++
		}
	}
	if alive == 0 {
		t.Fatal("ни один прогон не дал разговора — мерить нечего")
	}
	t.Logf("разговоров %d из %d, штормом %d, с молчуном %d", alive, runs, burst, withSilent)
	if share := float64(burst) / float64(alive); share >= 0.01 {
		t.Errorf("разговор уложился в %s в %.1f %% прогонов (%d из %d) — жители сбегаются строем",
			vacuumBurst, 100*share, burst, alive)
	}
	if withSilent <= runs/2 {
		t.Errorf("молчун нашёлся лишь в %d прогонах из %d — приходят все и всегда", withSilent, runs)
	}
}

// Разговор ЗАВОДИТСЯ ОТ ЧИСЛА ЖИТЕЛЕЙ, и растёт он быстрее, чем их становится
// больше. Замер 28.08.2026 на замеренных вероятностях (200 прогонов на размер):
//
//	жителей  3: реплик на заметку 0,8, с ответом друг другу  1 %, пустых 38 %
//	жителей  5: реплик на заметку 1,5, с ответом друг другу  5 %, пустых 17 %
//	жителей  8: реплик на заметку 2,8, с ответом друг другу 17 %, пустых  3 %
//	жителей 12: реплик на заметку 4,3, с ответом друг другу 33 %, пустых  0 %
//	жителей 16: реплик на заметку 6,3, с ответом друг другу 46 %, пустых  0 %
//
// Причина арифметическая и следует прямо из замера отклика: влезть в ЧУЖОЙ
// разговор человек готов в доле процента случаев, а полпроцента с двух соседей —
// это тишина. Настоящая разговорчивость донора в архиве держалась не на его
// готовности, а на людности треда: в оригинале рядом с ним говорили тридцать
// человек, и один процент от тридцати это уже разговор.
//
// Отсюда следствие для эпика, которое дороже самого числа: тройка жителей не
// «маленькая версия» сообщества, а другое явление — у неё 38 % заметок остаются
// без единой реплики, и разговор между собой случается раз на сотню. Десять
// персонажей в брифе перестали быть догадкой.
func TestVacuumConversationNeedsACrowd(t *testing.T) {
	talk := func(n int) int {
		const runs = 200
		chatty := 0
		for seed := range uint64(runs) {
			run, err := RunVacuum(context.Background(), sc0(), VacuumOpts{
				Actors: vacCast(n, measuredCard(), seed+1), MaxReplies: 200,
			})
			if err != nil {
				t.Fatal(err)
			}
			if run.Got.Depth >= 2 { // кто-то ответил не заметке, а человеку
				chatty++
			}
		}
		return chatty
	}
	small, big := talk(3), talk(12)
	t.Logf("разговоров между жителями: на тройке %d из 200, на дюжине %d из 200", small, big)
	if big <= small*3 {
		t.Errorf("дюжина жителей разговорилась не заметно лучше тройки (%d против %d) — "+
			"каскад не работает", big, small)
	}
}

// Модель зовут ровно на реплики жителей, и её отказ не роняет разговор: реплика
// остаётся на своём месте без слов, а отчёт называет, сколько таких.
func TestVacuumSpeakerFillsBodies(t *testing.T) {
	sp := &fakeSpeaker{}
	// Приходят все и наверняка: тест про ПОТОЛОК обращений к модели, и зависеть
	// от того, повезло ли зерну, он не должен.
	card := testCard()
	card.Dice.ComeToNote = 1
	actors := vacCast(3, card, 3)
	for i := range actors {
		actors[i].Speaker = sp
	}
	run, err := RunVacuum(context.Background(), sc0(), VacuumOpts{
		Actors: actors, MaxReplies: 10, MaxSpeak: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Speeches != 2 {
		t.Fatalf("модель дала %d реплик при потолке 2", run.Speeches)
	}
	if run.Speeches+run.Skipped != len(run.Thread.Comments) {
		t.Errorf("реплик %d, из них с текстом %d и пропущено %d — не сходится",
			len(run.Thread.Comments), run.Speeches, run.Skipped)
	}
	for i, c := range run.Thread.Comments {
		if i < 2 && c.Text == "" {
			t.Errorf("реплика %d осталась без текста, хотя потолок не выбран", c.ID)
		}
	}
}

// Жаккар: пустое против пустого — это совпадение, а не расхождение. Два
// разговора, в которых из состава не заговорил никто, сошлись полностью.
func TestJaccardEmptyAgrees(t *testing.T) {
	if got := jaccardStr(nil, nil); got != 1 {
		t.Errorf("пустое против пустого дало %v", got)
	}
	if got := jaccardStr([]string{"a"}, nil); got != 0 {
		t.Errorf("одно против пустого дало %v", got)
	}
	if got := jaccardStr([]string{"a", "b"}, []string{"b", "c"}); got != 1.0/3 {
		t.Errorf("«a,b» против «b,c» дало %v, ожидалась треть", got)
	}
}

// sc0 — заметка без единой реплики: вакууму больше ничего и не нужно.
func sc0() *archive.ThreadScript {
	sc := soloScript()
	sc.Comments = nil
	return sc
}

// Размер треда решает ЧИСЛО ЖИТЕЛЕЙ, а не качество кубика, и мерка этому —
// «во что разрастается реплика»: сколько всего сказано на одну реплику,
// пришедшую в саму заметку.
//
// Тест закрепляет обе половины замера 29.08.2026. Первая — арифметика: корни
// считаются отдельно от ответов, иначе величина не считается вовсе. Вторая —
// сам вывод: пока эта величина меньше единицы, тред затухает за два-три круга
// при любом содержании реплик, и объяснять его размер промптом, потолками или
// вероятностями бессмысленно. У десяти конфликтных заметок оригинала она
// вышла 0.82, у наших двенадцати жителей — 0.52.
func TestBranchingCountsRootsApart(t *testing.T) {
	t0 := time.Date(2016, 5, 12, 9, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	// Трое пришли в заметку, двое ответили друг другу: пять реплик на три корня.
	cs := []archive.ScriptComment{
		{ID: 1, AuthorID: 2, PublishedAt: at(1), ReplyTo: 0, TargetID: 1},
		{ID: 2, AuthorID: 9, PublishedAt: at(2), ReplyTo: 0, TargetID: 1},
		{ID: 3, AuthorID: 7, PublishedAt: at(3), ReplyTo: 0, TargetID: 1},
		{ID: 4, AuthorID: 9, PublishedAt: at(4), ReplyTo: 1, TargetID: 2},
		{ID: 5, AuthorID: 2, PublishedAt: at(5), ReplyTo: 4, TargetID: 9},
	}
	sh := shapeOf(cs, map[int64]bool{2: true, 9: true, 7: true}, t0)
	if sh.Roots != 3 {
		t.Errorf("корней %d, ожидалось 3", sh.Roots)
	}
	if got := branching(sh.Replies, sh.Roots); got < 0.39 || got > 0.41 {
		t.Errorf("разрастание %.2f, ожидалось 0.40 (5 реплик на 3 корня)", got)
	}
	// Тред, куда все только пришли и никто никому не ответил, не разрастается
	// вовсе — и это ноль, а не «мало данных».
	flat := cs[:3]
	sh = shapeOf(flat, map[int64]bool{2: true, 9: true, 7: true}, t0)
	if got := branching(sh.Replies, sh.Roots); got != 0 {
		t.Errorf("разрастание пустого треда %.2f, ожидался ноль", got)
	}
}

// Тишина убивает тред, и правило это ЗАМЕРЕНО, а не поставлено от руки:
// `archive.MineDecay` по 2981 треду говорит, что после часа молчания разговор
// продолжается в половине случаев, после трёх — в четверти, после двенадцати —
// в пяти процентах.
//
// Тест закрепляет три вещи. Первая — ступенька: между корзинами величина не
// интерполируется, берётся последняя пройденная. Вторая — короткая тишина не
// убивает вовсе (там живёт обычный ход беседы). Третья и главная — без кривой
// (Hazard == nil) тред не умирает от тишины совсем, и это законный режим для
// харнесса без archive.db, но НЕ для калибровки: замер 29.08.2026 показал, что
// без неё девять десятых реплик набирались к 11,3 ч при настоящих 1,7–7,1 —
// реплика с хвоста распределения задержек воскрешала затихший тред.
func TestHazardIsAStaircase(t *testing.T) {
	hz := []archive.DecayHazard{
		{SilenceSec: 720, P: 0.756},
		{SilenceSec: 3600, P: 0.502},
		{SilenceSec: 10800, P: 0.256},
	}
	cases := []struct {
		silence time.Duration
		want    float64
	}{
		{time.Minute, 1},          // короче первой корзины — не убивает
		{11 * time.Minute, 1},     // всё ещё короче
		{12 * time.Minute, 0.756}, // ровно на пороге — корзина уже действует
		{40 * time.Minute, 0.756}, // между корзинами берётся ПРОЙДЕННАЯ
		{2 * time.Hour, 0.502},    //
		{50 * time.Hour, 0.256},   // за последней корзиной — она же
	}
	for _, c := range cases {
		if got := hazardAt(hz, c.silence); got != c.want {
			t.Errorf("тишина %s: %.3f, ожидалось %.3f", c.silence, got, c.want)
		}
	}
	if got := hazardAt(nil, time.Hour); got != 1 {
		t.Errorf("без замера тред от тишины не умирает: %.3f", got)
	}
}

// Тред, затихший надолго, не воскресает: реплика, назначенная на десятый час,
// не имеет права снова завести разговор и потянуть за собой каскад.
//
// Проверяется поведением прогона, а не таблицей: правило живёт в цикле вакуума,
// и таблица подтвердила бы только арифметику hazardAt. Кривая здесь жёсткая
// (после часа тишины — ноль продолжений), потому что тест про МЕХАНИЗМ; сама
// величина замерена и живёт в archive.
func TestVacuumSilenceEndsTheThread(t *testing.T) {
	card := measuredCard()
	// Задержки длинные и почти без разброса: первая волна приходит через час, и
	// дальше каждый ответ — ещё через час. Без тормоза такой тред идёт до
	// горизонта.
	card.Latency.ToThreadSec = narod.Dist{P10: 3600, Median: 3600, P90: 3600, Max: 3600}
	card.Latency.ToReplySec = narod.Dist{P10: 3600, Median: 3600, P90: 3600, Max: 3600}
	card.Dice = narod.DiceParams{ComeToNote: 1, ReplyMention: 1, ReplyOther: 1, MaxPerThread: 20}

	run := func(hz []archive.DecayHazard) *VacuumRun {
		t.Helper()
		sc := soloScript()
		r, err := RunVacuum(context.Background(), sc, VacuumOpts{
			Actors: vacCast(7, card, 1), Hazard: hz,
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	free := run(nil)
	braked := run([]archive.DecayHazard{{SilenceSec: 1800, P: 0}})
	if braked.Got.Replies >= free.Got.Replies {
		t.Errorf("с тормозом реплик %d, без него %d — тормоз ничего не остановил",
			braked.Got.Replies, free.Got.Replies)
	}
	if braked.Stopped != "тишина — разговор кончился" {
		t.Errorf("тред кончился так: %q", braked.Stopped)
	}
	if braked.Got.SpanSec > 3600 {
		t.Errorf("разговор шёл %d с при тишине в полчаса — тред всё-таки воскрес",
			braked.Got.SpanSec)
	}
}

// moodSpeaker запоминает настроение каждой реплики: тест меряет не текст, а то,
// что до модели вообще доехало.
type moodSpeaker struct {
	moods []string
	heats []int
}

func (s *moodSpeaker) Speak(_ context.Context, p SpeechPoint) (Speech, error) {
	s.moods = append(s.moods, p.Mood)
	return Speech{Got: "реплика"}, nil
}

// Накал пары считает КОД и даром: у каждой реплики известны автор и адресат, а
// «сцепились» — это ровно длина их непрерывного обмена. Растёт он ПОСЛЕ реплики,
// поэтому пишущий видит ступень, на которой разговор стоял до него.
func TestVacuumCountsPairHeat(t *testing.T) {
	sp := &moodSpeaker{}
	card := testCard()
	card.Dice.ComeToNote = 1
	card.Dice.ReplyMention = 1
	card.Dice.ReplyOther = 1
	actors := vacCast(3, card, 7)
	for i := range actors {
		actors[i].Speaker = sp
	}
	seen := map[string]bool{}
	_, err := RunVacuum(context.Background(), sc0(), VacuumOpts{
		Actors: actors, MaxReplies: 40,
		Mood: func(actor, peer int64, heat int, tone float64) string {
			sp.heats = append(sp.heats, heat)
			return fmt.Sprintf("накал %d", heat)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sp.moods) == 0 {
		t.Fatal("настроение не спросили ни разу")
	}
	for _, m := range sp.moods {
		seen[m] = true
	}
	if !seen["накал 0"] {
		t.Fatalf("первая реплика паре пришла не с нулевого накала: %v", seen)
	}
	if len(seen) < 2 {
		t.Fatalf("накал не растёт вовсе: %v", seen)
	}
	for _, h := range sp.heats {
		if h < 0 {
			t.Fatalf("отрицательный накал %d", h)
		}
	}
}

// Молчащее замыкание — законное состояние: харнесс обязан гоняться там, где
// карточек нет вовсе, и настроение тогда просто не подаётся.
func TestVacuumWithoutMoodIsSilent(t *testing.T) {
	sp := &moodSpeaker{}
	card := testCard()
	card.Dice.ComeToNote = 1
	actors := vacCast(2, card, 11)
	for i := range actors {
		actors[i].Speaker = sp
	}
	if _, err := RunVacuum(context.Background(), sc0(), VacuumOpts{
		Actors: actors, MaxReplies: 10,
	}); err != nil {
		t.Fatal(err)
	}
	for _, m := range sp.moods {
		if m != "" {
			t.Fatalf("без замыкания приехало настроение %q", m)
		}
	}
}
