package archive

import (
	"testing"
	"time"
)

// vsScript — тред с настоящей веткой: 2000 → 2001 → 2003, и рядом посторонняя
// реплика 2002.
func vsScript() *ThreadScript {
	t0 := time.Date(2016, 5, 12, 9, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	return &ThreadScript{
		NoteID: 500,
		Note:   ScriptNote{AuthorID: 1, AuthorNick: "Хозяйка", Text: "заметка", PublishedAt: t0},
		Comments: []ScriptComment{
			{ID: 2000, AuthorID: 2, AuthorNick: "Ягода", Text: "раз", PublishedAt: at(1), ReplyTo: 0},
			{ID: 2001, AuthorID: 3, AuthorNick: "Гость", Text: "два", PublishedAt: at(2), ReplyTo: 2000},
			{ID: 2002, AuthorID: 4, AuthorNick: "Прохожий", Text: "не в тему", PublishedAt: at(3), ReplyTo: 0},
			{ID: 2003, AuthorID: 2, AuthorNick: "Ягода", Text: "три", PublishedAt: at(4), ReplyTo: 2001},
			{ID: 2004, AuthorID: 5, AuthorNick: "Поздний", Text: "БУДУЩЕЕ", PublishedAt: at(9), ReplyTo: 0},
		},
	}
}

// Главное свойство: в контекст не попадает НИЧЕГО, сказанного позже момента.
// Утечка будущего делает результат реплея лучше, чем он есть, и тем сильнее, чем
// разговорчивее тред, — а заметить её по самому результату нельзя.
func TestScriptVoiceThreadHidesFuture(t *testing.T) {
	sc := vsScript()
	// Пишем на месте реплики 2003 (индекс 3): видно ровно первые три.
	th := ScriptVoiceThread(sc, 3, 2001, []int64{2}, 20)
	for _, m := range th.Branch {
		if m.ID >= 2003 {
			t.Errorf("в контекст попало будущее: #%d %q", m.ID, m.Text)
		}
	}
	if len(th.Branch) != 3 {
		t.Errorf("строк ветки %d, ожидалось 3", len(th.Branch))
	}
}

func TestScriptVoiceThreadAddresseeAndRoot(t *testing.T) {
	sc := vsScript()
	th := ScriptVoiceThread(sc, 3, 2001, nil, 20)
	if th.ReplyToID != 2001 || th.AddresseeID != 3 || th.AddresseeNick != "Гость" {
		t.Errorf("адресат: %d / %d / %q", th.ReplyToID, th.AddresseeID, th.AddresseeNick)
	}
	// Корень ветки берётся ПОДЪЁМОМ ПО НАСТОЯЩИМ РЁБРАМ, а не по parent_id.
	if th.RootID != 2000 {
		t.Errorf("корень ветки %d, ожидался 2000", th.RootID)
	}
	if th.NoteAuthor != "Хозяйка" || th.NoteText != "заметка" {
		t.Errorf("заметка: %q / %q", th.NoteAuthor, th.NoteText)
	}
	var target int
	for _, m := range th.Branch {
		if m.Target {
			target++
			if m.ID != 2001 {
				t.Errorf("помечена как адресат не та реплика: %d", m.ID)
			}
		}
	}
	if target != 1 {
		t.Errorf("помеченных адресатов %d", target)
	}
}

// Свои прошлые слова видны и помечены: без них житель противоречил бы сам себе.
func TestScriptVoiceThreadMarksSelf(t *testing.T) {
	sc := vsScript()
	th := ScriptVoiceThread(sc, 3, 2001, []int64{2}, 20)
	if !th.SelfInBranch {
		t.Fatal("свои реплики в ветке не отмечены")
	}
	var self int
	for _, m := range th.Branch {
		if m.Self {
			self++
		}
	}
	if self != 1 {
		t.Errorf("своих реплик отмечено %d, ожидалась 1 (#2000)", self)
	}
}

// Потолок строк: цепочка предков входит целиком, посторонняя реплика вытесняется
// первой — без предков ответ повисает в воздухе, а сосед лишь показывает, о чём
// говорят вокруг.
func TestScriptVoiceThreadKeepsAncestorsUnderLimit(t *testing.T) {
	sc := vsScript()
	th := ScriptVoiceThread(sc, 3, 2001, nil, 2)
	if len(th.Branch) != 2 {
		t.Fatalf("строк %d при потолке 2", len(th.Branch))
	}
	got := map[int64]bool{}
	for _, m := range th.Branch {
		got[m.ID] = true
	}
	if !got[2000] || !got[2001] {
		t.Errorf("вытеснены предки, осталось %v", got)
	}
}

// Ответ самой заметке: адресата нет, корень — сам ответ.
func TestScriptVoiceThreadRootReply(t *testing.T) {
	sc := vsScript()
	th := ScriptVoiceThread(sc, 2, 0, nil, 20)
	if th.ReplyToID != 0 || th.AddresseeID != 0 || th.AddresseeNick != "" {
		t.Errorf("у ответа заметке появился адресат: %+v", th)
	}
	if len(th.Branch) != 2 {
		t.Errorf("строк %d, ожидалось 2", len(th.Branch))
	}
}

// Аноним остаётся анонимом и в контексте.
func TestScriptVoiceThreadAnonymousNote(t *testing.T) {
	sc := vsScript()
	sc.Note.AuthorID, sc.Note.AuthorNick = 0, ""
	if th := ScriptVoiceThread(sc, 1, 0, nil, 20); th.NoteAuthor != "аноним" {
		t.Errorf("автор анонимной заметки: %q", th.NoteAuthor)
	}
}

// Кольцо в рёбрах (битые данные) не должно вешать сборку.
func TestScriptVoiceThreadSurvivesCycle(t *testing.T) {
	sc := vsScript()
	sc.Comments[0].ReplyTo = 2001 // 2000 → 2001 → 2000
	done := make(chan *VoiceThread, 1)
	go func() { done <- ScriptVoiceThread(sc, 3, 2001, nil, 20) }()
	select {
	case th := <-done:
		if th == nil {
			t.Fatal("пустой контекст")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("сборка зациклилась на кольце рёбер")
	}
}
