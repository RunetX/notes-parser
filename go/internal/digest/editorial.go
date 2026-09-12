package digest

// Автоматическая LLM-редактура: тексты четырёх LLM-рубрик генерируются одним
// запросом к Claude по материалам выпуска (то же сырьё, что в materials.md
// полуручного цикла). Невалидный ответ — ошибка целиком: вызывающий
// откатывается на полуручный цикл с плейсхолдерами, а не публикует брак.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// JSONGenerator — онлайн-LLM, отвечающий строго по JSON-схеме
// (реализация — llm.Client).
type JSONGenerator interface {
	GenerateJSON(ctx context.Context, system, prompt string, schema map[string]any) ([]byte, error)
}

// Editorial — тексты LLM-рубрик выпуска (HTML-подмножество черновика).
// Пустое поле — рубрика опускается (нет кандидатов).
type Editorial struct {
	WeekSummary   string `json:"week_summary"`
	DisputeNoteID string `json:"dispute_note_id"`
	Dispute       string `json:"dispute"`
	Quote         string `json:"quote"`
	Topics        string `json:"topics"`
}

const editorialSystem = `Ты — редактор еженедельного дайджеста площадки «Зазеркалье» —
сообщества, переехавшего из раздела «Заметки» сайта знакомств love.ngs.ru. Ниже — материалы выпуска: статистика недели,
кандидаты рубрик и полные тексты. Верни JSON с текстами четырёх рубрик.

Тон: лёгкая ирония, тепло; без сарказма в адрес конкретных людей.
Запрещено: приписывать авторам намерения и диагнозы, делать выводы о
личностях, использовать факты сверх приведённых материалов.

Поля:
- week_summary — «О чём была неделя»: 2–3 предложения о том, что занимало
  людей и что сдвинулось против прошлой недели. Без чисел и без имён.
- dispute_note_id — id заметки самого накалённого треда, выбранного из
  кандидатов «спора недели» (пустая строка, если кандидатов нет).
- dispute — «Спор недели»: 2–3 предложения об этом треде: из-за чего
  сыр-бор и какая температура. Реплики дословно не цитировать. Пустая
  строка, если кандидатов нет.
- quote — «Цитата недели»: подводка в одно предложение, затем цитата из
  шорт-листа в кавычках и автор курсивом: «…» — <i>Имя</i>. Цитату можно
  сократить многоточием, но не переписывать. Пустая строка, если
  шорт-листа нет.
- topics — «Темы недели»: 2–4 строки, по короткой теме на строку; отметь,
  что появилось и что ушло против прошлой недели. Без чисел.

Разметка: только теги <i> и <b>; символы < > & вне тегов экранируй как
&lt; &gt; &amp;. Без markdown и без заголовков — заголовки рубрик уже
стоят в выпуске.`

var editorialSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"week_summary":    map[string]any{"type": "string"},
		"dispute_note_id": map[string]any{"type": "string"},
		"dispute":         map[string]any{"type": "string"},
		"quote":           map[string]any{"type": "string"},
		"topics":          map[string]any{"type": "string"},
	},
	"required":             []string{"week_summary", "dispute_note_id", "dispute", "quote", "topics"},
	"additionalProperties": false,
}

// editorialRetries — сколько раз просить рубрики, прежде чем откатиться на
// полуручной цикл. Брак бывает разовым: 15.08.2026 модель за считаные секунды
// вернула валидный по схеме JSON с пустыми week_summary и topics, а тот же
// запрос минутой позже отработал нормально (44 с). Выпуск еженедельный, и
// сорванная автопубликация ждёт руки до следующей субботы — переспрос дешевле
// осечки.
const editorialRetries = 3

// editorialBackoff — пауза перед повтором ПОСЛЕ ОТКАЗА ЗАПРОСА. Список короче
// числа попыток намеренно: последняя пауза повторяется.
//
// До 12.09.2026 отказ запроса не повторялся вовсе, и довод стоял тут же:
// «временные сбои сети и 429/5xx ретраит SDK». Довод неверен ровно для того
// случая, который и случился: канал до api.anthropic.com идёт через SOCKS-прокси
// (с RU-IP прямого пути нет), рукопожатие на нём стало срываться, и запрос
// выел весь llm.requestTimeout — пять минут — не дойдя до модели. SDK такое не
// ретраит: для него это истёкший дедлайн запроса, а не 5xx. Выпуск 2026-W37
// не вышел в срок именно поэтому.
//
// Арифметика худшего случая: три попытки по пять минут плюс паузы — около
// восемнадцати минут на слот. Для еженедельной рубрики это дёшево, а приёмник
// Telegram в ту же ночь выжил на том же канале ровно тем, что повторяет.
var editorialBackoff = []time.Duration{30 * time.Second, 2 * time.Minute}

