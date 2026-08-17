package pulpit

// Генерация реплики и её отправка на сайт. Здесь единственное необратимое
// действие всего проекта: комментарий, ушедший на love.ngs.ru, назад не
// отзывается. Поэтому переход queued → posting пишется ДО отправки и обратно не
// откатывается никогда — даже когда POST вернул ошибку: сайт вполне мог реплику
// принять и уже показать. Судьбу такой строки решает верификация треда
// (verify.go), а не повторная отправка.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// generateRetries — сколько раз переспрашиваем модель, прежде чем сдаться.
// Столько же, сколько у редактуры дайджеста: брак бывает разовым, а причина
// брака уезжает в промпт, так что второй заход не слепой.
const generateRetries = 3

// samplePool — из скольких своих комментариев выбираем образцы манеры.
const samplePool = 200

// sampleCount — сколько образцов показываем модели.
const sampleCount = 4

// postQuip генерирует реплику и отправляет её под заметку. Возвращает true,
// если POST состоялся (для суточного счёта). Заметка к этому моменту уже занята
// строкой queued — сюда приходят только с ней.
func (s *Service) postQuip(ctx context.Context, n love.Note, seenAt time.Time) bool {
	if s.gen == nil {
		s.skip(ctx, n.ID, store.PulpitFailed, reasonNoLLM)
		return false
	}
	// Свежесть — по сайту, а не по ленте: лента возраста не отдаёт, и без
	// этой проверки ветка stale на пути ленты была мертва — заметка,
	// вернувшаяся с премодерации старой (автор дописал картинку), выглядела
	// бы новой, и реплика легла бы под обжитый тред. Проверка стоит одного
	// запроса и идёт ДО генерации: не жечь LLM-бюджет под скип.
	if s.cfg.Freshness > 0 {
		age, ok := s.noteAge(ctx, n.ID)
		if !ok {
			// Страница не прочиталась: строка остаётся в queued, её дожмёт
			// resumeQueued со своим потолком возраста от момента claim'а.
			return false
		}
		if age > s.cfg.Freshness {
			s.log.Info("амвон: заметка уже обжита, молчим",
				"note", n.ID, "возраст", age.Round(time.Minute))
			s.skip(ctx, n.ID, store.PulpitSkipped, reasonStale)
			return false
		}
	}
	sm, err := s.Draft(ctx, n)
	if err != nil {
		s.log.Warn("амвон: реплика не сгенерирована", "note", n.ID, "err", err)
		s.skip(ctx, n.ID, store.PulpitFailed, shortReason(err))
		return false
	}
	if sm.Skip {
		// Под настоящей бедой шутить нечем, и это штатный исход, а не сбой:
		// молчание тут — часть голоса, а не его отказ.
		s.log.Info("амвон: под этой заметкой не шутим", "note", n.ID, "о_чём", sm.Idea)
		s.skip(ctx, n.ID, store.PulpitSkipped, reasonNoJoke)
		return false
	}
	// Опоздавшая реплика противоречит смыслу фичи (быть первым) и при этом
	// стоит того же риска, что своевременная: молчим.
	if late := time.Since(seenAt); late > s.cfg.MaxLatency {
		s.log.Warn("амвон: реплика опоздала, не отправляю",
			"note", n.ID, "задержка", late.Round(time.Second))
		s.skip(ctx, n.ID, store.PulpitSkipped, reasonTooLate)
		return false
	}
	cookies, err := s.cookies(ctx)
	if err != nil {
		s.log.Error("амвон: сессия владельца недоступна", "note", n.ID, "err", err)
		s.skip(ctx, n.ID, store.PulpitFailed, reasonNoSession)
		return false
	}

	// Точка невозврата: дальше повторной отправки не будет ни при каком исходе.
	started, err := s.st.TryStartPulpitPost(ctx, n.ID, sm.Form, sm.Text, time.Now())
	if err != nil {
		s.log.Error("амвон: фиксация отправки", "note", n.ID, "err", err)
		return false
	}
	if !started {
		return false // строку уже забрал другой цикл
	}
	if err := s.site.PostComment(ctx, cookies, n.ID, "", sm.Text); err != nil {
		s.noteSendResult(false)
		// Строка остаётся в posting: сайт мог реплику принять и всё равно
		// ответить ошибкой. Верификация решит, что там на самом деле.
		//
		// Но причину записываем сразу: тред-то читается, и без этой отметки
		// «реплики нет» стало бы уликой запрета писать в «Заметки» — то есть
		// 5xx-шторм сайта гасил бы фичу предохранителем (17.08.2026 сайт
		// отвечал 500 на любой комментарий, включая чужие, и первый промах
		// засчитался). Тот же state в CAS — не опечатка: меняем только
		// причину, а условие state=posting стережёт от затирания строки,
		// которую другой цикл успел увести дальше.
		if _, cerr := s.st.CASPulpitState(ctx, n.ID,
			store.PulpitPosting, store.PulpitPosting, reasonSendFailed); cerr != nil {
			s.log.Error("амвон: отметка сбоя отправки", "note", n.ID, "err", cerr)
		}
		s.log.Error("амвон: отправка реплики не удалась", "note", n.ID, "err", err)
		return true
	}
	s.noteSendResult(true)
	if _, err := s.st.CASPulpitState(ctx, n.ID, store.PulpitPosting, store.PulpitPosted, ""); err != nil {
		s.log.Error("амвон: фиксация отправленной реплики", "note", n.ID, "err", err)
	}
	s.log.Info("амвон: реплика отправлена", "note", n.ID, "форма", sm.Form,
		"знаков", len([]rune(sm.Text)), "задержка", time.Since(seenAt).Round(time.Second))
	return true
}

