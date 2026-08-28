package main

// lovegw narod replay -mode vacuum — калибровка в вакууме.
//
// От solo отличается тем, ЧТО берётся из архива: там подавался весь чужой
// разговор, здесь — одна заметка. Дальше жители говорят сами, и меряется форма
// того, что вышло.
//
// Прогон идёт СЕРИЕЙ заметок в сжатом времени, а не одной: знакомство копится от
// треда к треду, и на одиночной заметке этого не увидеть вовсе. По той же
// причине состав общий на всю серию — мир один.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/archive"
	"lovegw/internal/llm"
	"lovegw/internal/narod"
	"lovegw/internal/narodsim"
)

func narodVacuum(ctx context.Context, o replayOpts) error {
	tokens := splitTokens(o.actor)
	if len(tokens) == 0 {
		return fmt.Errorf("narod replay -mode vacuum: нужен состав, -actor u123,u456")
	}
	ar, err := archive.Open(ctx, o.dbPath)
	if err != nil {
		return err
	}
	defer ar.Close()

	cast, err := loadVacuumCast(o, tokens)
	if err != nil {
		return err
	}
	notes, err := vacuumNotes(ctx, ar, cast, o)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "состав %d, заметок %d\n", len(cast), len(notes))

	model, client, usage, err := vacuumSpeakers(ctx, ar, o, cast)
	if err != nil {
		return err
	}

	scripts := loadScripts(ctx, ar, notes)
	if len(scripts) == 0 {
		return fmt.Errorf("narod replay: не отработала ни одна заметка")
	}
	// Серия идёт ПО ВРЕМЕНИ: мир копится от треда к треду, и задом наперёд житель
	// «помнил» бы разговор, которого в его прошлом ещё не было.
	sortScripts(scripts)
	// Темы заметок разбираются ОДИН раз на все зёрна: от броска они не зависят,
	// а лексиконов дюжина и каждый — регэксп.
	topics, err := noteTopics(scripts)
	if err != nil {
		return err
	}

	now := time.Now()
	dir := filepath.Join(o.outDir, fmt.Sprintf("vacuum-%s", now.Format("20060102-150405")))

	// Мир открывается ВСЕГДА, даже когда модели нет: знакомство считается по
	// самому треду и копится даром, а летописец без модели просто молчит про
	// симпатию. Так у бесплатного прогона остаётся половина мира, которая ему
	// доступна, — и видно, что это именно половина.
	world, err := openVacWorld(ctx, filepath.Join(dir, "world.db"),
		chronicler(client), uint64(o.seed), cast, now)
	if err != nil {
		return err
	}
	defer world.Close()

	var runs []*narodsim.VacuumRun
	for i := range seedCount(o) {
		seed := uint64(o.seed) + uint64(i)
		actors := make([]narodsim.VacuumActor, 0, len(cast))
		for _, c := range cast {
			// Модель зовут ТОЛЬКО на первом зерне: форма разговора бесплатна и
			// потому повторяется, текст платный — платить впятеро не за что.
			sp := c.speaker()
			if i > 0 {
				sp = nil
			}
			actors = append(actors, narodsim.VacuumActor{
				UserID: c.userID, Nick: c.card.Persona.Nick, CardID: c.card.ID,
				Decider: &narodsim.CardDecider{Card: *c.card, Seed: seed},
				Speaker: sp,
			})
		}
		// Знакомство живёт дольше одного треда — карта общая на серию, но своя у
		// каждого зерна: перенеся её между зёрнами, мы дали бы второму прогону
		// знакомства, которых в нём не случалось.
		familiar := map[int64]map[int64]int{}
		// Мир копится тоже ровно один раз, на первом зерне: он держится на словах
		// реплик, а на прочих зёрнах слов нет вовсе. Пускать туда прогон без слов
		// значило бы копить знакомства пятикратно и объявить их одним миром.
		w := world
		if i > 0 {
			w = nil
		}
		for _, sc := range scripts {
			opts := narodsim.VacuumOpts{
				Actors: actors, MaxReplies: o.maxReply, MaxSpeak: o.maxSpeak,
				Topics: topics[sc.NoteID], Familiar: familiar,
			}
			if w != nil {
				if err := w.live(ctx, sc.Note.PublishedAt); err != nil {
					return err
				}
				if opts.Feel, err = w.feel(ctx); err != nil {
					return err
				}
				opts.Recall = w.recall
			}
			run, err := narodsim.RunVacuum(ctx, sc, opts)
			if err != nil {
				return err
			}
			run.Seed = seed
			if w != nil {
				if err := w.chronicle(ctx, run); err != nil {
					return err
				}
			}
			fmt.Fprintf(os.Stderr, "  зерно %-3d заметка %-8d наших %-3d (в оригинале состав %-3d из %-4d)  "+
				"заговорили %d/%d  шёл %.1f ч  %s\n",
				seed, run.NoteID, run.Got.Replies, run.Want.Replies, run.OrigReplies,
				len(run.Got.Spoke), len(run.Want.Spoke), float64(run.Got.SpanSec)/3600, run.Stopped)
			runs = append(runs, run)
		}
	}

	reports := make([]narodsim.VacuumActorReport, 0, len(cast))
	for _, c := range cast {
		reports = append(reports, narodsim.VacuumActorReport{
			UserID: c.userID, Nick: c.card.Persona.Nick, CardID: c.card.ID, Band: c.band,
		})
	}
	rep := narodsim.NewVacuumReport(model, uint64(o.seed), now, reports, runs)
	// Голос меряется ПОСЛЕ серии и по всем текстам жителя разом: пачка — это
	// подряд идущие реплики, и по одному треду их набирается меньше порога.
	for i, c := range cast {
		if c.voice == nil || !c.band.Usable {
			continue
		}
		if rep.Actors[i].Voice, err = c.voice.Measure(ctx, rep.TextsOf(c.userID)); err != nil {
			return err
		}
	}

	if rep.World, err = world.summary(ctx); err != nil {
		return err
	}
	if err := narodsim.WriteVacuumReport(dir, rep); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\n%s", rep.Markdown())
	fmt.Fprintf(os.Stderr, "отчёт: %s\n", dir)
	if usage != nil {
		fmt.Fprintf(os.Stderr, "расход: %s\n", usage())
	}
	return nil
}

