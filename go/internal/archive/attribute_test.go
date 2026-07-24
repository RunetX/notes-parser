package archive

import (
	"context"
	"testing"
)

func TestAttributeText(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	users := []User{{ID: 1, Name: "A"}, {ID: 2, Name: "B"}, {ID: 3, Name: "C"}}
	comments := []Comment{
		{ID: 10, NoteID: 1, AuthorID: 1, Text: "виджет фробникатор блёстки виджет фробникатор блёстки виджет"},
		{ID: 11, NoteID: 1, AuthorID: 2, Text: "квантовый поток конденсатор реактор плазма квантовый поток энергия"},
		{ID: 12, NoteID: 1, AuthorID: 3, Text: "быстрая бурая лиса прыгает через ленивого пса и спит днём"},
	}
	if _, err := s.SaveGrab(ctx, Note{ID: 1, AuthorID: 1, Text: "n"}, comments, users, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildStyleProfiles(ctx, 20, 128, testNow); err != nil {
		t.Fatal(err)
	}

	// Без лексического слоя — только стиль.
	at, err := s.AttributeText(ctx, "снова виджет и фробникатор в блёстках", 2, 0, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if at.StyleProfiles != 3 {
		t.Fatalf("стиль-профилей: got %d, want 3", at.StyleProfiles)
	}
	if at.LexProfiles != 0 {
		t.Fatalf("лексический слой не строился, ожидалось LexProfiles=0, got %d", at.LexProfiles)
	}
	if len(at.Candidates) != 2 {
		t.Fatalf("кандидатов: got %d, want 2", len(at.Candidates))
	}
	if got := at.Candidates[0]; got.UserID != 1 || got.Name != "A" || got.Rank != 1 {
		t.Errorf("топ-кандидат: got %+v, want анкета 1 (стиль A) с рангом 1", got)
	}
	if at.Candidates[0].HasLex {
		t.Error("без лексического слоя HasLex должен быть false")
	}
	if at.Candidates[0].Score <= at.Candidates[1].Score {
		t.Error("кандидаты не отсортированы по убыванию скора")
	}
	if at.Candidates[0].Ngrams == 0 {
		t.Error("объём стиль-профиля топ-кандидата не заполнен")
	}

	// Режим валидации: позиция известного автора.
	at, err = s.AttributeText(ctx, "квантовый реактор и снова плазма поток", 1, 2, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if at.Want == nil || at.Want.UserID != 2 {
		t.Fatalf("want: %+v, ожидался автор 2", at.Want)
	}
	if at.Want.Rank != 1 {
		t.Errorf("ранг автора 2: got %d, want 1", at.Want.Rank)
	}

	// Автор без профиля → Want остаётся пустым, ошибки нет.
	at, err = s.AttributeText(ctx, "квантовый реактор и снова плазма поток", 1, 999, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if at.Want != nil {
		t.Errorf("want для анкеты без профиля: got %+v, want nil", at.Want)
	}

	if _, err := s.AttributeText(ctx, "   ", 5, 0, 0.5); err == nil {
		t.Error("пустой текст должен вернуть ошибку")
	}
}

// TestAttributeWithLexis — после lexis build оба сигнала участвуют, лексика
// уводит выбор к автору с совпадающими словами.
func TestAttributeWithLexis(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	users := []User{{ID: 1, Name: "A"}, {ID: 2, Name: "B"}, {ID: 3, Name: "C"}}
	comments := []Comment{
		{ID: 10, NoteID: 1, AuthorID: 1, Text: "виджет фробникатор блёстки виджет фробникатор блёстки виджет карамель"},
		{ID: 11, NoteID: 1, AuthorID: 2, Text: "квантовый поток конденсатор реактор плазма квантовый поток энергия турбина"},
		{ID: 12, NoteID: 1, AuthorID: 3, Text: "быстрая бурая лиса прыгает через ленивого пса и спит днём в норе"},
	}
	if _, err := s.SaveGrab(ctx, Note{ID: 1, AuthorID: 1, Text: "n"}, comments, users, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildStyleProfiles(ctx, 20, 128, testNow); err != nil {
		t.Fatal(err)
	}
	lst, err := s.BuildLexisProfiles(ctx, 5, 256, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if lst.Eligible != 3 {
		t.Fatalf("лексических профилей: got %d, want 3", lst.Eligible)
	}

	// Текст со словами автора B → он на первом месте, с лексическим сигналом.
	at, err := s.AttributeText(ctx, "квантовый реактор плазма турбина конденсатор энергия поток", 3, 0, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if at.LexProfiles != 3 {
		t.Fatalf("лексический слой построен, ожидалось LexProfiles=3, got %d", at.LexProfiles)
	}
	if at.QueryTokens == 0 {
		t.Error("слов в запросе должно быть >0")
	}
	top := at.Candidates[0]
	if top.UserID != 2 {
		t.Errorf("топ-кандидат по лексике: got %d, want 2 (совпадающие слова)", top.UserID)
	}
	if !top.HasLex {
		t.Error("у топ-кандидата должен быть лексический сигнал")
	}
	if top.LexCos <= 0 {
		t.Errorf("лексический косинус топа: got %.3f, ожидался >0", top.LexCos)
	}
}
