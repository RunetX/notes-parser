package modwatch

import (
	"context"
	"sort"
	"time"
)

// Бан — самое частое действие модерации и единственное, которое НИЧЕГО не
// убирает с площадки: ни заметки, ни реплики. Наблюдать его нечем — но он
// оставляет след в ритме самой жертвы: человек, писавший каждый час, замолкает
// ровно на срок запрета и возвращается вскоре после его истечения.
//
// Замер по двум банам с известным временем (ГердаИзСемейкиАддамс, 05–07.08.2026,
// сроки со скриншотов личного лога): «Мат» 05.08 22:52 Нск — последняя реплика
// за 36 минут до запрета, возврат через 9 минут после снятия, пауза 24,75 ч;
// «Флуд» 07.08 00:13 — последняя реплика за 8 минут до, возврат смазан сном.
//
// Отсюда и окно самого действия: запрет наложен не раньше последней реплики и
// не позже, чем «возврат минус срок» (человек не мог вернуться раньше, чем
// истёк запрет). У расторопного возвращенца это десятки минут — не хуже, чем у
// пропавшего комментария.
//
// ВАЖНО, ЧЕГО ЭТОТ ПОИСК НЕ УМЕЕТ. Искать запреты вслепую им нельзя: на
// наблюдении 03–12.08.2026 (182 анкеты) подставные сроки дают столько же
// находок, сколько настоящий, — 8 при 24 ч против 9 при 20 ч и 5 при 22 ч, и
// «быстрых возвратов» поровну. В архиве за 2025–2026 (1,28 млн реплик) горба
// сразу за отметкой в сутки тоже нет: распределение пауз гладкое. Значит
// естественных суточных пауз столько, что подпись в них тонет — в списке ниже
// заведомо есть посторонние (в первом же прогоне туда попал модератор Хатуль
// мадан, которого забанить не могли).
//
// Годится инструмент для ДРУГОГО: когда запрет известен со стороны (человек
// сказал, показал лог), он восстанавливает окно наложения — на двух банах
// ГердаИзСемейкиАддамс окно накрыло настоящее время и уложилось в 45 минут.
// То есть это проверка и уточнение, а не разведка; в отчёт присутствия эти
// окна намеренно НЕ подмешиваются.
const (
	DefaultBanTolerance = 3 * time.Hour // насколько поздно человек возвращается после снятия
	DefaultBanMinAround = 3             // сколько реплик должно быть по обе стороны паузы
	banContextFactor    = 7             // окно оценки собственного ритма = срок × это
)

// BanTiers — известные сроки запрета. Суточный подтверждён скриншотами лога
// (12.08.2026), недельный и месячный — подписью пауз в архиве.
var BanTiers = []time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour}

// BanOptions — параметры поиска.
type BanOptions struct {
	Tiers     []time.Duration // сроки запрета (пусто — BanTiers)
	Tolerance time.Duration   // допустимое опоздание возврата (0 — DefaultBanTolerance)
	MinAround int             // реплик по обе стороны паузы (0 — DefaultBanMinAround)
}

// Ban — предполагаемый запрет: кого, на какой срок и в какое окно наложен.
type Ban struct {
	UserID   int64
	Tier     time.Duration // срок запрета
	Gap      time.Duration // фактическая пауза
	LastSeen time.Time     // последняя реплика перед паузой
	Return   time.Time     // первая реплика после
	From, To time.Time     // окно наложения запрета: [LastSeen, Return − Tier]
}

// Delay — насколько поздно человек вернулся после снятия запрета.
func (b Ban) Delay() time.Duration { return b.Gap - b.Tier }

func (o *BanOptions) defaults() {
	if len(o.Tiers) == 0 {
		o.Tiers = BanTiers
	}
	if o.Tolerance <= 0 {
		o.Tolerance = DefaultBanTolerance
	}
	if o.MinAround <= 0 {
		o.MinAround = DefaultBanMinAround
	}
}

// DetectBans ищет в активности одного человека паузы, похожие на запрет.
//
// Условия все три сразу, и третье — главное:
//  1. пауза укладывается в [срок, срок + Tolerance] — вернулся вскоре после снятия;
//  2. по обе стороны паузы человек писал (MinAround реплик) — значит молчание
//     не совпало с уходом с площадки;
//  3. в окружении этой паузы (срок × banContextFactor) она ЕДИНСТВЕННАЯ такой
//     длины. Без этого «через сутки» попадёт всякий, кто заходит через день:
//     подпись не в самой паузе, а в том, что она выбивается из его ритма.
//
// times должен быть отсортирован по возрастанию.
func DetectBans(userID int64, times []time.Time, opt BanOptions) []Ban {
	opt.defaults()
	var out []Ban
	for i := 1; i < len(times); i++ {
		prev, next := times[i-1], times[i]
		gap := next.Sub(prev)
		for _, tier := range opt.Tiers {
			if gap < tier || gap > tier+opt.Tolerance {
				continue
			}
			if !aroundEnough(times, i, tier, opt.MinAround) {
				continue
			}
			if !uniqueGap(times, i, tier) {
				continue
			}
			to := next.Add(-tier)
			if to.Before(prev) {
				to = prev // возврат раньше срока — окном считаем саму паузу
			}
			out = append(out, Ban{
				UserID: userID, Tier: tier, Gap: gap,
				LastSeen: prev, Return: next, From: prev, To: to,
			})
			break // сроки не пересекаются, засчитываем ближайший
		}
	}
	return out
}

// aroundEnough — писал ли человек по обе стороны паузы в пределах контекста.
func aroundEnough(times []time.Time, i int, tier time.Duration, want int) bool {
	ctx := tier * banContextFactor
	before, after := 0, 0
	for j := i - 1; j >= 0 && times[i-1].Sub(times[j]) <= ctx; j-- {
		before++
	}
	for j := i; j < len(times) && times[j].Sub(times[i]) <= ctx; j++ {
		after++
	}
	return before >= want && after >= want
}

// uniqueGap — нет ли рядом второй такой же длинной паузы. Если человек и так
// пропадает на сутки через раз, эта пауза ничего не говорит.
func uniqueGap(times []time.Time, i int, tier time.Duration) bool {
	ctx := tier * banContextFactor
	from, to := times[i-1].Add(-ctx), times[i].Add(ctx)
	for j := 1; j < len(times); j++ {
		if j == i || times[j].Before(from) || times[j-1].After(to) {
			continue
		}
		if times[j].Sub(times[j-1]) >= tier {
			return false
		}
	}
	return true
}

// Bans ищет запреты по всей наблюдённой активности.
func (s *Store) Bans(ctx context.Context, opt BanOptions) ([]Ban, error) {
	presence, err := s.PresenceLog(ctx, time.Unix(0, 0).UTC(), time.Now().UTC().AddDate(1, 0, 0))
	if err != nil {
		return nil, err
	}
	byUser := map[int64][]time.Time{}
	for _, p := range presence {
		byUser[p.UserID] = append(byUser[p.UserID], p.At)
	}
	var out []Ban
	for u, times := range byUser {
		out = append(out, DetectBans(u, times, opt)...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From.Before(out[j].From) })
	return out, nil
}

// Event переводит запрет в событие — для сверки присутствия, когда запрет
// известен со стороны. В БД такие события не пишутся: это вывод из наблюдений,
// а не наблюдение, и слепому поиску доверять нельзя (см. шапку файла).
func (b Ban) Event() Event {
	return Event{
		Kind: KindUserBanned, RefID: b.UserID,
		PrevSeen: b.From, DetectedAt: b.To, Age: Unknown, Idle: Unknown,
	}
}
