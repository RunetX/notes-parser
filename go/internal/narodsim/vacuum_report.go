package narodsim

// Отчёт прогона в вакууме.
//
// Числа здесь другие, чем в solo, и не потому, что режим другой, а потому, что
// спрашивается другое. В solo вопрос «угадал ли житель чужой ход», и ответ на
// него — матрица. Здесь чужого хода нет вовсе: вопрос в том, ПОХОЖ ЛИ НА
// РАЗГОВОР тот, что вышел сам, — и ответом служит пара портретов, наш против
// оригинала, суженного до того же состава.
//
// Правило печати остаётся прежним: рядом с каждым числом стоит то, без чего его
// нельзя читать. У «сколько вышло реплик» — доля состава в настоящем разговоре
// (иначе суженный оригинал читается как весь), у совпадения состава — сколько
// человек в нём вообще было, у длины разговора — чем он кончился: сам затих или
// его оборвал потолок.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lovegw/internal/archive"
	"lovegw/internal/narod"
)

// VacuumActorReport — житель на всех тредах серии.
type VacuumActorReport struct {
	UserID int64  `json:"user_id"`
	Nick   string `json:"nick"`
	CardID string `json:"card_id"`
	Said   int    `json:"said"` // реплик за всю серию
	Voice  Voice  `json:"voice"`
}

// VacuumReport — серия прогонов одним составом.
//
// Серией, а не по одному, потому что смысл вакуума в длинной дистанции: мир
// живёт от треда к треду, знакомство копится, и одиночная заметка не показывает
// ни того, ни другого.
type VacuumReport struct {
	Stamp  narod.Stamp         `json:"stamp"`
	Model  string              `json:"model"`
	Seed   uint64              `json:"seed"`
	Actors []VacuumActorReport `json:"actors"`
	Runs   []*VacuumRun        `json:"runs"`

	// World — что стало с миром. Заполняет его тот, у кого мир есть (CLI), и
	// живёт он только у платного прогона: симпатию называет летописец, читая
	// разговор, а на бесплатных зёрнах реплики стоят без слов.
	World *VacuumWorld `json:"world,omitempty"`
}

// VacuumWorld — итог серии для мира.
//
// Отдельным блоком, а не строкой рядом с формой разговора, потому что вопрос
// другой: форма отвечает «похоже ли это на живой тред», мир — «остался ли от
// разговора след». Второе меряется не совпадением с оригиналом (в оригинале
// графа нет вовсе), а тем, ДВИНУЛОСЬ ли что-нибудь и есть ли за сдвигом повод.
type VacuumWorld struct {
	Notes    int  `json:"notes"`    // сколько разговоров прочитал летописец
	Asked    int  `json:"asked"`    // из них — сколько дошло до модели
	Edges    int  `json:"edges"`    // рёбер всего (знакомство считается и даром)
	Moved    int  `json:"moved"`    // из них сдвинуто ОТНОШЕНИЕМ, а не встречами
	Episodes int  `json:"episodes"` // поводов, за которые можно вспомнить
	Free     bool `json:"free"`     // модели не было вовсе: двигалось одно знакомство
	Inner    int  `json:"inner"`    // событий в жизни жителей вне площадки

	// Top — самые сдвинувшиеся пары, готовыми строками: числа за ними лежат в
	// world.db, а здесь они нужны человеку, читающему отчёт.
	Top []string `json:"top,omitempty"`
	// Dropped — что летописец предложил, а код отверг. Список показывает, куда
	// модель тянет, и без него это видно только по отсутствию строк в базе.
	Dropped []string `json:"dropped,omitempty"`
}

