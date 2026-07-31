package digest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeGen — фейковый JSONGenerator.
type fakeGen struct {
	resp   map[string]string
	err    error
	system string
	prompt string
}

func (f *fakeGen) GenerateJSON(_ context.Context, system, prompt string, _ map[string]any) ([]byte, error) {
	f.system, f.prompt = system, prompt
	if f.err != nil {
		return nil, f.err
	}
	return json.Marshal(f.resp)
}

func fullEditorial() map[string]string {
	return map[string]string{
		"week_summary":    "Неделя прошла в спорах о <i>вечном</i>.",
		"dispute_note_id": "555",
		"dispute":         "Сыр-бор разгорелся из-за пустяка, но температура держалась до утра.",
		"quote":           "Лучше всех неделю подытожил один комментатор: «яркая мысль» — <i>Некто</i>.",
		"topics":          "Пришло: осень.\nУшло: лето.",
	}
}

func TestGenerateEditorialFillsDraft(t *testing.T) {
	is := sampleIssue()
	gen := &fakeGen{resp: fullEditorial()}
	ed, err := GenerateEditorial(context.Background(), gen, is)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gen.prompt, "Кандидат: заметка 555") {
		t.Error("промпт должен содержать материалы выпуска")
	}
	if !strings.Contains(gen.system, "ирония") {
		t.Error("системный промпт должен задавать тон")
	}
	is.Editorial = ed

	text := draftText(t, is)
	if strings.Contains(text, llmMark) {
		t.Error("в заполненном черновике не должно остаться плейсхолдеров")
	}
	for _, want := range []string{"вечном", "Сыр-бор", "яркая мысль", "Пришло: осень."} {
		if !strings.Contains(text, want) {
			t.Errorf("в черновике нет текста рубрики %q", want)
		}
	}
	// Заполненный черновик сразу публикуем: strict-парс проходит.
	if _, err := ParseDraft(strings.NewReader(text), true); err != nil {
		t.Fatalf("заполненный черновик должен парситься strict: %v", err)
	}
}

func TestGenerateEditorialDisputeFallback(t *testing.T) {
	is := sampleIssue()
	resp := fullEditorial()
	resp["dispute_note_id"] = "999" // LLM назвал заметку не из кандидатов
	ed, err := GenerateEditorial(context.Background(), &fakeGen{resp: resp}, is)
	if err != nil {
		t.Fatal(err)
	}
	if ed.DisputeNoteID != "" {
		t.Errorf("чужой id должен сбрасываться: %q", ed.DisputeNoteID)
	}
	is.Editorial = ed
	if got := disputePick(is).Note.ID; got != "555" {
		t.Errorf("маркер спора должен откатиться на главного кандидата: %q", got)
	}
}

func TestGenerateEditorialRejectsBadMarkup(t *testing.T) {
	is := sampleIssue()
	resp := fullEditorial()
	resp["quote"] = `цитата с <u>чужим тегом</u>`
	if _, err := GenerateEditorial(context.Background(), &fakeGen{resp: resp}, is); err == nil {
		t.Fatal("невалидная разметка должна быть ошибкой")
	}
	resp = fullEditorial()
	resp["week_summary"] = ""
	if _, err := GenerateEditorial(context.Background(), &fakeGen{resp: resp}, is); err == nil {
		t.Fatal("пустая сводка недели должна быть ошибкой")
	}
}

func TestGenerateEditorialErrorPropagates(t *testing.T) {
	wantErr := errors.New("сеть моргнула")
	_, err := GenerateEditorial(context.Background(), &fakeGen{err: wantErr}, sampleIssue())
	if !errors.Is(err, wantErr) {
		t.Fatalf("ошибка генератора должна пробрасываться: %v", err)
	}
}
