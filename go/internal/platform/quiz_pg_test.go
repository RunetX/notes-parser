package platform

// Пятничный вопрос (эпик J, миграция 0029) против настоящего Postgres.
//
// Подделкой это не проверить ничем: вопрос кладётся ТОЙ ЖЕ транзакцией, что и
// задавшая его реплика, ограничение «один ответ с человека» держит база, а
// правило «счёт видит только ответивший» — форма самого запроса.

import (
	"context"
	"errors"
	"testing"
)

// mustQuizNote — заметка, под которой задаются вопросы, плюс сама площадка как
// автор: вопрос вправе задать только она (или администратор).
func mustQuizNote(t *testing.T, p *Platform, admin int64) int64 {
	t.Helper()
	id, err := p.CreateNote(context.Background(), NewNote{AuthorID: admin, Body: "пятничные вопросы"})
	if err != nil {
		t.Fatalf("заметка рубрики: %v", err)
	}
	return id
}

func sampleQuiz() *NewQuiz {
	return &NewQuiz{
		Options:    []string{"2011", "2016", "2021"},
		Right:      1,
		Reveal:     "9 сентября 2016 года, 261 реплика.",
		SourceNote: 270011,
	}
}

// mustAsk — вопрос от имени площадки.
func mustAsk(t *testing.T, p *Platform, author, noteID int64, q *NewQuiz) int64 {
	t.Helper()
	id, err := p.CreateComment(context.Background(), NewComment{
		NoteID: noteID, AuthorID: author, Body: "В каком году это написано?", Quiz: q,
	})
	if err != nil {
		t.Fatalf("вопрос: %v", err)
	}
	return id
}

// Вопрос кладётся вместе с репликой и читается обратно целиком.
func TestQuizIsPublishedWithItsComment(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "админ рубрики")
	note := mustQuizNote(t, p, admin)
	cid := mustAsk(t, p, admin, note, sampleQuiz())

	got, err := p.NoteQuiz(ctx, admin, note)
	if err != nil {
		t.Fatalf("чтение вопросов: %v", err)
	}
	q, ok := got[cid]
	if !ok {
		t.Fatalf("вопроса %d нет среди %v", cid, got)
	}
	if len(q.Options) != 3 || q.Options[1].Text != "2016" {
		t.Errorf("варианты приехали как %+v", q.Options)
	}
	if q.Right != 1 || q.SourceNote != 270011 {
		t.Errorf("правильный %d, источник %d", q.Right, q.SourceNote)
	}
	if q.Answered() {
		t.Error("не отвечавший числится ответившим")
	}
}

// СЧЁТ ОТДАЁТСЯ ТОЛЬКО ОТВЕТИВШЕМУ: не ответившему проценты — подсказка, потому
// что большинство обычно право. Правило стоит в ядре, а не в шаблоне, ровно
// затем, чтобы чужие голоса не оказались в разметке «на всякий случай».
func TestQuizTallyIsHiddenUntilYouAnswer(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "ведущий")
	note := mustQuizNote(t, p, admin)
	cid := mustAsk(t, p, admin, note, sampleQuiz())

	answered := mustUser(t, p, "ответивший")
	watcher := mustUser(t, p, "молчун")
	if err := p.AnswerQuiz(ctx, QuizAnswer{UserID: answered, NoteID: note, CommentID: cid, Choice: 1}); err != nil {
		t.Fatalf("ответ: %v", err)
	}

	mine, err := p.NoteQuiz(ctx, answered, note)
	if err != nil {
		t.Fatalf("чтение своих: %v", err)
	}
	if q := mine[cid]; !q.Answered() || q.MyChoice != 1 || q.Total != 1 || q.Options[1].Votes != 1 {
		t.Errorf("ответившему счёт не показан: %+v", q)
	}

	theirs, err := p.NoteQuiz(ctx, watcher, note)
	if err != nil {
		t.Fatalf("чтение чужих: %v", err)
	}
	q := theirs[cid]
	if q.Answered() || q.Total != 0 {
		t.Errorf("молчун видит счёт: %+v", q)
	}
	for i, o := range q.Options {
		if o.Votes != 0 {
			t.Errorf("вариант %d показал %d голосов не ответившему", i, o.Votes)
		}
	}
}

// Первый ответ окончателен. Иначе после разгадки правильный выбрали бы все, и
// проценты под вопросом перестали бы что-либо значить.
func TestQuizAnswerCannotBeChanged(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "ведущий рубрики")
	note := mustQuizNote(t, p, admin)
	cid := mustAsk(t, p, admin, note, sampleQuiz())
	user := mustUser(t, p, "передумавший")

	if err := p.AnswerQuiz(ctx, QuizAnswer{UserID: user, NoteID: note, CommentID: cid, Choice: 0}); err != nil {
		t.Fatalf("первый ответ: %v", err)
	}
	// Повтор — не ошибка: двойной клик и «назад» в браузере обычное дело.
	if err := p.AnswerQuiz(ctx, QuizAnswer{UserID: user, NoteID: note, CommentID: cid, Choice: 1}); err != nil {
		t.Fatalf("повторный ответ отвечен ошибкой: %v", err)
	}
	got, err := p.NoteQuiz(ctx, user, note)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	if q := got[cid]; q.MyChoice != 0 {
		t.Errorf("ответ переписан на %d", q.MyChoice)
	}
}

