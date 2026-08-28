package narod

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fakeChronicler отдаёт заготовленный ответ вместо модели.
type fakeChronicler struct {
	reply  string
	prompt string
	calls  int
}

func (f *fakeChronicler) GenerateJSON(_ context.Context, _, prompt string, _ map[string]any) ([]byte, error) {
	f.calls++
	f.prompt = prompt
	return []byte(f.reply), nil
}

func chronicleWorld(t *testing.T) (*World, context.Context) {
	t.Helper()
	w := openTestWorld(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	for _, a := range []Actor{
		{ID: "ivan", Kind: ActorPersona, Nick: "Иван"},
		{ID: "olga", Kind: ActorPersona, Nick: "Ольга"},
	} {
		if err := w.UpsertActor(ctx, a, now); err != nil {
			t.Fatal(err)
		}
	}
	return w, ctx
}

func testThread() ChronicleThread {
	return ChronicleThread{
		NoteID: 42, NoteBy: "Автор", NoteText: "как жить",
		At: time.Date(2026, 8, 28, 20, 0, 0, 0, time.UTC),
		Replies: []ChronicleReply{
			{ID: 1, ActorID: "ivan", Nick: "Иван", Text: "по-моему, ерунда"},
			{ID: 2, ActorID: "olga", Nick: "Ольга", Text: "сам ерунда", ReplyTo: 1, Target: "ivan"},
			{ID: 3, ActorID: "ivan", Nick: "Иван", Text: "ну извини", ReplyTo: 2, Target: "olga"},
		},
	}
}

// Знакомство считает КОД, а не модель, и потому оно копится и в бесплатном
// прогоне. Симпатия при этом стоит на месте: без модели её взять неоткуда, и
// подменять её счётчиком встреч значило бы объявить, что видеться и нравиться —
// одно и то же.
func TestChronicleCountsMeetingsWithoutModel(t *testing.T) {
	w, ctx := chronicleWorld(t)
	res, err := Chronicle(ctx, w, nil, testThread())
	if err != nil {
		t.Fatal(err)
	}
	if res.Asked {
		t.Fatal("без генератора отчёт утверждает, что ходили к модели")
	}
	if res.Familiar != 2 {
		t.Fatalf("подвинулось пар знакомством: %d, ждали 2 (Ольга→Иван и Иван→Ольга)", res.Familiar)
	}
	e, err := w.EdgeOf(ctx, "olga", "ivan")
	if err != nil {
		t.Fatal(err)
	}
	if e.Familiarity != 1 {
		t.Fatalf("знакомство Ольги с Иваном %v, ждали 1", e.Familiarity)
	}
	if e.Sympathy != 0 || e.Irritation != 0 {
		t.Fatalf("без модели шкалы сдвинулись: %+v", e)
	}
}

// Один разговор двигает шкалу не больше чем на MaxDelta, даже если модель
// вернула двадцать: правило про размах живёт в коде, а не в промпте.
func TestChronicleClampsDeltas(t *testing.T) {
	w, ctx := chronicleWorld(t)
	gen := &fakeChronicler{reply: `{"edges":[
		{"src":"Ольга","dst":"Иван","sympathy":-20,"irritation":20,"why":"нагрубил"}],
		"episodes":[]}`}
	if _, err := Chronicle(ctx, w, gen, testThread()); err != nil {
		t.Fatal(err)
	}
	e, err := w.EdgeOf(ctx, "olga", "ivan")
	if err != nil {
		t.Fatal(err)
	}
	if e.Sympathy != -MaxDelta || e.Irritation != MaxDelta {
		t.Fatalf("шкалы %+v, ждали ровно ±%v", e, MaxDelta)
	}
}

// Имя, которого в разговоре не было, отбрасывается — и отказ называет себя.
// Молчаливое отбрасывание съело бы половину ответа модели незаметно.
func TestChronicleDropsStrangers(t *testing.T) {
	w, ctx := chronicleWorld(t)
	gen := &fakeChronicler{reply: `{"edges":[
		{"src":"Пётр","dst":"Иван","sympathy":1,"irritation":0,"why":"вступился"}],
		"episodes":[]}`}
	res, err := Chronicle(ctx, w, gen, testThread())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Dropped) != 1 || !strings.Contains(res.Dropped[0], "Пётр") {
		t.Fatalf("отвергнутое: %v — ждали строку про Петра", res.Dropped)
	}
	for _, e := range res.Edges {
		if e.Sympathy != 0 {
			t.Fatalf("постороннее ребро всё-таки записалось: %+v", e)
		}
	}
}

