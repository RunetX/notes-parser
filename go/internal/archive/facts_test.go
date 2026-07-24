package archive

import (
	"context"
	"strings"
	"testing"
)

func TestTopicCompileBoundaries(t *testing.T) {
	topics := DefaultTopics()
	byKey := map[string]TopicLexicon{}
	for _, tp := range topics {
		byKey[tp.Key] = tp
	}
	cases := []struct {
		topic string
		text  string
		want  bool
	}{
		// границы слов: точная форма не должна ловить надстроки
		{"cats", "который час", false},
		{"cats", "у меня кот Барсик", true},
		{"cats", "КОТ спит", true}, // (?i) и для кириллицы
		{"cats", "кошка на окошке", true},
		{"dogs", "психолог сказал", false},
		{"dogs", "мой пёс лает", true},
		{"dogs", "собаку выгуливаю", true}, // префикс собак*
		{"dogs", "особакненный", false},    // «собак» внутри слова — не считается
		{"dacha", "у нас задача простая", false},
		{"dacha", "передача была интересная", false},
		{"dacha", "поехали на дачу", true},
		{"dacha", "дачный сезон открыт", true},
		{"sea", "на море хочу", true},
		{"sea", "уморительно смешно", false},
		{"cars", "машина сломалась", true},
		{"cars", "за рулём с утра", true},
		{"kids", "детально разобрали", false},
		{"kids", "дети выросли", true},
	}
	for _, c := range cases {
		re, err := byKey[c.topic].compile()
		if err != nil {
			t.Fatalf("compile %s: %v", c.topic, err)
		}
		if got := re.MatchString(c.text); got != c.want {
			t.Errorf("%s: %q → %v, want %v", c.topic, c.text, got, c.want)
		}
	}
}

func TestDecidePolarity(t *testing.T) {
	cases := []struct {
		likes, dislikes, owns int
		want                  string
	}{
		{0, 0, 0, PolarityMentions},
		{1, 0, 0, PolarityMentions}, // одиночный маркер — шум
		{2, 0, 0, PolarityLikes},
		{0, 2, 0, PolarityDislikes},
		{2, 3, 0, PolarityDislikes}, // явное «не люблю» перевешивает
		{3, 2, 0, PolarityLikes},
		{0, 0, 2, PolarityOwns},
		{2, 2, 0, PolarityMentions}, // ничья — не решаем
	}
	for _, c := range cases {
		if got := decidePolarity(c.likes, c.dislikes, c.owns); got != c.want {
			t.Errorf("decidePolarity(%d,%d,%d) = %s, want %s", c.likes, c.dislikes, c.owns, got, c.want)
		}
	}
}

func TestSentenceAround(t *testing.T) {
	text := "Первое предложение. Я люблю собак очень сильно! Третье предложение."
	loc := strings.Index(text, "собак")
	got := sentenceAround(text, loc, loc+len("собак"))
	if got != "Я люблю собак очень сильно!" {
		t.Errorf("sentenceAround: got %q", got)
	}
	// без границ предложения — весь текст, UTF-8 не порван
	short := "просто собаки без знаков"
	loc = strings.Index(short, "собаки")
	if got := sentenceAround(short, loc, loc+len("собаки")); got != short {
		t.Errorf("sentenceAround без границ: got %q", got)
	}
}

func TestScanFactsAggregation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	users := []User{{ID: 1, Name: "Дог-фан"}, {ID: 2, Name: "Прохожий"}}
	// Пользователь 1: собаки в двух разных заметках с маркерами «люблю».
	if _, err := s.SaveGrab(ctx, Note{ID: 100, AuthorID: 2, Text: "про погоду"}, []Comment{
		{ID: 1, NoteID: 100, AuthorID: 1, Text: "Я люблю собак. Обожаю щенков!"},
		{ID: 2, NoteID: 100, AuthorID: 1, Text: "Собаки лучше всех, люблю их"},
		{ID: 3, NoteID: 100, AuthorID: 2, Text: "который час, кстати?"}, // не кошки
	}, users, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveGrab(ctx, Note{ID: 101, AuthorID: 2, Text: "ещё заметка"}, []Comment{
		{ID: 4, NoteID: 101, AuthorID: 1, Text: "гуляли с собакой в парке"},
	}, users, testNow); err != nil {
		t.Fatal(err)
	}

	st, err := s.ScanFacts(ctx, DefaultTopics(), FactScanParams{MinHits: 3, MinNotes: 2, EvidencePer: 5}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if st.Comments != 4 || st.Notes != 2 {
		t.Errorf("просмотрено: comments=%d notes=%d, want 4/2", st.Comments, st.Notes)
	}
	if st.Rows != 1 {
		t.Fatalf("фактов записано %d, want 1 (только dogs у u1)", st.Rows)
	}

	facts, err := s.IdentityFacts(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("фактов у u1: %d, want 1", len(facts))
	}
	f := facts[0]
	if f.Topic != "dogs" || f.Hits != 3 || f.NotesCount != 2 {
		t.Errorf("факт: %+v, want dogs hits=3 notes=2", f)
	}
	if f.Polarity != PolarityLikes {
		t.Errorf("полярность %s, want likes (два маркера «люблю»)", f.Polarity)
	}
	if len(f.Evidence) == 0 || f.Evidence[0].Quote == "" {
		t.Error("нет цитат-обоснований")
	}

	// «который» не должен дать кошек прохожему
	if n := count(t, s, "SELECT COUNT(*) FROM identity_facts WHERE identity='u2'"); n != 0 {
		t.Errorf("у u2 фактов %d, want 0", n)
	}

	// идемпотентность: повторный скан не задваивает
	if _, err := s.ScanFacts(ctx, DefaultTopics(), FactScanParams{MinHits: 3, MinNotes: 2, EvidencePer: 5}, testNow); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM identity_facts"); n != 1 {
		t.Errorf("после повторного скана фактов %d, want 1", n)
	}
}

