package main

// lovegw narod replay — калибровка слепка на архивных тредах.
//
// Прогон по умолчанию БЕСПЛАТНЫЙ: считается матрица решений «прийти или
// смолчать», а она чистая формула. Модель подключается отдельным флагом -speak,
// и только он делает прогон платным. Умолчание выбрано так намеренно: цикл
// «правка кубика → прогон → отчёт» должен крутиться сколько угодно раз без
// расхода, а деньги тратиться по осознанному нажатию.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/archive"
	"lovegw/internal/config"
	"lovegw/internal/narod"
	"lovegw/internal/narodsim"
)

// replayOpts — параметры прогона.
type replayOpts struct {
	dbPath   string
	cardsDir string
	outDir   string
	actor    string // u<id>
	notes    string // список id через запятую; пусто — подобрать самому
	threads  int    // сколько подобрать, если не заданы
	minSaid  int    // сколько реплик донора нужно в треде
	speak    bool   // звать модель (платно)
	maxSpeak int
	drafts   int
	rounds   int
	seed     int64
	cfgPath  string
	model    string
}

func narodReplay(ctx context.Context, o replayOpts) error {
	if o.actor == "" {
		return fmt.Errorf("narod replay: нужен -actor u<id>")
	}
	ar, err := archive.Open(ctx, o.dbPath)
	if err != nil {
		return err
	}
	defer ar.Close()

	actorID, err := strconv.ParseInt(strings.TrimPrefix(o.actor, "u"), 10, 64)
	if err != nil {
		return fmt.Errorf("narod replay: -actor должен быть u<id>: %w", err)
	}
	card, err := loadActorCard(o.cardsDir, o.actor)
	if err != nil {
		return err
	}

	notes, err := replayNotes(ctx, ar, actorID, o)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "слепок %s (%s), тредов %d\n", o.actor, card.Persona.Nick, len(notes))

	gen, err := replaySpeaker(ctx, ar, o, actorID, card)
	if err != nil {
		return err
	}
	var speaker narodsim.Speaker
	if gen == nil {
		fmt.Fprintln(os.Stderr, "модель НЕ подключена (-speak) — считается только матрица решений, бесплатно")
	} else {
		// Полоса спрашивается ДО первого запроса к модели. Непригодная означает,
		// что мерить нечем, а платить за измерение, которое ничего не измерит, —
		// единственное, чего калибровке делать нельзя.
		band, err := gen.speaker.Band(ctx)
		if err != nil {
			return err
		}
		if !band.Usable {
			return fmt.Errorf("мерить нечем, к модели не пошли: %s", band.Why)
		}
		fmt.Fprintf(os.Stderr, "полоса: %d пачек настоящих реплик, медиана места %d из %d\n",
			band.N, band.Median, band.Of)
		speaker = gen.speaker
	}

	dec := &narodsim.CardDecider{Card: *card, Seed: uint64(o.seed)}
	var runs []*narodsim.SoloRun
	for _, id := range notes {
		sc, err := ar.LoadThreadScript(ctx, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "тред %d пропущен: %v\n", id, err)
			continue
		}
		run, err := narodsim.RunSolo(ctx, sc, narodsim.SoloOpts{
			Actor: actorID, Decider: dec, Speaker: speaker, MaxSpeak: o.maxSpeak,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "тред %d пропущен: %v\n", id, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "  тред %-8d реплик %-5d своих %-3d  TP %-3d FP %-4d FN %-3d  реплик модели %d\n",
			run.NoteID, run.Replies, run.Mine, run.Matrix.TP, run.Matrix.FP, run.Matrix.FN, len(run.Speech))
		runs = append(runs, run)
	}
	if len(runs) == 0 {
		return fmt.Errorf("narod replay: не отработал ни один тред")
	}

	now := time.Now()
	// Модель называется РЕЗОЛЬВЕННАЯ, а не то, что набрали в -model: по
	// умолчанию там пусто, и отчёт, не называющий модель, нельзя сравнить с
	// другим отчётом — а сравнение это вся суть калибровки.
	model := o.model
	actor := narodsim.ActorReport{Actor: actorID, Nick: runs[0].Nick, CardID: card.ID, Runs: runs}
	// Разговорчивость донора по всему архиву — рядом с полнотой, иначе та
	// читается как свойство кубика, а бывает свойством выборки.
	if actor.Load, err = ar.MineThreadLoad(ctx, []int64{actorID}); err != nil {
		return err
	}
	if gen != nil {
		model = gen.model
		if actor.Voice, err = gen.speaker.Measure(ctx, actor.Texts()); err != nil {
			return err
		}
	}
	rep := narodsim.NewReport(model, uint64(o.seed), now, []narodsim.ActorReport{actor})
	dir := filepath.Join(o.outDir, fmt.Sprintf("%s-solo-%s", o.actor, now.Format("20060102-150405")))
	if err := narodsim.WriteSoloReport(dir, rep); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\n%s", rep.Markdown())
	fmt.Fprintf(os.Stderr, "отчёт: %s\n", dir)
	if gen != nil {
		fmt.Fprintf(os.Stderr, "расход: %s\n", gen.usage())
	}
	return nil
}