// Ссылка на реплику, которой в треде нет, — выдуманный повод. Через месяц он
// всплыл бы как «а помнишь, как ты…» про то, чего не случалось.
func TestChronicleDropsInventedCommentIDs(t *testing.T) {
	w, ctx := chronicleWorld(t)
	gen := &fakeChronicler{reply: `{"edges":[],"episodes":[
		{"src":"Ольга","dst":"Иван","kind":"сцепились","summary":"поругались","comment_ids":[2,99]}]}`}
	res, err := Chronicle(ctx, w, gen, testThread())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Episodes) != 0 {
		t.Fatalf("эпизод с выдуманной ссылкой записан: %+v", res.Episodes)
	}
	if len(res.Dropped) != 1 || !strings.Contains(res.Dropped[0], "99") {
		t.Fatalf("отвергнутое: %v — ждали упоминание реплики 99", res.Dropped)
	}
}

// Вид эпизода — из закрытого списка ядра. Иначе через десяток тредов заведётся
// «взаимное уважение с оттенком иронии», и сравнивать миры станет нечем.
func TestChronicleRejectsUnknownKind(t *testing.T) {
	w, ctx := chronicleWorld(t)
	gen := &fakeChronicler{reply: `{"edges":[],"episodes":[
		{"src":"Ольга","dst":"Иван","kind":"взаимное уважение","summary":"что-то","comment_ids":[2]}]}`}
	res, err := Chronicle(ctx, w, gen, testThread())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Episodes) != 0 {
		t.Fatalf("эпизод неизвестного вида записан: %+v", res.Episodes)
	}
	if len(res.Dropped) != 1 {
		t.Fatalf("отвергнутое: %v", res.Dropped)
	}
}

// Разговор, где сказал один, модели не показывают вовсе: отношений между одним
// человеком и тишиной не бывает, а запрос стоил бы столько же, сколько
// настоящий.
func TestChronicleSkipsMonologue(t *testing.T) {
	w, ctx := chronicleWorld(t)
	gen := &fakeChronicler{reply: `{"edges":[],"episodes":[]}`}
	th := testThread()
	th.Replies = th.Replies[:1]
	if _, err := Chronicle(ctx, w, gen, th); err != nil {
		t.Fatal(err)
	}
	if gen.calls != 0 {
		t.Fatalf("к модели сходили %d раз на монолог", gen.calls)
	}
}

// В промпте у каждой реплики стоит её номер — тот самый, на который модель
// потом ссылается. Разъедься нумерация с треда, и все ссылки поехали бы разом.
func TestChroniclePromptNumbersReplies(t *testing.T) {
	w, ctx := chronicleWorld(t)
	gen := &fakeChronicler{reply: `{"edges":[],"episodes":[]}`}
	if _, err := Chronicle(ctx, w, gen, testThread()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#1 [Иван]", "#2 [Ольга] → #1", "#3 [Иван] → #2"} {
		if !strings.Contains(gen.prompt, want) {
			t.Fatalf("в промпте нет %q:\n%s", want, gen.prompt)
		}
	}
}

// Схема — часть контракта: сериализуемость её проверяется здесь, а не первым
// платным запросом.
func TestChronicleSchemaSerializes(t *testing.T) {
	for _, s := range []map[string]any{chronicleSchema, innerSchema} {
		if _, err := json.Marshal(s); err != nil {
			t.Fatal(err)
		}
	}
}
