package archive

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// TestNoteGenreImprovesAttribution — суть брифа: если автор пишет заметки в
// одном регистре, а комментарии в другом, то all-эталон (смешанный) сравнивает
// заметку-запрос с чужим жанром — системная ошибка. Register-matched note-эталон
// (только заметки) ставит настоящего автора заметки выше.
//
// Сценарий (регистр = общий словарь → общие символьные 3-граммы):
//   - A: заметки в регистре X (природа), комментарии в регистре Z (реакции, много).
//   - B: заметка в регистре W (быт), комментарии в регистре X (много) — его
//     all-профиль «съезжает» в X и притворяется автором заметки-запроса.
//   - C: нейтральный фон.
//
// Запрос — невиданная заметка в регистре X (настоящий автор — A). Под all-эталоном
// контаминированный комментариями B обходит A; под note-эталоном A первый.
func TestNoteGenreImprovesAttribution(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	rep := func(sn string, n int) string { return strings.Repeat(sn, n) }

	// Регистр X — общий пул «заметочных» слов (A-заметки, B-комментарии, запрос
	// берут из него — сильное пересечение 3-грамм).
	xA1 := "закат море горизонт волны песок "
	xA2 := "чайки прибой солнце берег море "
	xQ := "море горизонт волны закат солнце берег "
	xB := "берег песок чайки волны прибой закат море горизонт солнце "
	// Регистр Z — «комментаторский» A: рубленые реакции (свой словарь).
	zA := "лол кек ахах ору ржу жиза кринж рофл имхо збс "
	// Регистр W — заметки B: бытовая лексика (свой словарь).
	wB := "ипотека кредит банк ставка процент квартира ремонт документы "
	// Нейтральный C.
	nC := "погода дождь ветер облака туман иней слякоть морось "

	users := []User{{ID: 1, Name: "A"}, {ID: 2, Name: "B"}, {ID: 3, Name: "C"}}
	// Заметка 1 (A, X) + комментарии: A в Z (много), B в X (много), C нейтр.
	comments := []Comment{
		{ID: 10, NoteID: 1, AuthorID: 1, Text: rep(zA, 8)},
		{ID: 11, NoteID: 1, AuthorID: 2, Text: rep(xB, 8)},
		{ID: 12, NoteID: 1, AuthorID: 3, Text: rep(nC, 4)},
	}
	saves := []struct {
		note     Note
		comments []Comment
	}{
		{Note{ID: 1, AuthorID: 1, Text: rep(xA1, 4)}, comments},
		{Note{ID: 2, AuthorID: 1, Text: rep(xA2, 4)}, nil}, // вторая заметка A в X
		{Note{ID: 3, AuthorID: 2, Text: rep(wB, 4)}, nil},  // заметка B в W
		{Note{ID: 4, AuthorID: 3, Text: rep(nC, 4)}, nil},  // заметка C нейтр
	}
	for _, sv := range saves {
		if _, err := s.SaveGrab(ctx, sv.note, sv.comments, users, testNow); err != nil {
			t.Fatal(err)
		}
	}

	// Оба слоя стиля.
	if _, err := s.BuildStyleProfiles(ctx, 20, 256, GenreAll, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildStyleProfiles(ctx, 20, 256, GenreNotes, testNow); err != nil {
		t.Fatal(err)
	}

	// lexWeight=0 — изолируем стилевой сигнал (регистр на уровне символьных 3-грамм).
	atAll, err := s.AttributeText(ctx, xQ, 3, 1, 0, 0, 0, GenreAll)
	if err != nil {
		t.Fatal(err)
	}
	atNotes, err := s.AttributeText(ctx, xQ, 3, 1, 0, 0, 0, GenreNotes)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("all-эталон:   топ=%d, ранг A=%s", atAll.Candidates[0].UserID, wantRankStr(atAll.Want))
	t.Logf("note-эталон:  топ=%d, ранг A=%s", atNotes.Candidates[0].UserID, wantRankStr(atNotes.Want))

	if atNotes.Genre != GenreNotes || atAll.Genre != GenreAll {
		t.Errorf("жанр эталона не проставлен: all=%q notes=%q", atAll.Genre, atNotes.Genre)
	}
	if atNotes.Want == nil || atNotes.Want.Rank != 1 {
		t.Fatalf("note-эталон: настоящий автор заметки A должен быть #1, got %s", wantRankStr(atNotes.Want))
	}
	if atAll.Want == nil {
		t.Fatal("all-эталон: у A должен быть стиль-профиль")
	}
	// Ключевое: note-эталон ставит настоящего автора ВЫШЕ (меньший ранг), чем all.
	if atAll.Want.Rank <= atNotes.Want.Rank {
		t.Errorf("note-эталон не поднял автора: ранг all=%d, notes=%d (ожидалось all > notes)",
			atAll.Want.Rank, atNotes.Want.Rank)
	}
	// Под all-эталоном автора обходит контаминированный комментариями B (=2).
	if atAll.Candidates[0].UserID == 1 {
		t.Errorf("all-эталон неожиданно поставил A первым — сценарий контаминации не воспроизвёлся")
	}
}

// TestNoteGenreCoverage — note-профиль есть ТОЛЬКО у писавших заметки. Чистый
// комментатор попадает в all-слой, но не в notes-слой: множество note-профилей ≈
// правдоподобные кандидаты жанрового фильтра (встроенный -min-author-notes).
func TestNoteGenreCoverage(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	rep := func(sn string, n int) string { return strings.Repeat(sn, n) }
	users := []User{{ID: 1, Name: "Автор"}, {ID: 2, Name: "Комментатор"}}
	// Автор 1 пишет заметку; автор 2 — только комментатор (заметок нет).
	comments := []Comment{
		{ID: 10, NoteID: 1, AuthorID: 2, Text: rep("реплика комментатора без единой заметки ", 6)},
	}
	if _, err := s.SaveGrab(ctx, Note{ID: 1, AuthorID: 1, Text: rep("длинный текст авторской заметки про жизнь ", 6)},
		comments, users, testNow); err != nil {
		t.Fatal(err)
	}

	if _, err := s.BuildStyleProfiles(ctx, 20, 256, GenreAll, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildStyleProfiles(ctx, 20, 256, GenreNotes, testNow); err != nil {
		t.Fatal(err)
	}

	allIDs, _, err := s.loadStyleProfiles(ctx, GenreAll)
	if err != nil {
		t.Fatal(err)
	}
	notesIDs, _, err := s.loadStyleProfiles(ctx, GenreNotes)
	if err != nil {
		t.Fatal(err)
	}
	if !hasID(allIDs, 2) {
		t.Errorf("all-слой должен включать чистого комментатора (2): %v", allIDs)
	}
	if hasID(notesIDs, 2) {
		t.Errorf("note-слой НЕ должен включать чистого комментатора (2): %v", notesIDs)
	}
	if !hasID(notesIDs, 1) {
		t.Errorf("note-слой должен включать автора заметки (1): %v", notesIDs)
	}
}

// TestNoteGenreLexisPerGenreMeta — lexis-слой и IDF независимы по жанру: build
// одного жанра не затирает meta другого.
func TestNoteGenreLexisPerGenreMeta(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	rep := func(sn string, n int) string { return strings.Repeat(sn, n) }
	users := []User{{ID: 1, Name: "A"}, {ID: 2, Name: "B"}}
	comments := []Comment{
		{ID: 10, NoteID: 1, AuthorID: 1, Text: rep("общий комментаторский поток слов реакции ", 6)},
		{ID: 11, NoteID: 1, AuthorID: 2, Text: rep("другой комментаторский поток иные слова ", 6)},
	}
	if _, err := s.SaveGrab(ctx, Note{ID: 1, AuthorID: 1, Text: rep("заметочный текст один про закат море ", 6)},
		comments, users, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveGrab(ctx, Note{ID: 2, AuthorID: 2, Text: rep("заметочный текст два про горы реки ", 6)},
		nil, users, testNow); err != nil {
		t.Fatal(err)
	}

	if _, err := s.BuildLexisProfiles(ctx, 5, 512, GenreAll, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildLexisProfiles(ctx, 5, 512, GenreNotes, testNow); err != nil {
		t.Fatal(err)
	}

	// Обе meta-строки живут (per-genre PK), IDF разный по жанрам.
	allIDs, _, _, allDims, err := s.loadLexisProfiles(ctx, GenreAll)
	if err != nil {
		t.Fatal(err)
	}
	notesIDs, _, _, notesDims, err := s.loadLexisProfiles(ctx, GenreNotes)
	if err != nil {
		t.Fatal(err)
	}
	if allDims == 0 {
		t.Error("all-lexis meta пропала после build notes (перезаписан чужой жанр)")
	}
	if notesDims == 0 {
		t.Error("notes-lexis meta не построена")
	}
	if len(allIDs) < 2 || len(notesIDs) < 2 {
		t.Errorf("лексических профилей мало: all=%d notes=%d", len(allIDs), len(notesIDs))
	}
}

func hasID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func wantRankStr(w *AttributionCandidate) string {
	if w == nil {
		return "—"
	}
	return strconv.Itoa(w.Rank)
}
