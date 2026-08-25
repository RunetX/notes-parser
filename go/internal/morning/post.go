package morning

// Сбор поводов, генерация текста и публикация.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"lovegw/internal/holidays"
	"lovegw/internal/sitetext"
	"lovegw/internal/store"
)

// generateRetries — сколько раз переспрашиваем модель, прежде чем сдаться.
// Столько же, сколько у амвона и у редактуры дайджеста: брак бывает разовым, а
// причина брака уезжает в промпт, так что второй заход не слепой.
const generateRetries = 3

// Draft собирает поводы дня и просит модель написать заметку, НИЧЕГО не
// публикуя. Стенд промпта: им же пользуется `lovegw morning draft`, и он обязан
// идти той же дорогой, что боевой прогон, — иначе черновик врал бы.
func (s *Service) Draft(ctx context.Context, slot time.Time) (draft, []holidays.Occasion, error) {
	facts, err := s.facts(ctx, slot)
	if err != nil {
		s.log.Warn("утренняя заметка: поводы дня не собрались", "err", err)
	}
	if s.gen == nil {
		return draft{}, facts, fmt.Errorf("генерировать нечем: %s", reasonNoLLM)
	}
	genCtx, cancel := context.WithTimeout(ctx, s.cfg.GenerateTimeout)
	defer cancel()

	in := promptInput{
		Nick:     s.nick(ctx),
		Weekday:  weekdays[int(slot.Weekday())],
		DateWord: fmt.Sprintf("%d %s %d года", slot.Day(), months[int(slot.Month())-1], slot.Year()),
		Facts:    facts,
		History:  s.history(ctx),
		MinRunes: s.cfg.MinRunes,
		MaxRunes: s.cfg.MaxRunes,
		MaxLines: s.cfg.MaxLines,
	}
	cfg := validateConfig{
		MinRunes: s.cfg.MinRunes, MaxRunes: s.cfg.MaxRunes, MaxLines: s.cfg.MaxLines,
		Day: slot.Day(), Month: int(slot.Month()), Weekday: weekdays[int(slot.Weekday())],
		Facts: facts,
	}
	d, err := s.ask(genCtx, morningSystem, buildPrompt(in), cfg)
	if err != nil || d.Skip {
		return d, facts, err
	}
	return s.punchUp(genCtx, d, in, cfg), facts, nil
}

// punchUp — второй проход по своему же черновику: убрать пояснение после удара,
// переставить ударное слово в конец, срезать разгон. Первый заход ищет угол,
// второй его затачивает — в один заход модель этого не делает, она бережёт уже
// написанное. Приём и его цена перенесены из амвона (`pulpit.punchUp`).
//
// Правка идёт ТЕМ ЖЕ циклом `ask`, то есть проверяется тем же валидатором:
// редактор, потерявший приветствие, эмодзи или дату, до сайта не доедет.
// Провал правки заметку НЕ отменяет — уходит черновик: он уже прошёл проверку,
// а правка была улучшением, а не условием.
func (s *Service) punchUp(ctx context.Context, d draft, in promptInput, cfg validateConfig) draft {
	sharp, err := s.ask(ctx, morningPunchSystem, buildPunchPrompt(d.Text, in), cfg)
	if err != nil {
		s.log.Info("утренняя заметка: правка не удалась, берём черновик", "err", err)
		return d
	}
	if sharp.Skip || strings.TrimSpace(sharp.Text) == "" {
		return d
	}
	if sharp.Text != d.Text {
		s.log.Debug("утренняя заметка поправлена", "было", d.Text, "стало", sharp.Text)
	}
	if sharp.Idea == "" {
		sharp.Idea = d.Idea
	}
	return sharp
}

// facts — поводы дня после слияния календарей и фильтра. Ошибка ВСЕХ источников
// не отменяет утро: заметка выйдет без поводов (см. промпт), а владельцу об
// этом скажут.
func (s *Service) facts(ctx context.Context, slot time.Time) ([]holidays.Occasion, error) {
	all, err := holidays.Collect(ctx, s.cfg.Sources, slot, s.log)
	if err != nil {
		return nil, err
	}
	return trimFacts(holidays.Filter(all), s.cfg.MaxFacts), nil
}