// vacActor — один житель на сцене вместе со своей платной половиной.
type vacActor struct {
	token  string
	userID int64
	card   *narod.Card
	voice  *narodsim.VoiceSpeaker // nil — прогон бесплатный
	// band — годится ли мерить голос этого жителя. Спрашивается ДО первого
	// запроса и в отчёт едет как есть: непригодная встаёт ВМЕСТО чисел.
	band archive.VoiceBand
}

// speaker отдаёт говорящего так, чтобы nil-интерфейс не притворялся живым:
// присвоив narodsim.Speaker нетипизированный nil-указатель, мы получили бы
// интерфейс, у которого `!= nil` истинно, и вакуум пошёл бы звать модель.
func (a vacActor) speaker() narodsim.Speaker {
	if a.voice == nil {
		return nil
	}
	return a.voice
}

// loadVacuumCast читает карточки состава.
func loadVacuumCast(o replayOpts, tokens []string) ([]*vacActor, error) {
	out := make([]*vacActor, 0, len(tokens))
	seen := map[int64]bool{}
	for _, token := range tokens {
		id, err := strconv.ParseInt(strings.TrimPrefix(token, "u"), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("narod replay: состав задаётся анкетами u<id>, а не %q", token)
		}
		if seen[id] {
			return nil, fmt.Errorf("narod replay: %s в составе дважды", token)
		}
		seen[id] = true
		card, err := loadActorCard(o.cardsDir, token)
		if err != nil {
			return nil, err
		}
		out = append(out, &vacActor{token: token, userID: id, card: card})
	}
	return out, nil
}

// vacuumSpeakers подключает модель всему составу разом — либо никому.
//
// Полоса спрашивается у КАЖДОГО и ДО первого запроса — но останавливает прогон
// теперь только тогда, когда она непригодна у ВСЕХ. Правило это правилось по
// живому случаю 28.08.2026: у «Инженера Шурика 54» слой не узнаёт и настоящие
// тексты автора (медиана места 5243 из 9361), и прежний порядок отменил из-за
// этого весь прогон — вместе с миром, летописью и памятью, которым полоса не
// нужна вовсе. Вопросы разные: «похож ли текст на донора» и «двинулся ли мир»,
// и первый, оставшись без ответа, не отменяет второго.
//
// Чего нельзя по-прежнему — печатать вместо ответа ноль: у непригодной полосы
// BandQuantile возвращает его молча. Поэтому причина едет в отчёт и встаёт
// ВМЕСТО чисел (VacuumActorReport.Band), а прогон целиком отменяется, только
// если мерить нечем ни у кого: тогда у платной половины не остаётся ни одной
// проверки, и тратить на неё деньги — решение человека, а не умолчание.
func vacuumSpeakers(ctx context.Context, ar *archive.Store, o replayOpts, cast []*vacActor) (string, *llm.Client, func() string, error) {
	if !o.speak {
		fmt.Fprintln(os.Stderr, "модель НЕ подключена (-speak) — реплики без текста, "+
			"мерится только форма разговора и мир не двигается, бесплатно")
		return o.model, nil, nil, nil
	}
	client, err := replayClient(o)
	if err != nil {
		return "", nil, nil, err
	}
	var usable int
	for _, c := range cast {
		if c.voice, err = replayVoice(ctx, ar, client, o, c.token, c.userID, c.card); err != nil {
			return "", nil, nil, err
		}
		if c.band, err = c.voice.Band(ctx); err != nil {
			return "", nil, nil, err
		}
		if !c.band.Usable {
			fmt.Fprintf(os.Stderr, "полоса %s НЕПРИГОДНА (%s) — говорить будет, "+
				"мерить голос не станем\n", c.token, c.band.Why)
			continue
		}
		usable++
		fmt.Fprintf(os.Stderr, "полоса %s: %d пачек, медиана места %d из %d\n",
			c.token, c.band.N, c.band.Median, c.band.Of)
	}
	if usable == 0 {
		return "", nil, nil, fmt.Errorf("мерить голос нечем ни у кого из состава — " +
			"к модели не пошли: платить за прогон, у которого не осталось ни одной " +
			"проверки, надо решать руками")
	}
	return client.Model(), client, func() string { return client.Usage().String() }, nil
}

