package tgx

import (
	"context"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"

	"lovegw/internal/asr"
	"lovegw/internal/store"
)

const testChatID = int64(-1001234567890)

// fakeQueue записывает поставленные задачи; full — имитация переполнения.
type fakeQueue struct {
	jobs []asr.Job
	full bool
}

func (q *fakeQueue) Enqueue(job asr.Job) bool {
	if q.full {
		return false
	}
	q.jobs = append(q.jobs, job)
	return true
}

func voiceUpdate(msg *models.Message) *models.Update { return &models.Update{Message: msg} }

func TestVoiceHandlerEnqueuesVoice(t *testing.T) {
	q := &fakeQueue{}
	h := NewVoiceHandler(nil, q, testChatID, nil)

	h.Handle(context.Background(), voiceUpdate(&models.Message{
		ID:    77,
		Chat:  models.Chat{ID: testChatID},
		From:  &models.User{ID: 42},
		Voice: &models.Voice{FileID: "AgADvoice", FileUniqueID: "AQADunique", Duration: 15},
	}))

	if len(q.jobs) != 1 {
		t.Fatalf("задач в очереди: %d", len(q.jobs))
	}
	job := q.jobs[0]
	if job.Messenger != store.MessengerTelegram || job.FileKey != "AQADunique" ||
		job.Duration != 15 || job.UserID != 42 {
		t.Errorf("задача: %+v", job)
	}
	if job.Fetch == nil || job.Reply == nil {
		t.Error("транспортные замыкания должны быть заполнены")
	}
}

func TestVoiceHandlerEnqueuesVideoNote(t *testing.T) {
	q := &fakeQueue{}
	h := NewVoiceHandler(nil, q, testChatID, nil)

	h.Handle(context.Background(), voiceUpdate(&models.Message{
		ID:        78,
		Chat:      models.Chat{ID: testChatID},
		From:      &models.User{ID: 42},
		VideoNote: &models.VideoNote{FileID: "AgADnote", FileUniqueID: "AQADnote", Duration: 20},
	}))

	if len(q.jobs) != 1 || q.jobs[0].FileKey != "AQADnote" || q.jobs[0].Duration != 20 {
		t.Fatalf("кружок: %+v", q.jobs)
	}
}

// Голосовое, запощенное в канал заметок, приходит в группу автофорвардом:
// распознаём и его, квоту пишем на канал.
func TestVoiceHandlerEnqueuesChannelAutoForward(t *testing.T) {
	const channelID = int64(-1009876543210)
	q := &fakeQueue{}
	h := NewVoiceHandler(nil, q, testChatID, nil)

	h.Handle(context.Background(), voiceUpdate(&models.Message{
		ID:                 79,
		Chat:               models.Chat{ID: testChatID},
		From:               &models.User{ID: 777, IsBot: true}, // служебный бот автофорварда
		SenderChat:         &models.Chat{ID: channelID},
		IsAutomaticForward: true,
		Voice:              &models.Voice{FileID: "AgADch", FileUniqueID: "AQADch", Duration: 30},
	}))

	if len(q.jobs) != 1 {
		t.Fatalf("голосовое из канала должно распознаваться: %+v", q.jobs)
	}
	if q.jobs[0].UserID != channelID {
		t.Errorf("квота автофорварда пишется на канал, а не %d", q.jobs[0].UserID)
	}
}

func TestVoiceHandlerIgnores(t *testing.T) {
	cases := []struct {
		name string
		msg  *models.Message
	}{
		{"чужой чат", &models.Message{
			ID: 1, Chat: models.Chat{ID: 999}, From: &models.User{ID: 42},
			Voice: &models.Voice{FileID: "a", FileUniqueID: "b", Duration: 5},
		}},
		{"текстовое сообщение", &models.Message{
			ID: 2, Chat: models.Chat{ID: testChatID}, From: &models.User{ID: 42}, Text: "просто текст",
		}},
		{"голосовое от бота", &models.Message{
			ID: 3, Chat: models.Chat{ID: testChatID}, From: &models.User{ID: 42, IsBot: true},
			Voice: &models.Voice{FileID: "a", FileUniqueID: "b", Duration: 5},
		}},
		{"автофорвард без голосового", &models.Message{
			ID: 4, Chat: models.Chat{ID: testChatID}, IsAutomaticForward: true,
			SenderChat: &models.Chat{ID: -100999}, Text: "текст заметки",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := &fakeQueue{}
			NewVoiceHandler(nil, q, testChatID, nil).Handle(context.Background(), voiceUpdate(c.msg))
			if len(q.jobs) != 0 {
				t.Errorf("задача не должна была появиться: %+v", q.jobs)
			}
		})
	}
	t.Run("пустое обновление", func(t *testing.T) {
		q := &fakeQueue{}
		h := NewVoiceHandler(nil, q, testChatID, nil)
		h.Handle(context.Background(), nil)
		h.Handle(context.Background(), &models.Update{})
		if len(q.jobs) != 0 {
			t.Errorf("задача не должна была появиться: %+v", q.jobs)
		}
	})
}

func TestVoiceHandlerSurvivesFullQueue(t *testing.T) {
	q := &fakeQueue{full: true}
	NewVoiceHandler(nil, q, testChatID, nil).Handle(context.Background(), voiceUpdate(&models.Message{
		ID: 80, Chat: models.Chat{ID: testChatID}, From: &models.User{ID: 42},
		Voice: &models.Voice{FileID: "a", FileUniqueID: "b", Duration: 5},
	}))
	// Проверяем только то, что переполнение не роняет хендлер: поллинг должен
	// продолжать работать, а голосовое просто теряется с записью в лог.
}

func TestSplitRunes(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		limit int
		want  []string
	}{
		{"пусто", "", 10, nil},
		{"влезает", "привет", 10, []string{"привет"}},
		{"режется по пробелу", "привет мир как дела", 10, []string{"привет", "мир как", "дела"}},
		{"без пробелов режется жёстко", strings.Repeat("я", 25), 10,
			[]string{strings.Repeat("я", 10), strings.Repeat("я", 10), strings.Repeat("я", 5)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitRunes(c.text, c.limit)
			if len(got) != len(c.want) {
				t.Fatalf("кусков %d, ожидалось %d: %q", len(got), len(c.want), got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("кусок %d: %q, ожидалось %q", i, got[i], c.want[i])
				}
				if len([]rune(got[i])) > c.limit {
					t.Errorf("кусок %d длиннее лимита: %d рун", i, len([]rune(got[i])))
				}
			}
		})
	}
}