// trimFacts обрезает список до потолка. Порядок уже расставлен слиянием: сперва
// праздники, внутри вида — подтверждённые несколькими календарями. Обрезаем
// хвост, а не выбираем «поинтереснее»: выбор — работа модели, наша — не врать.
//
// Единственное исключение — ИМЕНИНЫ: их строка одна на весь день и стоит после
// праздников, а в людный день (1 сентября) праздников бывает больше дюжины — и
// поздравлять было бы некого. Место им отдаётся из хвоста.
func trimFacts(list []holidays.Occasion, max int) []holidays.Occasion {
	if max <= 0 || len(list) <= max {
		return list
	}
	out := make([]holidays.Occasion, max)
	copy(out, list[:max])
	if hasKind(out, holidays.KindName) {
		return out
	}
	for _, o := range list[max:] {
		if o.Kind == holidays.KindName {
			out[max-1] = o
			break
		}
	}
	return out
}

func hasKind(list []holidays.Occasion, k holidays.Kind) bool {
	for _, o := range list {
		if o.Kind == k {
			return true
		}
	}
	return false
}

// ask — цикл «спросить → починить → проверить → переспросить». Системный
// промпт приходит параметром: черновик и правку пишет одна и та же модель, но
// работы у неё разные (`morningSystem` против `morningPunchSystem`).
func (s *Service) ask(ctx context.Context, system, base string, cfg validateConfig) (draft, error) {
	var lastErr error
	for attempt := 0; attempt < generateRetries; attempt++ {
		prompt := base
		if lastErr != nil {
			prompt += fmt.Sprintf(retryNote, lastErr)
		}
		d, retriable, err := s.askOnce(ctx, system, prompt, cfg)
		if err == nil {
			return d, nil
		}
		lastErr = err
		if !retriable {
			return draft{}, err
		}
	}
	return draft{}, fmt.Errorf("попытки исчерпаны: %w", lastErr)
}

// askOnce — одна попытка: запрос, разбор, отказ, нормализация, проверка.
// retriable отделяет брак ответа (пришёл, но не годится) от ошибки самого
// запроса — её повторять незачем, сеть и 429/5xx SDK уже отретраил сам.
func (s *Service) askOnce(ctx context.Context, system, prompt string, cfg validateConfig) (_ draft, retriable bool, err error) {
	raw, err := s.gen.GenerateJSON(ctx, system, prompt, morningSchema)
	if err != nil {
		return draft{}, false, err
	}
	var d draft
	if err := json.Unmarshal(raw, &d); err != nil {
		return draft{}, true, fmt.Errorf("разбор ответа LLM: %w", err)
	}
	if d.Skip {
		// Отказ не переспрашивают: модель посмотрела на поводы дня и сказала,
		// что светлого в них нет. Уговаривать её значит получить натянутую
		// шутку про чью-то беду.
		return draft{Skip: true, Idea: d.Idea}, false, nil
	}
	d.Text = sitetext.Normalize(d.Text)
	if reason := validate(d, cfg); reason != "" {
		return draft{}, true, fmt.Errorf("%s", reason)
	}
	return d, false, nil
}

// publish — боевой путь: написать и опубликовать.
//
// Порядок здесь важнее читаемости: строка дня заводится в БД ДО отправки и
// назад не откатывается никогда. `PostNote` возвращает одну лишь ошибку, без
// id, поэтому «сайт принял и ответил сбоем» неотличимо от «не принял» — а
// вторая заметка в ленте убирается только модератором.
func (s *Service) publish(ctx context.Context, day string, slot time.Time) {
	d, facts, err := s.Draft(ctx, slot)
	if err != nil {
		s.log.Error("утренняя заметка: черновик не вышел", "день", day, "err", err)
		if _, e := s.st.MarkMorning(ctx, day, store.MorningFailed, reasonBadDraft, s.now()); e != nil {
			s.log.Error("утренняя заметка: отметка дня", "день", day, "err", e)
		}
		s.notify(ctx, "🌅 Утренняя заметка сегодня не вышла: "+err.Error())
		return
	}
	if d.Skip {
		s.log.Info("утренняя заметка: модель отказалась", "день", day, "идея", d.Idea)
		if _, e := s.st.MarkMorning(ctx, day, store.MorningSkipped, reasonNoFacts, s.now()); e != nil {
			s.log.Error("утренняя заметка: отметка дня", "день", day, "err", e)
		}
		s.notify(ctx, "🌅 Утренняя заметка сегодня не вышла: в поводах дня не нашлось светлого ("+d.Idea+").")
		return
	}
	cookies, err := s.cookies(ctx)
	if err != nil {
		s.log.Error("утренняя заметка: нет сессии", "день", day, "err", err)
		if _, e := s.st.MarkMorning(ctx, day, store.MorningFailed, reasonNoSession, s.now()); e != nil {
			s.log.Error("утренняя заметка: отметка дня", "день", day, "err", e)
		}
		return
	}

	// Точка невозврата.
	started, err := s.st.TryStartMorning(ctx, day, d.Text, factsJSON(facts), s.now())
	if err != nil {
		s.log.Error("утренняя заметка: запись дня", "день", day, "err", err)
		return
	}
	if !started {
		s.log.Info("утренняя заметка: день уже занят", "день", day)
		return
	}
	if err := s.site.PostNote(ctx, cookies, d.Text, false); err != nil {
		// Состояние остаётся posting: сайт мог заметку и принять, ответив
		// ошибкой. Причина записана, и предохранитель по ней поймёт, что вина
		// не наша (17.08.2026 сайт отвечал 500 на любую публикацию).
		s.log.Error("утренняя заметка: отправка не удалась", "день", day, "err", err)
		if e := s.st.SetMorningState(ctx, day, store.MorningPosting, store.MorningReasonSendFailed); e != nil {
			s.log.Error("утренняя заметка: отметка отправки", "день", day, "err", e)
		}
		return
	}
	if err := s.st.SetMorningState(ctx, day, store.MorningPosted, ""); err != nil {
		s.log.Error("утренняя заметка: отметка отправки", "день", day, "err", err)
	}
	s.log.Info("утренняя заметка опубликована", "день", day,
		"знаков", len([]rune(d.Text)), "поводов", len(d.Used), "идея", d.Idea)
}

