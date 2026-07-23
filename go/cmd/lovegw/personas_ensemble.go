package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"lovegw/internal/archive"
)

// personasEnsemble — ансамблевая склейка: направленный ранг стиля, подкреплённый
// временным паттерном и пересечением круга собеседников. Пишет пары в
// alias_candidates(signal=ensemble); дальше их подхватывает `personas cluster`.
func personasEnsemble(ctx context.Context, ar *archive.Store, p archive.EnsembleParams, outDir string) error {
	st, err := ar.ClusterEnsemble(ctx, p, time.Now())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	reportPath := filepath.Join(outDir, "ensemble.json")
	if err := writeJSONFile(reportPath, st.Pairs); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "ensemble: стиль-кандидатов %d, записано %d (floor %.2f) → %s\n",
		st.StyleCandidates, st.Written, p.Floor, reportPath)
	for i, r := range st.Pairs {
		if i >= 15 {
			break
		}
		fmt.Fprintf(os.Stderr, "  %.2f  %s(%d) ↔ %s(%d)  [%s]\n",
			r.Score, r.NameA, r.A, r.NameB, r.B, r.Evidence)
	}
	return nil
}
