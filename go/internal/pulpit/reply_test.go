package pulpit

import (
	"testing"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

const (
	ownID  = "1472546"
	myNick = "Рантье"
	myID   = int64(100)
)

// branch — тред, где под нашей репликой (id 100) отвечают разные люди.
func branch() []love.Comment {
	return []love.Comment{
		{ID: 90, AuthorID: "u1", AuthorName: "Чужой", Text: "реплика в корне заметки"},
		{ID: 100, ParentID: 0, AuthorID: ownID, AuthorName: myNick, Text: "своя реплика"},
		{ID: 101, ParentID: myID, AuthorID: "u2", AuthorName: "Лампочка", Text: "Рантье, ты опять за своё"},
		{ID: 102, ParentID: myID, AuthorID: "u3", AuthorName: "Мавр", Text: "Лампочка, да он всегда такой"},
		{ID: 103, ParentID: 90, AuthorID: "u4", AuthorName: "Птичка", Text: "Рантье, а ты кто такой"},
	}
}

func TestReplyCandidates(t *testing.T) {
	got := replyCandidates(branch(), myID, ownID, myNick, nil, nil)
	if len(got) != 1 || got[0].ID != 101 {
		t.Fatalf("кандидаты: %+v", ids(got))
	}

	// Решение по реплике уже принято — монетку не перебрасываем.
	got = replyCandidates(branch(), myID, ownID, myNick, map[int64]bool{101: true}, nil)
	if len(got) != 0 {
		t.Errorf("решённая реплика снова в кандидатах: %v", ids(got))
	}

	// Этому автору в заметке уже отвечали.
	got = replyCandidates(branch(), myID, ownID, myNick, nil, map[string]bool{"u2": true})
	if len(got) != 0 {
		t.Errorf("повторный ответ одному человеку: %v", ids(got))
	}
}

// TestReplyCandidatesFirstInBranch — первому в ветке отвечают нам по
// построению: до него там были только мы, даже без обращения по нику.
func TestReplyCandidatesFirstInBranch(t *testing.T) {
	comments := []love.Comment{
		{ID: 100, AuthorID: ownID, AuthorName: myNick, Text: "своя реплика"},
		{ID: 101, ParentID: myID, AuthorID: "u2", AuthorName: "Лампочка", Text: "ну и зануда"},
		{ID: 102, ParentID: myID, AuthorID: "u3", AuthorName: "Мавр", Text: "Лампочка, точно"},
	}
	got := replyCandidates(comments, myID, ownID, myNick, nil, nil)
	if len(got) != 1 || got[0].ID != 101 {
		t.Fatalf("кандидаты: %v", ids(got))
	}
}

// TestReplyCandidatesStaleNick — протухший ник ломает детект молча, и это
// видно ровно так: обращённые к нам реплики перестают быть кандидатами.
func TestReplyCandidatesStaleNick(t *testing.T) {
	comments := []love.Comment{
		{ID: 100, AuthorID: ownID, AuthorName: myNick, Text: "своя реплика"},
		{ID: 101, ParentID: myID, AuthorID: "u9", AuthorName: "Некто", Text: "мимо проходил"},
		{ID: 102, ParentID: myID, AuthorID: "u2", AuthorName: "Лампочка", Text: "Рантье, ты опять за своё"},
	}
	if got := replyCandidates(comments, myID, ownID, "Монах", nil, nil); len(got) != 1 {
		t.Errorf("со старым ником в кандидатах остаётся только первый в ветке: %v", ids(got))
	}
	if got := replyCandidates(comments, myID, ownID, myNick, nil, nil); len(got) != 2 {
		t.Errorf("со свежим ником кандидатов двое: %v", ids(got))
	}
}

func TestReplyCandidatesGuards(t *testing.T) {
	if got := replyCandidates(branch(), 0, ownID, myNick, nil, nil); got != nil {
		t.Error("без своей реплики кандидатов быть не может")
	}
	if got := replyCandidates(branch(), myID, "", myNick, nil, nil); got != nil {
		t.Error("без своей анкеты кандидатов быть не может")
	}
}

func TestRenderReply(t *testing.T) {
	if got := renderReply("Лампочка", "смирение"); got != "Лампочка, смирение" {
		t.Errorf("обращение подставляет инструмент: %q", got)
	}
	if got := renderReply("  ", "смирение"); got != "смирение" {
		t.Errorf("без ника — без обращения: %q", got)
	}
}

func TestRepliesSentAndAnswered(t *testing.T) {
	decided := []store.PulpitReply{
		{ReplyToID: 1, AuthorID: "u1", State: store.PulpitSkipped, Reason: reasonCoin},
		{ReplyToID: 2, AuthorID: "u2", State: store.PulpitPosted},
		{ReplyToID: 3, AuthorID: "u3", State: store.PulpitFailed},
	}
	if n := repliesSent(decided); n != 1 {
		t.Errorf("отправленных ответов %d, ожидался 1", n)
	}
	seen, answered := decidedSets(decided)
	if len(seen) != 3 {
		t.Errorf("решения приняты по трём репликам: %v", seen)
	}
	if !answered["u2"] || answered["u1"] || answered["u3"] {
		t.Errorf("отвечали только u2: %v", answered)
	}
}

func ids(comments []love.Comment) []int64 {
	out := make([]int64, 0, len(comments))
	for _, c := range comments {
		out = append(out, c.ID)
	}
	return out
}
