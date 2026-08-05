package archive

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// TestVoiceCardMeasures — корпус подобран так, что у каждого признака известен
// ответ. Проверяются именно те измерения, на которых стоит генерация: скобочная
// подпись, финальная точка, регистр, разметка сайта и обращения.
func TestVoiceCardMeasures(t *testing.T) {
	texts := []voiceText{
		{id: 1, kind: "notes", text: "Хороший был день))"},            // «))», без финальной точки
		{id: 2, kind: "notes", text: "всё как всегда, ничего нового"}, // целиком строчными, без точки
		{id: 3, kind: "notes", text: "Ага [b]вот[/b] это да :::agree:::"},
		{id: 4, kind: "notes", text: "Сходил в кино. Понравилось!"},
	}
	sh := measureShape(texts, "notes", nil)

	if sh.Texts != 4 {
		t.Fatalf("текстов %d, ожидалось 4", sh.Texts)
	}
	if got := sh.ParenRuns["2"]; got != 0.25 {
		t.Errorf("доля текстов с «))» = %v, ожидалось 0.25", got)
	}
	if _, ok := sh.ParenRuns["1"]; ok {
		t.Errorf("одиночная «)» не встречается, но попала в подпись: %v", sh.ParenRuns)
	}
	// Знаком конца закрыт только текст 4 («Понравилось!»). Текст 1 обрывается
	// скобкой-улыбкой, текст 2 — буквой, текст 3 — двоеточием смайла: для
	// генерации все три означают «точку в конце не ставит».
	if got := sh.NoFinalPunct; got != 0.75 {
		t.Errorf("доля без финального знака = %v, ожидалось 0.75", got)
	}
	if got := sh.AllLower; got != 0.25 {
		t.Errorf("доля целиком строчных = %v, ожидалось 0.25", got)
	}
	if got := sh.Markup["[b]"]; got != 0.25 {
		t.Errorf("доля текстов с [b] = %v, ожидалось 0.25", got)
	}
	if len(sh.TopSmileys) != 1 || sh.TopSmileys[0].Text != "agree" {
		t.Errorf("смайлы сайта не разобраны: %+v", sh.TopSmileys)
	}
	if sh.EmojiRate != 0 {
		t.Errorf("эмодзи в корпусе нет, а EmojiRate=%v", sh.EmojiRate)
	}
	if sh.Runes.Median <= 0 || sh.Sentences.Median <= 0 {
		t.Errorf("квантили длин не посчитаны: %+v / %+v", sh.Runes, sh.Sentences)
	}
}

// TestVoiceShapeAddressPrefix — доля обращений считается только для комментариев,
// сверяется с реальными никами и зачин берётся по телу БЕЗ обращения (иначе «чем
// начинает» вырождается в список ников собеседников).
func TestVoiceShapeAddressPrefix(t *testing.T) {
	texts := []voiceText{
		{id: 1, kind: "comments", text: "Аня, да ладно тебе"},
		{id: 2, kind: "comments", text: "Аня, ну ты даёшь"},
		{id: 3, kind: "comments", text: "просто мимо проходил"},
		// Ранняя запятая, но «всё как всегда» — не ник: обращением НЕ считается.
		{id: 4, kind: "comments", text: "всё как всегда, ничего нового"},
	}
	nicks := map[string]bool{"аня": true}
	sh := measureShape(texts, "comments", nicks)
	if sh.AddressPrefix != 0.5 {
		t.Errorf("доля обращений = %v, ожидалось 0.5", sh.AddressPrefix)
	}
	for _, o := range sh.TopOpenings {
		if o.Text == "аня" {
			t.Errorf("ник адресата утёк в зачины: %+v", sh.TopOpenings)
		}
	}
	// Без сверки с никами ранняя запятая даёт ложное обращение — ровно та
	// завышенная оценка, ради которой сверка и заведена.
	if loose := measureShape(texts, "comments", nil); loose.AddressPrefix != 0.75 {
		t.Errorf("без сверки ожидалась завышенная доля 0.75, got %v", loose.AddressPrefix)
	}
	if notes := measureShape(texts, "notes", nicks); notes.AddressPrefix != 0 {
		t.Errorf("для заметок доля обращений должна быть 0, got %v", notes.AddressPrefix)
	}
}

