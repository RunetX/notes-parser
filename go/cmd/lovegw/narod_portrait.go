package main

// narod portrait — английские промпты для генерации аватаров жителям.
//
// Последний шаг конвейера производства персонажа и единственный, который
// кончается НЕ в базе: команда печатает текст, владелец несёт его в генератор
// картинок, а готовый файл ставит на `/mod/admin`. Замкнуть круг здесь нельзя —
// картинки мы не рисуем, — и притворяться, будто можно, незачем.
//
// Устройство промпта и все решения о том, что в него идёт, живут в
// `narod.Portrait`. Здесь только каталог, жребий и деньги.
//
// СЛЕПКАМ ОТКАЗЫВАЕТ, как и enroll, и по тому же доводу: портрет — вещь, которая
// появится на странице, а слепок это ник и манера ЖИВОГО человека. Нарисовать
// ему лицо и повесить рядом с его же словами было бы хуже имперсонации.
//
// БЕСПЛАТНО ПО УМОЛЧАНИЮ. Без `-speak` печатается скелет: кадр, свет, техника,
// возраст — всё, что считает код, — а внешность остаётся нейтральной. Это стенд
// для правки самого промпта, и крутить его можно сколько угодно. Флаг тот же,
// что у `replay`, намеренно: одно слово через весь эпик значит «зовём модель, и
// это стоит денег».

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lovegw/internal/config"
	"lovegw/internal/llm"
	"lovegw/internal/narod"
)

type portraitOpts struct {
	cardsDir string
	outDir   string
	slugs    []string
	seed     int64
	speak    bool
	mj       bool
	cfgPath  string
	model    string
}

func narodPortrait(ctx context.Context, o portraitOpts) error {
	slugs, err := portraitSlugs(o.cardsDir, o.slugs)
	if err != nil {
		return err
	}
	cards := make([]narod.Card, 0, len(slugs))
	for _, slug := range slugs {
		card, err := narod.LoadCard(cardPath(o.cardsDir, slug))
		if err != nil {
			return err
		}
		if card.Kind != narod.KindComposite {
			return fmt.Errorf("%s — %s: портрет рисуют только композиту, у слепка ник и манера живого человека",
				card.ID, card.Kind)
		}
		cards = append(cards, card)
	}

	// Модель зовут по одному разу на жителя, и сказать об этом надо ДО первого
	// запроса: счёт приходит помесячно и общий на всё, а «во что обошёлся этот
	// прогон» спрашивают сразу после него.
	var gen *llm.Client
	if o.speak {
		cfg, err := config.Load(o.cfgPath)
		if err != nil {
			return err
		}
		// Кэш-точка на системный блок: рамка у всех жителей одна и та же, и
		// повтор здесь ДОКАЗАН устройством прогона — тридцать запросов встык.
		gen, err = llmClientFor(cfg, o.model, "", 0, withSystemCache())
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "ПЛАТНО: %d обращений к модели (%s), по одному на жителя\n",
			len(cards), gen.Model())
	} else {
		fmt.Fprintln(os.Stderr, "без -speak внешность остаётся нейтральной: печатается только скелет кадра")
	}

	if o.outDir != "" {
		if err := os.MkdirAll(o.outDir, 0o755); err != nil {
			return fmt.Errorf("каталог промптов: %w", err)
		}
	}
	for _, card := range cards {
		var look narod.Look
		if gen != nil {
			if look, err = askLook(ctx, gen, card.Persona); err != nil {
				return fmt.Errorf("внешность жителя %s: %w", card.ID, err)
			}
		}
		prompt := narod.Portrait(card.Persona, card.ID, uint64(o.seed), look, o.mj)
		if o.outDir != "" {
			name := filepath.Join(o.outDir, card.ID+".txt")
			if err := os.WriteFile(name, []byte(prompt), 0o644); err != nil {
				return fmt.Errorf("промпт жителя %s: %w", card.ID, err)
			}
		}
		fmt.Printf("=== %s — %s ===\n%s\n", card.ID, card.Persona.Nick, prompt)
	}
	if o.outDir != "" {
		fmt.Fprintf(os.Stderr, "промпты сложены в %s\n", o.outDir)
	}
	if gen != nil {
		fmt.Fprintf(os.Stderr, "расход: %s\n", gen.Usage())
	}
	return nil
}

// askLook спрашивает у модели внешность одного жителя.
//
// Отдаёт ей не карточку, а ровно то, что собрал narod.PortraitRequest: ник и
// больное место туда не идут, и решается это в ядре, а не здесь.
func askLook(ctx context.Context, gen *llm.Client, b narod.Bio) (narod.Look, error) {
	system, prompt := narod.PortraitRequest(b)
	raw, err := gen.GenerateJSON(ctx, system, prompt, narod.PortraitSchema)
	if err != nil {
		return narod.Look{}, err
	}
	var look narod.Look
	if err := json.Unmarshal(raw, &look); err != nil {
		return narod.Look{}, fmt.Errorf("разбор ответа: %w", err)
	}
	return look, nil
}

// portraitSlugs — кого рисуем. Без аргументов — весь каталог: жителей тридцать,
// и перечислять их руками значит однажды забыть одного.
func portraitSlugs(dir string, args []string) ([]string, error) {
	if len(args) > 0 {
		return args, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("в %s нет ни одной карточки", dir)
	}
	slugs := make([]string, 0, len(files))
	for _, f := range files {
		slugs = append(slugs, strings.TrimSuffix(filepath.Base(f), ".json"))
	}
	sort.Strings(slugs)
	return slugs, nil
}
