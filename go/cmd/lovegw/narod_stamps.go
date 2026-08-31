package main

// narod stamps — ЧЕМ НАША РЕЧЬ ВЫДАЁТ СЕБЯ НА УРОВНЕ СЛОВ.
//
// Стенд под замер 01.09.2026: у модели оказался узкий набор связок, один на всех
// шестидесяти жителей, и говорит она ими в разы чаще живых (складно ×730,
// вслух ×50, будто ×20). Список штампов из этого замера — ПОЛ, а не потолок:
// после каждой правки промпта почерк уезжает в другие слова, и снимать свод надо
// заново. Поэтому он команда, а не разовый скрипт.
//
// Ни в сеть, ни в Postgres не ходит: архив читается, наши реплики подаются
// файлами, — значит гонять её можно рядом с работающим демоном и сколько угодно.
// Реплики подаются ПО ФАЙЛУ НА ТРЕД: слово, живущее в одном треде, — это тема,
// а не почерк, и без разбиения это правило невыразимо.
//
// ЧИТАТЬ ОТЧЁТ РУКАМИ, а не переносить его в список целиком. Свод ловит ЛЮБОЕ
// расхождение с корпусом, включая наши же нарочные: «тока» вместо «только» —
// это InjectErrors, характерная ошибка жителя, и запрещать её значило бы
// отменять замер соседнего слоя. Решает человек, глядя на слово.

import (
	"context"
	"fmt"
	"os"
	"strings"

	"lovegw/internal/archive"
	"lovegw/internal/speech"
)

type stampsOpts struct {
	dbPath string
	corpus int
	files  string // по файлу на тред, через запятую
}

func narodStamps(ctx context.Context, o stampsOpts) error {
	paths := splitList(o.files)
	if len(paths) < 2 {
		return fmt.Errorf("narod stamps: нужно не меньше двух файлов (по одному на тред) — " +
			"иначе тему не отличить от почерка")
	}
	var threads [][]string
	for _, p := range paths {
		texts, err := readSpeechFile(p)
		if err != nil {
			return err
		}
		threads = append(threads, texts)
	}

	st, err := archive.Open(ctx, o.dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	corpus, err := st.CorpusTexts(ctx, o.corpus)
	if err != nil {
		return err
	}

	res := speech.Sweep(threads, corpus)
	os.Stdout.WriteString(stampsReport(res, paths))
	return nil
}

func stampsReport(r speech.SweepResult, paths []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "наши: %d реплик в %d тредах (%d слов), корпус: %d слов\n",
		r.Texts, len(paths), r.OurWords, r.CorpWords)
	fmt.Fprintf(&b, "полоса сравнения по длине: %d–%d слов в реплике\n\n", r.LoWords, r.HiWords)

	fmt.Fprintf(&b, "ДОЛЯ РЕПЛИК С ИЗВЕСТНЫМ ШТАМПОМ\n")
	fmt.Fprintf(&b, "  у нас   %5.1f %%\n", 100*r.OurStamp)
	fmt.Fprintf(&b, "  у живых %5.1f %%   (реплики той же длины)\n", 100*r.HumanStamp)
	// Отношение, а не разность: именно во столько раз надо ужаться, и именно оно
	// сравнимо между прогонами, когда длина реплик поедет.
	if r.HumanStamp > 0 {
		fmt.Fprintf(&b, "  отрыв   %5.1fx\n", r.OurStamp/r.HumanStamp)
	}

	fmt.Fprintf(&b, "\nКАНДИДАТЫ (отрыв от корпуса, частота на 1000 слов)\n")
	if len(r.Words) == 0 {
		b.WriteString("  нет — ни одно слово не оторвалось от корпуса вчетверо\n")
	}
	fmt.Fprintf(&b, "  %-14s %8s %9s %8s %6s\n", "слово", "у нас", "корпус", "во ск.", "раз")
	for _, w := range r.Words {
		mark := ""
		if w.Known {
			mark = "  (уже в списке)"
		}
		fmt.Fprintf(&b, "  %-14s %8.2f %9.2f %7.1fx %5d%s\n",
			w.Word, w.Ours, w.Corpus, w.Times, w.Count, mark)
	}

	// ПОЧТИ НЕТ В КОРПУСЕ — раздел сильнее предыдущего, и отношения в нём не
	// печатается намеренно: делить на десяток вхождений значит выдать случайное
	// число за замер.
	fmt.Fprintf(&b, "\nПОЧТИ НЕТ В КОРПУСЕ (реже %d раз на %d слов — отрыв не считается)\n",
		speech.SweepMinCorpus, r.CorpWords)
	for _, w := range r.Rare {
		mark := ""
		if w.Known {
			mark = "  (уже в списке)"
		}
		fmt.Fprintf(&b, "  %-14s %8.2f %9.2f %13d%s\n", w.Word, w.Ours, w.Corpus, w.Count, mark)
	}

	// КОНТРОЛЬ печатается всегда и не по вежливости: систематический сдвиг
	// сравнения виден только по словам, где мы НЕ переигрываем, — первый прогон
	// поймался именно на них. Пустой контроль означает, что верить своду нельзя.
	fmt.Fprintf(&b, "\nКОНТРОЛЬ (обычные слова, где мы не переигрываем)\n")
	if len(r.Control) == 0 {
		b.WriteString("  ПУСТО — сравнению верить нельзя: завышено всё подряд\n")
	}
	for _, w := range r.Control {
		fmt.Fprintf(&b, "  %-14s %8.2f %9.2f %7.1fx %5d\n", w.Word, w.Ours, w.Corpus, w.Times, w.Count)
	}
	return b.String()
}