// TestVoiceSplitDeterministicAndDisjoint — главная гарантия карты: образцы и
// полоса НЕ пересекаются (иначе полоса меряется на текстах, которые модель
// видела, и весь замер врёт), а один seed даёт одну и ту же выборку.
func TestVoiceSplitDeterministicAndDisjoint(t *testing.T) {
	var corpus []voiceText
	for i := 1; i <= 40; i++ {
		corpus = append(corpus, voiceText{id: int64(i), kind: "notes",
			text: strings.Repeat("слово ", i)}) // длины разные — послойный отбор осмыслен
	}
	s1, h1 := voiceSplit(corpus, 6, 10, 1)
	s2, h2 := voiceSplit(corpus, 6, 10, 1)
	s3, _ := voiceSplit(corpus, 6, 10, 42)

	if len(s1) != 6 || len(h1) != 10 {
		t.Fatalf("размеры выборок: образцов %d (ждали 6), полоса %d (ждали 10)", len(s1), len(h1))
	}
	if ids(s1) != ids(s2) || ids(h1) != ids(h2) {
		t.Errorf("один seed дал разные выборки:\n%s\n%s", ids(s1), ids(s2))
	}
	if ids(s1) == ids(s3) {
		t.Errorf("разные seed дали одинаковые образцы: %s", ids(s1))
	}
	in := map[int64]bool{}
	for _, t := range s1 {
		in[t.id] = true
	}
	for _, h := range h1 {
		if in[h.id] {
			t.Fatalf("образец %d попал и в полосу — контаминация промпта", h.id)
		}
	}
	// Послойность: между самым коротким и самым длинным образцом должен быть
	// заметный разброс, иначе промпт уедет в один регистр.
	shortest, longest := len([]rune(s1[0].text)), len([]rune(s1[0].text))
	for _, x := range s1 {
		n := len([]rune(x.text))
		if n < shortest {
			shortest = n
		}
		if n > longest {
			longest = n
		}
	}
	if longest < shortest*3 {
		t.Errorf("образцы не расслоены по длине: от %d до %d рун", shortest, longest)
	}
}

// TestBuildVoiceCardEndToEnd — карта собирается на живой схеме, словарь берёт вес
// из построенного lexis-слоя, а образцы и полоса не пересекаются.
func TestBuildVoiceCardEndToEnd(t *testing.T) {
	ctx := context.Background()
	s := voiceFixture(t)

	p := VoiceCardDefaults()
	p.Genre, p.Samples, p.Band, p.TopWords = GenreNotes, 2, 5, 10
	card, err := s.BuildVoiceCard(ctx, "u1", p, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if card.Identity != "u1" {
		t.Errorf("identity = %q", card.Identity)
	}
	if !card.Stamp.Machine || !card.Stamp.DoNotPublish || card.Stamp.Warning == "" {
		t.Errorf("артефакт без марки машинной генерации: %+v", card.Stamp)
	}
	if card.Notes.Texts == 0 {
		t.Fatal("заметки автора не попали в замер")
	}
	if card.Notes.TotalHave < card.Notes.Texts {
		t.Errorf("знаменатель меньше замера: have=%d texts=%d", card.Notes.TotalHave, card.Notes.Texts)
	}
	if len(card.Vocab) == 0 {
		t.Errorf("словарь пуст при построенном lexis-слое (%s)", card.VocabNote)
	}
	if len(card.words) == 0 {
		t.Error("полный словарь автора не собран — диффу нечем будет ловить чужую лексику")
	}
	held := map[int64]bool{}
	for _, id := range card.HeldIDs {
		held[id] = true
	}
	for _, sm := range card.Samples {
		if held[sm.ID] {
			t.Errorf("образец %d одновременно в полосе", sm.ID)
		}
	}
	var b strings.Builder
	if err := WriteVoiceBrief(&b, card, "notes"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"КАРТА ПИСЬМА", "Длина:", "Пунктуация"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("в карте нет блока %q:\n%s", want, b.String())
		}
	}
}

