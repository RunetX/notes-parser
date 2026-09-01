package platform

// Вход через бота: ключ рождается в ЛИЧНОМ разговоре, а не на экране.
//
// Это тот же довод, которым когда-то держался канал «код в личку НГС»:
// сообщение лежит в ящике, читать который может только владелец, — поэтому
// второй половины доказательства не нужно. Здесь ящик наш собственный: человек
// уже вошёл в РюмкинЪ логином и паролем НГС, бот держит его живую сессию, и
// «этот собеседник владеет анкетой N» доказано ДО того, как зашла речь о
// площадке.
//
// Направление важно и выбрано не для удобства. Ключ идёт БОТ → ЧЕЛОВЕК →
// ПЛОЩАДКА, а не наоборот: пойди он с нашей страницы в бота, злоумышленник мог
// бы подсунуть жертве свою ссылку, жертва подтвердила бы её своим ботом — и
// вошёл бы он, под её именем. При нашем направлении подсунуть нечего: ключ
// рождается в личном чате того, кто его получит, и годен ровно один раз.
//
// Пароль НГС на форме площадки при этом по-прежнему не спрашивается и не будет
// (см. шапку auth.go): вводить пароль сайта на чужом домене — привычка, из-за
// которой одна подделка адреса собирает пароли всего сообщества. Бот — не чужой
// домен, а тот самый бот, которому пароль уже отдан осознанно и однажды.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// BotLoginTTL — сколько живёт ссылка. Десять минут: человек получает её в
	// мессенджере и переходит сразу, а долгоживущий ключ в истории браузера и в
	// логах прокси — это ровно то, чего у одноразовой ссылки быть не должно.
	BotLoginTTL = 10 * time.Minute
	// botKeyBytes — длина ключа. Он НЕ для рук: его не диктуют и не переписывают,
	// а нажимают, — поэтому алфавит и длина взяты не как у T3H-кода (40 бит под
	// человеческий ввод), а как у сессионного токена.
	botKeyBytes = 32
)

// MethodBotDeeplink — как человек доказал право на анкету. Значение стоит в
// комментарии схемы с первого дня (миграция 0001), потому что путь был задуман
// сразу, а сделан 01.09.2026 — когда умер канал лички и площадка осталась вовсе
// без быстрого входа.
const MethodBotDeeplink = "bot_deeplink"

// ErrBotKeyInvalid — ключ не найден, истёк или уже использован. Все три случая
// человеку значат одно и то же: «попросите новую ссылку», — и разводить их в
// ответе значило бы рассказывать постороннему, угадал он ключ или нет.
var ErrBotKeyInvalid = errors.New("ссылка входа недействительна")

// StartBotLogin заводит одноразовый ключ входа для анкеты, право на которую
// ВЫЗЫВАЮЩИЙ уже доказал. Ядро это право не проверяет и проверить не может:
// доказательством служит живая сессия НГС в боте, а она лежит в другой базе и
// другого процесса. Отсюда правило: звать эту функцию можно только оттуда, где
// сессия только что прочитана.
//
// Прежние ключи этого человека гасятся: попросил новую ссылку — старая должна
// умереть, иначе забытое в чужой переписке письмо остаётся годным.
func (p *Platform) StartBotLogin(ctx context.Context, userID int64, messenger string, messengerUserID int64) (string, time.Time, error) {
	if userID <= 0 {
		return "", time.Time{}, fmt.Errorf("вход через бота: не назван участник")
	}
	raw := make([]byte, botKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("вход через бота: %w", err)
	}
	key := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(BotLoginTTL)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("вход через бота: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	if _, err := tx.Exec(ctx,
		`DELETE FROM login_nonces WHERE user_id = $1`, userID); err != nil {
		return "", time.Time{}, fmt.Errorf("вход через бота: %w", err)
	}
	// confirmed_at заполняется СРАЗУ: подтверждением служит сам факт, что ключ
	// родился в личном разговоре с владельцем анкеты. Отдельного шага «а это
	// точно вы?» здесь нет и не нужно — он был бы нужен при обратном
	// направлении, когда ссылку показывают на экране.
	if _, err := tx.Exec(ctx, `
		INSERT INTO login_nonces (nonce_sha, expires_at, messenger, messenger_user_id, user_id, confirmed_at)
		VALUES ($1, $2, $3, $4, $5, now())`,
		botKeyDigest(key), expires, messenger, messengerUserID, userID); err != nil {
		return "", time.Time{}, fmt.Errorf("вход через бота: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", time.Time{}, fmt.Errorf("вход через бота: %w", err)
	}
	return key, expires, nil
}

// RedeemBotLogin гасит ключ и говорит, кого впускать.
//
// Гасится он УДАЛЕНИЕМ строки, а не отметкой: одноразовость тогда держит сама
// база (строки нет — ключа нет), и её нельзя обойти гонкой двух запросов —
// DELETE ... RETURNING достаётся ровно одному.
func (p *Platform) RedeemBotLogin(ctx context.Context, key string) (int64, error) {
	if key == "" {
		return 0, ErrBotKeyInvalid
	}
	var userID *int64
	err := p.pool.QueryRow(ctx, `
		DELETE FROM login_nonces
		 WHERE nonce_sha = $1 AND expires_at > now() AND confirmed_at IS NOT NULL
		 RETURNING user_id`, botKeyDigest(key)).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrBotKeyInvalid
	}
	if err != nil {
		return 0, fmt.Errorf("вход через бота: %w", err)
	}
	if userID == nil {
		return 0, ErrBotKeyInvalid
	}
	return *userID, nil
}

func botKeyDigest(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// CompleteBotLogin впускает того, чей ключ только что погашен.
//
// От CompleteNGSLogin отличается тем, чего здесь НЕ делается: ник и аватар не
// трогаются вовсе. Свежесть их — забота той стороны, где живёт сессия НГС: бот
// зовёт EnsureShadow перед выдачей ключа, и там же latest-wins. Переписывать их
// второй раз отсюда значило бы завести второе место, где решается, чей ник
// главнее, — а правило «ник, выбранный у нас, сильнее ника с сайта» должно
// жить в одном месте.
func (p *Platform) CompleteBotLogin(ctx context.Context, userID int64) (int64, error) {
	if !IsNGS(userID) {
		return 0, fmt.Errorf("номер анкеты %d вне полосы НГС", userID)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("вход анкеты %d: %w", userID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	var anonymized *time.Time
	err = tx.QueryRow(ctx, `SELECT anonymized_at FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&anonymized)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Тень заводит бот перед выдачей ключа. Её отсутствие здесь означает не
		// «новый человек», а рассогласование двух баз, и молча заводить строку
		// на этом месте нельзя: ника у нас нет, вышел бы участник без имени.
		return 0, ErrBotKeyInvalid
	case err != nil:
		return 0, fmt.Errorf("вход анкеты %d: %w", userID, err)
	case anonymized != nil:
		return 0, ErrAnonymized
	}
	if _, err := tx.Exec(ctx,
		`UPDATE users SET kind = $2 WHERE id = $1`, userID, KindMember); err != nil {
		return 0, fmt.Errorf("вход анкеты %d: %w", userID, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO identities (kind, external_id, user_id, method)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (kind, external_id) DO UPDATE SET verified_at = now()`,
		IdentityNGS, subjectOf(userID), userID, MethodBotDeeplink); err != nil {
		return 0, fmt.Errorf("вход анкеты %d: %w", userID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("вход анкеты %d: %w", userID, err)
	}
	return userID, nil
}
