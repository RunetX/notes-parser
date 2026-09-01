package platform

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Полный круг: тень → ключ → вход. Ключ одноразовый, и держится это удалением
// строки, а не отметкой рядом с ней — иначе гонка двух запросов впустила бы
// дважды.
func TestВходИзБотаОдноразовый(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	const profile = 1493279

	if _, err := p.EnsureShadow(ctx, MirroredAuthor{ID: profile, Nick: "Рио"}); err != nil {
		t.Fatalf("тень: %v", err)
	}
	key, expires, err := p.StartBotLogin(ctx, profile, "telegram", 777)
	if err != nil {
		t.Fatalf("выдача ключа: %v", err)
	}
	if key == "" || !expires.After(time.Now()) {
		t.Fatalf("ключ %q, срок %v", key, expires)
	}

	got, err := p.RedeemBotLogin(ctx, key)
	if err != nil || got != profile {
		t.Fatalf("погашение: %d, %v", got, err)
	}
	if _, err := p.CompleteBotLogin(ctx, profile); err != nil {
		t.Fatalf("вход: %v", err)
	}
	u, err := p.UserByID(ctx, profile)
	if err != nil {
		t.Fatalf("чтение участника: %v", err)
	}
	if u.Kind != KindMember {
		t.Errorf("тень не стала участником: kind %d", u.Kind)
	}

	// Второй раз тот же ключ не работает.
	if _, err := p.RedeemBotLogin(ctx, key); !errors.Is(err, ErrBotKeyInvalid) {
		t.Errorf("повторное погашение: %v, ожидался ErrBotKeyInvalid", err)
	}
}

// Новая просьба гасит прежний ключ: забытая в переписке ссылка не должна
// оставаться годной, раз человек попросил другую.
func TestНовыйКлючГаситПрежний(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	const profile = 1038894

	if _, err := p.EnsureShadow(ctx, MirroredAuthor{ID: profile, Nick: "Пух"}); err != nil {
		t.Fatalf("тень: %v", err)
	}
	first, _, err := p.StartBotLogin(ctx, profile, "telegram", 1)
	if err != nil {
		t.Fatalf("первый ключ: %v", err)
	}
	second, _, err := p.StartBotLogin(ctx, profile, "telegram", 1)
	if err != nil {
		t.Fatalf("второй ключ: %v", err)
	}
	if first == second {
		t.Fatal("выдан тот же ключ — plaintext мы не храним, значит он новый всегда")
	}
	if _, err := p.RedeemBotLogin(ctx, first); !errors.Is(err, ErrBotKeyInvalid) {
		t.Errorf("прежний ключ пережил выдачу нового: %v", err)
	}
	if _, err := p.RedeemBotLogin(ctx, second); err != nil {
		t.Errorf("свежий ключ не работает: %v", err)
	}
}

// Обезличенного вход не воскрешает: вернуть ему ник автоматически значило бы
// отменить исполненное требование субъекта.
func TestОбезличенногоБотНеВпускает(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	const profile = 1372959

	if _, err := p.EnsureShadow(ctx, MirroredAuthor{ID: profile, Nick: "Полынь"}); err != nil {
		t.Fatalf("тень: %v", err)
	}
	if _, err := p.pool.Exec(ctx,
		`UPDATE users SET anonymized_at = now() WHERE id = $1`, profile); err != nil {
		t.Fatalf("обезличивание: %v", err)
	}
	if _, err := p.CompleteBotLogin(ctx, profile); !errors.Is(err, ErrAnonymized) {
		t.Errorf("вход обезличенного: %v, ожидался ErrAnonymized", err)
	}
}

// Истёкший ключ не годится: срок проверяет сама выборка, а не код рядом с ней.
func TestИстёкшийКлючНеГодится(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	const profile = 498196

	if _, err := p.EnsureShadow(ctx, MirroredAuthor{ID: profile, Nick: "ДВ"}); err != nil {
		t.Fatalf("тень: %v", err)
	}
	key, _, err := p.StartBotLogin(ctx, profile, "max", 42)
	if err != nil {
		t.Fatalf("ключ: %v", err)
	}
	if _, err := p.pool.Exec(ctx,
		`UPDATE login_nonces SET expires_at = now() - interval '1 minute' WHERE user_id = $1`,
		profile); err != nil {
		t.Fatalf("состаривание: %v", err)
	}
	if _, err := p.RedeemBotLogin(ctx, key); !errors.Is(err, ErrBotKeyInvalid) {
		t.Errorf("истёкший ключ впустил: %v", err)
	}
}