// NewVacuumReport собирает отчёт и досчитывает, кто сколько сказал.
func NewVacuumReport(model string, seed uint64, now time.Time,
	actors []VacuumActorReport, runs []*VacuumRun) *VacuumReport {

	rep := &VacuumReport{
		Stamp: narod.NewStamp("lovegw narod replay -mode vacuum", now),
		Model: model, Seed: seed, Actors: actors, Runs: runs,
	}
	for i := range rep.Actors {
		a := &rep.Actors[i]
		a.Said = 0
		for _, r := range runs {
			a.Said += r.Got.ByActor[a.UserID]
		}
	}
	return rep
}

// TextsOf — что житель написал за всю серию, по порядку. Порядок нужен мерке:
// пачка это подряд идущие реплики разговора.
func (rep *VacuumReport) TextsOf(userID int64) []string {
	var out []string
	for _, r := range rep.Runs {
		for _, c := range r.Thread.Comments {
			if c.AuthorID == userID && c.Text != "" {
				out = append(out, c.Text)
			}
		}
	}
	return out
}

// WriteVacuumReport кладёт отчёт в каталог dir.
func WriteVacuumReport(dir string, rep *VacuumReport) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "report.md"), []byte(rep.Markdown())); err != nil {
		return err
	}
	metrics, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "metrics.json"), metrics); err != nil {
		return err
	}
	// Сами разговоры — отдельным файлом и целиком: главное, что даёт вакуум,
	// читается глазами, а не в квантилях.
	var b strings.Builder
	for _, r := range rep.Runs {
		fmt.Fprintf(&b, "\n=== заметка %d (%s) ===\n%s\n\n", r.NoteID, r.Stopped, r.Thread.Note.Text)
		for _, c := range r.Thread.Comments {
			fmt.Fprintf(&b, "[%s] %s → %s: %s\n",
				c.PublishedAt.UTC().Format("2006-01-02 15:04"), c.AuthorNick,
				addressOf(c), c.Text)
		}
	}
	return writeAtomic(filepath.Join(dir, "threads.txt"), []byte(b.String()))
}

func addressOf(c archive.ScriptComment) string {
	if c.Address != "" {
		return c.Address
	}
	return "заметке"
}

// Markdown — отчёт для человека.
func (rep *VacuumReport) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Калибровка в вакууме\n\n%s\n\n", rep.Stamp.Warning)
	fmt.Fprintf(&b, "Модель: `%s`, зерно: %d, снято: %s\n\n",
		rep.Model, rep.Seed, rep.Stamp.CreatedAt)
	b.WriteString("Из архива взята только ЗАМЕТКА: разговор вырос сам. " +
		"Оригинал для сравнения сужен до того же состава — в настоящем треде " +
		"говорили и те, у кого карточки нет.\n\n")

	writeVacuumCast(&b, rep)
	writeVacuumNotes(&b, rep)
	writeVacuumShapes(&b, rep)
	writeVacuumVoices(&b, rep)
	writeVacuumWorld(&b, rep)
	return b.String()
}

