package main

import (
	"context"
	"fmt"
	"time"

	"lovegw/internal/archive"
)

// personasAddressees пересчитывает слой адресатов: кому реально адресована
// каждая реплика. Без него весь социальный граф считает адресатом автора корня
// ветки, а это верно лишь в трети случаев (дерево комментариев на сайте
// двухуровневое, настоящий адресат — в тексте: «Ник, …»).
//
// Операция разовая и тяжёлая: полный проход по всем ответам архива. Пересчёт
// идемпотентен — обе таблицы слоя строятся с нуля.
func personasAddressees(ctx context.Context, ar *archive.Store) error {
	start := time.Now()
	fmt.Println("Пересчёт слоя адресатов (полный проход по архиву, это долго)")

	st, err := ar.BuildAddressees(ctx, func(stage string) {
		fmt.Printf("  [%6s] %s\n", time.Since(start).Round(time.Second), stage)
	})
	if err != nil {
		return err
	}

	fmt.Printf("\nГотово за %s\n\n", time.Since(start).Round(time.Second))
	fmt.Printf("  ответов в архиве:          %9d\n", st.Replies)
	fmt.Printf("  из них с обращением:       %9d  (%.1f%%)\n",
		st.WithPrefix, pct(st.WithPrefix, st.Replies))
	fmt.Println()
	fmt.Printf("  адресат по ветке:          %9d  (%.1f%%)\n", st.Branch, pct(st.Branch, st.WithPrefix))
	fmt.Printf("  по истории ников, в ветке: %9d  (%.1f%%)\n",
		st.HistoryBranch, pct(st.HistoryBranch, st.WithPrefix))
	fmt.Printf("  по истории ников:          %9d  (%.1f%%)\n", st.History, pct(st.History, st.WithPrefix))
	fmt.Printf("  ИТОГО точных адресатов:    %9d  (%.1f%% обращений)\n",
		st.Resolved(), 100*st.Coverage())
	fmt.Println()
	fmt.Printf("  периодов владения ником:   %9d\n", st.Nicks)
	fmt.Println()
	fmt.Println("Неразрешённый хвост graph-вьюхи по-прежнему относят к автору корня ветки")
	fmt.Println("(COALESCE), так что старое поведение сохраняется там, где точного адресата нет.")
	return nil
}

func pct(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(part) / float64(total)
}
