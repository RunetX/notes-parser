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
	"sync"
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

	// Подсказки приёмника: в какие заметки только что пришла реплика с
	// угаданным ребром. Живут в памяти и рестарт переживать не обязаны — после
	// него та же работа найдётся в общей очереди свежих тредов, просто минутами
	// позже.
	mu    sync.Mutex
	want  map[int64]bool      // названные, ещё не обойдённые
	last  map[int64]time.Time // когда по подсказке ходили в прошлый раз
	fails map[int64]int       // сколько раз подряд подсказка кончилась отказом
}

func NewReplyScanner(p *platform.Platform, site TreeSource, log *slog.Logger) *ReplyScanner {
	if log == nil {
		log = slog.Default()
	}
	return &ReplyScanner{
		p: p, site: site, log: log,
		want:  map[int64]bool{},
		last:  map[int64]time.Time{},
		fails: map[int64]int{},
	}
}

// Nudge — приёмник говорит: в эту заметку пришла реплика, и ребро у неё
// УГАДАНО по обращению «Ник, …» (Sink.addressee). Настоящее знает только
// мобильная страница треда, а момент, когда догадка появилась, известен ТОЧНО —
// и ждать ради него общей очереди (RescanGap, пять минут) незачем: всё это
// время ответ висит в чужой ветке у каждого, кто читает страницу, а под
// открытой страницей он потом ещё и переезжает на глазах.
//
// Ничего не обещает и не блокирует: подсказки схлопываются по заметке, ходят
// своим тактом и не чаще NudgeGap по одной. Заметка, дважды подряд отказавшая,
// замолкает совсем — её судьбу решает общая очередь, где счётчик неудач живёт
// в базе и гасит заметку насовсем (MarkReplyScan). Это не осторожность: сайт
// отвечает 500 как раз на самых длинных тредах, то есть на самых людных, — там
// подсказки идут чаще всего.
func (s *ReplyScanner) Nudge(noteID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fails[noteID] >= NudgeFails {
		return
	}
	s.want[noteID] = true
}

// dueNudged — какие подсказки пора обойти. Названную слишком рано заметку из
// набора НЕ выбрасываем: она дождётся своего срока, иначе бойкий тред,
// назвавший себя сразу после обхода, потерял бы подсказку вовсе.
func (s *ReplyScanner) dueNudged(now time.Time, limit int) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int64
	for id := range s.want {
		if len(out) >= limit {
			break
		}
		if t, ok := s.last[id]; ok && now.Sub(t) < NudgeGap {
			continue
		}
		out = append(out, id)
	}
	for _, id := range out {
		delete(s.want, id)
		s.last[id] = now
	}
	// Заметка, о которой сутки не вспоминали, — уже история: помнить про неё
	// нечего, и общая очередь свежих тредов её тоже не видит.
	for id, t := range s.last {
		if now.Sub(t) > FreshWindow {
			delete(s.last, id)
			delete(s.fails, id)
		}
	}
	return out
}

func (s *ReplyScanner) nudgeFailed(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails[id]++
}

func (s *ReplyScanner) nudgeDone(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.fails, id)
}

// nudged обходит названные приёмником заметки: ТОЛЬКО дерево, без пола.
//
// Пола здесь нет намеренно. Он меняется раз в жизни и приезжает общей очередью,
// а подсказка приходит на каждую реплику — второй запрос к сайту стоил бы ровно
// вдвое и не давал бы ничего.
//
// Отметку обхода в базе подсказка тоже НЕ ставит: общая очередь ходит своим
// ритмом и полным проходом, и сбив ей отсчёт, мы отложили бы пол ровно на то
// время, пока тред горячий, — то есть на всё то время, когда он и нужен.
func (s *ReplyScanner) nudged(ctx context.Context) {
	for _, id := range s.dueNudged(time.Now(), NudgeBatch) {
		if ctx.Err() != nil {
			return
		}
		st, err := s.tree(ctx, id)
		if err != nil {
			s.nudgeFailed(id)
			s.log.Warn("обход по подсказке не удался", "note", id, "err", err)
			continue
		}
		s.nudgeDone(id)
		if st.Edges > 0 {
			s.log.Info("ветка встала на место", "note", id,
				"рёбер", st.Edges, "обращений снято", st.Trimmed)
		}
	}
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

// tree — только дерево заметки. Половина note(), выделенная ради подсказок:
// им нужно ребро и как можно скорее, а пол подождёт общей очереди.
func (s *ReplyScanner) tree(ctx context.Context, id int64) (ReplyScanStats, error) {
	var st ReplyScanStats
	tree, err := s.site.FetchNoteReplyTree(ctx, strconv.FormatInt(id, 10))
	if err != nil {
		return st, fmt.Errorf("дерево заметки %d: %w", id, err)
	}
	applied, err := s.p.ApplyReplyTree(ctx, id, tree)
	if err != nil {
		return st, err
	}
	st.Comments, st.Edges, st.Trimmed = applied.Total, applied.Edges, applied.Trimmed
	return st, nil
}

// note обходит одну заметку. Пол — вторым запросом и «по возможности»: его
// отказ не должен отменять уже снятое дерево, ради которого всё и затевалось.
func (s *ReplyScanner) note(ctx context.Context, id int64) (ReplyScanStats, error) {
	st, err := s.tree(ctx, id)
	if err != nil {
		return st, err
	}

	genders, err := s.site.FetchGenders(ctx, strconv.FormatInt(id, 10))
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

	// Подсказки приёмника (Nudge). Очередь выше ходит по следам — «в заметке
	// дописали», — а подсказка приходит в тот самый момент, когда появилась
	// догадка, и потому стоит дешевле: обойти надо одну названную заметку, а не
	// перебирать живые.
	//
	// NudgeInterval — как часто смотреть на названные. Пять секунд: подсказка
	// затем и заведена, чтобы догадка не стояла минутами.
	NudgeInterval = 5 * time.Second
	// NudgeGap — не чаще, чем раз в столько, по одной заметке. Полминуты — это
	// такт самого зеркала (mirror.PollInterval у живой заметки), то есть в тред
	// мы заглядываем не чаще, чем оно само туда ходит.
	NudgeGap = 30 * time.Second
	// NudgeBatch — сколько названных заметок за такт. Больше двух незачем:
	// живых тредов единицы, а очередь никуда не девается.
	NudgeBatch = 2
	// NudgeFails — после скольких отказов подряд заметка перестаёт слушаться
	// подсказок. Двух хватает: 500 сайт отдаёт на длинных тредах устойчиво, а
	// повторять его каждые полминуты — это 45 секунд ожидания на каждую реплику.
	NudgeFails = 2
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
	// Второй такт — подсказки приёмника. Тикера два, а не один частый, потому
	// что работы у них разные: очередь свежих тредов это запрос к базе и до трёх
	// полных обходов, подсказка — одна названная заметка и только дерево.
	n := time.NewTicker(NudgeInterval)
	defer n.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-n.C:
			s.nudged(ctx)
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
