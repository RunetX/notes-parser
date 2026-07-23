package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"lovegw/internal/archive"
)

// styloOpts — параметры действия stylometry (из флагов cmdPersonas).
type styloOpts struct {
	minChars  int
	dims      int
	minCosine float64
	topK      int
	maxPairs  int
}

// personasStylometry — Фаза 2b: склейка альтов по стилю письма (для тех, кто
// сменил фото). Под-действие: build (построить профили) или cluster (склеить
// похожих → alias_candidates(stylometry)). Сигнал слабый и шумный (все пишут
// короткий неформальный русский на одну тему) — только для ревью.
func personasStylometry(ctx context.Context, ar *archive.Store, args []string, opt styloOpts) error {
	if len(args) < 1 {
		return fmt.Errorf("personas stylometry: нужно под-действие (build|cluster)")
	}
	switch sub := args[0]; sub {
	case "build":
		return stylometryBuild(ctx, ar, opt)
	case "cluster":
		return stylometryCluster(ctx, ar, opt)
	default:
		return fmt.Errorf("personas stylometry: неизвестное под-действие %q (build|cluster)", sub)
	}
}

// stylometryBuild строит стилометрические профили авторов (один проход по всем
// комментариям). Тяжёлая операция (десятки секунд — минуты на 10-млн корпусе).
func stylometryBuild(ctx context.Context, ar *archive.Store, opt styloOpts) error {
	fmt.Fprintf(os.Stderr, "stylometry build: строю профили (min-chars=%d, dims=%d)…\n", opt.minChars, opt.dims)
	start := time.Now()
	st, err := ar.BuildStyleProfiles(ctx, opt.minChars, opt.dims, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "stylometry build: авторов просмотрено %d, профилей построено %d за %s\n",
		st.Authors, st.Eligible, time.Since(start).Round(time.Second))
	return nil
}

// stylometryCluster ищет похожих по стилю и пишет пары в alias_candidates(stylometry).
func stylometryCluster(ctx context.Context, ar *archive.Store, opt styloOpts) error {
	st, err := ar.ClusterStylometry(ctx, opt.minCosine, opt.topK, opt.maxPairs, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr,
		"stylometry cluster: профилей=%d, пар записано=%d (min-cosine=%.2f, top-k=%d)\n",
		st.Profiles, st.Pairs, opt.minCosine, opt.topK)
	for i, p := range st.Top {
		na := userLabel(ctx, ar, p.A)
		nb := userLabel(ctx, ar, p.B)
		fmt.Fprintf(os.Stderr, "  %2d. cos=%.3f  %s(%d) ↔ %s(%d)\n", i+1, p.Cosine, na, p.A, nb, p.B)
	}
	fmt.Fprintln(os.Stderr, "дальше: `personas cluster` — склейка всех сигналов в личности")
	return nil
}

// userLabel — имя пользователя для отчёта (пустое → "?").
func userLabel(ctx context.Context, ar *archive.Store, id int64) string {
	if n, ok := ar.UserName(ctx, id); ok && n != "" {
		return n
	}
	return "?"
}
