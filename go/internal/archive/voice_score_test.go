package archive

import (
	"context"
	"strings"
	"testing"
)

// TestRankTextScoresMatchesAttributeText — страховка рефакторинга: ядро
// rankTextScores и штатный путь AttributeText обязаны давать ОДИН ранг автора.
// Оба считают фон по всем профилям (фильтр выключен), поэтому расхождение
// означало бы, что вынос ядра поменял арифметику, а не только форму.
func TestRankTextScoresMatchesAttributeText(t *testing.T) {
	ctx := context.Background()
	s := voiceFixture(t)

	const query = "море горизонт волны закат солнце берег чайки прибой песок "
	for _, genre := range []string{GenreAll, GenreNotes} {
		sa, la, err := s.loadAttributors(ctx, genre)
		if err != nil {
			t.Fatalf("%s: loadAttributors: %v", genre, err)
		}
		sIndex := make(map[int64]int, len(sa.ids))
		for i, id := range sa.ids {
			sIndex[id] = i
		}
		idx, ok := sIndex[1]
		if !ok {
			t.Fatalf("%s: у автора 1 нет стиль-профиля", genre)
		}
		tr, ok := rankTextScores(query, sa, la, 0.5, nil)
		if !ok {
			t.Fatalf("%s: rankTextScores отказался скорить текст", genre)
		}
		got := 1 + countGreater(tr.scores, tr.scores[idx])

		at, err := s.AttributeText(ctx, query, 5, 1, 0.5, 0, 0, genre)
		if err != nil {
			t.Fatalf("%s: AttributeText: %v", genre, err)
		}
		if at.Want == nil {
			t.Fatalf("%s: AttributeText не вернул позицию автора", genre)
		}
		if got != at.Want.Rank {
			t.Errorf("%s: ранг автора разошёлся: ядро=%d, AttributeText=%d", genre, got, at.Want.Rank)
		}
		if want := sa.ids[tr.topIdx]; want != at.Candidates[0].UserID {
			t.Errorf("%s: топ-1 разошёлся: ядро=%d, AttributeText=%d", genre, want, at.Candidates[0].UserID)
		}
	}
}

// TestRankTextScoresFiltersBackground — при включённом фильтре фон считается по
// правдоподобным кандидатам, а не по всем: Z того же текста обязан измениться.
// Это то, чего у scoreNote не было и ради чего ядро принимает candidateFilter.
func TestRankTextScoresFiltersBackground(t *testing.T) {
	ctx := context.Background()
	s := voiceFixture(t)

	sa, la, err := s.loadAttributors(ctx, GenreAll)
	if err != nil {
		t.Fatal(err)
	}
	// minAuthorNotes=1 выкидывает чистых комментаторов (у нас это автор 4).
	cf, err := s.buildCandidateFilter(ctx, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !cf.on() {
		t.Fatal("фильтр кандидатов не включился")
	}
	const query = "море горизонт волны закат солнце берег "
	full, ok := rankTextScores(query, sa, la, 0.5, nil)
	if !ok {
		t.Fatal("rankTextScores(nil) отказался")
	}
	filtered, ok := rankTextScores(query, sa, la, 0.5, cf)
	if !ok {
		t.Fatal("rankTextScores(cf) отказался")
	}
	if full.at.StyleCosMean == filtered.at.StyleCosMean {
		t.Errorf("фон стиля не изменился при сужении популяции: %v", full.at.StyleCosMean)
	}
}

// TestRankTextScoresRejectsTinyText — текст короче трёх символов скорить нечем.
func TestRankTextScoresRejectsTinyText(t *testing.T) {
	ctx := context.Background()
	s := voiceFixture(t)
	sa, la, err := s.loadAttributors(ctx, GenreAll)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rankTextScores("!", sa, la, 0.5, nil); ok {
		t.Error("ядро приняло текст короче трёх символов")
	}
}

// voiceFixture — крошечный архив: четыре автора с разными словарями, у одного
// (4) только комментарии — он проверяет жанровые срезы и фильтр кандидатов.
func voiceFixture(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s := openTemp(t)
	rep := func(sn string, n int) string { return strings.Repeat(sn, n) }

	sea := "закат море горизонт волны песок чайки прибой солнце берег "
	town := "ипотека кредит банк ставка процент квартира ремонт документы "
	weather := "погода дождь ветер облака туман иней слякоть морось "
	chat := "лол кек ахах ору ржу жиза кринж рофл имхо збс "

	users := []User{{ID: 1, Name: "Море"}, {ID: 2, Name: "Быт"}, {ID: 3, Name: "Погода"}, {ID: 4, Name: "Болтун"}}
	saves := []struct {
		note     Note
		comments []Comment
	}{
		{Note{ID: 1, AuthorID: 1, Text: rep(sea, 5)}, []Comment{
			{ID: 10, NoteID: 1, AuthorID: 4, Text: rep(chat, 6)},
			{ID: 11, NoteID: 1, AuthorID: 2, Text: rep(town, 3)},
		}},
		{Note{ID: 2, AuthorID: 1, Text: rep(sea, 4)}, nil},
		{Note{ID: 3, AuthorID: 2, Text: rep(town, 5)}, nil},
		{Note{ID: 4, AuthorID: 3, Text: rep(weather, 5)}, nil},
	}
	for _, sv := range saves {
		if _, err := s.SaveGrab(ctx, sv.note, sv.comments, users, testNow); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range []string{GenreAll, GenreNotes} {
		if _, err := s.BuildStyleProfiles(ctx, 20, 256, g, testNow); err != nil {
			t.Fatal(err)
		}
		if _, err := s.BuildLexisProfiles(ctx, 5, 512, g, testNow); err != nil {
			t.Fatal(err)
		}
	}
	return s
}