// cardPath — где лежит карточка, названная так, как её называют в соседних
// командах. `narod card u498196` кладёт файл по номеру анкеты, и требовать у
// `show` и `replay` полный путь значило бы держать в одной семье команд два
// разных смысла одного и того же слова; путь при этом принимается по-прежнему —
// карточка бывает и не из каталога.
func cardPath(dir, arg string) string {
	if strings.ContainsAny(arg, `/\`) || strings.HasSuffix(arg, narod.CardExt) {
		return arg
	}
	return filepath.Join(dir, arg+narod.CardExt)
}

// loadActorCard — карточка слепка из каталога (её кладёт `narod card`).
func loadActorCard(dir, actor string) (*narod.Card, error) {
	path := cardPath(dir, actor)
	card, err := narod.LoadCard(path)
	if err != nil {
		return nil, fmt.Errorf("карточка %s не прочитана (снимите её `narod card %s`): %w",
			path, actor, err)
	}
	return &card, nil
}

// replayNotes — треды прогона: заданные руками либо подобранные по донору.
func replayNotes(ctx context.Context, ar *archive.Store, actorID int64, o replayOpts) ([]int64, error) {
	if o.notes != "" {
		var out []int64
		for _, s := range strings.Split(o.notes, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("narod replay: -note %q: %w", s, err)
			}
			out = append(out, id)
		}
		return out, nil
	}
	picks, err := ar.PickCalibrationThreads(ctx, []int64{actorID}, o.minSaid, o.threads)
	if err != nil {
		return nil, err
	}
	if len(picks) == 0 {
		return nil, fmt.Errorf("narod replay: у анкеты %d нет тредов с %d+ репликами — "+
			"опустите -min-said", actorID, o.minSaid)
	}
	out := make([]int64, 0, len(picks))
	for _, p := range picks {
		out = append(out, p.NoteID)
	}
	return out, nil
}

// replayGen — платная половина прогона: кто пишет, чем меряет и во что обошлось.
type replayGen struct {
	speaker *narodsim.VoiceSpeaker
	model   string
	usage   func() string
}

// replaySpeaker собирает генератор, если прогон платный; nil означает
// бесплатный прогон, а не отсутствие настроек.
func replaySpeaker(ctx context.Context, ar *archive.Store, o replayOpts, actorID int64, card *narod.Card) (*replayGen, error) {
	if !o.speak {
		return nil, nil
	}
	cfg, err := config.Load(o.cfgPath)
	if err != nil {
		return nil, err
	}
	// Кэш-точка окупается: на каждой реплике системный промпт один и тот же, а
	// точек в прогоне десятки, и идут они встык.
	client, err := llmClientFor(cfg, o.model, "low", 0, withSystemCache())
	if err != nil {
		return nil, err
	}
	p := archive.VoiceCardDefaults()
	p.Genre, p.Kind, p.Solo = archive.GenreAll, archive.VoiceKindComments, true
	p.Seed = o.seed
	// Полоса нужна ПАЧКАМИ, а пачка набирается из нескольких комментариев —
	// значит отложенных текстов нужно во столько же раз больше, иначе полоса
	// выйдет из трёх точек и квантиль по ней будет грубее, чем разница, которую
	// им меряют.
	p.Band = replayBand
	vcard, err := ar.BuildVoiceCard(ctx, o.actor, p, time.Now())
	if err != nil {
		return nil, err
	}
	return &replayGen{
		speaker: &narodsim.VoiceSpeaker{
			Store: ar, Gen: client, Card: vcard, SelfIDs: []int64{actorID},
			Runes: card.Register.Runes, Seed: uint64(o.seed),
			Req: archive.VoiceRequest{
				Drafts: o.drafts, Rounds: o.rounds, Model: client.Model(),
			},
		},
		model: client.Model(),
		usage: func() string { return client.Usage().String() },
	}, nil
}

// replayBand — сколько реплик донора откладывать под полосу. Двести при
// медиане в 75 знаков дают около двадцати пяти пачек — столько же точек, сколько
// прежде давала полоса из отдельных комментариев, только теперь пригодных.
const replayBand = 200