// PublishToday — ручной догон: опубликовать сегодняшнюю заметку прямо сейчас.
// Тумблер и срок слота при этом не спрашиваются (человек решил сам), а вот
// «не сказал ли утро кто-то другой» спрашивается всегда: ради этой проверки
// фича и заведена. Снять её может только явное force.
func (s *Service) PublishToday(ctx context.Context, force bool) (string, error) {
	day, slot := SlotFor(s.now(), s.cfg.Loc, s.cfg.Hour)
	if done, err := s.dayDone(ctx, day); err != nil {
		return "", err
	} else if done {
		n, _ := s.st.MorningByDay(ctx, day)
		return "", fmt.Errorf("день %s уже закрыт: %s", day, stateText(n))
	}
	if !force {
		feed, err := s.site.FetchNotes(ctx)
		if err != nil {
			return "", fmt.Errorf("лента сайта: %w", err)
		}
		start, end := DayBounds(slot)
		if id := s.foreignGreeting(ctx, feed, start, end); id != "" {
			return "", fmt.Errorf("сегодня доброе утро уже написал кто-то другой (заметка %s); -force, если всё равно надо", id)
		}
	}
	s.publish(ctx, day, slot)
	n, err := s.st.MorningByDay(ctx, day)
	if err != nil {
		return "", err
	}
	return day + " — " + stateText(n), nil
}

// factsJSON — поводы в том виде, в каком их увидел прогон. Нужны не заметке, а
// разбору: «почему сегодня вышло так» через неделю отвечается строкой из базы,
// а календари к тому времени показывают уже другой день.
func factsJSON(facts []holidays.Occasion) string {
	if len(facts) == 0 {
		return ""
	}
	b, err := json.Marshal(facts)
	if err != nil {
		return ""
	}
	return string(b)
}

// history — свои заметки прошлых дней одной строкой каждая: модели нужен их
// ЗАХОД, чтобы не повториться.
func (s *Service) history(ctx context.Context) []string {
	rows, err := s.st.MorningRecent(ctx, s.cfg.HistorySize)
	if err != nil {
		s.log.Error("утренняя заметка: чтение истории", "err", err)
		return nil
	}
	var out []string
	for _, r := range rows {
		if strings.TrimSpace(r.Text) != "" {
			out = append(out, r.Text)
		}
	}
	return out
}

// nick — ник владельца на сайте. Берётся из сессии: своего обхода анкеты у
// службы нет, а протухший ник в промпте стоит дешевле лишнего запроса — он
// влияет только на то, как модель представляет себе автора.
func (s *Service) nick(ctx context.Context) string {
	messenger, userID, err := s.st.SessionForProfile(ctx, s.cfg.OwnerProfileID)
	if err != nil {
		return ""
	}
	_, _, nick, err := s.st.SessionIdentity(ctx, messenger, userID)
	if err != nil {
		return ""
	}
	return nick
}
