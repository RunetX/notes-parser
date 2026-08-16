package pulpit

// Ответы на ответы. Проповедник в переписку не вступает — но иногда отзывается,
// и эта редкость намеренная: постоянный ответчик читается как спорщик, а нужен
// голос, который «просто всегда там».
//
// Монетка бросается РОВНО ОДИН РАЗ на чужую реплику: решение пишется в
// pulpit_replies до броска (PK по reply_to_id). Иначе вероятность 15 % за
// десяток циклов превратилась бы в 80 % — каждый такт кидал бы заново.
//
// Дерево комментариев на сайте двухуровневое: parent_id указывает на КОРЕНЬ
// ветки, а не на реплику, которой отвечают. Наш комментарий первого уровня и
// есть корень своей ветки, поэтому «ответ нам» — это прямой ребёнок корня, чей
// адресат (префикс «Ник, …») совпал с нашим ником либо в ветке до него никого,
// кроме нас, не было.

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// replyMaxRunes — потолок ответа: он короче реплики под заметкой по замыслу.
// В ответе сетап уже не нужен (он в чужой реплике), остаётся один панч.
const replyMaxRunes = 140

// Темп обхода веток. Ответы никуда не спешат (в отличие от реплики под заметкой,
// которая гонится за первым местом), а треды за сутки набираются десятками:
// смотреть их каждый такт значило бы дать к сайту вчетверо больше запросов, чем
// весь обход ленты. Поэтому раз в пять минут и по три ветки за раз, по кругу.
const (
	replyScanInterval = 5 * time.Minute
	replyScanNotes    = 3
)

// replyCycle — один проход по части своих свежих реплик.
func (s *Service) replyCycle(ctx context.Context) {
	if s.cfg.ReplyProbability <= 0 || s.gen == nil || !s.replyDue() {
		return
	}
	rows, err := s.st.PulpitConfirmedSince(ctx, time.Now().Add(-s.cfg.ReplyWindow))
	if err != nil {
		s.log.Error("амвон: чтение своих реплик", "err", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	rows = s.replyBatch(rows)
	sent, err := s.st.PulpitReplySentSince(ctx, dayStart(time.Now()))
	if err != nil {
		s.log.Error("амвон: суточный счёт ответов", "err", err)
		return
	}
	if sent >= s.cfg.RepliesPerDay {
		return
	}
	nick := s.ownNick(ctx)
	for _, row := range rows {
		if sent >= s.cfg.RepliesPerDay {
			return
		}
		if s.replyUnder(ctx, row, nick) {
			sent++
		}
	}
}

// replyDue — пора ли смотреть ветки. Отметка в памяти, а не в БД: пропустить
// круг после рестарта не страшно, ответы не срочные.
func (s *Service) replyDue() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.replyAt) < replyScanInterval {
		return false
	}
	s.replyAt = time.Now()
	return true
}

// replyBatch отрезает от списка порцию, начиная с того места, где остановились
// в прошлый раз: так за несколько кругов обойдутся все ветки, а сайт не получит
// десяток запросов подряд.
func (s *Service) replyBatch(rows []store.PulpitComment) []store.PulpitComment {
	if len(rows) <= replyScanNotes {
		return rows
	}
	s.mu.Lock()
	start := s.replyCursor % len(rows)
	s.replyCursor = (start + replyScanNotes) % len(rows)
	s.mu.Unlock()

	out := make([]store.PulpitComment, 0, replyScanNotes)
	for i := range replyScanNotes {
		out = append(out, rows[(start+i)%len(rows)])
	}
	return out
}

