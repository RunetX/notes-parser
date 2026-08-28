package main

// Мир в реплее: граф, память и летопись вокруг вакуумного прогона.
//
// Живёт связка ЗДЕСЬ, а не в narodsim, по той же причине, по которой здесь же
// живёт конвертер карточки: харнесс обязан гоняться там, где базы мира нет
// вовсе, и знать про SQLite ему незачем. Наружу он отдаёт два замыкания — «как
// я к нему отношусь» и «что я про него помню», — а всё остальное остаётся тут.
//
// МИР ЖИВЁТ ТОЛЬКО НА ПЕРВОМ ЗЕРНЕ, и это не экономия ради экономии. Симпатию
// называет летописец, ЧИТАЯ разговор; на прочих зёрнах модель не зовут вовсе, и
// реплики там стоят без слов — читать в них нечего, а запрос стоил бы столько
// же, сколько настоящий. Отсюда деление, повторяющее уже принятое для голоса:
// бесплатные зёрна меряют КУБИК (форму разговора, с разбросом), первое зерно —
// МИР (двинулся ли граф, всплыло ли «а помнишь»). Числа эти отвечают на разные
// вопросы и в одну таблицу не складываются.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"lovegw/internal/archive"
	"lovegw/internal/llm"
	"lovegw/internal/narod"
	"lovegw/internal/narodsim"
)

// innerWindow — за сколько дней до заметки прокручиваются внутренние события.
//
// Трое суток, а не всё время с прошлой заметки: между двумя архивными заметками
// проходят недели виртуального времени, и прокрутка их подряд стоила бы дороже
// самого разговора. Смысл же у события ровно один — быть свежим к моменту,
// когда житель заговорит.
const innerWindow = 3 * 24 * time.Hour

// vacWorld — мир вокруг серии.
type vacWorld struct {
	w     *narod.World
	gen   narod.JSONGenerator
	seed  uint64
	byID  map[int64]*vacActor // анкета → житель
	actor map[int64]string    // анкета → актор мира
	nick  map[int64]string

	Notes    int
	Asked    int
	Episodes int
	Inner    int
	Dropped  []string
}

// openVacWorld заводит мир серии и вписывает в него состав.
func openVacWorld(ctx context.Context, path string, gen narod.JSONGenerator,
	seed uint64, cast []*vacActor, now time.Time) (*vacWorld, error) {

	w, err := narod.OpenWorld(ctx, path)
	if err != nil {
		return nil, err
	}
	vw := &vacWorld{w: w, gen: gen, seed: seed,
		byID: map[int64]*vacActor{}, actor: map[int64]string{}, nick: map[int64]string{}}
	for _, c := range cast {
		// Актор мира — id карточки: он же ключ жителя на площадке, и заводить
		// рядом второй идентификатор значило бы держать таблицу соответствий
		// ровно затем, чтобы однажды с ней разойтись.
		vw.byID[c.userID] = c
		vw.actor[c.userID] = c.card.ID
		vw.nick[c.userID] = c.card.Persona.Nick
		if err := w.UpsertActor(ctx, narod.Actor{
			ID: c.card.ID, Kind: narod.ActorPersona,
			PlatformUserID: c.userID, Nick: c.card.Persona.Nick,
		}, now); err != nil {
			w.Close()
			return nil, err
		}
	}
	return vw, nil
}

func (vw *vacWorld) Close() error { return vw.w.Close() }

// feel снимает СНИМОК отношений на начало треда.
//
// Снимок, а не живой запрос на каждую точку решения: точек в треде десятки
// тысяч (жителей дюжина, реплик сотни), и ходить за каждой в базу значило бы
// платить временем за то, чему внутри треда всё равно неоткуда меняться —
// симпатию называет летописец ПОСЛЕ разговора.
func (vw *vacWorld) feel(ctx context.Context) (map[int64]map[int64]float64, error) {
	edges, err := vw.w.Edges(ctx)
	if err != nil {
		return nil, err
	}
	byActor := map[string]int64{}
	for id, a := range vw.actor {
		byActor[a] = id
	}
	out := map[int64]map[int64]float64{}
	for _, e := range edges {
		src, ok := byActor[e.Src]
		if !ok {
			continue
		}
		dst, ok := byActor[e.Dst]
		if !ok {
			continue
		}
		if t := e.Tone(); t != 0 {
			if out[src] == nil {
				out[src] = map[int64]float64{}
			}
			out[src][dst] = t
		}
	}
	return out, nil
}

