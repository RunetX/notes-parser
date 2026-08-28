package narod

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Свёртка НИЧЕГО НЕ СТИРАЕТ: в промпт идут последние, но все строки остаются в
// таблице. Иначе свидетельство о том, что между людьми было, уничтожалось бы
// ради места в промпте.
func TestCompactEpisodesHidesButKeeps(t *testing.T) {
	w, ctx := chronicleWorld(t)
	day := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < EpisodeCap+3; i++ {
		_, err := w.AddEpisode(ctx, Episode{Src: "ivan", Dst: "olga",
			At: day.AddDate(0, 0, i), Kind: EpisodeTease,
			Summary: fmt.Sprintf("случай %d", i)})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := w.CompactEpisodes(ctx, "ivan", "olga", day.AddDate(0, 0, 20)); err != nil {
		t.Fatal(err)
	}
	got, err := w.EpisodesOf(ctx, "ivan", "olga", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != EpisodeCap+1 {
		t.Fatalf("в промпт идёт %d эпизодов, ждали %d названных плюс выжимку",
			len(got), EpisodeCap)
	}
	var digests int
	for _, e := range got {
		if e.Kind == EpisodeDigest {
			digests++
			if !strings.Contains(e.Summary, EpisodeTease) || !strings.Contains(e.Summary, "3") {
				t.Fatalf("выжимка не называет, чего и сколько: %q", e.Summary)
			}
		}
	}
	if digests != 1 {
		t.Fatalf("выжимок %d, ждали одну", digests)
	}

	var all int
	if err := w.db.QueryRowContext(ctx,
		"SELECT count(*) FROM episodes WHERE src = 'ivan' AND dst = 'olga'").Scan(&all); err != nil {
		t.Fatal(err)
	}
	if all != EpisodeCap+4 {
		t.Fatalf("в таблице %d строк — свёртка что-то стёрла", all)
	}
}

// У незнакомых память пуста, и это законный ответ: в мире, который только
// начался, никто ни с кем не знаком, а пустой заголовок «что ты помнишь»
// предлагал бы модели вспомнить хоть что-нибудь.
func TestWriteMemoryEmptyForStrangers(t *testing.T) {
	w, ctx := chronicleWorld(t)
	got, err := WriteMemory(ctx, w, "ivan", []MemoryPeer{{ActorID: "olga", Nick: "Ольга"}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("память незнакомых непуста:\n%s", got)
	}
}

// В памяти нет ЧИСЕЛ: скажи модели «симпатия 6.2 из 10» — и она начнёт писать о
// шкале вместо того, чтобы говорить исходя из неё.
func TestWriteMemorySpeaksInWordsNotNumbers(t *testing.T) {
	w, ctx := chronicleWorld(t)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if _, err := w.Nudge(ctx, EdgeDelta{Src: "ivan", Dst: "olga",
		Sympathy: -6, Irritation: 4, Familiarity: 12}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := w.AddEpisode(ctx, Episode{Src: "olga", Dst: "ivan", At: now,
		Kind: EpisodeFight, Summary: "назвала его ответ отпиской"}); err != nil {
		t.Fatal(err)
	}
	got, err := WriteMemory(ctx, w, "ivan", []MemoryPeer{{ActorID: "olga", Nick: "Ольга"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "раздражает") || !strings.Contains(got, "давние собеседники") {
		t.Fatalf("шкалы не переведены в слова:\n%s", got)
	}
	if !strings.Contains(got, "отпиской") {
		t.Fatalf("повода в памяти нет — осталась одна оценка:\n%s", got)
	}
	for _, digit := range []string{"-6", "12", "4.0", "0.5"} {
		if strings.Contains(got, digit) {
			t.Fatalf("в памяти проступило число %q:\n%s", digit, got)
		}
	}
}

// Бросок «что-то случилось» — формула: он повторяется и не зависит ни от
// порядка вызовов, ни от того, спрашивали ли соседний день.
func TestInnerHappenedIsStable(t *testing.T) {
	day := time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)
	first := InnerHappened(7, "ivan", day)
	for i := 0; i < 50; i++ {
		InnerHappened(7, "olga", day.AddDate(0, 0, i))
		if got := InnerHappened(7, "ivan", day); got != first {
			t.Fatalf("бросок того же дня поменялся на %d-м обращении", i)
		}
	}
}

// Частота событий — примерно раз в InnerMeanDays. Проверяется на длинной
// дистанции, потому что заявлено именно среднее: у отдельного жителя неделя без
// событий — рабочее состояние.
func TestInnerHappenedKeepsTheRate(t *testing.T) {
	day := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	const days = 4000
	var n int
	for i := 0; i < days; i++ {
		if InnerHappened(11, "ivan", day.AddDate(0, 0, i)) {
			n++
		}
	}
	got := float64(n) / days
	want := 1 / InnerMeanDays
	if got < want*0.85 || got > want*1.15 {
		t.Fatalf("событий %.3f на день, ждали около %.3f", got, want)
	}
}

// Внутреннее событие — это жизнь СНАРУЖИ. «Прочитал заметку и подумал»
// превращает механизм в самого себя, и запрет стоит в коде, а не только в
// просьбе к модели.
func TestInnerRejectsPlatformTalk(t *testing.T) {
	cases := map[string]bool{
		"Забрал шкаф, привезли не того цвета.":  false,
		"Прочитал заметку про дачу и задумался": true,
		"Ответил в треде и пошёл спать":         true,
	}
	for text, wantRejected := range cases {
		if got := innerReject(text) != ""; got != wantRejected {
			t.Fatalf("%q: отвергнуто=%v, ждали %v", text, got, wantRejected)
		}
	}
}

// Один и тот же день не проживают дважды: прогон серии повторяют, а второе
// событие того же дня стоило бы денег и сделало бы жизнь жителя вдвое гуще.
func TestInnerTickIsIdempotentPerDay(t *testing.T) {
	w, ctx := chronicleWorld(t)
	gen := &fakeChronicler{reply: `{"text":"Свозил мать в поликлинику."}`}
	card := &Card{Persona: Bio{Nick: "Иван", Age: 40}}
	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 30)

	first, err := InnerTick(ctx, w, gen, card, "ivan", 3, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("за месяц не случилось ничего — бросок не работает")
	}
	calls := gen.calls
	again, err := InnerTick(ctx, w, gen, card, "ivan", 3, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("повтор прогона записал ещё %d событий", len(again))
	}
	if gen.calls != calls {
		t.Fatalf("повтор сходил к модели ещё %d раз", gen.calls-calls)
	}
}