// replyUnder разбирает ветку одной своей реплики: подтверждает уже
// отправленные ответы, проверяет, на месте ли сама реплика, и решает по новым
// собеседникам.
func (s *Service) replyUnder(ctx context.Context, row store.PulpitComment, nick string) bool {
	comments, err := s.site.TreeComments(ctx, row.NoteID)
	if err != nil {
		s.log.Warn("амвон: дерево треда недоступно", "note", row.NoteID, "err", err)
		return false
	}
	if !hasComment(comments, row.CommentID) {
		// Тред читается, а нашей реплики в нём больше нет: вычистила модерация.
		if err := s.st.SetPulpitState(ctx, row.NoteID, store.PulpitMissing, reasonDeleted, time.Now()); err != nil {
			s.log.Error("амвон: отметка удаления", "note", row.NoteID, "err", err)
		}
		s.log.Warn("амвон: подтверждённую реплику удалили", "note", row.NoteID, "comment", row.CommentID)
		return false
	}
	decided, err := s.st.PulpitRepliesByNote(ctx, row.NoteID)
	if err != nil {
		s.log.Error("амвон: чтение решений по ответам", "note", row.NoteID, "err", err)
		return false
	}
	s.confirmReplies(ctx, comments, decided)
	if repliesSent(decided) >= s.cfg.RepliesPerNote {
		return false
	}

	seen, answered := decidedSets(decided)
	for _, c := range replyCandidates(comments, row.CommentID, s.cfg.OwnerProfileID, nick, seen, answered) {
		heads := s.rand() < s.cfg.ReplyProbability
		state, reason := store.PulpitQueued, ""
		if !heads {
			state, reason = store.PulpitSkipped, reasonCoin
		}
		fresh, err := s.st.TryDecideReply(ctx, store.PulpitReply{
			ReplyToID: c.ID, NoteID: row.NoteID, AuthorID: c.AuthorID,
			State: state, Reason: reason, DecidedAt: time.Now(),
		})
		if err != nil {
			s.log.Error("амвон: решение по ответу", "comment", c.ID, "err", err)
			return false
		}
		if !fresh || !heads {
			continue
		}
		if s.sendReply(ctx, row, c, nick) {
			return true // не больше одного ответа за проход по заметке
		}
	}
	return false
}

// sendReply генерирует и отправляет ответ. Префикс «Ник, » подставляет
// ИНСТРУМЕНТ, а не модель (правило проекта): так ник нельзя выдумать, а
// валидатор бракует тело, в котором модель написала обращение сама.
func (s *Service) sendReply(ctx context.Context, row store.PulpitComment, c love.Comment, nick string) bool {
	n, err := s.noteByID(ctx, row.NoteID)
	if err != nil {
		s.log.Warn("амвон: заметка для ответа не читается", "note", row.NoteID, "err", err)
		return false
	}
	genCtx, cancel := context.WithTimeout(ctx, s.cfg.GenerateTimeout)
	defer cancel()

	maxRunes := min(replyMaxRunes, s.cfg.MaxRunes)
	prompt := buildReplyPrompt(replyPromptInput{
		Note: n.Text, Mine: row.Text, Their: c.Text, TheirNick: c.AuthorName,
		MaxRunes: maxRunes, AllowEmoji: s.cfg.AllowEmoji,
	})
	sm, err := s.ask(genCtx, replySystem, prompt, replySchema, validateConfig{
		MinRunes: 1, MaxRunes: maxRunes, MaxLines: s.cfg.MaxLines,
		AllowEmoji: s.cfg.AllowEmoji, NoteText: n.Text,
		// Обращение к собеседнику подставляет инструмент — в теле его быть
		// не должно.
		Nicks: []string{c.AuthorName, nick},
	})
	if err != nil {
		s.log.Warn("амвон: ответ не сгенерирован", "comment", c.ID, "err", err)
		if err := s.st.SetPulpitReplyState(ctx, c.ID, store.PulpitFailed, shortReason(err)); err != nil {
			s.log.Error("амвон: отметка неудачи ответа", "comment", c.ID, "err", err)
		}
		return false
	}
	cookies, err := s.cookies(ctx)
	if err != nil {
		s.log.Error("амвон: сессия владельца недоступна", "comment", c.ID, "err", err)
		_ = s.st.SetPulpitReplyState(ctx, c.ID, store.PulpitFailed, reasonNoSession)
		return false
	}

	text := renderReply(c.AuthorName, sm.Text)
	started, err := s.st.TryStartPulpitReply(ctx, c.ID, text, time.Now())
	if err != nil || !started {
		return false
	}
	if err := s.site.PostComment(ctx, cookies, row.NoteID,
		strconv.FormatInt(c.ID, 10), text); err != nil {
		// Как и с репликой под заметкой: не откатываем и не повторяем — сайт мог принять.
		s.log.Error("амвон: ответ не отправлен", "comment", c.ID, "err", err)
		return true
	}
	if err := s.st.SetPulpitReplyState(ctx, c.ID, store.PulpitPosted, ""); err != nil {
		s.log.Error("амвон: фиксация ответа", "comment", c.ID, "err", err)
	}
	s.log.Info("амвон: ответ отправлен", "note", row.NoteID, "кому", c.AuthorName, "comment", c.ID)
	return true
}

