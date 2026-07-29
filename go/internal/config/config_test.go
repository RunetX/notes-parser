package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Легаси-конфиг без секции messengers читается как telegram (включён по
// факту наличия токена).
func TestLoadLegacyFlatConfig(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{
		"mirror_bot": {"token": "tg-token", "channel_id": -100, "discussion_chat_id": -200},
		"dm_bot": {"token": "dm-token"},
		"admin_tg_user_id": 42
	}`))
	if err != nil {
		t.Fatal(err)
	}
	tg := cfg.Messengers.Telegram
	if !tg.Enabled || tg.Token != "tg-token" || tg.ChannelID != -100 ||
		tg.DiscussionChatID != -200 || tg.DMToken != "dm-token" || tg.AdminUserID != 42 {
		t.Errorf("telegram из плоского конфига: %+v", tg)
	}
	if cfg.Messengers.Max.Enabled {
		t.Error("max не должен включиться сам")
	}
}

// Новый формат: секция messengers, telegram добирает недостающее из плоских
// полей (env-переопределения пишут в плоские).
func TestLoadMessengersSection(t *testing.T) {
	t.Setenv("LOVEGW_MAX_TOKEN", "env-max-token")
	t.Setenv("LOVEGW_MAX_DM_TOKEN", "env-max-dm-token")
	t.Setenv("LOVEGW_MIRROR_TOKEN", "env-tg-token")
	cfg, err := Load(writeConfig(t, `{
		"messengers": {
			"max": {"enabled": true, "channel_id": 77, "discussion_chat_id": 78},
			"telegram": {"enabled": false, "channel_id": -100, "discussion_chat_id": -200}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	mx := cfg.Messengers.Max
	// Легаси-dm_token у max — это бот переписки: значение переезжает в talks_token.
	if !mx.Enabled || mx.Token != "env-max-token" || mx.TalksToken != "env-max-dm-token" ||
		mx.DMToken != "" || mx.ChannelID != 77 {
		t.Errorf("max: %+v", mx)
	}
	tg := cfg.Messengers.Telegram
	if tg.Enabled || tg.Token != "env-tg-token" {
		t.Errorf("telegram: %+v", tg)
	}
}

// Бот переписки: talks_token читается у обоих мессенджеров, у max легаси
// dm_token переезжает в него, у telegram dm_token остаётся ботом команд.
func TestLoadTalksTokens(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{
		"messengers": {
			"max": {"enabled": true, "token": "max-token", "dm_token": "max-legacy-dm"},
			"telegram": {"enabled": true, "token": "tg-token", "dm_token": "tg-dm",
			             "talks_token": "tg-talks"}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	mx := cfg.Messengers.Max
	if mx.TalksToken != "max-legacy-dm" || mx.DMToken != "" {
		t.Errorf("max: dm_token должен стать talks_token: %+v", mx)
	}
	tg := cfg.Messengers.Telegram
	if tg.DMToken != "tg-dm" || tg.TalksToken != "tg-talks" {
		t.Errorf("telegram: dm_token — бот команд, talks_token — бот переписки: %+v", tg)
	}
}

// Явный talks_token у max переезда не допускает: dm_token остаётся как есть.
func TestLoadTalksTokenExplicitWins(t *testing.T) {
	t.Setenv("LOVEGW_TG_TALKS_TOKEN", "env-tg-talks")
	cfg, err := Load(writeConfig(t, `{
		"messengers": {
			"max": {"enabled": true, "token": "max-token",
			        "dm_token": "max-dm", "talks_token": "max-talks"},
			"telegram": {"enabled": true, "token": "tg-token", "talks_token": "json-tg-talks"}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if mx := cfg.Messengers.Max; mx.TalksToken != "max-talks" || mx.DMToken != "max-dm" {
		t.Errorf("max: %+v", mx)
	}
	if tg := cfg.Messengers.Telegram; tg.TalksToken != "env-tg-talks" {
		t.Errorf("env должен перебивать talks_token из конфига: %+v", tg)
	}
}

// Подпись per-messenger: своя перекрывает общую, пустая — наследует.
func TestLoadPerMessengerSignature(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{
		"signature": "@common",
		"messengers": {
			"max": {"enabled": true, "signature": "канал в MAX"},
			"telegram": {"enabled": true}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Messengers.Max.Signature; got != "канал в MAX" {
		t.Errorf("подпись max: %q", got)
	}
	if got := cfg.Messengers.Telegram.Signature; got != "@common" {
		t.Errorf("подпись telegram (наследование): %q", got)
	}
}
