package archive

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeGen отдаёт заранее записанные ответы по кругу и запоминает промпты — весь
// цикл (валидация → скоринг → дифф → повторный запрос) проверяется офлайн.
type fakeGen struct {
	replies []string
	err     error
	systems []string
	prompts []string
	calls   int
}

func (f *fakeGen) GenerateJSON(_ context.Context, system, prompt string, _ map[string]any) ([]byte, error) {
	f.systems = append(f.systems, system)
	f.prompts = append(f.prompts, prompt)
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.replies[(f.calls-1)%len(f.replies)]), nil
}

func draftsJSON(texts ...string) string {
	type d struct {
		Text string `json:"text"`
		Idea string `json:"idea"`
	}
	var out struct {
		Drafts []d `json:"drafts"`
	}
	for i, t := range texts {
		out.Drafts = append(out.Drafts, d{Text: t, Idea: "вариант " + string(rune('A'+i))})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// voiceGenFixture — автор с достаточным числом заметок, чтобы полоса вообще
// строилась: контаминация должна остаться ниже BandContaminationMax.
func voiceGenFixture(t *testing.T) (*Store, *VoiceCard) {
	t.Helper()
	ctx := context.Background()
	s := openTemp(t)

	sea := "закат море горизонт волны песок чайки прибой солнце берег вечер тишина "
	town := "ипотека кредит банк ставка процент квартира ремонт документы очередь "
	weather := "погода дождь ветер облака туман иней слякоть морось сугробы "
	users := []User{{ID: 1, Name: "Море"}, {ID: 2, Name: "Быт"}, {ID: 3, Name: "Погода"}}

	id := int64(1)
	save := func(author int64, body string, n int) {
		note := Note{ID: id, AuthorID: author, Text: strings.Repeat(body, n)}
		if _, err := s.SaveGrab(ctx, note, nil, users, testNow); err != nil {
			t.Fatal(err)
		}
		id++
	}
	// У цели много заметок: один текст полосы — малая доля профиля.
	for i := 0; i < 60; i++ {
		save(1, sea, 3+i%4)
	}
	for i := 0; i < 20; i++ {
		save(2, town, 3+i%3)
	}
	for i := 0; i < 20; i++ {
		save(3, weather, 3+i%3)
	}
	for _, g := range []string{GenreAll, GenreNotes} {
		if _, err := s.BuildStyleProfiles(ctx, 20, 256, g, testNow); err != nil {
			t.Fatal(err)
		}
		if _, err := s.BuildLexisProfiles(ctx, 5, 512, g, testNow); err != nil {
			t.Fatal(err)
		}
	}
	p := VoiceCardDefaults()
	p.Samples, p.Band = 3, 20
	card, err := s.BuildVoiceCard(ctx, "u1", p, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return s, card
}

func noteRequest() VoiceRequest {
	return VoiceRequest{Mode: VoiceModeNote, Topic: "про вечер у воды", Drafts: 2, Rounds: 1, LexWeight: 0.5}
}

// TestVoiceSchemaHasNoArrayConstraints — structured outputs Claude API не
// поддерживают minItems/maxItems и отвечают на них 400. Python/TS SDK вырезают
// такие ключи молча, Go шлёт как есть, поэтому ошибка видна только на живом
// запросе — этот тест ловит её на сборке.
func TestVoiceSchemaHasNoArrayConstraints(t *testing.T) {
	raw, err := json.Marshal(voiceSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"minItems", "maxItems", "minLength", "maxLength", "minimum", "maximum"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("схема содержит неподдерживаемый ключ %q — живой запрос вернёт 400", bad)
		}
	}
	// Обязательное для structured outputs при этом на месте.
	for _, want := range []string{"required", "additionalProperties"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("схема потеряла обязательный ключ %q", want)
		}
	}
}

// TestGenerateVoiceScoresAndStamps — базовый прогон: черновики отскорены против
// полосы, артефакт помечен машинным, вердикт заполнен.
func TestGenerateVoiceScoresAndStamps(t *testing.T) {
	ctx := context.Background()
	s, card := voiceGenFixture(t)
	body := strings.Repeat("закат море горизонт волны песок чайки ", 4)
	gen := &fakeGen{replies: []string{draftsJSON(body, body+"тишина вечер")}}

	run, err := s.GenerateVoice(ctx, gen, card, noteRequest(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Stamp.Machine || !run.Stamp.DoNotPublish || run.Stamp.Warning == "" {
		t.Errorf("журнал прогона без марки машинной генерации: %+v", run.Stamp)
	}
	if len(run.Rounds) != 1 || len(run.Rounds[0].Drafts) != 2 {
		t.Fatalf("ожидался один раунд с двумя черновиками, получено %d раундов", len(run.Rounds))
	}
	if !run.Band.Usable {
		t.Fatalf("полоса непригодна: %s (контаминация %.3f)", run.Band.Why, run.Band.Contamination)
	}
	if run.Band.N == 0 || run.Band.Median == 0 {
		t.Errorf("полоса пуста: %+v", run.Band)
	}
	if run.Verdict == "" {
		t.Error("вердикт не заполнен")
	}
	if run.Best == nil {
		t.Fatal("лучший черновик не выбран")
	}
	if run.Best.Score.Rank == 0 {
		t.Errorf("черновик не отскорен: %+v", run.Best.Score)
	}
	if !strings.Contains(gen.systems[0], "ЗАМЕТКУ") {
		t.Errorf("системный промпт не про заметку:\n%s", gen.systems[0])
	}
	if !strings.Contains(gen.prompts[0], "про вечер у воды") {
		t.Error("тема не попала в промпт")
	}
	if !strings.Contains(gen.prompts[0], "ОБРАЗЦЫ") {
		t.Error("образцы не попали в промпт")
	}
}

// TestGenerateVoiceFeedbackRound — второй раунд получает ИЗМЕРИМЫЙ дифф, а не
// «попробуй ещё раз».
func TestGenerateVoiceFeedbackRound(t *testing.T) {
	ctx := context.Background()
	s, card := voiceGenFixture(t)
	// Длину держим в коридоре автора (иначе отбраковка случится ДО скоринга и
	// дифф не дойдёт до атрибуции), а лексику берём чужую — такой черновик
	// проходит валидацию и честно проигрывает по рангу.
	weak := strings.Repeat("ипотека кредит банк ставка процент квартира ремонт документы очередь ", 3)
	gen := &fakeGen{replies: []string{draftsJSON(weak)}}

	req := noteRequest()
	req.Rounds, req.Accept = 2, 0.99
	run, err := s.GenerateVoice(ctx, gen, card, req, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Rounds) != 2 {
		t.Fatalf("ожидалось 2 раунда, получено %d", len(run.Rounds))
	}
	second := run.Rounds[1].Prompt
	if !strings.Contains(second, "ЧТО НЕ СОШЛОСЬ") {
		t.Fatalf("во втором промпте нет блока обратной связи:\n%s", second)
	}
	if !strings.Contains(second, "АТРИБУЦИЯ") {
		t.Errorf("в обратной связи нет результата атрибуции:\n%s", second)
	}
	if run.Accepted {
		t.Error("слабый черновик принят при пороге 0.99")
	}
	if !strings.HasPrefix(run.Verdict, "НЕ ПРИНЯТ") {
		t.Errorf("вердикт: %s", run.Verdict)
	}
}

// TestGenerateVoiceRejects — каждая проверка валидации отбраковывает свой брак.
func TestGenerateVoiceRejects(t *testing.T) {
	ctx := context.Background()
	s, card := voiceGenFixture(t)
	good := strings.Repeat("закат море горизонт волны песок чайки ", 4)

	cases := []struct {
		name string
		text string
		want string
	}{
		{"пустой", "", "пустой текст"},
		{"markdown", good + "\n\n**жирный**", "markdown"},
		{"эмодзи", good + " 🌊", "эмодзи"},
		{"копирование", card.Samples[0].Text, "пересечение с образцами"},
		{"длина", "коротко", "вне диапазона"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gen := &fakeGen{replies: []string{draftsJSON(c.text)}}
			run, err := s.GenerateVoice(ctx, gen, card, noteRequest(), testNow)
			if err != nil {
				t.Fatal(err)
			}
			got := run.Rounds[0].Drafts[0].Rejected
			if !strings.Contains(got, c.want) {
				t.Errorf("причина отбраковки %q, ожидалась подстрока %q", got, c.want)
			}
		})
	}
}

// TestGenerateVoicePropagatesModelError — отказ модели наверх, без тихого отката.
func TestGenerateVoicePropagatesModelError(t *testing.T) {
	ctx := context.Background()
	s, card := voiceGenFixture(t)
	boom := errors.New("модель отклонила запрос (refusal)")
	gen := &fakeGen{replies: []string{"{}"}, err: boom}

	if _, err := s.GenerateVoice(ctx, gen, card, noteRequest(), testNow); err == nil {
		t.Fatal("ошибка модели проглочена")
	} else if !errors.Is(err, boom) {
		t.Errorf("ошибка потеряла причину: %v", err)
	}
}

// TestBandRefusesContaminatedProfile — у автора с парой коротких заметок один
// текст полосы это заметная доля его профиля; полоса обязана отказаться.
func TestBandRefusesContaminatedProfile(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	users := []User{{ID: 1, Name: "Мало"}, {ID: 2, Name: "Много"}}
	sea := "закат море горизонт волны песок чайки прибой солнце берег "
	town := "ипотека кредит банк ставка процент квартира ремонт документы "
	id := int64(1)
	for i := 0; i < 3; i++ { // у цели всего три заметки
		if _, err := s.SaveGrab(ctx, Note{ID: id, AuthorID: 1, Text: strings.Repeat(sea, 4)}, nil, users, testNow); err != nil {
			t.Fatal(err)
		}
		id++
	}
	for i := 0; i < 20; i++ {
		if _, err := s.SaveGrab(ctx, Note{ID: id, AuthorID: 2, Text: strings.Repeat(town, 4)}, nil, users, testNow); err != nil {
			t.Fatal(err)
		}
		id++
	}
	if _, err := s.BuildStyleProfiles(ctx, 20, 256, GenreNotes, testNow); err != nil {
		t.Fatal(err)
	}
	p := VoiceCardDefaults()
	p.Samples, p.Band = 1, 2
	card, err := s.BuildVoiceCard(ctx, "u1", p, testNow)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.newVoiceScorer(ctx, GenreNotes, 0.5, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	member := s.identityMembers(ctx, "u1")
	pn, err := s.profileNgrams(ctx, []int64{1}, GenreNotes)
	if err != nil {
		t.Fatal(err)
	}
	band, err := s.BuildVoiceBand(ctx, "u1", "notes", card.HeldOut(), v, member, pn)
	if err != nil {
		t.Fatal(err)
	}
	if band.Usable {
		t.Errorf("полоса признана пригодной при контаминации %.2f", band.Contamination)
	}
	if band.Why == "" {
		t.Error("причина отказа не названа")
	}
}

// TestBandQuantileEdges — крайние случаи меры «лучше скольких настоящих текстов».
func TestBandQuantileEdges(t *testing.T) {
	b := VoiceBand{N: 4, Ranks: []int{2, 5, 9, 40}, Usable: true}
	if q := BandQuantile(b, 1); q != 1 {
		t.Errorf("ранг лучше всех: q=%v, ожидалось 1", q)
	}
	if q := BandQuantile(b, 100); q != 0 {
		t.Errorf("ранг хуже всех: q=%v, ожидалось 0", q)
	}
	if q := BandQuantile(b, 6); q != 0.5 {
		t.Errorf("ранг посередине: q=%v, ожидалось 0.5", q)
	}
	if q := BandQuantile(VoiceBand{N: 4, Ranks: []int{1}}, 1); q != 0 {
		t.Error("непригодная полоса обязана давать 0")
	}
}

// TestCopyOverlapCatchesRetelling — страж копирования ловит пересказ образца и
// не срабатывает на своём тексте.
func TestCopyOverlapCatchesRetelling(t *testing.T) {
	sample := "вчера ходил на берег и долго смотрел как солнце садится в воду"
	if got := CopyOverlap(sample, []string{sample}); got < 0.9 {
		t.Errorf("дословная копия дала %v, ожидалось ~1", got)
	}
	if got := CopyOverlap("совсем другой текст про ремонт кухни и очередь в банке", []string{sample}); got != 0 {
		t.Errorf("несвязанный текст дал %v, ожидалось 0", got)
	}
}

// TestLoadVoiceThreadUsesReplyTarget — адресат берётся у реплики, которой
// отвечают, а НЕ у автора корня ветки: parent_id указывает на корень и совпадает
// с адресатом лишь в трети случаев.
func TestLoadVoiceThreadUsesReplyTarget(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	users := []User{{ID: 1, Name: "Автор"}, {ID: 2, Name: "КореньВетки"}, {ID: 3, Name: "НастоящийАдресат"}}
	note := Note{ID: 100, AuthorID: 1, Text: "текст заметки"}
	comments := []Comment{
		{ID: 10, NoteID: 100, ParentID: 0, AuthorID: 2, Text: "корень ветки"},
		{ID: 11, NoteID: 100, ParentID: 10, AuthorID: 3, Text: "реплика внутри ветки"},
		{ID: 12, NoteID: 100, ParentID: 10, AuthorID: 1, Text: "ещё реплика"},
	}
	if _, err := s.SaveGrab(ctx, note, comments, users, testNow); err != nil {
		t.Fatal(err)
	}
	th, err := s.LoadVoiceThread(ctx, 11, []int64{1}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if th.AddresseeNick != "НастоящийАдресат" {
		t.Errorf("адресат = %q, а должен быть автором реплики 11, а не корня ветки", th.AddresseeNick)
	}
	if th.RootID != 10 {
		t.Errorf("корень ветки = %d, ожидалось 10", th.RootID)
	}
	if th.NoteAuthor != "Автор" {
		t.Errorf("автор заметки = %q", th.NoteAuthor)
	}
	var target, self bool
	for _, m := range th.Branch {
		if m.ID == 11 && m.Target {
			target = true
		}
		if m.ID == 12 && m.Self {
			self = true
		}
	}
	if !target {
		t.Error("реплика-адресат не помечена")
	}
	if !self {
		t.Error("собственная реплика цели в ветке не помечена")
	}
}

// TestRenderDraftComposesPrefix — обращение подставляет инструмент, и скорится
// именно эта форма.
func TestRenderDraftComposesPrefix(t *testing.T) {
	card := &VoiceCard{Comments: VoiceShape{Kind: "comments", AddressPrefix: 0.8}}
	req := VoiceRequest{Mode: VoiceModeComment, Thread: &VoiceThread{ReplyToID: 11, AddresseeNick: "Аня"}}
	if got := renderDraft("да ладно тебе", card, req); got != "Аня, да ладно тебе" {
		t.Errorf("обращение не подставлено: %q", got)
	}
	// Автор, который обращений почти не пишет, не должен их получать.
	shy := &VoiceCard{Comments: VoiceShape{Kind: "comments", AddressPrefix: 0.1}}
	if got := renderDraft("да ладно тебе", shy, req); got != "да ладно тебе" {
		t.Errorf("обращение подставлено автору, который их не пишет: %q", got)
	}
	// Модель попыталась написать обращение сама — брак.
	if why := validateDraft("Аня, да ладно тебе", card, req, "comments", 0); !strings.Contains(why, "обращение") {
		t.Errorf("обращение в теле не отбраковано: %q", why)
	}
}

var _ = time.Now