// confirmReplies отмечает свои ответы, найденные в дереве. Тред уже в руках,
// так что проверка бесплатна: сверяем по точному тексту, который сами и
// отправили.
func (s *Service) confirmReplies(ctx context.Context, comments []love.Comment, decided []store.PulpitReply) {
	for _, r := range decided {
		if r.State != store.PulpitPosted && r.State != store.PulpitPosting {
			continue
		}
		if r.CommentID != 0 || r.Text == "" {
			continue
		}
		for _, c := range comments {
			if c.AuthorID != s.cfg.OwnerProfileID || strings.TrimSpace(c.Text) != strings.TrimSpace(r.Text) {
				continue
			}
			if err := s.st.ConfirmPulpitReply(ctx, r.ReplyToID, c.ID); err != nil {
				s.log.Error("амвон: подтверждение ответа", "comment", r.ReplyToID, "err", err)
			}
			break
		}
	}
}

// replyCandidates — чужие реплики, на которые уместно ответить. Чистая:
// решение о необратимой отправке проверяется таблицей.
func replyCandidates(comments []love.Comment, myCommentID int64, ownProfileID, nick string,
	decided map[int64]bool, answered map[string]bool) []love.Comment {
	if myCommentID == 0 || ownProfileID == "" {
		return nil
	}
	// Ветка нашей реплики: у сайта parent_id ветки указывает на её корень.
	var branch []love.Comment
	for _, c := range comments {
		if c.ParentID == myCommentID {
			branch = append(branch, c)
		}
	}
	sortByID(branch)

	var out []love.Comment
	for i, c := range branch {
		switch {
		case c.AuthorID == ownProfileID: // свой же ответ
			continue
		case decided[c.ID]: // решение уже принято, монетку не перебрасываем
			continue
		case answered[c.AuthorID]: // одному человеку в заметке отвечаем один раз
			continue
		}
		// Либо обратились к нам по нику, либо до этой реплики в ветке никого,
		// кроме нас, не было — значит отвечают нам.
		if !addressedTo(c.Text, nick) && !onlyMineBefore(branch[:i], ownProfileID) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// addressedTo — обращение «Ник, …» в начале реплики совпало с нашим ником.
func addressedTo(text, nick string) bool {
	if nick == "" {
		return false
	}
	return love.AddressPrefix(text) == strings.ToLower(nick)
}

// onlyMineBefore — в ветке до этой реплики писали только мы.
func onlyMineBefore(before []love.Comment, ownProfileID string) bool {
	for _, c := range before {
		if c.AuthorID != ownProfileID {
			return false
		}
	}
	return true
}

// renderReply — то, что уйдёт на сайт: обращение подставляет инструмент.
func renderReply(nick, text string) string {
	nick = strings.TrimSpace(nick)
	if nick == "" {
		return text
	}
	return nick + ", " + text
}

// decidedSets — по каким репликам решение уже принято и каким авторам в этой
// заметке уже ответили.
func decidedSets(decided []store.PulpitReply) (seen map[int64]bool, answered map[string]bool) {
	seen = make(map[int64]bool, len(decided))
	answered = make(map[string]bool, len(decided))
	for _, r := range decided {
		seen[r.ReplyToID] = true
		if r.AuthorID != "" && sentState(r.State) {
			answered[r.AuthorID] = true
		}
	}
	return seen, answered
}

// repliesSent — сколько ответов в этой заметке уже ушло на сайт.
func repliesSent(decided []store.PulpitReply) int {
	n := 0
	for _, r := range decided {
		if sentState(r.State) {
			n++
		}
	}
	return n
}

// sentState — состояние, в котором ответ уже на сайте (posting считается: POST
// мог дойти).
func sentState(state string) bool {
	switch state {
	case store.PulpitPosting, store.PulpitPosted, store.PulpitConfirmed:
		return true
	}
	return false
}

func hasComment(comments []love.Comment, id int64) bool {
	for _, c := range comments {
		if c.ID == id {
			return true
		}
	}
	return false
}

func sortByID(comments []love.Comment) {
	slices.SortFunc(comments, func(a, b love.Comment) int { return cmp.Compare(a.ID, b.ID) })
}