// failKind — почему попытка не удалась. Разница не косметическая: брак ответа
// чинится ПЕРЕСПРОСОМ (причина едет в промпт), отказ запроса — ОЖИДАНИЕМ
// (промпт не трогаем, ответа не было вовсе).
type failKind int

const (
	failNone failKind = iota
	failRequest
	failBadAnswer
)

// pause ждёт d, но не глуше отмены: демон гасят по контексту, а пауза тут
// измеряется минутами.
func pause(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// backoffFor — пауза перед (waits+1)-м ожиданием канала.
func backoffFor(waits int) time.Duration {
	if len(editorialBackoff) == 0 {
		return 0
	}
	if waits >= len(editorialBackoff) {
		waits = len(editorialBackoff) - 1
	}
	return editorialBackoff[waits]
}

// retryNote — хвост переспроса. Слепой повтор того же запроса повторил бы и
// вырожденный ответ, поэтому причина брака едет в промпт.
const retryNote = `

## Переспрос

Предыдущий ответ забракован: %v. Напиши рубрики заново — обязательные поля
пустыми быть не должны.`

// GenerateEditorial запрашивает у LLM тексты рубрик по материалам выпуска и
// валидирует их той же проверкой, что правки админа в черновике.
func GenerateEditorial(ctx context.Context, gen JSONGenerator, is *Issue) (*Editorial, error) {
	var materials strings.Builder
	if err := WriteMaterials(&materials, is); err != nil {
		return nil, err
	}
	base := materials.String()
	var (
		badAnswer error // последний БРАК — его причина едет в переспрос
		lastErr   error // последняя ошибка любого рода: с ней и сдаёмся
		waits     int   // сколько раз уже ждали канал
	)
	for attempt := 0; attempt < editorialRetries; attempt++ {
		prompt := base
		if badAnswer != nil {
			prompt += fmt.Sprintf(retryNote, badAnswer)
		}
		ed, kind, err := generateOnce(ctx, gen, prompt, is)
		if err == nil {
			return ed, nil
		}
		lastErr = err
		if kind == failBadAnswer {
			badAnswer = err
			continue
		}
		// Отказ запроса: ответа не было вовсе, переспрашивать не о чем —
		// ждём канал и повторяем тем же промптом.
		if attempt+1 < editorialRetries {
			if err := pause(ctx, backoffFor(waits)); err != nil {
				return nil, errors.Join(lastErr, err)
			}
			waits++
		}
	}
	return nil, fmt.Errorf("редактура: попытки исчерпаны: %w", lastErr)
}

// generateOnce — одна попытка: запрос, разбор, валидация. Вид отказа отделяет
// брак ответа (пришёл, но не годится) от отказа запроса (ответа не было): у них
// разное лечение — переспрос против ожидания канала.
func generateOnce(ctx context.Context, gen JSONGenerator, prompt string, is *Issue) (_ *Editorial, kind failKind, err error) {
	raw, err := gen.GenerateJSON(ctx, editorialSystem, prompt, editorialSchema)
	if err != nil {
		return nil, failRequest, err
	}
	var ed Editorial
	if err := json.Unmarshal(raw, &ed); err != nil {
		return nil, failBadAnswer, fmt.Errorf("разбор ответа LLM: %w", err)
	}
	if err := validateEditorial(&ed, is); err != nil {
		return nil, failBadAnswer, fmt.Errorf("ответ LLM не прошёл валидацию: %w", err)
	}
	return &ed, failNone, nil
}

// validateEditorial проверяет разметку полей и обязательность рубрик.
func validateEditorial(ed *Editorial, is *Issue) error {
	var errs []string
	check := func(name, text string) {
		if text == "" {
			return
		}
		if err := validateElement(text); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		}
	}
	check("week_summary", ed.WeekSummary)
	check("dispute", ed.Dispute)
	check("quote", ed.Quote)
	check("topics", ed.Topics)
	if ed.WeekSummary == "" {
		errs = append(errs, "week_summary пуст")
	}
	if len(is.ThisWeekNotes) > 0 && ed.Topics == "" {
		errs = append(errs, "topics пуст при непустой неделе")
	}
	if len(is.Disputes) > 0 && ed.Dispute != "" && ed.DisputeNoteID != "" {
		found := false
		for _, s := range is.Disputes {
			if s.Note.ID == ed.DisputeNoteID {
				found = true
			}
		}
		if !found {
			// Не срываем выпуск: маркер откатится на главного кандидата.
			ed.DisputeNoteID = ""
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// disputePick — заметка «спора недели»: выбор LLM или главный кандидат.
func disputePick(is *Issue) NoteStat {
	if is.Editorial != nil && is.Editorial.DisputeNoteID != "" {
		for _, s := range is.Disputes {
			if s.Note.ID == is.Editorial.DisputeNoteID {
				return s
			}
		}
	}
	return is.Disputes[0]
}
