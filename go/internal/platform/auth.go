package platform

// Вход.
//
// Человек называет номер анкеты НГС, получает одноразовый код и возвращает нам
// доказательство, что анкета его. Ни пароля, ни бота, ни сотрудничества НГС для
// этого не нужно, и это работает у забаненного в «Заметках»: запрет писать
// ничего не убирает с площадки, анкета жива.
//
// Главное свойство пути: id строки users РАВЕН id анкеты, а тень заведена
// зеркалом заранее, поэтому вход не переносит ни одной строки — он меняет kind
// у уже существующего ряда, и весь след прошлых лет мгновенно становится своим.
//
// КАНАЛ ДОСТАВКИ КОДА ОДИН — ПОЛЕ «О СЕБЕ» (ChallengeProfile), и проверка у
// него ДВУСТОРОННЯЯ. Это не перестраховка: пока код висит в «о себе», его видит
// кто угодно — анкета публичная, мы сами читаем её анонимно. Значит «в анкете
// лежит наш код» доказывает контроль над анкетой, но НЕ то, что проверку
// запросил её владелец. Второй половиной служит кука, выданная вместе с кодом.
// Нужны обе, иначе посторонний, заметивший чужой код, входил бы одним нажатием.
//
// Отсюда правило, которое нельзя нарушать при правках: код, ПОКАЗАННЫЙ на
// экране, нельзя принимать введённым обратно. Иначе вход под чужой анкетой
// стоит одного нажатия — запросил код на чужой номер, увидел его у себя,
// переписал в поле. Держится правило теперь СТРУКТУРНО: поля ввода кода нет ни
// на одном экране вовсе, а сверка берёт код из куки и ищет его в тексте анкеты.
//
// ВТОРОГО КАНАЛА БОЛЬШЕ НЕТ. До 01.09.2026 основным был код ЛИЧНЫМ СООБЩЕНИЕМ
// на НГС (вид челленджа `ngs_talks`): читать сообщение мог только владелец
// ящика, поэтому достаточно было назвать код обратно, и модерации личка не
// проходила. Служебный аккаунт, от которого уходили эти письма, УДАЛЁН
// (владелец, 01.09.2026), и завести новый — это снова живая сессия на чужом
// сайте, который и комментариев-то не принимает с 17.08.2026.
//
// Убран путь ЦЕЛИКОМ, а не оставлен выключенным, и это главное решение здесь:
// канал с ОДНОСТОРОННЕЙ проверкой, дремлющий рядом с двусторонним, — ровно та
// развилка, на которой однажды примут показанный код введённым обратно. Мёртвый
// код такой развилки не сторожит; отсутствующий — сторожит сам собой.
//
// Цена названа честно: правка поля «о себе» уходит на МОДЕРАЦИЮ НГС, одобряют
// её не сразу и не наверняка (владелец, 18.08.2026), — то есть единственный
// оставшийся код медленный, и именно поэтому личку когда-то и завели. Кому он
// не годится, остаётся приглашение (`/login/invite`); оно же остаётся на случай
// снесённой анкеты и закрывшегося сайта.
//
// Пароль НГС на нашей форме отвергнут навсегда: единственная его выгода — кука
// для обратной публикации, которой в MVP нет, а цена — приучить сообщество
// вводить пароль сайта на стороннем домене. После этого любая подделка нашего
// адреса собирает пароли всех разом.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Ошибки входа. Все они — рабочие ответы человеку, а не сбои: их показывают на
// странице словами, поэтому и заведены отдельно от общей ErrNotFound.
var (
	// ErrNoChallenge — код не выдавался, истёк или был заменён новым.
	ErrNoChallenge = errors.New("код входа не выдавался или истёк")
	// ErrCodeNotFound — кода нет в поле «о себе».
	ErrCodeNotFound = errors.New("кода нет в анкете")
	// ErrCodeMismatch — введён не тот код. Отдельно от ErrNoChallenge: «вы
	// ошиблись в коде» и «код истёк» требуют от человека разного.
	ErrCodeMismatch = errors.New("код не совпадает")
	// ErrTooManyAttempts — слишком много проверок одного кода.
	ErrTooManyAttempts = errors.New("слишком много попыток")
	// ErrAnonymized — владелец анкеты потребовал обезличивания; вернуть ему ник
	// автоматическим входом значило бы отменить исполненное требование.
	ErrAnonymized = errors.New("данные этой анкеты обезличены по требованию владельца")
	// ErrInviteInvalid — приглашение не найдено, использовано или истекло.
	ErrInviteInvalid = errors.New("приглашение недействительно")
)

