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

// ASR по умолчанию выключен, лимиты — консервативные.
func TestLoadASRDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"site": {"base_url": "https://love.ngs.ru"}}`))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.ASR
	if a.Enabled || a.Provider != "nexara" || a.MaxDurationSec != 90 ||
		a.UserDailyLimitSec != 600 || a.Concurrency != 2 || a.TimeoutSec != 60 {
		t.Errorf("дефолты asr: %+v", a)
	}
}

// Слот дайджеста по умолчанию — суббота 09:00 Нск. Пин намеренный: день и час
// заданы ровно здесь (в digest мёртвые константы-дубли убраны), и молчаливый
// переезд слота сдвинул бы ещё и шов недели — см. комментарий digest.DefaultTZ.
func TestLoadDigestSlotDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"site": {"base_url": "https://love.ngs.ru"}}`))
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Digest
	if d.Enabled || d.Weekday != 6 || d.Hour != 9 || d.TZ != "Asia/Novosibirsk" {
		t.Errorf("дефолты дайджеста: %+v (ждали выключен, сб 09:00 Нск)", d)
	}
}

// Секция asr читается из JSON, env перебивает её.
func TestLoadASREnvOverrides(t *testing.T) {
	t.Setenv("LOVEGW_ASR_ENABLED", "true")
	t.Setenv("LOVEGW_ASR_API_KEY", "nx-from-env")
	t.Setenv("LOVEGW_ASR_BASE_URL", "https://asr.example/v1")
	t.Setenv("LOVEGW_ASR_FFMPEG", "/usr/local/bin/ffmpeg")
	t.Setenv("LOVEGW_ASR_MAX_DURATION_SEC", "45")
	t.Setenv("LOVEGW_ASR_USER_DAILY_LIMIT_SEC", "300")
	t.Setenv("LOVEGW_ASR_CONCURRENCY", "4")
	t.Setenv("LOVEGW_ASR_TIMEOUT_SEC", "30")
	cfg, err := Load(writeConfig(t, `{
		"asr": {"enabled": false, "api_key": "nx-from-file", "max_duration_sec": 120}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.ASR
	if !a.Enabled || a.APIKey != "nx-from-env" || a.BaseURL != "https://asr.example/v1" ||
		a.FFmpegPath != "/usr/local/bin/ffmpeg" || a.MaxDurationSec != 45 ||
		a.UserDailyLimitSec != 300 || a.Concurrency != 4 || a.TimeoutSec != 30 {
		t.Errorf("env-переопределения asr: %+v", a)
	}
}

// Амвон по умолчанию выключен, а пороги длины сняты с самого владельца
// (p25 = 42 руны, p50 = 79) и подрезаны под голос прикольщика: удар шутки
// бывает в четыре слова, а длинная шутка — уже рассказ.
func TestLoadPulpitDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"site": {"base_url": "https://love.ngs.ru"}}`))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Pulpit
	if p.Enabled || p.FeedIntervalS != 20 || p.FreshnessMin != 15 || p.MaxPerDay != 25 ||
		p.GenerateTimeoutS != 90 || p.MinRunes != 25 || p.MaxRunes != 300 ||
		!p.AllowEmoji || p.Effort != "medium" ||
		p.ReplyProbability != 0.15 || p.FuseMisses != 3 {
		t.Errorf("дефолты амвона: %+v", p)
	}
}

// Секция pulpit читается из JSON, env перебивает её.
func TestLoadPulpitEnvOverrides(t *testing.T) {
	t.Setenv("LOVEGW_PULPIT_ENABLED", "true")
	t.Setenv("LOVEGW_PULPIT_MODEL", "claude-sonnet-5")
	t.Setenv("LOVEGW_PULPIT_REPLY_PROBABILITY", "0")
	cfg, err := Load(writeConfig(t, `{
		"pulpit": {"enabled": false, "owner_profile_id": "1472546", "model": "claude-opus-5",
		           "reply_probability": 0.5, "allow_emoji": false}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Pulpit
	if !p.Enabled || p.Model != "claude-sonnet-5" || p.ReplyProbability != 0 ||
		p.OwnerProfileID != "1472546" || p.AllowEmoji {
		t.Errorf("env-переопределения амвона: %+v", p)
	}
}

// Мусор в числовой/булевой переменной — ошибка конфига, а не молчаливый ноль.
func TestLoadASRBadEnv(t *testing.T) {
	for _, name := range []string{
		"LOVEGW_ASR_CONCURRENCY", "LOVEGW_ASR_ENABLED",
		"LOVEGW_PULPIT_ENABLED", "LOVEGW_PULPIT_REPLY_PROBABILITY",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "не число")
			if _, err := Load(writeConfig(t, `{}`)); err == nil {
				t.Errorf("%s с мусором должен ломать загрузку", name)
			}
		})
	}
}
