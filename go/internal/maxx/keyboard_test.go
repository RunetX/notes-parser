package maxx

import (
	"context"
	"testing"

	"lovegw/internal/kbd"
)

// attachments достаёт вложения из сырого тела запроса.
func attachments(raw map[string]any) []any {
	list, _ := raw["attachments"].([]any)
	return list
}

// firstButtons — кнопки первой строки первой inline-клавиатуры в теле.
func firstButtons(t *testing.T, raw map[string]any) []any {
	t.Helper()
	for _, a := range attachments(raw) {
		att, _ := a.(map[string]any)
		if att["type"] != "inline_keyboard" {
			continue
		}
		payload, _ := att["payload"].(map[string]any)
		rows, _ := payload["buttons"].([]any)
		if len(rows) == 0 {
			t.Fatalf("клавиатура без строк: %v", att)
		}
		row, _ := rows[0].([]any)
		return row
	}
	return nil
}

func TestSendKeyboardRequest(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)

	m.SendKeyboard(context.Background(), 42, "Выберите",
		kbd.New().Row(
			kbd.Button{Text: "От своего имени", Payload: kbd.Pack("note", "own")},
			kbd.Button{Text: "Анонимно", Payload: kbd.Pack("note", "anon")},
		))

	sent := f.last()
	if sent.userID != "42" {
		t.Errorf("user_id: %q", sent.userID)
	}
	if sent.body.Text != "Выберите" {
		t.Errorf("текст: %q", sent.body.Text)
	}
	btns := firstButtons(t, sent.rawBody)
	if len(btns) != 2 {
		t.Fatalf("кнопок в строке: %d (%v)", len(btns), btns)
	}
	first, _ := btns[0].(map[string]any)
	if first["type"] != "callback" {
		t.Errorf("тип кнопки: %v", first["type"])
	}
	if first["payload"] != "1:note:own" {
		t.Errorf("payload: %v", first["payload"])
	}
	if first["text"] != "От своего имени" {
		t.Errorf("подпись: %v", first["text"])
	}
}

func TestAnswerCallbackRequest(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)
	ctx := context.Background()

	m.AnswerCallback(ctx, kbd.Callback{AnswerID: "cb-1"}, "Публикую…")
	m.AnswerCallback(ctx, kbd.Callback{AnswerID: "cb-2"}, "")

	if len(f.answers) != 2 {
		t.Fatalf("ответов на нажатия: %d", len(f.answers))
	}
	if f.answers[0].callbackID != "cb-1" {
		t.Errorf("callback_id: %q", f.answers[0].callbackID)
	}
	if f.answers[0].rawBody["notification"] != "Публикую…" {
		t.Errorf("notification: %v", f.answers[0].rawBody["notification"])
	}
	// Пустой toast не должен превращаться в пустую плашку.
	if _, ok := f.answers[1].rawBody["notification"]; ok {
		t.Errorf("молчаливый ответ не должен нести notification: %v", f.answers[1].rawBody)
	}
}

func TestEditMessageRequest(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)
	ctx := context.Background()

	m.EditMessage(ctx, 42, "mid.777", "Готово", nil)
	m.EditMessage(ctx, 42, "mid.778", "Ещё раз?",
		kbd.New().Row(kbd.Button{Text: "Опубликовать", Payload: kbd.Pack("news", "20260804-193012")}))

	if len(f.edits) != 2 {
		t.Fatalf("правок: %d", len(f.edits))
	}
	if f.edits[0].messageID != "mid.777" || f.edits[0].body.Text != "Готово" {
		t.Errorf("первая правка: %+v", f.edits[0])
	}
	// Снятие кнопок — правка с пустым списком вложений.
	if len(attachments(f.edits[0].rawBody)) != 0 {
		t.Errorf("kb == nil должен убирать клавиатуру: %v", f.edits[0].rawBody["attachments"])
	}
	btns := firstButtons(t, f.edits[1].rawBody)
	if len(btns) != 1 {
		t.Fatalf("кнопок во второй правке: %d", len(btns))
	}
	if b, _ := btns[0].(map[string]any); b["payload"] != "1:news:20260804-193012" {
		t.Errorf("payload кнопки повтора: %v", b["payload"])
	}
}