const (
	// ChallengeTTL — сколько живёт код. Час: человеку нужно открыть НГС,
	// отредактировать анкету и вернуться, а вечная строка в чужом «о себе» —
	// мусор, который потом объясняй.
	ChallengeTTL = time.Hour
	// challengeMaxAttempts — потолок проверок одного кода. Ограничивает не
	// подбор (40 бит по сети не подбирают), а долбёж по НГС: каждая проверка —
	// это наш запрос к сайту, и его темп мы бережём.
	challengeMaxAttempts = 20

	// SessionTTL — срок жизни сессии. Три месяца: площадка для сообщества, где
	// заходят не каждый день, а перелогин руками — это ещё один визит на НГС за
	// кодом.
	SessionTTL = 90 * 24 * time.Hour
)

// Формат кода: T3H-7K3M-Q2XZ. Алфавит без 0/O/1/I/L — код диктуют и переписывают
// руками; ~40 бит энтропии при этом остаются.
const (
	codePrefix   = "T3H"
	codeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	codeGroups   = 2
	codeGroupLen = 4
)

// IdentityNGS и прочие виды подтверждений — значения колонки identities.kind.
const (
	IdentityNGS    = "ngs_profile"
	IdentityInvite = "invite"

	MethodProfileCode = "profile_code"
	MethodInvite      = "admin_invite"
)

// Вид челленджа — значение auth_challenges.kind. Вид определяет СПОСОБ
// ПРОВЕРКИ, а не только канал доставки (см. шапку файла), и потому он остаётся
// колонкой, хотя вид сейчас ровно один: заведись второй, он обязан отличаться
// здесь, а не порядком аргументов.
const (
	// ChallengeProfile — код в поле «о себе». Значение совпадает с IdentityNGS
	// по историческим причинам: строки этого вида лежат в базе с первого дня.
	ChallengeProfile = IdentityNGS
)

// Challenge — выданный код и его срок. Код возвращается ОДИН раз, в базе лежит
// только sha256: показать его повторно нельзя, и это осознанно — иначе строка
// «покажите мой код ещё раз» становится точкой, где чужой код отдают чужому.
type Challenge struct {
	Code      string
	ExpiresAt time.Time
}

