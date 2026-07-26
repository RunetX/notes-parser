// Пакет legacy читает файлы состояния старой Python-версии
// (notes.json, subscribers.json, sessions_export.json) и импортирует их
// в хранилище. Импорт идемпотентен: повторный запуск ничего не меняет.
package legacy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// Stats — итоги импорта для вывода пользователю.
type Stats struct {
	Notes         int
	Comments      int
	Sessions      int
	Subscriptions int
	Warnings      []string
}

// flexInt терпит старый формат: поле бывает числом, строкой ("" — нет значения)
// или null (note_model инициализирует tg_message_id пустой строкой).
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" || s == `""` {
		*f = 0
		return nil
	}
	if len(s) >= 2 && s[0] == '"' {
		n, err := strconv.ParseInt(s[1:len(s)-1], 10, 64)
		if err != nil {
			return fmt.Errorf("flexInt: %q", s)
		}
		*f = flexInt(n)
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("flexInt: %q", s)
	}
	*f = flexInt(n)
	return nil
}

// Формат notes.json (poster.py: note_model / comment_model).
type legacyNote struct {
	ID             string          `json:"id"`
	AuthorID       string          `json:"author_id"`
	AuthorName     string          `json:"author_name"`
	Text           string          `json:"text"`
	TGMessageID    flexInt         `json:"tg_message_id"`
	TGDiscussionID flexInt         `json:"tg_discussion_id"`
	Comments       []legacyComment `json:"comments"`
}

type legacyComment struct {
	ID          int64   `json:"id"`
	AuthorName  string  `json:"author_name"`
	AuthorAge   string  `json:"author_age"`
	AuthorLink  string  `json:"author_link"`
	Avatar      string  `json:"avatar"`
	Date        string  `json:"date"`
	Text        string  `json:"text"`
	TGMessageID flexInt `json:"tg_message_id"`
}

// legacyDateLayout — формат дат в notes.json (новосибирское время).
const legacyDateLayout = "02.01.2006, 15:04:05"

var nsk = loadNSK()

func loadNSK() *time.Location {
	if loc, err := time.LoadLocation("Asia/Novosibirsk"); err == nil {
		return loc
	}
	return time.FixedZone("NOVT", 7*3600)
}

// ImportNotes переносит notes.json: заметки получают статус posted (они уже
// в канале), комментарии сохраняют свои tg id — мост продолжает работать.
func ImportNotes(ctx context.Context, st *store.Store, r io.Reader, now time.Time) (Stats, error) {
	var stats Stats
	var notes []legacyNote
	if err := json.NewDecoder(r).Decode(&notes); err != nil {
		return stats, fmt.Errorf("разбор notes.json: %w", err)
	}
	for _, ln := range notes {
		if ln.ID == "" {
			stats.Warnings = append(stats.Warnings, "заметка без id пропущена")
			continue
		}
		added, err := st.InsertNote(ctx, store.Note{
			ID:          ln.ID,
			AuthorID:    orDefault(ln.AuthorID, "0"),
			AuthorName:  orDefault(ln.AuthorName, "Анонимно"),
			Text:        ln.Text,
			Status:      store.StatusPosted,
			TGMessageID: int64(ln.TGMessageID),
			TGThreadID:  int64(ln.TGDiscussionID),
			FirstSeenAt: now,
		})
		if err != nil {
			return stats, err
		}
		if added {
			stats.Notes++
		}
		for _, lc := range ln.Comments {
			published, err := time.ParseInLocation(legacyDateLayout, lc.Date, nsk)
			if err != nil {
				// Дата в старых данных бывает битой — импортируем без неё.
				stats.Warnings = append(stats.Warnings,
					fmt.Sprintf("комментарий %d: не разобрана дата %q", lc.ID, lc.Date))
				published = time.Time{}
			}
			added, err := st.InsertComment(ctx, store.Comment{
				ID:          lc.ID,
				NoteID:      ln.ID,
				AuthorName:  lc.AuthorName,
				AuthorAge:   lc.AuthorAge,
				AuthorLink:  lc.AuthorLink,
				AvatarURL:   lc.Avatar,
				PublishedAt: published,
				Text:        lc.Text,
				TGMessageID: int64(lc.TGMessageID),
				CreatedAt:   now,
			})
			if err != nil {
				return stats, err
			}
			if added {
				stats.Comments++
			}
		}
	}
	return stats, nil
}

// Cookie — формат куки в sessions_export.json (см. tools/export_sessions.py).
// Совпадает с форматом хранения в БД.
type Cookie = love.SessionCookie

// ImportSessions переносит куки пользователей из sessions_export.json.
func ImportSessions(ctx context.Context, st *store.Store, r io.Reader, now time.Time) (Stats, error) {
	var stats Stats
	var sessions map[string][]Cookie
	if err := json.NewDecoder(r).Decode(&sessions); err != nil {
		return stats, fmt.Errorf("разбор sessions_export.json: %w", err)
	}
	for uid, cookies := range sessions {
		tgUserID, err := strconv.ParseInt(uid, 10, 64)
		if err != nil {
			stats.Warnings = append(stats.Warnings, "сессия с нечисловым id пропущена: "+uid)
			continue
		}
		if len(cookies) == 0 {
			stats.Warnings = append(stats.Warnings, "сессия без кук пропущена: "+uid)
			continue
		}
		cookiesJSON, err := json.Marshal(cookies)
		if err != nil {
			return stats, err
		}
		if err := st.UpsertSession(ctx, store.MessengerTelegram, tgUserID, string(cookiesJSON), now); err != nil {
			return stats, err
		}
		stats.Sessions++
	}
	return stats, nil
}

// Формат subscribers.json: [{"key": "слово", "value": tg_user_id}].
type legacySubscriber struct {
	Key   string  `json:"key"`
	Value flexInt `json:"value"`
}

// ImportSubscribers переносит подписки на ключевые слова.
func ImportSubscribers(ctx context.Context, st *store.Store, r io.Reader) (Stats, error) {
	var stats Stats
	var subs []legacySubscriber
	if err := json.NewDecoder(r).Decode(&subs); err != nil {
		return stats, fmt.Errorf("разбор subscribers.json: %w", err)
	}
	for _, sub := range subs {
		if sub.Key == "" || sub.Value == 0 {
			stats.Warnings = append(stats.Warnings,
				fmt.Sprintf("подписка пропущена: key=%q value=%d", sub.Key, int64(sub.Value)))
			continue
		}
		added, err := st.AddSubscription(ctx, store.MessengerTelegram, sub.Key, int64(sub.Value))
		if err != nil {
			return stats, err
		}
		if added {
			stats.Subscriptions++
		}
	}
	return stats, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
