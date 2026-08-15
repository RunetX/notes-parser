package digest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeGen — фейковый JSONGenerator. queue задаёт ответы по попыткам (последний
// повторяется дальше); пусто — всегда resp.
type fakeGen struct {
	resp   map[string]string
	queue  []map[string]string
	calls  int
	err    error
	system string
	prompt string
}

func (f *fakeGen) GenerateJSON(_ context.Context, system, prompt string, _ map[string]any) ([]byte, error) {
	f.system, f.prompt = system, prompt
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	resp := f.resp
	if len(f.queue) > 0 {
		resp = f.queue[min(f.calls-1, len(f.queue)-1)]
	}
	return json.Marshal(resp)
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

// Вырожденный ответ бывает разовым (15.08.2026): переспрос спасает выпуск.
func TestGenerateEditorialRetriesBadAnswer(t *testing.T) {
	empty := fullEditorial()
	empty["week_summary"], empty["topics"] = "", ""
	gen := &fakeGen{queue: []map[string]string{empty, fullEditorial()}}
	ed, err := GenerateEditorial(context.Background(), gen, sampleIssue())
	if err != nil {
		t.Fatal(err)
	}
	if gen.calls != 2 {
		t.Errorf("попыток: %d, ожидались 2", gen.calls)
	}
	if ed.WeekSummary == "" {
		t.Error("рубрики должны прийти со второй попытки")
	}
	// Переспрос несёт причину брака: слепой повтор повторил бы и брак.
	if !strings.Contains(gen.prompt, "Переспрос") || !strings.Contains(gen.prompt, "week_summary пуст") {
		t.Error("во втором промпте должна быть причина брака")
	}
}

func TestGenerateEditorialGivesUpAfterRetries(t *testing.T) {
	empty := fullEditorial()
	empty["week_summary"] = ""
	gen := &fakeGen{resp: empty}
	_, err := GenerateEditorial(context.Background(), gen, sampleIssue())
	if err == nil {
		t.Fatal("исчерпанные попытки должны быть ошибкой")
	}
	if gen.calls != editorialRetries {
		t.Errorf("попыток: %d, ожидалось %d", gen.calls, editorialRetries)
	}
	if !strings.Contains(err.Error(), "week_summary пуст") {
		t.Errorf("ошибка должна называть причину брака: %v", err)
	}
}

func TestGenerateEditorialErrorPropagates(t *testing.T) {
	wantErr := errors.New("сеть моргнула")
	gen := &fakeGen{err: wantErr}
	_, err := GenerateEditorial(context.Background(), gen, sampleIssue())
	if !errors.Is(err, wantErr) {
		t.Fatalf("ошибка генератора должна пробрасываться: %v", err)
	}
	// Ошибку запроса SDK ретраит сам, второй раз спрашивать незачем.
	if gen.calls != 1 {
		t.Errorf("попыток: %d, ожидалась 1", gen.calls)
	}
}