// TestVoiceCorpusFollowsKind — корпус образцов и словаря идёт от РОДА текста, а не
// от жанра эталона: у комментария жанр `all`, и без этого в промпт комментария
// попадали заметки автора (живой замер: 378-рунная заметка среди шести образцов).
func TestVoiceCorpusFollowsKind(t *testing.T) {
	notes := []voiceText{{id: 1, kind: "notes", text: "длинная заметка про всё"}}
	comments := []voiceText{{id: 2, kind: "comments", text: "короткая реплика"}}

	cases := []struct {
		name  string
		p     VoiceCardParams
		wantN int // сколько текстов в корпусе
		kind  string
	}{
		{"комментарий при жанре all", VoiceCardParams{Genre: GenreAll, Kind: "comments"}, 1, "comments"},
		{"заметка при жанре all", VoiceCardParams{Genre: GenreAll, Kind: "notes"}, 1, "notes"},
		{"card: как раньше по жанру", VoiceCardParams{Genre: GenreAll}, 2, ""},
		{"card при жанре notes", VoiceCardParams{Genre: GenreNotes}, 1, "notes"},
	}
	for _, c := range cases {
		got := voiceCorpus(c.p, notes, comments)
		if len(got) != c.wantN {
			t.Errorf("%s: в корпусе %d текстов, ожидалось %d", c.name, len(got), c.wantN)
			continue
		}
		if c.kind != "" && got[0].kind != c.kind {
			t.Errorf("%s: род корпуса %q, ожидался %q", c.name, got[0].kind, c.kind)
		}
	}
}

// TestVoiceTargetSolo — solo берёт ровно указанную анкету, а не склеенную
// личность: иначе «профиль X» означает всё, что с ним склеено, включая анкеты
// других эпох (реальный случай — кластер из 11 анкет 2010–2026 годов).
func TestVoiceTargetSolo(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	users := []User{{ID: 1, Name: "Старая"}, {ID: 2, Name: "Нынешняя"}}
	for i, uid := range []int64{1, 2} {
		note := Note{ID: int64(10 + i), AuthorID: uid, Text: strings.Repeat("текст заметки ", 20)}
		if _, err := s.SaveGrab(ctx, note, nil, users, testNow); err != nil {
			t.Fatal(err)
		}
	}
	identity, accs, err := s.voiceTarget(ctx, "u2", true)
	if err != nil {
		t.Fatal(err)
	}
	if identity != "u2" || len(accs) != 1 || accs[0] != 2 {
		t.Errorf("solo дал identity=%q анкеты=%v, ожидалось u2 / [2]", identity, accs)
	}
	if _, _, err := s.voiceTarget(ctx, "p7", true); err == nil {
		t.Error("solo принял личность p7, а должен требовать анкету u<id>")
	}
	if _, _, err := s.voiceTarget(ctx, "u999999", true); err == nil {
		t.Error("solo принял несуществующую анкету")
	}
}

// TestVoiceAutoSamples — число образцов считается по медиане корпуса: на коротких
// репликах шести примеров мало, чтобы манера вообще проявилась.
func TestVoiceAutoSamples(t *testing.T) {
	mk := func(runes, n int) []voiceText {
		var out []voiceText
		for i := 0; i < n; i++ {
			out = append(out, voiceText{id: int64(i + 1), text: strings.Repeat("х", runes)})
		}
		return out
	}
	cases := []struct {
		runes, want int
	}{{80, 24}, {300, 12}, {900, 6}}
	for _, c := range cases {
		if got := voiceAutoSamples(mk(c.runes, 50)); got != c.want {
			t.Errorf("при медиане %d рун образцов %d, ожидалось %d", c.runes, got, c.want)
		}
	}
	if got := voiceAutoSamples(nil); got != 6 {
		t.Errorf("на пустом корпусе образцов %d, ожидалось 6", got)
	}
}