// writeVacuumWorld — что осталось от разговоров, когда они кончились.
//
// Сравнивать здесь не с чем и не нужно: в архиве графа отношений нет вовсе, а
// выдумать «правильный» — значило бы поставить себе цель по собственной
// догадке. Поэтому мерка тут другая, ЗАМКНУТАЯ на самом мире: двинулось ли
// что-нибудь, стоит ли за сдвигом названный повод и не тянет ли летописец в
// одну сторону (это видно по отвергнутому).
func writeVacuumWorld(b *strings.Builder, rep *VacuumReport) {
	w := rep.World
	if w == nil {
		return
	}
	b.WriteString("## Мир\n\n")
	switch {
	case w.Free:
		b.WriteString("Модель не подключена, и потому двинулось одно ЗНАКОМСТВО: его считает " +
			"код по самому треду. Симпатию называет летописец, ЧИТАЯ разговор, — а читать " +
			"здесь нечего, реплики стоят без слов.\n\n")
	case w.Asked < w.Notes:
		fmt.Fprintf(b, "Летописец прочитал %d разговоров из %d: в остальных говорил один "+
			"человек, и отношений между ним и тишиной не бывает.\n\n", w.Asked, w.Notes)
	}
	fmt.Fprintf(b, "| рёбер | из них сдвинуто отношением | эпизодов | событий вне площадки |\n"+
		"|---:|---:|---:|---:|\n| %d | %d | %d | %d |\n\n",
		w.Edges, w.Moved, w.Episodes, w.Inner)
	b.WriteString("Ребро заводит уже само знакомство — оно считается по треду и даром; " +
		"второй столбец — те пары, где симпатия или раздражение ушли от нуля, то есть " +
		"где разговор что-то изменил.\n\n")
	if len(w.Top) > 0 {
		b.WriteString("Самые сдвинувшиеся пары:\n\n")
		for _, l := range w.Top {
			fmt.Fprintf(b, "- %s\n", l)
		}
		b.WriteString("\n")
	}
	if len(w.Dropped) > 0 {
		fmt.Fprintf(b, "Отвергнуто кодом у летописца — %d:\n\n", len(w.Dropped))
		for i, d := range w.Dropped {
			if i >= 10 {
				fmt.Fprintf(b, "- …и ещё %d\n", len(w.Dropped)-i)
				break
			}
			fmt.Fprintf(b, "- %s\n", d)
		}
		b.WriteString("\n")
	}
}

func writeVacuumCast(b *strings.Builder, rep *VacuumReport) {
	fmt.Fprintf(b, "## Состав\n\n| житель | анкета | карточка | реплик за серию |\n|---|---:|---|---:|\n")
	for _, a := range rep.Actors {
		fmt.Fprintf(b, "| %s | u%d | `%s` | %d |\n", a.Nick, a.UserID, a.CardID, a.Said)
	}
	b.WriteString("\n")
}

// writeVacuumNotes — по одной строке на заметку.
//
// Столбец «чем кончился» здесь не служебный: разговор, оборванный потолком, и
// разговор, затихший сам, — это разные исходы, и первый вообще не отвечает на
// вопрос «когда они замолкают».
func writeVacuumNotes(b *strings.Builder, rep *VacuumReport) {
	if len(rep.Runs) == 0 {
		return
	}
	first := rep.Runs[0].Seed
	fmt.Fprintf(b, "## По заметкам (зерно %d)\n\n", first)
	fmt.Fprintf(b, "| заметка | наших реплик | в оригинале (состав / всего) | "+
		"заговорили (наши / было) | шёл, ч | чем кончился |\n")
	fmt.Fprintf(b, "|---:|---:|---:|---:|---:|---|\n")
	for _, r := range rep.Runs {
		if r.Seed != first {
			continue
		}
		fmt.Fprintf(b, "| %d | %d | %d / %d | %d / %d | %.1f | %s |\n",
			r.NoteID, r.Got.Replies, r.Want.Replies, r.OrigReplies,
			len(r.Got.Spoke), len(r.Want.Spoke),
			float64(r.Got.SpanSec)/3600, r.Stopped)
	}
	b.WriteString("\n")
	writeVacuumSeeds(b, rep)
}

