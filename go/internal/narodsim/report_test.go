package narodsim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleReport() *Report {
	runs := []*SoloRun{
		{NoteID: 500, Replies: 40, Mine: 3, Matrix: Matrix{TP: 2, FP: 5, TN: 30, FN: 1},
			Speech: []Speech{{Quantile: 0.4, Rank: 2}, {Quantile: 0.6, Rank: 4}, {Rejected: "набивка словарём"}},
			Points: []Point{{Spoke: true, Truth: true, After: 5 * time.Minute, TrueAfter: 3 * time.Minute}}},
		{NoteID: 501, Replies: 20, Mine: 2, Matrix: Matrix{TP: 1, FP: 2, TN: 14, FN: 1},
			Speech: []Speech{{Quantile: 0.5, Rank: 3}}, Skipped: 1,
			Points: []Point{{Spoke: true, Truth: true, After: time.Minute, TrueAfter: 2 * time.Minute}}},
	}
	return NewReport("claude-opus-5", 7, time.Date(2026, 8, 28, 15, 0, 0, 0, time.UTC),
		[]ActorReport{{Actor: 498196, Nick: "ДВ", CardID: "u498196", Runs: runs}})
}

// Суммы по слепку складываются из тредов, а отказы и пропуски считаются
// ОТДЕЛЬНО от удач: иначе медиана качества считалась бы по одним удачам.
func TestNewReportAggregates(t *testing.T) {
	rep := sampleReport()
	a := rep.Actors[0]
	if a.Matrix != (Matrix{TP: 3, FP: 7, TN: 44, FN: 2}) {
		t.Errorf("матрица слепка %+v", a.Matrix)
	}
	if a.Speeches != 3 || a.Rejected != 1 || a.Skipped != 1 {
		t.Errorf("реплик %d, отказов %d, пропущено %d", a.Speeches, a.Rejected, a.Skipped)
	}
	if got := a.MedianQuantile(); got != 0.5 {
		t.Errorf("медианный квантиль %v", got)
	}
	if got := a.MedianRank(); got != 3 {
		t.Errorf("медианный ранг %v", got)
	}
	// Ошибки задержки 2 мин и 1 мин: медиана берёт верхнюю.
	if got := a.MedianLatencyError(); got != 2*time.Minute {
		t.Errorf("медианная ошибка задержки %v", got)
	}
}

// Точность решений НИКОГДА не показывается без того, с чем её сравнивают:
// молчун набирает своё, ничего не умея, и без этой строки отчёт хвалил бы
// бездействие.
func TestMarkdownAlwaysShowsSilentBaseline(t *testing.T) {
	md := sampleReport().Markdown()
	if !strings.Contains(md, "«никогда не приходить»") {
		t.Error("в отчёте нет базлайна молчуна рядом с точностью")
	}
	// Считается он верно: TN+FP из 56 точек — это 91 %.
	if !strings.Contains(md, "91 %") {
		t.Errorf("базлайн молчуна посчитан неверно:\n%s", md)
	}
}

func TestMarkdownCarriesStampAndNumbers(t *testing.T) {
	md := sampleReport().Markdown()
	for _, want := range []string{
		"Не публиковать", // марка машинного артефакта
		"claude-opus-5",
		"ДВ (u498196",
		"0.50", // медианный квантиль
		"500", "501",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("в отчёте нет %q:\n%s", want, md)
		}
	}
}

// Пустой прогон отчёт не роняет и не выдумывает чисел.
func TestMarkdownEmptyRun(t *testing.T) {
	rep := NewReport("", 0, time.Now(), []ActorReport{{Actor: 1, Nick: "Никто"}})
	md := rep.Markdown()
	if !strings.Contains(md, "не выдала ни одной реплики") {
		t.Errorf("пустой прогон описан неверно:\n%s", md)
	}
}

func TestWriteSoloReport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "500-solo-20260828")
	if err := WriteSoloReport(dir, sampleReport()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"report.md", "metrics.json", "points.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("нет файла %s: %v", name, err)
		}
	}
	// Временных файлов после себя не оставляем.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("остался временный файл %s", e.Name())
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("metrics.json не читается обратно: %v", err)
	}
	if back.Actors[0].Matrix.TP != 3 || back.Model != "claude-opus-5" {
		t.Errorf("в metrics.json приехало не то: %+v", back.Actors[0].Matrix)
	}
	// В points.jsonl строка на точку решения, и каждая знает свой тред.
	lines := strings.Split(strings.TrimSpace(string(mustRead(t, filepath.Join(dir, "points.jsonl")))), "\n")
	if len(lines) != 2 {
		t.Fatalf("строк в points.jsonl %d, ожидалось 2", len(lines))
	}
	var p struct {
		Actor int64 `json:"actor"`
		Note  int64 `json:"note"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &p); err != nil {
		t.Fatal(err)
	}
	if p.Actor != 498196 || p.Note != 500 {
		t.Errorf("строка точки: %+v", p)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
