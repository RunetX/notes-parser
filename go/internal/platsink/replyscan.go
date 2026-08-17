package platsink

// Обход тредов сайта ради двух вещей, которых зеркалу взять неоткуда:
// настоящего дерева ответов и пола участников.
//
// Дерево. Живое зеркало знает адресата только по обращению «Ник, …» и
// разрешает его в ПОСЛЕДНЮЮ реплику этого человека в заметке — угадывание с
// точностью около половины. Настоящее ребро отдаёт МОБИЛЬНАЯ версия
// (`love.FetchNoteReplyTree`), там 92 %.
//
// Пол. Красит ник, как на сайте, и стоит прямо в разметке ДЕСКТОПНОЙ страницы
// комментариев рядом с номером анкеты — в мобильной его нет вовсе (проверено
// 18.08.2026). Поэтому страниц две, но и та, и другая берутся одним заходом на
// заметку, а не обходом полутора тысяч анкет.
//
// Окно закрывается вместе с сайтом: НГС уже не принимает комментарии, и всё,
// что не снято сегодня, не будет снято никогда.

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/platform"
)

// TreeSource — что нужно обходчику от сайта. Интерфейс, а не *love.Client:
// тесты не должны ходить в интернет.
type TreeSource interface {
	FetchNoteReplyTree(ctx context.Context, noteID string) (map[int64]int64, error)
	FetchGenders(ctx context.Context, noteID string) (map[int64]string, error)
}

// ReplyScanStats — итог прохода.
type ReplyScanStats struct {
	Notes    int // заметок обойдено
	Failed   int // из них с отказом сайта
	Edges    int // переставлено рёбер
	Trimmed  int // снято обращений из тела
	Genders  int // проставлено полов
	Comments int // комментариев просмотрено
}

// ReplyScanner — обходчик. Темп задаёт сам клиент сайта (StrictPacing), здесь
// только очередь и учёт.
type ReplyScanner struct {
	p    *platform.Platform
	site TreeSource
	log  *slog.Logger
}

func NewReplyScanner(p *platform.Platform, site TreeSource, log *slog.Logger) *ReplyScanner {
	if log == nil {
		log = slog.Default()
	}
	return &ReplyScanner{p: p, site: site, log: log}
}

// Once обходит до limit заметок из очереди. Отказ сайта на одной заметке обход
// не рвёт: 403 и 500 приходят волнами, а очередь устроена так, что следующий
// проход вернётся к той же заметке.
func (s *ReplyScanner) Once(ctx context.Context, limit int) (ReplyScanStats, error) {
	var st ReplyScanStats
	ids, err := s.p.ReplyScanDue(ctx, limit)
	if err != nil {
		return st, err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		one, err := s.note(ctx, id)
		st.Notes++
		st.Comments += one.Comments
		st.Edges += one.Edges
		st.Trimmed += one.Trimmed
		st.Genders += one.Genders
		if err != nil {
			st.Failed++
			s.log.Warn("обход дерева не удался", "note", id, "err", err)
			if merr := s.p.MarkReplyScan(ctx, id, false); merr != nil {
				return st, merr
			}
			continue
		}
		if err := s.p.MarkReplyScan(ctx, id, true); err != nil {
			return st, err
		}
	}
	return st, nil
}

// Note обходит одну заметку и отмечает её. Стенд для проверки: видно, что
// именно поменялось в конкретном треде.
func (s *ReplyScanner) Note(ctx context.Context, id int64) (ReplyScanStats, error) {
	st, err := s.note(ctx, id)
	st.Notes = 1
	if err != nil {
		st.Failed = 1
		if merr := s.p.MarkReplyScan(ctx, id, false); merr != nil {
			return st, merr
		}
		return st, err
	}
	return st, s.p.MarkReplyScan(ctx, id, true)
}

// note обходит одну заметку. Пол — вторым запросом и «по возможности»: его
// отказ не должен отменять уже снятое дерево, ради которого всё и затевалось.
func (s *ReplyScanner) note(ctx context.Context, id int64) (ReplyScanStats, error) {
	var st ReplyScanStats
	sid := strconv.FormatInt(id, 10)

	tree, err := s.site.FetchNoteReplyTree(ctx, sid)
	if err != nil {
		return st, fmt.Errorf("дерево заметки %d: %w", id, err)
	}
	applied, err := s.p.ApplyReplyTree(ctx, id, tree)
	if err != nil {
		return st, err
	}
	st.Comments, st.Edges, st.Trimmed = applied.Total, applied.Edges, applied.Trimmed

	genders, err := s.site.FetchGenders(ctx, sid)
	if err != nil {
		s.log.Warn("пол участников не снят", "note", id, "err", err)
		return st, nil
	}
	n, err := s.p.SetGenders(ctx, convertGenders(genders))
	if err != nil {
		return st, err
	}
	st.Genders = n
	return st, nil
}

// convertGenders переводит значения сайта в наши. Перевод живёт здесь, а не в
// ядре: `platform` о существовании НГС не знает и знать не должен.
func convertGenders(in map[int64]string) map[int64]platform.Gender {
	out := make(map[int64]platform.Gender, len(in))
	for id, g := range in {
		switch g {
		case love.GenderMale:
			out[id] = platform.GenderMale
		case love.GenderFemale:
			out[id] = platform.GenderFemale
		}
	}
	return out
}

// Run гоняет обход в фоне. Пауза между кругами большая намеренно: дерево у
// заметки меняется только вместе с новыми комментариями, а их на НГС сейчас
// нет вовсе — сегодня это добор истории, а не слежение.
func (s *ReplyScanner) Run(ctx context.Context, every time.Duration, batch int) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			st, err := s.Once(ctx, batch)
			if err != nil && ctx.Err() == nil {
				s.log.Error("обход дерева", "err", err)
			}
			if st.Edges > 0 || st.Genders > 0 {
				s.log.Info("дерево уточнено", "заметок", st.Notes,
					"рёбер", st.Edges, "обращений снято", st.Trimmed, "полов", st.Genders)
			}
		}
	}
}