// recall — замыкание для вакуума: что житель помнит про названных.
func (vw *vacWorld) recall(ctx context.Context, actor int64, peers []int64) (string, error) {
	id, ok := vw.actor[actor]
	if !ok {
		return "", nil
	}
	list := make([]narod.MemoryPeer, 0, len(peers))
	for _, p := range peers {
		if a, ok := vw.actor[p]; ok {
			list = append(list, narod.MemoryPeer{ActorID: a, Nick: vw.nick[p]})
		}
	}
	return narod.WriteMemory(ctx, vw.w, id, list)
}

// live прокручивает дни перед заметкой: с жителями что-то случается и до того,
// как они пришли в тред.
func (vw *vacWorld) live(ctx context.Context, at time.Time) error {
	for _, c := range vw.byID {
		got, err := narod.InnerTick(ctx, vw.w, vw.gen, c.card,
			vw.actor[c.userID], vw.seed, at.Add(-innerWindow), at)
		if err != nil {
			return err
		}
		vw.Inner += len(got)
	}
	return nil
}

// chronicle отдаёт закончившийся разговор летописцу.
func (vw *vacWorld) chronicle(ctx context.Context, run *narodsim.VacuumRun) error {
	th := narod.ChronicleThread{
		NoteID: run.NoteID, NoteBy: run.Thread.Note.AuthorNick,
		NoteText: run.Thread.Note.Text,
		At:       run.Thread.Note.PublishedAt.Add(time.Duration(run.Got.SpanSec) * time.Second),
	}
	for _, c := range run.Thread.Comments {
		th.Replies = append(th.Replies, narod.ChronicleReply{
			ID: c.ID, ActorID: vw.actor[c.AuthorID], Nick: vw.nick[c.AuthorID],
			Text: c.Text, ReplyTo: c.ReplyTo, Target: vw.actor[c.TargetID],
		})
	}
	res, err := narod.Chronicle(ctx, vw.w, vw.gen, th)
	if err != nil {
		return err
	}
	vw.Notes++
	if res.Asked {
		vw.Asked++
	}
	vw.Episodes += len(res.Episodes)
	vw.Dropped = append(vw.Dropped, res.Dropped...)
	return nil
}

// summary — что стало с миром за серию.
func (vw *vacWorld) summary(ctx context.Context) (*narodsim.VacuumWorld, error) {
	edges, err := vw.w.Edges(ctx)
	if err != nil {
		return nil, err
	}
	out := &narodsim.VacuumWorld{
		Notes: vw.Notes, Asked: vw.Asked, Episodes: vw.Episodes,
		Inner: vw.Inner, Dropped: vw.Dropped, Free: vw.gen == nil,
	}
	moved := make([]narod.Edge, 0, len(edges))
	for _, e := range edges {
		if e.Sympathy != 0 || e.Irritation != 0 {
			moved = append(moved, e)
		}
	}
	out.Moved, out.Edges = len(moved), len(edges)
	sort.Slice(moved, func(i, j int) bool {
		return abs(moved[i].Tone()) > abs(moved[j].Tone())
	})
	byActor := map[string]string{}
	for id, a := range vw.actor {
		byActor[a] = vw.nick[id]
	}
	for i, e := range moved {
		if i >= 8 {
			break
		}
		out.Top = append(out.Top, fmt.Sprintf("%s → %s: симпатия %+.1f, раздражение %+.1f, виделись %.0f",
			byActor[e.Src], byActor[e.Dst], e.Sympathy, e.Irritation, e.Familiarity))
	}
	return out, nil
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// sortScripts выстраивает серию ПО ВРЕМЕНИ.
//
// Иначе мир накапливается задом наперёд: житель «помнит» разговор, которого в
// его прошлом ещё не было, и «а помнишь» в позднем треде оказывается ссылкой в
// будущее. Порядок подбора заметок к этому отношения не имеет — он про то, где
// состав говорил, а не про то, когда.
func sortScripts(scripts []*archive.ThreadScript) {
	sort.SliceStable(scripts, func(i, j int) bool {
		return scripts[i].Note.PublishedAt.Before(scripts[j].Note.PublishedAt)
	})
}

// chronicler отдаёт клиента летописцу так, чтобы nil-интерфейс не притворялся
// живым. Тот же приём, что у vacActor.speaker, и по той же причине: интерфейс с
// нетипизированным nil внутри проходит проверку `!= nil` и уводит прогон к
// модели, которой нет.
func chronicler(c *llm.Client) narod.JSONGenerator {
	if c == nil {
		return nil
	}
	return c
}
