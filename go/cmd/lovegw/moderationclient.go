package main

// Сборка классификатора модерации — единственное место, где решается, ЧЕЙ API
// увидит тексты участников площадки.
//
// Почему это отдельная развилка, а не ещё один вызов llmClientFor. У дайджеста
// и амвона выбор провайдера — вопрос качества и цены; здесь он вопрос
// соблюдения обещания, данного людям: согласие говорит «не вывозит данные за
// пределы России», а на проверку уходит написанное ими же. Поэтому провайдер
// назван явно в конфиге и по умолчанию российский, а не наследуется из секции
// llm, где живёт Claude.
//
// Ветка anthropic оставлена намеренно и ровно для двух вещей: сравнить модели
// на стенде (`platform triage`) и не потерять работающий путь на случай, если
// у площадки когда-нибудь появится согласие, это разрешающее. Боевой выбор при
// нынешней бумаге один.

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/platmod"
	"lovegw/internal/rullm"
)

// providerYandex / providerAnthropic — значения platform.moderation.provider.
const (
	providerYandex    = "yandex"
	providerAnthropic = "anthropic"
)

// anthropicModerationModel — дешёвая модель Claude для триажа. Здесь, а не в
// умолчаниях конфига: у каждого провайдера имя модели своё, и одно поле на всех
// пришлось бы либо оставлять пустым, либо заполнять чужим значением. `effort`
// ей передавать нельзя — Haiku 4.5 отвечает на него ошибкой запроса.
const anthropicModerationModel = "claude-haiku-4-5"

// moderationClient строит классификатор по секции platform.moderation и
// возвращает вместе с ним РАЗРЕШЁННОЕ имя модели: оно уезжает в карточку
// проверки, а «пусто» там читалось бы как «неизвестно, кто решил».
func moderationClient(cfg *config.Config) (platmod.JSONGenerator, string, error) {
	m := cfg.Platform.Moderation
	switch provider := cmp.Or(m.Provider, providerYandex); provider {
	case providerYandex:
		model := cmp.Or(m.Model, rullm.DefaultModel)
		// Имя модели Anthropic при российском провайдере — самая вероятная
		// ошибка переезда: старый конфиг несёт «claude-haiku-4-5», а AI Studio
		// на него ответит отказом уже в бою. Ловим на сборке и называем прямо.
		if strings.HasPrefix(model, "claude") {
			return nil, "", fmt.Errorf(
				"moderation.model = %q при provider=yandex: это имя модели Anthropic, ожидается вида %q",
				model, rullm.DefaultModel)
		}
		if m.Effort != "" {
			return nil, "", fmt.Errorf("moderation.effort = %q: у provider=yandex усердия нет, поле должно быть пустым", m.Effort)
		}
		c, err := rullm.New(rullm.Config{
			APIKey:   m.APIKey,
			FolderID: m.FolderID,
			Model:    model,
			Timeout:  time.Duration(m.TimeoutS) * time.Second,
		}, m.BaseURL)
		if err != nil {
			return nil, "", fmt.Errorf("классификатор (yandex): %w (platform.moderation.api_key / folder_id, env LOVEGW_MODERATION_KEY / LOVEGW_MODERATION_FOLDER)", err)
		}
		return c, model, nil

	case providerAnthropic:
		model := cmp.Or(m.Model, anthropicModerationModel)
		c, err := llmClientFor(cfg, model, m.Effort, time.Duration(m.TimeoutS)*time.Second)
		if err != nil {
			return nil, "", fmt.Errorf("классификатор (anthropic): %w", err)
		}
		return c, model, nil

	default:
		return nil, "", fmt.Errorf("platform.moderation.provider = %q: известны %q и %q",
			provider, providerYandex, providerAnthropic)
	}
}
