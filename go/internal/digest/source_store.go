package digest

// Зеркало НГС как источник выпуска: SQLite, из которого дайджест жил с самого
// начала. Остаётся для работы без площадки — и как то, по чему считались
// выпуски W31…W34.
//
// Идентичность человека здесь — ССЫЛКА НА АНКЕТУ: числового id у комментария в
// зеркале нет вовсе, есть только href в шапке. Поэтому Author у комментатора —
// это URL, а у автора заметки — номер анкеты, и приводит их к одному виду сам
// адаптер (иначе один и тот же человек оказался бы в выпуске дважды).

import (
	"context"
	"strings"
	"time"

	"lovegw/internal/store"
)

// StoreSource — источник поверх зеркальной базы.
type StoreSource struct {
	st       *store.Store
	siteBase string // адрес НГС для ссылок на анкеты
}

// NewStoreSource — источник поверх SQLite зеркала.
func NewStoreSource(st *store.Store, siteBase string) *StoreSource {
	return &StoreSource{st: st, siteBase: strings.TrimSuffix(siteBase, "/")}
}

// profileKey приводит ссылку на анкету к ключу слияния: без схемы, хвостового
// «/» и различий в написании — иначе комментатор и автор заметок разъезжаются.
func profileKey(url string) string {
	if url == "" {
		return ""
	}
	return strings.TrimSuffix(url, "/")
}

func (s *StoreSource) authorKey(profileID string) string {
	if profileID == "" || profileID == "0" {
		return ""
	}
	return profileKey(s.siteBase + "/profile/" + profileID)
}

func (s *StoreSource) CommentsBetween(ctx context.Context, start, end time.Time) ([]Comment, error) {
	rows, err := s.st.CommentsBetween(ctx, start, end)
	if err != nil {
		return nil, err
	}
	out := make([]Comment, 0, len(rows))
	for _, c := range rows {
		out = append(out, s.comment(c))
	}
	return out, nil
}

func (s *StoreSource) comment(c store.Comment) Comment {
	at := c.PublishedAt
	if at.IsZero() {
		at = c.CreatedAt // время сайта бывает неизвестно, момент вставки — всегда
	}
	return Comment{
		ID: c.ID, NoteID: c.NoteID, Author: profileKey(c.AuthorLink),
		AuthorName: c.AuthorName, Text: c.Text, PublishedAt: at,
	}
}

func (s *StoreSource) note(n store.Note) Note {
	return Note{
		ID: n.ID, Author: s.authorKey(n.AuthorID), AuthorName: n.AuthorName,
		Text: n.Text, PublishedAt: n.FirstSeenAt,
	}
}

func (s *StoreSource) NotesByIDs(ctx context.Context, ids []string) (map[string]Note, error) {
	rows, err := s.st.NotesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Note, len(rows))
	for id, n := range rows {
		out[id] = s.note(n)
	}
	return out, nil
}

func (s *StoreSource) NotesPublishedBetween(ctx context.Context, start, end time.Time) ([]Note, error) {
	rows, err := s.st.NotesSeenBetween(ctx, start, end)
	if err != nil {
		return nil, err
	}
	return s.notes(rows), nil
}

func (s *StoreSource) ActiveNotesSince(ctx context.Context, since time.Time) ([]Note, error) {
	rows, err := s.st.ActiveNotesSince(ctx, since)
	if err != nil {
		return nil, err
	}
	return s.notes(rows), nil
}

func (s *StoreSource) notes(rows []store.Note) []Note {
	out := make([]Note, 0, len(rows))
	for _, n := range rows {
		out = append(out, s.note(n))
	}
	return out
}

func (s *StoreSource) CommenterHistory(ctx context.Context, start, end time.Time) ([]CommenterSeen, error) {
	rows, err := s.st.CommenterHistory(ctx, start, end)
	if err != nil {
		return nil, err
	}
	out := make([]CommenterSeen, 0, len(rows))
	for _, c := range rows {
		out = append(out, CommenterSeen{
			Author: profileKey(c.Link), Name: c.Name, InWindow: c.InWindow,
			FirstInWindow: c.FirstInWindow, PrevSeenAt: c.PrevSeenAt,
		})
	}
	return out, nil
}

func (s *StoreSource) NoteAuthorHistory(ctx context.Context, start, end time.Time) ([]AuthorSeen, error) {
	rows, err := s.st.NoteAuthorHistory(ctx, start, end)
	if err != nil {
		return nil, err
	}
	out := make([]AuthorSeen, 0, len(rows))
	for _, a := range rows {
		out = append(out, AuthorSeen{
			Author: s.authorKey(a.AuthorID), Name: a.Name,
			NotesInWindow: a.NotesInWindow, PrevNoteAt: a.PrevNoteAt,
		})
	}
	return out, nil
}

// NoteTotals — итоги за горизонт. Зеркальная база мельче площадки на три
// порядка, поэтому запрос считает всё, а горизонт применяется здесь: правило
// про «рекорд сезона» — свойство ВЫПУСКА, а не размера базы.
func (s *StoreSource) NoteTotals(ctx context.Context, since time.Time) ([]NoteTotals, error) {
	rows, err := s.st.NoteCommentTotals(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]NoteTotals, 0, len(rows))
	for _, t := range rows {
		if t.FirstSeenAt.Before(since) {
			continue
		}
		out = append(out, NoteTotals{
			NoteID: t.NoteID, PublishedAt: t.FirstSeenAt, Comments: t.Comments,
			Commenters: t.Commenters, FirstAt: t.FirstAt, LastAt: t.LastAt,
		})
	}
	return out, nil
}

func (s *StoreSource) PeakCommentHour(ctx context.Context, _ time.Time) (time.Time, string, int, error) {
	return s.st.PeakCommentHour(ctx)
}
