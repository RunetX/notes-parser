package main

// Стенд классификатора: `platform triage [-limit N]`.
//
// Прогоняет очередь проверки через модель и печатает, ЧТО БЫ автомат сделал, —
// не делая ничего. Ни вердиктов, ни попыток, ни расхода суточного потолка; при
// работающем демоне безопасен.
//
// Зачем команда есть. Включённый автомат вправе скрыть чужие слова, и «работает
// ли он» на глаз не проверяется: единственная честная проверка — прогнать
// настоящие реплики и посмотреть глазами. Мера при этом не «точность вообще», а
// ЛОЖНЫЕ СКРЫТИЯ: ссора, грубость и откровенность — жанр раздела, и модель,
// принявшая их за нарушение, хуже отсутствующей. Поэтому сводка внизу считает
// отдельно то, что ушло бы под нож без человека.
//
// Она же — единственный способ сравнить провайдеров и модели на одних и тех же
// данных: `-model` подменяет модель, не трогая конфиг.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"lovegw/internal/config"
	"lovegw/internal/platform"
	"lovegw/internal/platmod"
)

// platformTriage прогоняет очередь (или один тред) через классификатор
// вхолостую.
func platformTriage(ctx context.Context, cfg *config.Config, limit int, model string, noteID int64) error {
	m := cfg.Platform.Moderation
	if model != "" {
		cfg.Platform.Moderation.Model = model
	}
	gen, resolved, err := moderationClient(cfg)
	if err != nil {
		return err
	}

	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	var items []platform.Pending
	if noteID != 0 {
		// Реплики треда — без тела самой заметки, и это про ЗАМЕР, а не про
		// то, что автомат проверяет в бою: там в очередь идёт весь НАШ контент,
		// заметки наравне с репликами, а зеркальное НГС не идёт вовсе.
		// Стенд же ходит именно по зеркальному — просто потому, что настоящих
		// строк в базе только они, и мерить больше не на чем. Отсюда правило:
		// то, что стенд туда ходит, боевого правила НЕ отменяет.
		items, err = p.PendingOfNote(ctx, noteID, limit)
	} else {
		// maxAttempts здесь ноль: стенду интересны и те строки, на которых
		// боевой автомат уже спотыкался, — как раз они и объясняют, почему
		// очередь стоит.
		items, err = p.PendingChecks(ctx, limit, 0)
	}
	if err != nil {
		return err
	}
	if len(items) == 0 {
		if noteID != 0 {
			fmt.Printf("в заметке %d нет видимых публикаций — прогонять нечего\n", noteID)
			return nil
		}
		fmt.Println("очередь проверки пуста — прогонять нечего")
		return nil
	}

	batch := m.BatchSize
	if batch <= 0 {
		batch = len(items)
	}
	// Суточный потолок и попытки службе здесь не нужны: Triage их не трогает.
	svc := platmod.New(platmod.Config{Model: resolved, Batch: batch}, p, gen, nil)

	fmt.Printf("модель %s, публикаций %d, пачками по %d — НИЧЕГО НЕ ЗАПИСЫВАЕТСЯ\n\n",
		resolved, len(items), batch)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "объект\tрешение\tкатегория\tтекст")
	var t triageTally
	for start := 0; start < len(items); start += batch {
		end := min(start+batch, len(items))
		chunk := items[start:end]
		verdicts, err := svc.Triage(ctx, chunk)
		if err != nil {
			// Пачка целиком не удалась — и прогон на этом НЕ кончается.
			// Провайдер умеет отказаться читать входной текст, а отказ
			// прилетает на ВСЮ пачку из-за одной строки; обрываясь, стенд
			// показывал бы очередь только до первой такой строки — то есть
			// прятал бы ровно то, ради чего его зовут.
			fmt.Fprintf(w, "пачка %d–%d\tОТКАЗ\t\t%s\n", start+1, end, err)
			t.failed += len(chunk)
			continue
		}
		for i, v := range verdicts {
			t.row(w, chunk[i], v)
		}
	}
	w.Flush() //nolint:errcheck // сводка ниже важнее ошибки вывода

	fmt.Printf("\nчисто %d · человеку %d · СКРЫТЬ БЕЗ ЧЕЛОВЕКА %d", t.clean, t.review, t.hidden)
	if t.silent > 0 {
		fmt.Printf(" · модель промолчала %d", t.silent)
	}
	if t.failed > 0 {
		fmt.Printf(" · не проверено из-за отказа %d", t.failed)
	}
	fmt.Println("\nсмотреть в первую очередь на «СКРЫТЬ»: ссора и грубость — жанр раздела, и там их быть не должно")
	return nil
}

// triageTally — счёт прогона. Отдельно от печати не разнесён намеренно: строка
// и счётчик описывают одно и то же решение, и разъехаться они не должны.
type triageTally struct{ clean, review, hidden, silent, failed int }

// row печатает одну строку прогона и учитывает её в сводке.
func (t *triageTally) row(w io.Writer, it platform.Pending, v *platform.VerdictRecord) {
	if v == nil {
		// Модель промолчала про этот номер. В бою строка просто ждёт
		// следующего захода, но на стенде это факт о модели, и прятать его
		// нельзя: пачка, из которой систематически выпадают номера, означает,
		// что её пора уменьшать.
		t.silent++
		fmt.Fprintf(w, "%s\t—\t\t%s\n", it.Subject.String(), excerptOf(it.Body))
		return
	}
	var decision string
	switch v.Verdict {
	case platform.VerdictHidden:
		decision, t.hidden = "СКРЫТЬ", t.hidden+1
	case platform.VerdictReview:
		decision, t.review = "человеку", t.review+1
	default:
		decision, t.clean = "чисто", t.clean+1
	}
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
		it.Subject.String(), decision, v.Category, excerptOf(it.Body))
	// Цитата печатается рядом с причиной не для красоты: скрытие требует её
	// непустой (platmod.decide), и пустая цитата под вердиктом «СКРЫТЬ»
	// означала бы, что правило где-то разъехалось.
	if v.Verdict != platform.VerdictClean {
		fmt.Fprintf(w, "\t\t\t↳ %s | цитата: %s\n", v.Reason, excerptOf(v.Quote))
	}
}

// excerptOf — начало реплики для таблицы. Переносы строк схлопываются: строка
// таблицы обязана остаться строкой.
func excerptOf(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= 60 {
		return s
	}
	return string(r[:60]) + "…"
}
