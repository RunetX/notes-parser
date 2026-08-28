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

	"lovegw/internal/archive"
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
	Voice    Voice      `json:"voice"`    // мерка голоса пачками

	// Load — сколько реплик донор пишет в треде ПО ВСЕМУ архиву. Стоит рядом с
	// полнотой по той же причине, по какой рядом с точностью стоит базлайн
	// молчуна: калибровка идёт по тредам, где донор говорил, то есть заведомо
	// не по средним, — и не назвав, насколько они не средние, отчёт выдал бы
	// свойство ВЫБОРКИ за свойство кубика. Ровно это и вышло 28.08.2026.
	Load archive.Dist `json:"load"`
}

// Texts — что модель выдала, в порядке появления. Порядок нужен мерке: пачка
// это подряд идущие реплики разговора.
func (a ActorReport) Texts() []string {
	var out []string
	for _, r := range a.Runs {
		for _, s := range r.Speech {
			if s.Rejected == "" && s.Got != "" {
				out = append(out, s.Got)
			}
		}
	}
	return out
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
		writeSample(&b, a)

		fmt.Fprintf(&b, "### Голос\n\n")
		if a.Speeches == 0 {
			fmt.Fprintf(&b, "Модель не выдала ни одной реплики (%d отказов).\n\n", a.Rejected)
		} else {
			fmt.Fprintf(&b, "Реплик: **%d**, отказов: %d", a.Speeches, a.Rejected)
			if a.Skipped > 0 {
				fmt.Fprintf(&b, ", не взято из-за потолка: %d", a.Skipped)
			}
			fmt.Fprintf(&b, "\n\n")
			writeVoice(&b, a.Voice)
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

// writeVoice печатает мерку голоса — или причину, по которой её нет.
//
// Причина стоит ВМЕСТО чисел, а не рядом с ними. Непригодная полоса даёт
// правдоподобный ноль, и приписка мелким шрифтом читается как примечание к
// измерению, а не как «измерения не было»: ровно так первый платный прогон
// сообщил о провале голоса, которого не мерил.
func writeVoice(b *strings.Builder, v Voice) {
	bt := v.Batch
	switch {
	case bt.Why != "":
		fmt.Fprintf(b, "Мерка не состоялась: %s.\n\n", bt.Why)
	case bt.Chunks == 0:
		// Пустая мерка — это отчёт, собранный без неё, а не нулевой результат.
		// Нули здесь печатались бы как приговор голосу.
		fmt.Fprintf(b, "Голос не мерили.\n\n")
	default:
		// Знаков названо ДВА числа, и это не педантизм: хвост короче пачки в
		// мерку не идёт, а одно число «сколько написано» выдавало бы за
		// измеренное то, что измерено не было.
		fmt.Fprintf(b, "Пачками против таких же пачек настоящих реплик автора; "+
			"в мерку вошло %d знаков из %d написанных (хвост короче пачки отброшен):\n\n",
			bt.Used, bt.Runes)
		fmt.Fprintf(b, "- медианный квантиль полосы: **%.2f** — доля пачек настоящих текстов, "+
			"узнанных атрибутором ХУЖЕ нашей (0,5 — «неотличим от собственной середины»)\n",
			bt.MedianQuantile())
		fmt.Fprintf(b, "- медианное место в атрибуции: **%d** из %d; пачек наших %d, в полосе %d\n\n",
			bt.MedianRank(), bt.Band.Of, bt.Chunks, bt.Band.N)
	}
	writeShape(b, v.Got, v.Want)
}

// writeShape — портрет против портрета. Ранг говорит «не похоже», портрет
// говорит ЧЕМ, и без него отчёт не подсказывает, что править.
func writeShape(b *strings.Builder, got, want archive.VoiceShape) {
	if got.Texts == 0 || want.Texts == 0 {
		return
	}
	fmt.Fprintf(b, "Механика письма — житель против донора:\n\n")
	fmt.Fprintf(b, "| | житель | донор |\n|---|---:|---:|\n")
	fmt.Fprintf(b, "| знаков в реплике (медиана) | %d | %d |\n", got.Runes.Median, want.Runes.Median)
	fmt.Fprintf(b, "| знаков, p90 | %d | %d |\n", got.Runes.P90, want.Runes.P90)
	fmt.Fprintf(b, "| слов в предложении (медиана) | %d | %d |\n", got.SentWords.Median, want.SentWords.Median)
	fmt.Fprintf(b, "| доля реплик со смайлом | %.2f | %.2f |\n", got.EmojiRate, want.EmojiRate)
	b.WriteString("\n")
}

// writeSample называет, насколько выборка тредов типична для донора.
//
// Без этой строки полнота нечитаема. Калибровка идёт по тредам, где донор
// говорил не меньше нескольких раз, — то есть по верхнему хвосту его
// разговорчивости, — а потолок реплик на тред взят по всему архиву. Разойдись
// они сильно, и полнота упрётся в потолок, ничего не сказав о догадке: у
// Полынь-Травы 28.08.2026 потолок 5 стоял против 68–107 реплик в отобранных
// тредах, и выше 7 % полнота не поднялась бы ни при какой настройке кубика.
func writeSample(b *strings.Builder, a ActorReport) {
	if len(a.Runs) == 0 {
		return
	}
	lo, hi := a.Runs[0].Mine, a.Runs[0].Mine
	for _, r := range a.Runs[1:] {
		lo, hi = min(lo, r.Mine), max(hi, r.Mine)
	}
	fmt.Fprintf(b, "Выборка: в отобранных тредах донор написал %d–%d реплик", lo, hi)
	if a.Load.Median > 0 {
		fmt.Fprintf(b, "; по всему архиву у него медиана %d на тред, p90 — %d",
			a.Load.Median, a.Load.P90)
	}
	b.WriteString(".\n\n")
}