// Вопрос задаёт площадка, а не участник: под ним стоят кнопки, которых нет ни у
// кого другого, и «правильный ответ» объявляет он же.
func TestOnlyPlatformAsksQuestions(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "главный")
	note := mustQuizNote(t, p, admin)
	member := mustUser(t, p, "участник")

	_, err := p.CreateComment(ctx, NewComment{
		NoteID: note, AuthorID: member, Body: "а вот кто здесь дурак?", Quiz: sampleQuiz(),
	})
	if !errors.Is(err, ErrNotQuizMaster) {
		t.Fatalf("участнику дали задать вопрос: %v", err)
	}
	// Обычная реплика того же участника проходит: закрыт именно вопрос.
	if _, err := p.CreateComment(ctx, NewComment{
		NoteID: note, AuthorID: member, Body: "а я думаю, 2016",
	}); err != nil {
		t.Fatalf("обычная реплика отвергнута: %v", err)
	}
}

// Служебная анкета площадки — тот, кто рубрику и ведёт.
func TestSystemAccountAsksQuestions(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "заводящий")
	note := mustQuizNote(t, p, admin)

	system, err := p.EnsureSystemUser(ctx, "Зазеркалье")
	if err != nil {
		t.Fatalf("служебная анкета: %v", err)
	}
	if _, err := p.CreateComment(ctx, NewComment{
		NoteID: note, AuthorID: system, Body: "Сколько реплик она собрала?", Quiz: sampleQuiz(),
	}); err != nil {
		t.Fatalf("площадке не дали задать вопрос: %v", err)
	}
}

// Кривой вопрос не публикуется вовсе — и реплики после отказа тоже не остаётся:
// вопрос и его реплика живут одной транзакцией.
func TestBadQuizPublishesNothing(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "торопливый")
	note := mustQuizNote(t, p, admin)

	for _, tc := range []struct {
		name string
		q    *NewQuiz
	}{
		{"один вариант", &NewQuiz{Options: []string{"да"}, Right: 0}},
		{"пустой вариант", &NewQuiz{Options: []string{"да", "  "}, Right: 0}},
		{"правильный за краем", &NewQuiz{Options: []string{"да", "нет"}, Right: 5}},
	} {
		if _, err := p.CreateComment(ctx, NewComment{
			NoteID: note, AuthorID: admin, Body: "вопрос", Quiz: tc.q,
		}); !errors.Is(err, ErrBadQuiz) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// В треде — ни одной реплики: все три отката унесли за собой и комментарий.
	thread, err := p.Thread(ctx, Viewer{UserID: admin, Role: RoleAdmin}, note)
	if err != nil {
		t.Fatalf("тред: %v", err)
	}
	if len(thread) != 0 {
		t.Errorf("после отказов в треде осталось %d реплик", len(thread))
	}
}

// Ответ на чужой вопрос с подменённой заметкой отвергается: форму правят руками,
// и голос уехал бы к чужому треду.
func TestQuizAnswerChecksItsNote(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "хозяин")
	note := mustQuizNote(t, p, admin)
	// Вторая заметка — от ДРУГОГО автора: свой потолок частоты (одна заметка в
	// пять минут) иначе отбил бы её у того же самого.
	other := mustQuizNote(t, p, mustAdmin(t, p, "сосед"))
	cid := mustAsk(t, p, admin, note, sampleQuiz())
	user := mustUser(t, p, "хитрец")

	err := p.AnswerQuiz(ctx, QuizAnswer{UserID: user, NoteID: other, CommentID: cid, Choice: 1})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("голос ушёл в чужой тред: %v", err)
	}
	err = p.AnswerQuiz(ctx, QuizAnswer{UserID: user, NoteID: note, CommentID: cid, Choice: 9})
	if !errors.Is(err, ErrBadQuiz) {
		t.Fatalf("принят несуществующий вариант: %v", err)
	}
}

// Сводка вопроса — для команд и отчётов: сколько ответили и сколько угадали.
func TestQuizStatsCountRight(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "счетовод")
	note := mustQuizNote(t, p, admin)
	cid := mustAsk(t, p, admin, note, sampleQuiz())

	for i, choice := range []int{1, 1, 0} {
		u := mustUser(t, p, "игрок"+string(rune('A'+i)))
		if err := p.AnswerQuiz(ctx, QuizAnswer{UserID: u, NoteID: note, CommentID: cid, Choice: choice}); err != nil {
			t.Fatalf("ответ %d: %v", i, err)
		}
	}
	stats, err := p.QuizStats(ctx, note)
	if err != nil {
		t.Fatalf("сводка: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("вопросов в сводке %d", len(stats))
	}
	if s := stats[0]; s.Votes != 3 || s.Right != 2 || s.SourceNote != 270011 {
		t.Errorf("сводка = %+v", s)
	}
}