// pauseAfterSendFails — сколько не дошедших подряд POST'ов считаем штормом.
// Двух хватает: одиночный 5xx у сайта бывает и в обычный день, а два подряд
// означают, что запись лежит.
const pauseAfterSendFails = 2

// sendProbeAfter — через сколько после отказа пробуем снова. Одна пробная
// заметка в десять минут: столько стоит узнать, что сайт ожил, и это дешевле
// генерации под каждую заметку шторма.
const sendProbeAfter = 10 * time.Minute

// noteSendResult запоминает исход отправки. Успех снимает паузу немедленно:
// сайт принял комментарий — значит запись работает.
func (s *Service) noteSendResult(ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.sendFails = 0
		return
	}
	s.sendFails++
	s.lastSendFail = time.Now()
}

// writePaused — ждём ли конца шторма. Полуоткрытое состояние: после
// sendProbeAfter одна заметка проходит как проба, и её исход решает, ждать ли
// дальше.
func (s *Service) writePaused(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sendFails >= pauseAfterSendFails && now.Sub(s.lastSendFail) < sendProbeAfter
}

// noteAge — возраст заметки со страницы комментариев. Первоисточник —
// PublishedAt из шапки; шапка без времени (дрейф вёрстки) — нижняя граница по
// старейшему комментарию страницы; нет ни того ни другого — считаем свежей:
// многодневная заметка без единого комментария — вырожденный случай, штатные
// прикрыты холодным стартом и claim'ом. ok=false — страница не прочиталась.
func (s *Service) noteAge(ctx context.Context, noteID string) (time.Duration, bool) {
	page, err := s.site.FetchCommentsPage(ctx, noteID)
	if err != nil {
		s.log.Warn("амвон: возраст заметки не выяснен", "note", noteID, "err", err)
		return 0, false
	}
	now := time.Now()
	if page.Note != nil && !page.Note.PublishedAt.IsZero() {
		return now.Sub(page.Note.PublishedAt), true
	}
	var oldest time.Time
	for _, c := range page.Comments {
		if !c.PublishedAt.IsZero() && (oldest.IsZero() || c.PublishedAt.Before(oldest)) {
			oldest = c.PublishedAt
		}
	}
	if oldest.IsZero() {
		return 0, true
	}
	return now.Sub(oldest), true
}

