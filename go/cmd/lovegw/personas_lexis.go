package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"lovegw/internal/archive"
)

// lexisOpts — параметры действия lexis (из флагов cmdPersonas).
type lexisOpts struct {
	minTokens int
	dims      int
}

// personasLexis — лексический слой атрибуции (TF-IDF по словам). Под-действие:
// build (построить lexis_profiles + глобальный IDF). Сигнал темы/лексикона,
// дополняющий стилометрию в `personas attribute`.
func personasLexis(ctx context.Context, ar *archive.Store, args []string, opt lexisOpts) error {
	if len(args) < 1 {
		return fmt.Errorf("personas lexis: нужно под-действие (build)")
	}
	switch sub := args[0]; sub {
	case "build":
		return lexisBuild(ctx, ar, opt)
	default:
		return fmt.Errorf("personas lexis: неизвестное под-действие %q (build)", sub)
	}
}

// lexisBuild строит лексические TF-IDF-профили авторов (один проход по всему
// тексту). Тяжёлая операция (десятки секунд — минуты на большом корпусе).
func lexisBuild(ctx context.Context, ar *archive.Store, opt lexisOpts) error {
	fmt.Fprintf(os.Stderr, "lexis build: строю TF-IDF-профили (min-tokens=%d, lex-dims=%d)…\n", opt.minTokens, opt.dims)
	start := time.Now()
	st, err := ar.BuildLexisProfiles(ctx, opt.minTokens, opt.dims, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "lexis build: авторов просмотрено %d, профилей построено %d за %s\n",
		st.Authors, st.Eligible, time.Since(start).Round(time.Second))
	fmt.Fprintln(os.Stderr, "дальше: `personas attribute` — скоринг авторства (стиль + лексика)")
	return nil
}