// StartProfileChallenge выдаёт новый код для анкеты, заменяя прежний живой.
//
// Замена, а не переиспользование: plaintext прошлого кода мы не храним, значит
// показать его снова нечем. Побочный эффект честный — начатая кем-то другим
// проверка этой же анкеты обесценивается, и максимум, чего добьётся такой
// шутник, это «попробуйте ещё раз».
func (p *Platform) StartProfileChallenge(ctx context.Context, profileID int64) (Challenge, error) {
	if !IsNGS(profileID) {
		return Challenge{}, fmt.Errorf("номер анкеты %d вне полосы НГС", profileID)
	}
	code, err := newCode()
	if err != nil {
		return Challenge{}, err
	}
	sum := codeDigest(code)
	expires := time.Now().Add(ChallengeTTL)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Challenge{}, fmt.Errorf("выдача кода анкете %d: %w", profileID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	// Уникальный индекс держит «живой челлендж на анкету ровно один», поэтому
	// прежний снимается явно, а не молчаливым ON CONFLICT: удаление здесь —
	// смысл операции, а не разрешение конфликта.
	if _, err := tx.Exec(ctx,
		`DELETE FROM auth_challenges WHERE kind = $1 AND subject = $2 AND verified_at IS NULL`,
		ChallengeProfile, subjectOf(profileID)); err != nil {
		return Challenge{}, fmt.Errorf("выдача кода анкете %d: %w", profileID, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO auth_challenges (kind, subject, code_sha, expires_at)
		VALUES ($1, $2, $3, $4)`,
		ChallengeProfile, subjectOf(profileID), sum, expires); err != nil {
		return Challenge{}, fmt.Errorf("выдача кода анкете %d: %w", profileID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Challenge{}, fmt.Errorf("выдача кода анкете %d: %w", profileID, err)
	}
	return Challenge{Code: code, ExpiresAt: expires}, nil
}

// VerifyProfileChallenge сверяет живой код анкеты с тем, что нашлось в «о себе».
//
// code — код из куки того, кто нажал «Проверить»; aboutMe — текст поля с сайта.
// Совпасть должны ОБА: кука доказывает «челлендж начал я», анкета — «анкета
// моя». Про необходимость обеих половин см. шапку файла.
func (p *Platform) VerifyProfileChallenge(ctx context.Context, profileID int64, code, aboutMe string) error {
	if code == "" {
		return ErrNoChallenge
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("проверка кода анкеты %d: %w", profileID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	var id int64
	var stored []byte
	var attempts int16
	err = tx.QueryRow(ctx, `
		SELECT id, code_sha, attempts FROM auth_challenges
		 WHERE kind = $1 AND subject = $2 AND verified_at IS NULL AND expires_at > now()
		 FOR UPDATE`, ChallengeProfile, subjectOf(profileID)).Scan(&id, &stored, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoChallenge
	}
	if err != nil {
		return fmt.Errorf("проверка кода анкеты %d: %w", profileID, err)
	}
	if attempts >= challengeMaxAttempts {
		return ErrTooManyAttempts
	}
	if _, err := tx.Exec(ctx,
		`UPDATE auth_challenges SET attempts = attempts + 1 WHERE id = $1`, id); err != nil {
		return fmt.Errorf("проверка кода анкеты %d: %w", profileID, err)
	}
	// Попытка засчитывается в любом случае, поэтому коммит идёт и на отказе:
	// иначе счётчик откатывался бы вместе с неудачей и не считал бы ничего.
	fail := func(e error) error {
		if cerr := tx.Commit(ctx); cerr != nil {
			return fmt.Errorf("проверка кода анкеты %d: %w", profileID, cerr)
		}
		return e
	}
	if subtle.ConstantTimeCompare(codeDigest(code), stored) != 1 {
		return fail(ErrNoChallenge)
	}
	if !containsCode(aboutMe, code) {
		return fail(ErrCodeNotFound)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE auth_challenges SET verified_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("проверка кода анкеты %d: %w", profileID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("проверка кода анкеты %d: %w", profileID, err)
	}
	return nil
}

// ProfileVerified — код этой анкеты уже сверен и предъявленная кука ему
// соответствует. Спрашивается на шаге согласий: между проверкой кода и
// созданием аккаунта человек читает два документа, и всё это время его
// «доказано» должно чем-то держаться.
func (p *Platform) ProfileVerified(ctx context.Context, profileID int64, code string) (bool, error) {
	if code == "" {
		return false, nil
	}
	var stored []byte
	err := p.pool.QueryRow(ctx, `
		SELECT code_sha FROM auth_challenges
		 WHERE kind = $1 AND subject = $2 AND verified_at IS NOT NULL
		 ORDER BY verified_at DESC LIMIT 1`, ChallengeProfile, subjectOf(profileID)).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("состояние кода анкеты %d: %w", profileID, err)
	}
	return subtle.ConstantTimeCompare(codeDigest(code), stored) == 1, nil
}

// CompleteNGSLogin превращает доказанную анкету в участника. Идемпотентна:
// повторный вход тем же кодом ничего не ломает.
//
// Заводится всё в одной транзакции, и порядок важен: строка users могла уже
// существовать тенью (зеркало видело этого человека годами), поэтому вход — это
// UPDATE kind, а не INSERT, и его прошлые реплики становятся своими сами собой.
func (p *Platform) CompleteNGSLogin(ctx context.Context, prof MirroredAuthor, gender Gender) (int64, error) {
	if !IsNGS(prof.ID) {
		return 0, fmt.Errorf("номер анкеты %d вне полосы НГС", prof.ID)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("вход анкеты %d: %w", prof.ID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	var anonymized *time.Time
	err = tx.QueryRow(ctx, `SELECT anonymized_at FROM users WHERE id = $1 FOR UPDATE`, prof.ID).Scan(&anonymized)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Анкеты в зеркале нет вовсе: человек с НГС, который у нас не писал.
		if _, err := tx.Exec(ctx, `
			INSERT INTO users (id, nick, ngs_avatar_url, kind, gender)
			VALUES ($1, $2, $3, $4, $5)`,
			prof.ID, prof.Nick, prof.AvatarURL, KindMember, gender); err != nil {
			return 0, fmt.Errorf("вход анкеты %d: %w", prof.ID, err)
		}
	case err != nil:
		return 0, fmt.Errorf("вход анкеты %d: %w", prof.ID, err)
	case anonymized != nil:
		return 0, ErrAnonymized
	default:
		// Ник и аватар обновляем: человек только что доказал, что анкета его, и
		// показывать ему прошлогодний ник незачем. Пол — только если известен.
		//
		// Но НЕ ник, выбранный у нас (nick_custom): текст согласия обещает «ник вы
		// меняете сами», и без этой оговорки следующий же вход отменял бы выбор
		// человека молча.
		if _, err := tx.Exec(ctx, `
			UPDATE users
			   SET kind = $2,
			       nick = CASE WHEN nick_custom THEN nick ELSE $3 END,
			       ngs_avatar_url = $4,
			       gender = CASE WHEN $5::smallint = 0 THEN gender ELSE $5 END
			 WHERE id = $1`,
			prof.ID, KindMember, prof.Nick, prof.AvatarURL, gender); err != nil {
			return 0, fmt.Errorf("вход анкеты %d: %w", prof.ID, err)
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO identities (kind, external_id, user_id, method)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (kind, external_id) DO UPDATE SET verified_at = now()`,
		IdentityNGS, subjectOf(prof.ID), prof.ID, MethodProfileCode); err != nil {
		return 0, fmt.Errorf("вход анкеты %d: %w", prof.ID, err)
	}
	// Использованный челлендж убираем: код больше не нужен, а строка в чужом
	// «о себе» и так просится на удаление — незачем держать её ещё и у себя.
	if _, err := tx.Exec(ctx,
		`DELETE FROM auth_challenges WHERE kind = $1 AND subject = $2`,
		IdentityNGS, subjectOf(prof.ID)); err != nil {
		return 0, fmt.Errorf("вход анкеты %d: %w", prof.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("вход анкеты %d: %w", prof.ID, err)
	}
	return prof.ID, nil
}

// AbortLogin откатывает незавершённый вход: человек дошёл до экрана согласий и
// отказался. Возвращает всё к тому, что было до входа, — тень зеркала без
// подтверждённой личности и без сессий.
//
// Отказ обязан откатывать, а не просто «не пускать дальше»: иначе в users
// копились бы участники, которые ни на что не соглашались, и объяснить, на
// каком основании они там числятся, было бы нечем.
//
// Предохранитель — «согласий нет ни одного»: если человек уже подписал хотя бы
// один документ, это больше не незавершённый вход, а действующий участник, и
// сносить его личность отказом на втором экране нельзя.
func (p *Platform) AbortLogin(ctx context.Context, userID int64) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("откат входа %d: %w", userID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	var granted int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM consents WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&granted); err != nil {
		return fmt.Errorf("откат входа %d: %w", userID, err)
	}
	if granted > 0 {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE users SET kind = $2 WHERE id = $1 AND kind = $3`,
		userID, KindShadow, KindMember); err != nil {
		return fmt.Errorf("откат входа %d: %w", userID, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM identities WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("откат входа %d: %w", userID, err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE web_sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`,
		userID); err != nil {
		return fmt.Errorf("откат входа %d: %w", userID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("откат входа %d: %w", userID, err)
	}
	return nil
}

// ---------------------------------------------------------------- приглашения

// CreateInvite заводит одноразовое приглашение и возвращает его код (один раз:
// в базе лежит только sha256).
//
// Инвайт — третий путь и единственный, переживающий смерть НГС. bindUser
// привязывает пришедшего к УЖЕ СУЩЕСТВУЮЩЕЙ тени: если админ знает, кто перед
// ним, весь зеркальный след этого человека становится его — включая подписи
// «Ник, » в чужих ответах, которые рисуются из текущего ника.
//
// Это путь КОНСОЛИ, и он остаётся ради одного случая — первого приглашения на
// площадке, где администратора ещё нет вовсе (тогда выдающим записывается сам
// приглашённый). Со страницы выдаёт IssueInvite: там есть живой актор, и право
// проверяется по нему. Обе дороги сходятся на createInvite, поэтому в журнал
// попадают обе.
func (p *Platform) CreateInvite(ctx context.Context, issuedBy, bindUser int64, note string, ttl time.Duration) (string, error) {
	// Актор нулевой: из консоли действие совершается БЕЗ вошедшего человека, и
	// журнал показывает это честно — ровно как первое назначение администратора.
	return p.createInvite(ctx, 0, issuedBy, bindUser, note, ttl)
}

// RedeemInvite обменивает код на участника. nick используется только когда
// приглашение ни к кому не привязано и человека приходится заводить с нуля.
func (p *Platform) RedeemInvite(ctx context.Context, code, nick string) (int64, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("приглашение: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	sum := codeDigest(normalizeCode(code))
	var bind *int64
	err = tx.QueryRow(ctx, `
		SELECT bind_user FROM invites
		 WHERE code_sha = $1 AND used_at IS NULL AND expires_at > now()
		 FOR UPDATE`, sum).Scan(&bind)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrInviteInvalid
	}
	if err != nil {
		return 0, fmt.Errorf("приглашение: %w", err)
	}

	userID := idOf(bind)
	if userID == 0 {
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (id, nick, kind)
			VALUES (nextval('users_native_seq'), $1, $2) RETURNING id`,
			nick, KindMember).Scan(&userID); err != nil {
			return 0, fmt.Errorf("приглашение: %w", err)
		}
	} else {
		var anonymized *time.Time
		if err := tx.QueryRow(ctx,
			`SELECT anonymized_at FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&anonymized); err != nil {
			return 0, fmt.Errorf("приглашение: %w", err)
		}
		if anonymized != nil {
			return 0, ErrAnonymized
		}
		if _, err := tx.Exec(ctx,
			`UPDATE users SET kind = $2 WHERE id = $1`, userID, KindMember); err != nil {
			return 0, fmt.Errorf("приглашение: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO identities (kind, external_id, user_id, method)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (kind, external_id) DO NOTHING`,
		IdentityInvite, hex.EncodeToString(sum), userID, MethodInvite); err != nil {
		return 0, fmt.Errorf("приглашение: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE invites SET used_at = now(), used_by = $2 WHERE code_sha = $1`, sum, userID); err != nil {
		return 0, fmt.Errorf("приглашение: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("приглашение: %w", err)
	}
	return userID, nil
}

// ------------------------------------------------- приглашения администратора

// Выдаются приглашения СО СТРАНИЦЫ, а не только из консоли (24.08.2026, просьба
// владельца). Довод тот же, по которому в морде вообще появилась модерация:
// впустить человека администратор должен уметь, не заходя на боевой хост, —
// лишний повод открыть там консоль дороже любой ошибки в форме, тем более что
// ошибку эту можно отозвать.
//
// Что при этом НЕ переехало: код по-прежнему показывается ОДИН раз, и в базе
// лежит только его sha256. «Покажите ещё раз» страница не умеет и уметь не
// должна — потерянное приглашение отзывается и выдаётся заново.
const (
	// InviteTTL — сколько живёт приглашение по умолчанию. Месяц: код диктуют в
	// переписке, а переезжает человек не в тот же вечер.
	InviteTTL = 30 * 24 * time.Hour
	// MaxInviteTTL — потолок срока. Год живущий код — это уже не приглашение, а
	// забытый в чужой переписке ключ от учётной записи.
	MaxInviteTTL = 365 * 24 * time.Hour
	// InviteListLimit — сколько строк отдаём списком по умолчанию.
	InviteListLimit = 50
)

// Invite — выданное приглашение, каким его видит администратор. САМОГО КОДА
// здесь нет и быть не может: в базе лежит его sha256, а список отвечает на
// вопрос «кому и когда выдано», а не «какие коды ходят по рукам».
type Invite struct {
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
	Note      string

	IssuedBy   int64
	IssuerNick string

	// BindUser — к кому привязано; 0 означает «ни к кому», и тогда пришедший
	// заводится с нуля в нативной полосе. BindKind нужен показу: привязка к
	// УЧАСТНИКУ (а не к тени) отдаёт доступ к учётной записи, в которую кто-то
	// уже входил, и предупредить об этом обязана страница, а не память.
	BindUser int64
	BindNick string
	BindKind Kind

	UsedBy   int64
	UsedNick string
}

// Live — приглашением ещё можно воспользоваться.
func (i Invite) Live(now time.Time) bool { return i.UsedAt == nil && i.ExpiresAt.After(now) }

// IssueInvite — выдача приглашения тем, кто вошёл администратором.
//
// От CreateInvite отличается тремя вещами, и все три — следствие того, что
// действие совершает ЖИВОЙ человек со страницы: право проверяется здесь, в
// ядре (форма — не место для этой проверки: список того, что позволено, обязан
// читаться в одном месте), выдающим записывается он сам, а не приглашённый, и
// срок ограничен потолком.
func (p *Platform) IssueInvite(ctx context.Context, actor Viewer, bindUser int64, note string, ttl time.Duration) (string, error) {
	if !actor.CanAdmin() {
		return "", ErrNotAdmin
	}
	switch {
	case ttl <= 0:
		ttl = InviteTTL
	case ttl > MaxInviteTTL:
		ttl = MaxInviteTTL
	}
	return p.createInvite(ctx, actor.UserID, actor.UserID, bindUser, note, ttl)
}

// createInvite — общая часть обоих путей: строка приглашения и запись в журнал
// ОДНОЙ транзакцией. Журнал обязателен: «кого впустили» — такая же часть
// модерации, как «кого скрыли», и выясняться через месяц перепиской это не
// должно. Кода в журнале нет — его читают модераторы, а код это ключ.
func (p *Platform) createInvite(ctx context.Context, actor, issuedBy, bindUser int64, note string, ttl time.Duration) (string, error) {
	code, err := newCode()
	if err != nil {
		return "", err
	}
	note = trimReason(note)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("выдача приглашения: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	if bindUser != 0 {
		// Привязка — самая дорогая ошибка этой формы: пришедший получает ЧУЖОЙ
		// след целиком. Поэтому человек проверяется здесь, а не отдаётся на
		// откуп внешнему ключу: «такого участника нет» обязано прозвучать при
		// выдаче, а не при попытке войти по уже разосланному коду.
		var anonymized *time.Time
		err := tx.QueryRow(ctx,
			`SELECT anonymized_at FROM users WHERE id = $1`, bindUser).Scan(&anonymized)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("участник %d: %w", bindUser, ErrNotFound)
		}
		if err != nil {
			return "", fmt.Errorf("выдача приглашения: %w", err)
		}
		if anonymized != nil {
			return "", ErrAnonymized
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO invites (code_sha, issued_by, bind_user, note, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		codeDigest(code), issuedBy, nullID(bindUser), note, time.Now().Add(ttl)); err != nil {
		return "", fmt.Errorf("выдача приглашения: %w", err)
	}
	if err := audit(ctx, tx, actor, ActionInvite, Subject{Kind: SubjectInvite, ID: bindUser},
		map[string]any{"days": int(ttl / (24 * time.Hour)), "reason": note}); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("выдача приглашения: %w", err)
	}
	return code, nil
}

// Invites — выданные приглашения, свежие первыми. Показываются ВСЕ, включая
// использованные и истёкшие: страница отвечает на вопрос «кому я уже выдавал»,
// а он про прошлое.
func (p *Platform) Invites(ctx context.Context, limit int) ([]Invite, error) {
	if limit <= 0 {
		limit = InviteListLimit
	}
	rows, err := p.pool.Query(ctx, `
		SELECT i.created_at, i.expires_at, i.used_at, i.note,
		       i.issued_by, coalesce(iu.nick, ''),
		       i.bind_user, coalesce(bu.nick, ''), coalesce(bu.kind, 0),
		       i.used_by, coalesce(su.nick, '')
		  FROM invites i
		  LEFT JOIN users iu ON iu.id = i.issued_by
		  LEFT JOIN users bu ON bu.id = i.bind_user
		  LEFT JOIN users su ON su.id = i.used_by
		 ORDER BY i.created_at DESC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("приглашения: %w", err)
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var (
			in         Invite
			bind, used *int64
		)
		if err := rows.Scan(&in.CreatedAt, &in.ExpiresAt, &in.UsedAt, &in.Note,
			&in.IssuedBy, &in.IssuerNick,
			&bind, &in.BindNick, &in.BindKind,
			&used, &in.UsedNick); err != nil {
			return nil, fmt.Errorf("приглашения: %w", err)
		}
		in.BindUser, in.UsedBy = idOf(bind), idOf(used)
		out = append(out, in)
	}
	return out, rows.Err()
}

// RevokeInvite гасит ещё не использованное приглашение — «выдал не тому» и «код
// уехал не в ту переписку». Строка при этом остаётся: выдача уже случилась, и
// стирать её значило бы править историю.
//
// Ключ строки — ВРЕМЯ ВЫДАЧИ, и это не лень. Своего идентификатора у
// приглашения нет вовсе (первичный ключ — хеш кода), а показать хеш нельзя:
// кодов в алфавите 31^8, и по хешу код подбирается перебором. Время выдачи на
// странице и так напечатано, совпасть до микросекунды двум строкам практически
// неоткуда — вставки идут по одной, — а на случай совпадения гасится РОВНО
// одна.
func (p *Platform) RevokeInvite(ctx context.Context, actor Viewer, issuedAt time.Time) error {
	if !actor.CanAdmin() {
		return ErrNotAdmin
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("отзыв приглашения: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	var bind *int64
	err = tx.QueryRow(ctx, `
		UPDATE invites SET expires_at = now()
		 WHERE code_sha = (SELECT code_sha FROM invites
		                    WHERE created_at = $1 AND used_at IS NULL AND expires_at > now()
		                    ORDER BY code_sha LIMIT 1)
	 RETURNING bind_user`, issuedAt).Scan(&bind)
	if errors.Is(err, pgx.ErrNoRows) {
		// Использованное или уже погасшее приглашение — не ошибка: два
		// администратора, нажавших одно и то же, не должны видеть отказ.
		return ErrNothingToDo
	}
	if err != nil {
		return fmt.Errorf("отзыв приглашения: %w", err)
	}
	if err := audit(ctx, tx, actor.UserID, ActionInviteOff,
		Subject{Kind: SubjectInvite, ID: idOf(bind)}, nil); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("отзыв приглашения: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- сессии

// CreateSession выдаёт непрозрачный токен сессии; в базе только его sha256.
//
// Не JWT намеренно: бан и «выйти со всех устройств» обязаны действовать
// мгновенно, а отзыв JWT требует ровно того обращения к базе, ради избавления
// от которого JWT и заводят.
func (p *Platform) CreateSession(ctx context.Context, userID int64, ua string) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("сессия: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(SessionTTL)
	sum := sha256.Sum256([]byte(token))
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO web_sessions (user_id, token_sha, expires_at, ua)
		VALUES ($1, $2, $3, $4)`, userID, sum[:], expires, trimUA(ua)); err != nil {
		return "", time.Time{}, fmt.Errorf("сессия: %w", err)
	}
	return token, expires, nil
}

// SessionUser отдаёт хозяина живой сессии. ErrNotFound — сессии нет, истекла или
// отозвана; для морды это просто «гость».
func (p *Platform) SessionUser(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, ErrNotFound
	}
	sum := sha256.Sum256([]byte(token))
	var lastSeen time.Time
	row := p.pool.QueryRow(ctx, `
		SELECT `+prefixed("u", userColumns)+`, s.last_seen_at
		  FROM web_sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.token_sha = $1 AND s.revoked_at IS NULL AND s.expires_at > now()`, sum[:])
	var u User
	// Получатели — общие с UserByID (userDest), своей копии тут быть не должно:
	// именно она и разошлась со списком колонок в Ш7.
	err := row.Scan(append(userDest(&u), &lastSeen)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("чтение сессии: %w", err)
	}
	// Отметка визита огрублена: без порога строка сессии переписывалась бы на
	// КАЖДЫЙ запрос страницы, то есть на пустом месте пухли бы и WAL, и таблица.
	if time.Since(lastSeen) > time.Hour {
		if _, err := p.pool.Exec(ctx,
			`UPDATE web_sessions SET last_seen_at = now() WHERE token_sha = $1`, sum[:]); err != nil {
			return u, nil // отметка визита не повод отказать во входе
		}
	}
	return u, nil
}

// RevokeSession гасит одну сессию — «выйти здесь».
func (p *Platform) RevokeSession(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(token))
	_, err := p.pool.Exec(ctx,
		`UPDATE web_sessions SET revoked_at = now() WHERE token_sha = $1 AND revoked_at IS NULL`, sum[:])
	return wrapf(err, "выход")
}

// RevokeUserSessions гасит все сессии человека — «выйти со всех устройств», и
// это же исполняется при отзыве согласия и при бане.
func (p *Platform) RevokeUserSessions(ctx context.Context, userID int64) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE web_sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return wrapf(err, "выход со всех устройств %d", userID)
}

// MemberCard — как показать вошедшего: тот же Author, что и под публикациями,
// чтобы подпись человека нигде не собиралась вторым способом.
func (p *Platform) MemberCard(ctx context.Context, id int64) (Author, error) {
	var a Author
	var sha []byte
	var mime *string
	err := p.pool.QueryRow(ctx, `
		SELECT u.id, u.nick, u.avatar_sha, m.mime, u.gender
		  FROM users u LEFT JOIN media m ON m.sha256 = u.avatar_sha
		 WHERE u.id = $1`, id).Scan(&a.ID, &a.Nick, &sha, &mime, &a.Gender)
	if errors.Is(err, pgx.ErrNoRows) {
		return Author{}, ErrNotFound
	}
	if err != nil {
		return Author{}, fmt.Errorf("карточка участника %d: %w", id, err)
	}
	a.AvatarURL = MediaURL(sha, strOf(mime))
	return a, nil
}

// AnyAdmin — любой администратор площадки. Нужен там, где действие обязано
// иметь автора, а совершается оно из командной строки: у приглашения колонка
// issued_by не необязательная, потому что история выдач — часть модерации.
func (p *Platform) AnyAdmin(ctx context.Context) (int64, error) {
	var id int64
	err := p.pool.QueryRow(ctx,
		`SELECT id FROM users WHERE role >= $1 ORDER BY id LIMIT 1`, RoleAdmin).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("администратора на площадке нет: %w", ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("поиск администратора: %w", err)
	}
	return id, nil
}

// SetRole меняет права.
//
// Первого администратора назначить можно только из командной строки — там
// actor нулевой, и журнал честно показывает действие без автора. Дальше роли
// раздаёт админ со страницы участника, и запись в audit_log обязательна: право
// скрывать чужие слова выдаётся человеком человеку, и «кто дал» — часть этого
// права.
func (p *Platform) SetRole(ctx context.Context, actor Viewer, id int64, role Role) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("права пользователя %d: %w", id, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	tag, err := tx.Exec(ctx, `UPDATE users SET role = $2 WHERE id = $1`, id, role)
	if err != nil {
		return fmt.Errorf("права пользователя %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("пользователь %d: %w", id, ErrNotFound)
	}
	if err := audit(ctx, tx, actor.UserID, ActionRole, UserSubject(id),
		map[string]any{"role": int(role)}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("права пользователя %d: %w", id, err)
	}
	return nil
}

// ---------------------------------------------------------------- коды

func newCode() (string, error) {
	var b strings.Builder
	b.WriteString(codePrefix)
	max := big.NewInt(int64(len(codeAlphabet)))
	for g := 0; g < codeGroups; g++ {
		b.WriteByte('-')
		for i := 0; i < codeGroupLen; i++ {
			n, err := rand.Int(rand.Reader, max)
			if err != nil {
				return "", fmt.Errorf("код входа: %w", err)
			}
			b.WriteByte(codeAlphabet[n.Int64()])
		}
	}
	return b.String(), nil
}

func codeDigest(code string) []byte {
	sum := sha256.Sum256([]byte(normalizeCode(code)))
	return sum[:]
}

// homoglyphs — кириллические двойники латинских букв. Код диктуют по телефону и
// набирают, не переключив раскладку; «Т3Н» вместо «T3H» — это не ошибка
// человека, а наша, если мы её не приняли.
var homoglyphs = strings.NewReplacer(
	"А", "A", "В", "B", "Е", "E", "К", "K", "М", "M", "Н", "H",
	"О", "O", "Р", "P", "С", "C", "Т", "T", "У", "Y", "Х", "X",
)

// normalizeCode приводит код к канону: заглавные, кириллические двойники в
// латиницу, всё лишнее (дефисы, пробелы, кавычки) прочь. Сверяются именно
// каноны, поэтому «t3h 7k3m q2xz» и «T3H-7K3M-Q2XZ» — один код.
func normalizeCode(s string) string {
	s = homoglyphs.Replace(strings.ToUpper(strings.TrimSpace(s)))
	var b strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// containsCode ищет код в тексте поля «о себе». Сравнение идёт по канону обеих
// сторон: сайт схлопывает пробелы, подменяет эмодзи картинками и вообще волен
// трогать текст, а человек волен обвести код кавычками.
func containsCode(text, code string) bool {
	want := normalizeCode(code)
	return want != "" && strings.Contains(normalizeCode(text), want)
}

// CodeRe — как код выглядит; нужен морде, чтобы подсказать формат, и тестам.
var CodeRe = regexp.MustCompile(`(?i)` + codePrefix + `-[` + codeAlphabet + `]{4}-[` + codeAlphabet + `]{4}`)

func subjectOf(id int64) string { return fmt.Sprintf("%d", id) }

// prefixed — те же колонки, но с псевдонимом таблицы: список колонок один на
// пакет, а запрос с JOIN требует уточнения.
func prefixed(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, c := range parts {
		parts[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

// trimUA обрезает User-Agent: он пишется в сессию для строки «откуда вы
// заходили» и легко бывает километровым.
func trimUA(ua string) string {
	const max = 200
	if len(ua) > max {
		return ua[:max]
	}
	return ua
}
