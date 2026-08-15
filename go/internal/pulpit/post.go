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

// preach генерирует реплику и отправляет её под заметку. Возвращает true, если
// POST состоялся (для суточного счёта). Заметка к этому моменту уже занята
// строкой queued — сюда приходят только с ней.
func (s *Service) preach(ctx context.Context, n love.Note, seenAt time.Time) bool {
	if s.gen == nil {
		s.skip(ctx, n.ID, store.PulpitFailed, reasonNoLLM)
		return false
	}
	sm, err := s.Draft(ctx, n)
	if err != nil {
		s.log.Warn("амвон: реплика не сгенерирована", "note", n.ID, "err", err)
		s.skip(ctx, n.ID, store.PulpitFailed, shortReason(err))
		return false
	}
	// Опоздавшая проповедь противоречит смыслу фичи (быть первым) и при этом
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
		// Строка остаётся в posting: сайт мог реплику принять и всё равно
		// ответить ошибкой. Верификация решит, что там на самом деле.
		s.log.Error("амвон: отправка реплики не удалась", "note", n.ID, "err", err)
		return true
	}
	if _, err := s.st.CASPulpitState(ctx, n.ID, store.PulpitPosting, store.PulpitPosted, ""); err != nil {
		s.log.Error("амвон: фиксация отправленной реплики", "note", n.ID, "err", err)
	}
	s.log.Info("амвон: реплика отправлена", "note", n.ID, "форма", sm.Form,
		"знаков", len([]rune(sm.Text)), "задержка", time.Since(seenAt).Round(time.Second))
	return true
}

// Draft генерирует реплику под заметку, ничего не отправляя. Экспортируется для
// CLI (`lovegw pulpit draft <id>`): промпт правится по вчерашним заметкам
// локально, а не боевыми публикациями.
func (s *Service) Draft(ctx context.Context, n love.Note) (Sermon, error) {
	genCtx, cancel := context.WithTimeout(ctx, s.cfg.GenerateTimeout)
	defer cancel()

	history, forms := s.history(ctx)
	allowed := pickForms(forms, sermonForms, s.cfg.FormCooldown)
	base := buildSermonPrompt(promptInput{
		Note:       n.Text,
		AuthorName: n.AuthorName,
		Anonymous:  n.AuthorID == "" || n.AuthorID == "0",
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
	sm, err := s.ask(genCtx, sermonSystem, base, sermonSchema, cfg)
	return Sermon(sm), err
}

// Sermon — сгенерированная реплика (наружу её показывает только CLI-черновик).
type Sermon = sermon

// ask — общий цикл «спросить, починить, проверить, переспросить». Причина
// брака едет хвостом в промпт: слепой повтор повторил бы и брак.
func (s *Service) ask(ctx context.Context, system, base string, schema map[string]any, cfg validateConfig) (sermon, error) {
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
			return sermon{}, err
		}
	}
	return sermon{}, fmt.Errorf("попытки исчерпаны: %w", lastErr)
}

// askOnce — одна попытка: запрос, разбор, нормализация, проверка. retriable
// отделяет брак ответа (пришёл, но не годится) от ошибки запроса — её повторять
// незачем, сеть и 429/5xx SDK уже отретраил сам.
func (s *Service) askOnce(ctx context.Context, system, prompt string, schema map[string]any, cfg validateConfig) (_ sermon, retriable bool, err error) {
	raw, err := s.gen.GenerateJSON(ctx, system, prompt, schema)
	if err != nil {
		return sermon{}, false, err
	}
	var sm sermon
	if err := json.Unmarshal(raw, &sm); err != nil {
		return sermon{}, true, fmt.Errorf("разбор ответа модели: %w", err)
	}
	sm.Text = normalize(sm.Text)
	if reason := validate(sm.Text, sm.Form, cfg); reason != "" {
		return sermon{}, true, fmt.Errorf("%s", reason)
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
// задан отдельно, системным промптом.
func (s *Service) styleSamples(ctx context.Context, seed string) []string {
	pool, err := s.st.OwnerComments(ctx, s.cfg.OwnerProfileID, s.cfg.MinRunes, s.cfg.MaxRunes, samplePool)
	if err != nil {
		s.log.Error("амвон: чтение своих комментариев", "err", err)
		return nil
	}
	return pickSamples(pool, seed, sampleCount)
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