// writeVacuumSeeds — разброс серии по зёрнам.
//
// Вакууму он нужнее, чем solo: серия из десяти заметок при трёх жителях даёт
// около десятка реплик всего, и одиночное зерно там не «слегка неточно», а
// вопрос удачи. Правило то же самое: правка кубика удачна, только если вывела
// число за разброс (см. writeSeeds).
func writeVacuumSeeds(b *strings.Builder, rep *VacuumReport) {
	bySeed := map[uint64][2]int{} // зерно → реплик, разговоров
	var order []uint64
	for _, r := range rep.Runs {
		v, ok := bySeed[r.Seed]
		if !ok {
			order = append(order, r.Seed)
		}
		v[0] += r.Got.Replies
		if r.Got.Depth >= 2 {
			v[1]++
		}
		bySeed[r.Seed] = v
	}
	if len(order) < 2 {
		b.WriteString("Прогон на ОДНОМ зерне: это бросок, а не замер (`-seeds`).\n\n")
		return
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	fmt.Fprintf(b, "Разброс по зёрнам (те же заметки, другие броски):\n\n")
	fmt.Fprintf(b, "| зерно | реплик за серию | заметок с разговором между жителями |\n|---:|---:|---:|\n")
	for _, s := range order {
		v := bySeed[s]
		fmt.Fprintf(b, "| %d | %d | %d |\n", s, v[0], v[1])
	}
	b.WriteString("\n")
}

// writeVacuumShapes — портрет разговора против портрета.
func writeVacuumShapes(b *strings.Builder, rep *VacuumReport) {
	var got, want VacuumShape
	spoke, pairs, heard, luck := 0.0, 0.0, 0.0, 0.0
	burst, judged, paired := 0, 0, 0
	gotPairs, wantPairs := 0, 0
	var firsts, gaps, wfirsts, wgaps []int
	seeds := map[uint64]bool{}
	for _, r := range rep.Runs {
		seeds[r.Seed] = true
	}
	for _, r := range rep.Runs {
		got.Replies += r.Got.Replies
		// Оригинал считается ОДИН раз: он на всех зёрнах один и тот же, а
		// сложенный по разу на зерно он вырос бы впятеро и сделал бы нас впятеро
		// молчаливее, чем мы есть.
		if r.Seed == rep.Runs[0].Seed {
			want.Replies += r.Want.Replies
		}
		got.Depth = max(got.Depth, r.Got.Depth)
		want.Depth = max(want.Depth, r.Want.Depth)
		if r.Got.BurstOnly() {
			burst++
		}
		firsts = append(firsts, r.Got.First.Median)
		wfirsts = append(wfirsts, r.Want.First.Median)
		// Пауза между репликами существует только там, где реплик было хотя бы
		// две. Заметка с одной репликой даёт ноль, и десяток таких нулей утянул
		// бы медиану в «отвечают мгновенно» — при том, что отвечать было некому.
		if r.Got.Gap.N > 0 {
			gaps = append(gaps, r.Got.Gap.Median)
		}
		if r.Want.Gap.N > 0 {
			wgaps = append(wgaps, r.Want.Gap.Median)
		}
		// Заметка, где из состава не заговорил НИКТО ни у нас, ни в оригинале,
		// в среднее не идёт. Жаккар считает её полным совпадением — и по своему
		// определению правильно, — но десяток пустых заметок так набирает
		// девяносто процентов согласия, не сказав ни слова. Ровно это и напечатал
		// первый прогон 28.08.2026, когда кубик не пустил в тред никого.
		if len(r.Got.Spoke) == 0 && len(r.Want.Spoke) == 0 {
			continue
		}
		judged++
		spoke += JaccardSpoke(r.Got, r.Want)
		luck += ChanceJaccard(len(r.Got.Spoke), len(r.Want.Spoke), len(r.Cast))
		// Граф считается ОТДЕЛЬНО и по своему условию: заметка, где из состава
		// никто никому не ответил, для «кто заговорил» законна, а для «кто кому»
		// — пустое против пустого. Девять таких из десяти дают девяносто
		// процентов согласия у графа, которого не было ни с одной стороны.
		if len(r.Got.Pairs) > 0 || len(r.Want.Pairs) > 0 {
			paired++
			pairs += JaccardPairs(r.Got, r.Want)
			heard += JaccardHeard(r.Got, r.Want)
			gotPairs += len(r.Got.Pairs)
			wantPairs += len(r.Want.Pairs)
		}
	}

	fmt.Fprintf(b, "## Форма разговора\n\n")
	fmt.Fprintf(b, "| | у нас | в оригинале (тот же состав) |\n|---|---:|---:|\n")
	fmt.Fprintf(b, "| реплик за серию (в среднем на зерно) | %.0f | %d |\n",
		float64(got.Replies)/float64(max(len(seeds), 1)), want.Replies)
	fmt.Fprintf(b, "| первая реплика через, мин (медиана по заметкам) | %.0f | %.0f |\n",
		float64(archive.NewDist(firsts).Median)/60, float64(archive.NewDist(wfirsts).Median)/60)
	fmt.Fprintf(b, "| пауза между репликами, мин | %.0f | %.0f |\n",
		float64(archive.NewDist(gaps).Median)/60, float64(archive.NewDist(wgaps).Median)/60)
	fmt.Fprintf(b, "| самая длинная цепочка ответов | %d | %d |\n\n", got.Depth, want.Depth)

	if got.Replies == 0 {
		b.WriteString("**Жители не сказали ни слова.** Согласие с оригиналом при таком " +
			"прогоне не считается вовсе: числа вышли бы про то, чего не было.\n\n")
	} else if judged == 0 {
		b.WriteString("Ни в одной заметке из состава не заговорил никто — сравнивать нечего.\n\n")
	} else {
		n := float64(judged)
		// Жребий стоит рядом по той же причине, по какой рядом с точностью решений
		// стоит молчун: пятеро наших и четверо настоящих из двенадцати пересекаются
		// вслепую почти на четверть, и число без этой приписки читается как умение.
		fmt.Fprintf(b, "- состав заговоривших сошёлся на **%.0f %%**, а ЖРЕБИЙ на тех же числах "+
			"дал бы **%.0f %%** (Жаккар, среднее по %d заметкам из %d, где хоть кто-то говорил)\n",
			100*spoke/n, 100*luck/n, judged, len(rep.Runs))
		if paired == 0 {
			b.WriteString("- граф «кто кому отвечал» не считался: ни в одной заметке " +
				"из состава никто никому не ответил — ни у нас, ни в оригинале\n")
		} else {
			p := float64(paired)
			fmt.Fprintf(b, "- круг тех, кого СЛУШАЮТ (кому хоть раз ответили), сошёлся на **%.0f %%** "+
				"— по %d заметкам с ответами внутри состава\n", 100*heard/p, paired)
			// Пары печатаются рядом и с сырым счётом, потому что мерка эта заведомо
			// невыполнимая: направленная пара — выбор СОДЕРЖАНИЯ, а кубик содержания
			// не видит. Ноль здесь читается как провал только тем, кому не сказали.
			fmt.Fprintf(b, "- поимённые пары «кто→кому» — **%.1f %%** (%d наших пар против %d настоящих). "+
				"Мерка держится как потолок, а не как цель: кто кому ответит, решает СОДЕРЖАНИЕ реплики, "+
				"а кубик его не видит вовсе\n", 100*pairs/p, gotPairs, wantPairs)
		}
	}
	// Шторм печатается ВСЕГДА, включая ноль: это признак провала, и молчание о
	// нём читалось бы как «не проверяли».
	fmt.Fprintf(b, "- разговоров, уложившихся в %s целиком: **%d** из %d "+
		"(живые приходят россыпью — тред, написанный за десять минут, выдаёт машину)\n\n",
		vacuumBurst, burst, len(rep.Runs))
}

func writeVacuumVoices(b *strings.Builder, rep *VacuumReport) {
	measured := false
	for _, a := range rep.Actors {
		if a.Voice.Batch.Chunks > 0 || a.Voice.Batch.Why != "" {
			measured = true
		}
	}
	if !measured {
		b.WriteString("## Голос\n\nМодель не подключена — реплики без текста, " +
			"мерилась только форма разговора.\n\n")
		return
	}
	for _, a := range rep.Actors {
		fmt.Fprintf(b, "## Голос: %s (u%d)\n\n", a.Nick, a.UserID)
		writeVoice(b, a.Voice)
	}
}