func TestImportFactsAndBestSource(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	users := []User{{ID: 1, Name: "u1"}}
	if _, err := s.SaveGrab(ctx, Note{ID: 100, AuthorID: 1, Text: "n"}, []Comment{
		{ID: 1, NoteID: 100, AuthorID: 1, Text: "собака собака"},
	}, users, testNow); err != nil {
		t.Fatal(err)
	}
	// lexicon-строка (низкие пороги, чтобы записалась)
	if _, err := s.ScanFacts(ctx, DefaultTopics(), FactScanParams{MinHits: 1, MinNotes: 1, EvidencePer: 3}, testNow); err != nil {
		t.Fatal(err)
	}

	st, err := s.ImportFacts(ctx, []FactImport{
		{Identity: "u1", Topic: "dogs", Polarity: PolarityLikes, Confidence: 0.9,
			Evidence: []FactEvidence{{CommentID: 1, NoteID: 100, Quote: "собака собака"}}},
		{Identity: "u1", Topic: "cats", Verdict: "reject"},                     // отклонено разметкой
		{Identity: "u404", Topic: "sea", Polarity: PolarityOwns},               // личности нет, ремапа нет
		{Identity: "uСтарый", AccountIDs: []int64{1}, Topic: "sea", Polarity: PolarityOwns}, // ремап по анкете
		{Identity: "u1", Topic: "dogs", Polarity: "чушь"},                      // кривая полярность
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if st.Written != 2 || st.Rejected != 1 || st.Skipped != 2 || st.Remapped != 1 {
		t.Errorf("итог импорта %+v, want written=2 rejected=1 skipped=2 remapped=1", st)
	}

	// llm перекрывает lexicon по той же теме; hits наследуются от lexicon-строки
	facts, err := s.IdentityFacts(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	byTopic := map[string]IdentityFact{}
	for _, f := range facts {
		byTopic[f.Topic] = f
	}
	dogs := byTopic["dogs"]
	if dogs.Source != FactSourceLLM || dogs.Polarity != PolarityLikes {
		t.Errorf("dogs: source=%s polarity=%s, want llm/likes", dogs.Source, dogs.Polarity)
	}
	if dogs.Hits != 1 {
		t.Errorf("dogs hits=%d, want 1 (унаследовано от lexicon)", dogs.Hits)
	}
	if sea := byTopic["sea"]; sea.Source != FactSourceLLM {
		t.Errorf("sea после ремапа: source=%s, want llm", sea.Source)
	}
}

func TestRejectRemovesFact(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	users := []User{{ID: 1, Name: "u1"}}
	if _, err := s.SaveGrab(ctx, Note{ID: 100, AuthorID: 1, Text: "n"}, []Comment{
		{ID: 1, NoteID: 100, AuthorID: 1, Text: "собака кошка"},
	}, users, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ScanFacts(ctx, DefaultTopics(), FactScanParams{MinHits: 1, MinNotes: 1, EvidencePer: 3}, testNow); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM identity_facts WHERE identity='u1' AND topic='dogs'"); n != 1 {
		t.Fatalf("до reject: dogs=%d, want 1 (lexicon)", n)
	}
	// reject должен снять ложный факт целиком (не оставить lexicon как fallback)
	st, err := s.ImportFacts(ctx, []FactImport{{Identity: "u1", Topic: "dogs", Verdict: "reject"}}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if st.Rejected != 1 {
		t.Errorf("Rejected=%d, want 1", st.Rejected)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM identity_facts WHERE identity='u1' AND topic='dogs'"); n != 0 {
		t.Errorf("после reject: dogs=%d, want 0 (факт удалён)", n)
	}
	// cats не тронут
	if n := count(t, s, "SELECT COUNT(*) FROM identity_facts WHERE identity='u1' AND topic='cats'"); n != 1 {
		t.Errorf("cats задет reject'ом: %d, want 1", n)
	}
}