// TestVocabIgnoresSiteMarkupAndSingleUse — из «[color=red]» forEachWord достаёт
// «color» и «red», и эти токены возглавляли список характерных слов; слово из
// одного текста — разовая тема, а не привычка.
func TestVocabIgnoresSiteMarkupAndSingleUse(t *testing.T) {
	corpus := []voiceText{
		{id: 1, text: "[color=red]ага[/color] погода дрянь :::smile:::"},
		{id: 2, text: "[color=red]ага[/color] погода опять"},
		{id: 3, text: "погода разовоеслово"},
	}
	counts, docs := wordCounts(corpus)
	for _, bad := range []string{"color", "red", "smile"} {
		if counts[bad] > 0 {
			t.Errorf("токен разметки %q попал в словарь (%d)", bad, counts[bad])
		}
	}
	if counts["погода"] != 3 || docs["погода"] != 3 {
		t.Errorf("погода: count=%d docs=%d, ожидалось 3/3", counts["погода"], docs["погода"])
	}
	if docs["разовоеслово"] != 1 {
		t.Errorf("разовоеслово: docs=%d, ожидалось 1", docs["разовоеслово"])
	}

	// Норма набивки считается по тому же чистому тексту.
	vocab := []VoiceWord{{Word: "погода"}, {Word: "ага"}}
	if r := vocabRate(corpus, vocab); r <= 0 {
		t.Errorf("норма характерных слов = %v, ожидалась положительная", r)
	}
	if r, hits := VocabUse("погода погода погода ага", vocab); r != 100 || hits != 4 {
		t.Errorf("текст целиком из характерных слов дал %v при %d попаданиях, ожидалось 100/4", r, hits)
	}
}

// TestSampleContextIsReplyTarget — контекст образца берётся из настоящей цели
// ответа (comment_reply), а не из parent_id (тот указывает на корень ВЕТКИ);
// у реплики первого уровня контекст — сама заметка.
func TestSampleContextIsReplyTarget(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	users := []User{{ID: 1, Name: "Цель"}, {ID: 2, Name: "КореньВетки"}, {ID: 3, Name: "НастоящийАдресат"}}
	note := Note{ID: 100, AuthorID: 2, Text: "текст заметки"}
	comments := []Comment{
		{ID: 10, NoteID: 100, ParentID: 0, AuthorID: 2, Text: "корень ветки"},
		{ID: 11, NoteID: 100, ParentID: 10, AuthorID: 3, Text: "настоящая цель ответа"},
		{ID: 12, NoteID: 100, ParentID: 10, AuthorID: 1, Text: "ответ цели"},
		{ID: 13, NoteID: 100, ParentID: 0, AuthorID: 1, Text: "реплика первого уровня"},
	}
	if _, err := s.SaveGrab(ctx, note, comments, users, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveReplyTree(ctx, 100, map[int64]int64{12: 11}); err != nil {
		t.Fatal(err)
	}
	samples := []VoiceSample{
		{ID: 12, Kind: "comments"},
		{ID: 13, Kind: "comments"},
	}
	if err := s.fillSampleContexts(ctx, samples); err != nil {
		t.Fatal(err)
	}
	if samples[0].Context != "настоящая цель ответа" {
		t.Errorf("контекст ответа = %q, ожидалась настоящая цель, а не корень ветки", samples[0].Context)
	}
	if samples[0].ContextAuthor != "НастоящийАдресат" {
		t.Errorf("автор контекста = %q", samples[0].ContextAuthor)
	}
	if samples[1].Context != "текст заметки" || !strings.Contains(samples[1].ContextAuthor, "заметка") {
		t.Errorf("реплика первого уровня отвечает заметке, а контекст = %q (%q)",
			samples[1].Context, samples[1].ContextAuthor)
	}
}

// TestBuildVoiceCardWithoutLexisLayer — без lexis-слоя карта строится, словарь
// пуст, и причина названа (контракт «сигнал недоступен», а не ошибка).
func TestBuildVoiceCardWithoutLexisLayer(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	users := []User{{ID: 1, Name: "Один"}}
	note := Note{ID: 1, AuthorID: 1, Text: strings.Repeat("закат море горизонт волны песок ", 6)}
	if _, err := s.SaveGrab(ctx, note, nil, users, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildStyleProfiles(ctx, 20, 256, GenreNotes, testNow); err != nil {
		t.Fatal(err)
	}
	p := VoiceCardDefaults()
	p.Samples, p.Band = 1, 0
	card, err := s.BuildVoiceCard(ctx, "u1", p, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(card.Vocab) != 0 {
		t.Errorf("словарь непуст без lexis-слоя: %+v", card.Vocab)
	}
	if card.VocabNote == "" {
		t.Error("причина пустого словаря не названа")
	}
}

func ids(ts []voiceText) string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, strconv.FormatInt(t.id, 10))
	}
	return strings.Join(out, ",")
}