// Draft генерирует реплику под заметку, ничего не отправляя. Экспортируется для
// CLI (`lovegw pulpit draft <id>`): промпт правится по вчерашним заметкам
// локально, а не боевыми публикациями.
func (s *Service) Draft(ctx context.Context, n love.Note) (Quip, error) {
	genCtx, cancel := context.WithTimeout(ctx, s.cfg.GenerateTimeout)
	defer cancel()

	history, forms := s.history(ctx)
	allowed := pickForms(forms, quipForms, s.cfg.FormCooldown)
	base := buildQuipPrompt(promptInput{
		Note:       n.Text,
		AuthorName: n.AuthorName,
		Anonymous:  n.AuthorID == "" || n.AuthorID == "0",
		Nick:       s.currentNick(),
		Forms:      allowed,
		History:    history,
		Samples:    s.styleSamples(ctx, n.ID),
		TargetRune: targetRunes,
		MaxRunes:   s.cfg.MaxRunes,
		MaxLines:   s.cfg.MaxLines,
		AllowEmoji: s.cfg.AllowEmoji,
	})
	cfg := validateConfig{
		MinRunes: s.cfg.MinRunes, MaxRunes: s.cfg.MaxRunes, MaxLines: s.cfg.MaxLines,
		AllowEmoji: s.cfg.AllowEmoji, Forms: allowed, NoteText: n.Text,
		// К автору заметки по имени не обращаемся: реплика первого уровня —
		// это отклик на текст, а не письмо человеку.
		Nicks: []string{n.AuthorName, s.currentNick()},
	}
	sm, err := s.ask(genCtx, quipSystem, base, quipSchema, cfg)
	if err != nil || sm.Skip {
		return Quip(sm), err
	}
	return Quip(s.punchUp(genCtx, n.Text, sm, cfg)), nil
}

// punchUp — второй проход по своему же черновику: убрать пояснение после удара,
// заменить общее конкретным, добавить добивку. Первый заход находит угол, второй
// его затачивает — в один заход модель этого не делает, она бережёт уже
// написанное.
//
// Провал правки НЕ отменяет реплику: возвращаем черновик. Он уже прошёл
// валидацию, а вторая попытка была улучшением, а не условием.
func (s *Service) punchUp(ctx context.Context, note string, draft quip, cfg validateConfig) quip {
	prompt := buildPunchupPrompt(note, draft.Text, draft.Form, cfg.MaxRunes)
	// Форму редактор менять не должен, но своей строкой он её и повторяет —
	// сверяем по тому же списку, что и черновик.
	sharp, err := s.ask(ctx, punchupSystem, prompt, quipSchema, cfg)
	if err != nil {
		s.log.Info("амвон: правка не удалась, берём черновик", "err", err)
		return draft
	}
	if sharp.Skip || strings.TrimSpace(sharp.Text) == "" {
		return draft
	}
	if sharp.Text != draft.Text {
		s.log.Debug("амвон: реплика поправлена", "было", draft.Text, "стало", sharp.Text)
	}
	// Форму, деталь и мысль дневника берём от черновика, если редактор их потерял.
	if sharp.Form == "" {
		sharp.Form = draft.Form
	}
	if sharp.Hook == "" {
		sharp.Hook = draft.Hook
	}
	if sharp.Idea == "" {
		sharp.Idea = draft.Idea
	}
	return sharp
}

// Quip — сгенерированная реплика (наружу её показывает только CLI-черновик).
type Quip = quip

