package narodsim

// Отчёт калибровочной сессии.
//
// Пишется в каталог с временной меткой и НЕ перезаписывает прошлые: сравнивать
// прогоны между собой — вся суть этапа, а отчёт, затирающий предыдущий, оставил
// бы от сравнения одно воспоминание.
//
// Числа отчёта — это ответ на «стало ли лучше», поэтому у каждого рядом стоит
// то, без чего его нельзя читать: у точности решений — полнота (молчун набирает
// девяносто процентов, ничего не умея), у медианы квантиля — сколько реплик
// вообще вышло, у пропущенного — сколько его.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lovegw/internal/narod"
)

// ActorReport — итог по одному слепку на всех его тредах.
type ActorReport struct {
	Actor    int64      `json:"actor"`
	Nick     string     `json:"nick"`
	CardID   string     `json:"card_id"`
	Runs     []*SoloRun `json:"runs"`
	Matrix   Matrix     `json:"matrix"`
	Speeches int        `json:"speeches"` // реплик, которые модель выдала
	Rejected int        `json:"rejected"` // точек, где не выдала ничего
	Skipped  int        `json:"skipped"`  // не взятых из-за потолка
}

// Report — сессия целиком.
type Report struct {
	Stamp  narod.Stamp   `json:"stamp"`
	Model  string        `json:"model"`
	Seed   uint64        `json:"seed"`
	Actors []ActorReport `json:"actors"`
}

// NewReport собирает отчёт из прогонов, сгруппированных по слепкам.
func NewReport(model string, seed uint64, now time.Time, actors []ActorReport) *Report {
	rep := &Report{
		Stamp: narod.NewStamp("lovegw narod replay", now),
		Model: model, Seed: seed, Actors: actors,
	}
	for i := range rep.Actors {
		a := &rep.Actors[i]
		a.Matrix = Matrix{}
		a.Speeches, a.Rejected, a.Skipped = 0, 0, 0
		for _, r := range a.Runs {
			a.Matrix.TP += r.Matrix.TP
			a.Matrix.FP += r.Matrix.FP
			a.Matrix.TN += r.Matrix.TN
			a.Matrix.FN += r.Matrix.FN
			a.Skipped += r.Skipped
			for _, s := range r.Speech {
				if s.Rejected != "" {
					a.Rejected++
					continue
				}
				a.Speeches++
			}
		}
	}
	return rep
}

// MedianQuantile — медианный квантиль слепка по всем его репликам.
func (a ActorReport) MedianQuantile() float64 {
	var qs []float64
	for _, r := range a.Runs {
		for _, s := range r.Speech {
			if s.Rejected == "" {
				qs = append(qs, s.Quantile)
			}
		}
	}
	return median(qs)
}

// MedianRank — медианное место автора в атрибуции. Ранг 1 значит, что атрибутор
// уверенно узнаёт донора; для КОМПОЗИТА это было бы плохо (сторож близости), а
// для слепка — цель.
func (a ActorReport) MedianRank() float64 {
	var rs []float64
	for _, r := range a.Runs {
		for _, s := range r.Speech {
			if s.Rejected == "" && s.Rank > 0 {
				rs = append(rs, float64(s.Rank))
			}
		}
	}
	return median(rs)
}

// MedianLatencyError — медианная ошибка задержки на верно угаданных приходах.
func (a ActorReport) MedianLatencyError() time.Duration {
	var errs []float64
	for _, r := range a.Runs {
		for _, p := range r.Points {
			if !p.Spoke || !p.Truth {
				continue
			}
			d := p.After - p.TrueAfter
			if d < 0 {
				d = -d
			}
			errs = append(errs, float64(d))
		}
	}
	return time.Duration(median(errs))
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	return xs[len(xs)/2]
}