// vacuumNotes — заметки серии: заданные руками либо подобранные по составу.
//
// Подбираются по СОСТАВУ целиком, а не по каждому порознь: смысл вакуума в том,
// что жители встречаются, и заметка, где из них говорил один, показывает
// монолог.
func vacuumNotes(ctx context.Context, ar *archive.Store, cast []*vacActor, o replayOpts) ([]int64, error) {
	if o.notes != "" {
		return parseNoteIDs(o.notes)
	}
	ids := make([]int64, 0, len(cast))
	for _, c := range cast {
		ids = append(ids, c.userID)
	}
	if o.hot {
		return hotNotes(ctx, ar, ids, o)
	}
	picks, err := ar.PickCalibrationThreads(ctx, ids, o.minSaid, o.threads)
	if err != nil {
		return nil, err
	}
	if len(picks) == 0 {
		return nil, fmt.Errorf("narod replay: у состава нет тредов с %d+ репликами — опустите -min-said", o.minSaid)
	}
	out := make([]int64, 0, len(picks))
	for _, p := range picks {
		out = append(out, p.NoteID)
	}
	return out, nil
}

func splitTokens(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// noteTopics — темы каждой заметки серии, ключами лексикона архива.
//
// Разбираются ТЕМ ЖЕ лексиконом, каким снят перекос в карточке: разойдись они,
// и множитель прикладывался бы к теме, которой в замере не было, — а заметить
// это было бы нечем, кубик молча стал бы звать не тех.
func noteTopics(scripts []*archive.ThreadScript) (map[int64][]string, error) {
	lex := archive.DefaultTopics()
	out := make(map[int64][]string, len(scripts))
	for _, sc := range scripts {
		t, err := archive.TopicsOf(sc.Note.Text, lex)
		if err != nil {
			return nil, err
		}
		out[sc.NoteID] = t
	}
	return out, nil
}

// hotNotes — заметки, где разговор БУРЛИЛ: люди отвечали друг другу, а не
// высказывались рядом.
//
// Печатает жар каждой отобранной вместе с числом настоящих рёбер: ноль дуэлей у
// треда без рёбер значит «мобильное дерево по нему не обходили», а у треда с
// тремя сотнями рёбер — «не спорили», и путать эти два состояния нельзя.
func hotNotes(ctx context.Context, ar *archive.Store, ids []int64, o replayOpts) ([]int64, error) {
	picks, err := ar.PickHotThreads(ctx, ids, o.minSaid, o.threads)
	if err != nil {
		return nil, err
	}
	if len(picks) == 0 {
		return nil, fmt.Errorf("narod replay: у состава нет тредов с %d+ репликами — опустите -min-said", o.minSaid)
	}
	out := make([]int64, 0, len(picks))
	var silent int
	for _, p := range picks {
		if p.Duels == 0 {
			silent++
		}
		fmt.Fprintf(os.Stderr, "  заметка %-8d реплик %-4d наших %-3d  перепалок %-3d "+
			"(%.0f на сотню)  самая длинная %-3d  настоящих рёбер %d\n",
			p.NoteID, p.Total, p.Said, p.Duels, p.Heat(), p.Longest, p.Edges)
		out = append(out, p.NoteID)
	}
	if silent > 0 {
		fmt.Fprintf(os.Stderr, "из них без единой перепалки: %d — бурлящих тредов у состава "+
			"меньше, чем просили\n", silent)
	}
	return out, nil
}
