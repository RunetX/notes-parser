package main

import (
	"strings"
	"testing"

	"lovegw/internal/config"
	"lovegw/internal/rullm"
)

// base — конфиг с настроенной площадкой и ключами обоих провайдеров.
func base() *config.Config {
	c := &config.Config{}
	c.LLM.APIKey = "claude-test-key"
	c.Platform.Moderation.APIKey = "yc-test-key"
	c.Platform.Moderation.FolderID = "b1gtest"
	return c
}

// Провайдер по умолчанию — российский, и это не деталь настройки: согласие
// обещает участникам, что их тексты не уедут за пределы России.
func TestDefaultProviderIsRussian(t *testing.T) {
	gen, model, err := moderationClient(base())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gen.(*rullm.Client); !ok {
		t.Errorf("по умолчанию собрался не российский клиент: %T", gen)
	}
	if model != rullm.DefaultModel {
		t.Errorf("модель: %q, ожидалась %q", model, rullm.DefaultModel)
	}
}

// Самая вероятная ошибка переезда: конфиг несёт прежнее имя модели Anthropic, а
// провайдер уже российский. Ловится на сборке, а не отказом AI Studio в бою.
func TestAnthropicModelUnderYandexIsRefused(t *testing.T) {
	c := base()
	c.Platform.Moderation.Model = "claude-haiku-4-5"
	_, _, err := moderationClient(c)
	if err == nil || !strings.Contains(err.Error(), "Anthropic") {
		t.Fatalf("ожидался отказ с объяснением, получено: %v", err)
	}
}

// Усердие — рычаг Anthropic; у российского провайдера его нет, и молча
// проглотить его нельзя: человек будет думать, что настроил глубину проверки.
func TestEffortUnderYandexIsRefused(t *testing.T) {
	c := base()
	c.Platform.Moderation.Effort = "low"
	if _, _, err := moderationClient(c); err == nil {
		t.Fatal("effort при provider=yandex обязан быть отказом")
	}
}

// Без ключа или каталога классификатор не собирается, и текст ошибки обязан
// называть, где их взять: демон от этого не падает, а пишет предупреждение, и
// другого шанса объясниться у него нет.
func TestYandexWithoutCredentialsNamesWhereToPutThem(t *testing.T) {
	c := base()
	c.Platform.Moderation.APIKey = ""
	_, _, err := moderationClient(c)
	if err == nil {
		t.Fatal("ожидался отказ без ключа")
	}
	if !strings.Contains(err.Error(), "LOVEGW_MODERATION_KEY") {
		t.Errorf("ошибка не говорит, куда класть ключ: %v", err)
	}
}

// Ветка Anthropic остаётся рабочей — она нужна стенду для сравнения моделей.
func TestAnthropicBranchStillWorks(t *testing.T) {
	c := base()
	c.Platform.Moderation.Provider = providerAnthropic
	gen, model, err := moderationClient(c)
	if err != nil {
		t.Fatal(err)
	}
	if gen == nil {
		t.Fatal("клиент не собран")
	}
	if model != anthropicModerationModel {
		t.Errorf("модель: %q, ожидалась %q", model, anthropicModerationModel)
	}
}

func TestUnknownProviderIsRefused(t *testing.T) {
	c := base()
	c.Platform.Moderation.Provider = "openai"
	if _, _, err := moderationClient(c); err == nil {
		t.Fatal("неизвестный провайдер обязан быть отказом")
	}
}