// WriteSoloReport кладёт отчёт в каталог dir: report.md человеку, metrics.json
// машине, points.jsonl — сырые точки решения на разбор глазами.
//
// Пишется во временный файл и переименовывается: оборвавшийся прогон не должен
// оставить полуотчёт, который потом прочтут как полный.
func WriteSoloReport(dir string, rep *Report) error {
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
	var b strings.Builder
	for _, a := range rep.Actors {
		for _, r := range a.Runs {
			for _, p := range r.Points {
				line, err := json.Marshal(struct {
					Actor int64 `json:"actor"`
					Note  int64 `json:"note"`
					Point
				}{a.Actor, r.NoteID, p})
				if err != nil {
					return err
				}
				b.Write(line)
				b.WriteByte('\n')
			}
		}
	}
	return writeAtomic(filepath.Join(dir, "points.jsonl"), []byte(b.String()))
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Markdown — отчёт для человека.
func (rep *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Калибровка solo\n\n%s\n\n", rep.Stamp.Warning)
	fmt.Fprintf(&b, "Модель: `%s`, зерно: %d, снято: %s\n\n",
		rep.Model, rep.Seed, rep.Stamp.CreatedAt)

	for _, a := range rep.Actors {
		fmt.Fprintf(&b, "## %s (u%d, карточка `%s`)\n\n", a.Nick, a.Actor, a.CardID)

		m := a.Matrix
		fmt.Fprintf(&b, "### Решение «прийти или смолчать»\n\n")
		fmt.Fprintf(&b, "Точек решения: **%d** на %d тредах.\n\n", m.Total(), len(a.Runs))
		fmt.Fprintf(&b, "| | человек ответил | человек смолчал |\n|---|---:|---:|\n")
		fmt.Fprintf(&b, "| житель пришёл | %d | %d |\n", m.TP, m.FP)
		fmt.Fprintf(&b, "| житель смолчал | %d | %d |\n\n", m.FN, m.TN)
		fmt.Fprintf(&b, "- полнота (не проспал): **%.0f %%**\n", 100*m.Recall())
		fmt.Fprintf(&b, "- точность прихода: **%.0f %%**\n", 100*m.Precision())
		fmt.Fprintf(&b, "- верных решений: %.0f %% — само по себе это число ничего не значит: "+
			"стратегия «никогда не приходить» дала бы %.0f %%\n\n",
			100*m.Accuracy(), 100*silentBaseline(m))
		if err := a.MedianLatencyError(); err > 0 {
			fmt.Fprintf(&b, "- медианная ошибка задержки на верных приходах: **%s**\n\n",
				err.Truncate(time.Second))
		}

		fmt.Fprintf(&b, "### Голос\n\n")
		if a.Speeches == 0 {
			fmt.Fprintf(&b, "Модель не выдала ни одной реплики (%d отказов).\n\n", a.Rejected)
		} else {
			fmt.Fprintf(&b, "Реплик: **%d**, отказов: %d", a.Speeches, a.Rejected)
			if a.Skipped > 0 {
				fmt.Fprintf(&b, ", не взято из-за потолка: %d", a.Skipped)
			}
			fmt.Fprintf(&b, "\n\n")
			fmt.Fprintf(&b, "- медианный квантиль полосы: **%.2f** — доля настоящих текстов "+
				"автора, узнанных атрибутором ХУЖЕ нашего (0,5 — «неотличим от собственной середины»)\n",
				a.MedianQuantile())
			fmt.Fprintf(&b, "- медианное место в атрибуции: **%.0f**\n\n", a.MedianRank())
		}

		fmt.Fprintf(&b, "### По тредам\n\n")
		fmt.Fprintf(&b, "| заметка | реплик | своих | TP | FP | FN | реплик модели |\n")
		fmt.Fprintf(&b, "|---:|---:|---:|---:|---:|---:|---:|\n")
		for _, r := range a.Runs {
			fmt.Fprintf(&b, "| %d | %d | %d | %d | %d | %d | %d |\n",
				r.NoteID, r.Replies, r.Mine, r.Matrix.TP, r.Matrix.FP, r.Matrix.FN, len(r.Speech))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// silentBaseline — сколько верных решений набрала бы стратегия «всегда молчать».
// Стоит рядом с точностью не для красоты: без неё точность читается как оценка
// умения, а она в людном треде почти целиком состоит из молчания.
func silentBaseline(m Matrix) float64 {
	if m.Total() == 0 {
		return 0
	}
	return float64(m.TN+m.FP) / float64(m.Total())
}