// ask — общий цикл «спросить, починить, проверить, переспросить». Причина
// брака едет хвостом в промпт: слепой повтор повторил бы и брак.
func (s *Service) ask(ctx context.Context, system, base string, schema map[string]any, cfg validateConfig) (quip, error) {
	var lastErr error
	for attempt := range generateRetries {
		prompt := base
		if lastErr != nil {
			prompt += fmt.Sprintf(retryNote, lastErr)
		}
		sm, retriable, err := s.askOnce(ctx, system, prompt, schema, cfg)
		if err == nil {
			if attempt > 0 {
				s.log.Info("амвон: реплика получена с переспроса", "попытка", attempt+1)
			}
			return sm, nil
		}
		lastErr = err
		if !retriable {
			return quip{}, err
		}
	}
	return quip{}, fmt.Errorf("попытки исчерпаны: %w", lastErr)
}

// askOnce — одна попытка: запрос, разбор, нормализация, проверка. retriable
// отделяет брак ответа (пришёл, но не годится) от ошибки запроса — её повторять
// незачем, сеть и 429/5xx SDK уже отретраил сам.
func (s *Service) askOnce(ctx context.Context, system, prompt string, schema map[string]any, cfg validateConfig) (_ quip, retriable bool, err error) {
	raw, err := s.gen.GenerateJSON(ctx, system, prompt, schema)
	if err != nil {
		return quip{}, false, err
	}
	var sm quip
	if err := json.Unmarshal(raw, &sm); err != nil {
		return quip{}, true, fmt.Errorf("разбор ответа модели: %w", err)
	}
	if sm.Skip {
		// Отказ шутить проверять нечем: текста нет, и он не нужен. Переспрос
		// тут был бы уговором — а красная линия на то и красная.
		return quip{Skip: true, Idea: sm.Idea}, false, nil
	}
	sm.Text = normalize(sm.Text)
	if reason := validate(sm, cfg); reason != "" {
		return quip{}, true, fmt.Errorf("%s", reason)
	}
	return sm, false, nil
}

// history — свои последние реплики и их формы (от новых к старым). Отправленные
// и только они: пропуски и неудачи ни повторять, ни избегать не нужно.
func (s *Service) history(ctx context.Context) (texts, forms []string) {
	rows, err := s.st.PulpitRecent(ctx, s.cfg.HistorySize*3)
	if err != nil {
		s.log.Error("амвон: чтение истории реплик", "err", err)
		return nil, nil
	}
	for _, row := range rows {
		if row.Text == "" {
			continue
		}
		if len(texts) < s.cfg.HistorySize {
			texts = append(texts, row.Text)
		}
		if row.Form != "" {
			forms = append(forms, row.Form)
		}
	}
	return texts, forms
}

// styleSamples — собственные комментарии владельца из живой БД: они задают
// манеру (длина фразы, пунктуация, финальная точка), а не регистр — регистр
// задан отдельно, системным промптом. Из пула берутся те, где владелец смеялся
// (preferFunny): манеру шутки лучше показывать шуткой, а не спором.
func (s *Service) styleSamples(ctx context.Context, seed string) []string {
	pool, err := s.st.OwnerComments(ctx, s.cfg.OwnerProfileID, s.cfg.MinRunes, s.cfg.MaxRunes, samplePool)
	if err != nil {
		s.log.Error("амвон: чтение своих комментариев", "err", err)
		return nil
	}
	return pickSamples(preferFunny(pool), seed, sampleCount)
}

// skip помечает строку пропущенной/неудавшейся, не трогая уже отправленное.
func (s *Service) skip(ctx context.Context, noteID, state, reason string) {
	if _, err := s.st.CASPulpitState(ctx, noteID, store.PulpitQueued, state, reason); err != nil {
		s.log.Error("амвон: снятие заметки", "note", noteID, "err", err)
	}
}

// shortReason — причина в БД: одно слово, а не весь текст ошибки (подробности
// уже в логе).
func shortReason(err error) string {
	const limit = 60
	r := []rune(err.Error())
	if len(r) > limit {
		return string(r[:limit])
	}
	return string(r)
}
