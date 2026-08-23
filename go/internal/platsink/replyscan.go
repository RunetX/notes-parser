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

// Once обходит до limit заметок из очереди ДОБОРА ИСТОРИИ. Отказ сайта на одной
// заметке обход не рвёт: 403 и 500 приходят волнами, а очередь устроена так, что
// следующий проход вернётся к той же заметке.
func (s *ReplyScanner) Once(ctx context.Context, limit int) (ReplyScanStats, error) {
	ids, err := s.p.ReplyScanDue(ctx, limit)
	if err != nil {
		return ReplyScanStats{}, err
	}
	return s.walk(ctx, ids)
}

// Fresh обходит ЖИВЫЕ треды — те, где реплики появились после прошлого обхода
// (platform.ReplyScanFresh). Это и есть работа демона: историю добирает админ
// командой, а живому треду настоящие рёбра нужны сейчас, пока люди в нём
// разговаривают.
func (s *ReplyScanner) Fresh(ctx context.Context, limit int, fresh, gap time.Duration) (ReplyScanStats, error) {
	ids, err := s.p.ReplyScanFresh(ctx, limit, fresh, gap)
	if err != nil {
		return ReplyScanStats{}, err
	}
	return s.walk(ctx, ids)
}

// walk обходит названные заметки. Общий ход обеих очередей: отличаются они
// только тем, КОГО обходить.
func (s *ReplyScanner) walk(ctx context.Context, ids []int64) (ReplyScanStats, error) {
	var st ReplyScanStats
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

// Такт службы. Числа отвечают за разное, поэтому их три.
const (
	// ScanInterval — как часто заглядывать в очередь. Минута: обход стоит двух
	// запросов к сайту на заметку, и живых тредов там единицы.
	ScanInterval = time.Minute
	// ScanBatch — сколько заметок за проход. Больше незачем: очередь всё равно
	// вернётся через минуту, а сайт делится полосой с зеркалом.
	ScanBatch = 3
	// FreshWindow — какой тред считается живым. Сутки: дальше это уже история,
	// и её добирает своя очередь (Once), которую водит админ.
	FreshWindow = 24 * time.Hour
	// RescanGap — не чаще, чем раз в столько, по одной заметке. Иначе бойкий
	// тред обходился бы на каждую реплику; пять минут — цена того, что чужой
	// ответ несколько минут повисит в угаданной ветке.
	RescanGap = 5 * time.Minute
)

// Run следит за ЖИВЫМИ тредами: раз в такт берёт те, где после прошлого обхода
// дописали, и переставляет рёбра по мобильной версии.
//
// Служба, а не разовая команда, потому что угадывание ошибается заметно: замер
// по заметке 313000 — 187 переставленных рёбер из 444, а 23.08.2026 ответ и
// вовсе уехал в чужую ветку. Историю при этом по-прежнему добирает админ
// (`platform reply-scan`): это тысячи запросов, и решать, когда их тратить,
// демону не по чину.
//
// Отказ прохода демона не роняет: логируется и всё. Дерево — уточнение поверх
// разговора, а сам разговор несёт зеркало.
func (s *ReplyScanner) Run(ctx context.Context) error {
	t := time.NewTicker(ScanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			st, err := s.Fresh(ctx, ScanBatch, FreshWindow, RescanGap)
			if err != nil && ctx.Err() == nil {
				s.log.Error("обход живых тредов", "err", err)
			}
			if st.Edges > 0 || st.Genders > 0 {
				s.log.Info("дерево уточнено", "заметок", st.Notes,
					"рёбер", st.Edges, "обращений снято", st.Trimmed, "полов", st.Genders)
			}
		}
	}
}
