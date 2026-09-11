package platform

// Привязка мессенджера — ВТОРАЯ ПРИРОДА ДОКАЗАТЕЛЬСТВА (11.09.2026).
//
// Дверей на площадку было три, но природы у них две, а не три: и код в поле
// «о себе», и ссылка из бота доказывают право одним и тем же — живой анкетой на
// love.ngs.ru (первая читает её страницу, вторая держится на сессии сайта,
// которую хранит бот). Анкету можно удалить, потерять или лишиться её вместе с
// сайтом — и обе двери закрываются РАЗОМ, оставляя одно приглашение, которое
// выдаёт кто-то другой. Владелец площадки в этом положении уже находится.
//
// Мессенджер такой зависимости не имеет вовсе: бот узнаёт собеседника в личном
// разговоре, и чужой сайт в этом доказательстве не участвует.
//
// НАПРАВЛЕНИЕ ЗДЕСЬ ОБРАТНОЕ ВХОДУ, и это не исключение из правила, а само
// правило, прочитанное точно: ключ рождается там, где сидит тот, кого он
// НАЗНАЧАЕТ. Ключ входа назначает, КОГО впустить, — значит рождается в личном
// разговоре с этим человеком, у бота (см. botlogin.go). Ключ привязки
// назначает, К КАКОЙ УЧЁТНОЙ ЗАПИСИ прицепить мессенджер, — значит обязан
// родиться в живой сессии на площадке.
//
// Разницу видно по тому, чем кончается подсунутая ссылка. При нашем
// направлении злоумышленник может выдать ключ только на СВОЮ запись: жертва,
// клюнув, привяжет свой мессенджер к его учётной записи — неприятно, но
// захвата нет. Родись ключ у бота, он подсунул бы жертве ключ со своим
// messenger_user_id, жертва открыла бы его вошедшей — и его мессенджер стал бы
// вечной отмычкой к её записи. Клюнуть на чужую ссылку люди готовы всегда;
// отдать наружу код со своего экрана — почти никогда.
//
// Подпирается это тем, что подтверждает привязку БОТ и называет ник учётной
// записи: подсунутый код выдаёт себя чужим именем ещё до нажатия.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// BindTTL — сколько живёт код привязки. Десять минут, как у ключа входа:
	// человек держит его на экране и отправляет боту сразу, а долгоживущий
	// ключ от учётной записи в чужой переписке — ровно то, чего не должно быть.
	BindTTL = 10 * time.Minute

	// purposeLogin / purposeBind — значения login_nonces.purpose (миграция
	// 0030). Ключи двух пород живут в одной таблице, но гасят прежние строки
	// каждый по своей породе: иначе просьба «дай ссылку входа» убивала бы
	// начатую привязку, и наоборот.
	purposeLogin = "login"
	purposeBind  = "bind"

	// bindPrefix — код привязки НЕ носит префикса T3H, и это не косметика.
	// Код входа человек вставляет в публичное поле «о себе» на чужом сайте;
	// перепутав их, он опубликовал бы там живой ключ от своей учётной записи.
	// Разные буквы в начале — самая дешёвая защита от этой ошибки.
	bindPrefix = "MSG"

	// MethodBind — как доказано право (identities.method).
	MethodBind = "messenger_bind"
)

// Мессенджеры, которые можно привязать, — значения identities.kind. Список
// ЗАКРЫТ: kind с опечаткой завёл бы строку, которую человек не может ни
// увидеть на «Моей странице», ни отвязать.
const (
	IdentityTelegram = "telegram"
	IdentityMAX      = "max"
)

func knownMessenger(kind string) bool {
	return kind == IdentityTelegram || kind == IdentityMAX
}

var (
	// ErrBindCodeInvalid — код не найден, истёк или уже использован. Три случая
	// человеку значат одно и то же, и разводить их в ответе значило бы
	// рассказывать постороннему, угадал он код или нет.
	ErrBindCodeInvalid = errors.New("код привязки недействителен")
	// ErrMessengerTaken — этот аккаунт мессенджера уже привязан к ДРУГОЙ
	// учётной записи. Молча переписать привязку нельзя: это отняло бы у того
	// человека способ входа, о чём он узнал бы в самый неподходящий момент.
	ErrMessengerTaken = errors.New("этот аккаунт мессенджера уже привязан к другой учётной записи")
	// ErrNoBinding — по этому мессенджеру входить некому.
	ErrNoBinding = errors.New("мессенджер ни к кому не привязан")
	// ErrUnknownMessenger — вид вне закрытого списка.
	ErrUnknownMessenger = errors.New("неизвестный мессенджер")
)

// Binding — привязанный мессенджер, каким его видит хозяин на «Моей странице».
// Самого идентификатора здесь нет: показывать человеку его же номер в телеграме
// незачем, а отвязка адресуется мессенджером — вторую привязку той же породы
// площадка не заводит (см. BindMessenger).
type Binding struct {
	Messenger  string
	VerifiedAt time.Time
}

// StartBinding заводит код привязки для того, кто ВОШЁЛ, и той же транзакцией
// записывает его согласие.
//
// Согласие пишется ЗДЕСЬ, а не в момент, когда бот погасит код, и это не
// случайность: согласие даёт ЧЕЛОВЕК, прочитав текст на экране, а бот за него
// ничего не даёт. Строка появится и у того, кто нажал и передумал, — это
// правильно: подпись была, а обрабатывать по ней просто нечего.
//
// Право нужно ровно одно — быть участником. Запрет публиковать (бан) привязку
// не отнимает: он про слова, а не про дверь, и человек, которому запрещено
// писать, обязан иметь возможность прочитать на «Моей странице», за что и до
// когда.
func (p *Platform) StartBinding(ctx context.Context, userID int64, ua string) (string, time.Time, error) {
	doc, err := ConsentDocOf(Operator{}, ConsentBinding)
	if err != nil {
		return "", time.Time{}, err
	}
	code, err := newCodeWith(bindPrefix)
	if err != nil {
		return "", time.Time{}, err
	}
	expires := time.Now().Add(BindTTL)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("привязка мессенджера: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	if err := bindableGuard(ctx, tx, userID); err != nil {
		return "", time.Time{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO consents (user_id, kind, version, ua) VALUES ($1, $2, $3, $4)`,
		userID, ConsentBinding, doc.Version, trimUA(ua)); err != nil {
		return "", time.Time{}, fmt.Errorf("привязка мессенджера: %w", err)
	}
	// Прежний код этого человека умирает: попросил новый — старый должен
	// перестать годиться, иначе забытый в переписке код остаётся ключом.
	if _, err := tx.Exec(ctx,
		`DELETE FROM login_nonces WHERE user_id = $1 AND purpose = $2`, userID, purposeBind); err != nil {
		return "", time.Time{}, fmt.Errorf("привязка мессенджера: %w", err)
	}
	// confirmed_at пуст: подтверждает привязку БОТ, и до этого момента ключ не
	// годится ни на что. Заодно он не годится и как ключ входа — RedeemBotLogin
	// требует и confirmed_at, и своей породы.
	if _, err := tx.Exec(ctx, `
		INSERT INTO login_nonces (nonce_sha, expires_at, user_id, purpose)
		VALUES ($1, $2, $3, $4)`,
		codeDigest(code), expires, userID, purposeBind); err != nil {
		return "", time.Time{}, fmt.Errorf("привязка мессенджера: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", time.Time{}, fmt.Errorf("привязка мессенджера: %w", err)
	}
	return code, expires, nil
}

// bindableGuard — кому вообще можно привязать мессенджер. Отдельно от
// writeGuard: тот про право ПУБЛИКОВАТЬ, а здесь вопрос про дверь.
func bindableGuard(ctx context.Context, q querier, userID int64) error {
	var (
		kind       Kind
		persona    bool
		anonymized *time.Time
	)
	err := q.QueryRow(ctx,
		`SELECT kind, persona, anonymized_at FROM users WHERE id = $1`, userID).
		Scan(&kind, &persona, &anonymized)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("привязка мессенджера: %w", err)
	}
	switch {
	case anonymized != nil:
		return ErrAnonymized
	// Житель и служебная анкета площадки — не субъекты персональных данных
	// (notASubject), и согласия за них не даёт никто. Привязка без согласия
	// была бы строкой о данных, на которые никто не соглашался.
	case persona, kind != KindMember:
		return ErrNotMember
	}
	return nil
}

// BindingOffer — чью учётную запись назовёт этот код, НЕ гася его. Нужен боту:
// прежде чем привязать, он обязан показать человеку ник и спросить, — иначе
// подсунутый чужой код проходит молча.
func (p *Platform) BindingOffer(ctx context.Context, code string) (int64, string, error) {
	var (
		userID int64
		nick   string
	)
	err := p.pool.QueryRow(ctx, `
		SELECT n.user_id, u.nick
		  FROM login_nonces n JOIN users u ON u.id = n.user_id
		 WHERE n.nonce_sha = $1 AND n.purpose = $2 AND n.expires_at > now()`,
		codeDigest(code), purposeBind).Scan(&userID, &nick)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", ErrBindCodeInvalid
	}
	if err != nil {
		return 0, "", fmt.Errorf("привязка мессенджера: %w", err)
	}
	return userID, nick, nil
}

// BindMessenger гасит код и заводит привязку. Зовётся СО СТОРОНЫ БОТА: только
// там известно, кто собеседник, — и только там это знание чего-то стоит.
//
// Гасится код УДАЛЕНИЕМ строки, как ключ входа: одноразовость держит сама база,
// и обойти её гонкой двух запросов нельзя — DELETE ... RETURNING достаётся
// ровно одному.
func (p *Platform) BindMessenger(ctx context.Context, code, messenger string, messengerUserID int64) (int64, string, error) {
	if !knownMessenger(messenger) {
		return 0, "", ErrUnknownMessenger
	}
	if messengerUserID <= 0 {
		return 0, "", fmt.Errorf("привязка мессенджера: не назван собеседник")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("привязка мессенджера: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	var userID int64
	err = tx.QueryRow(ctx, `
		DELETE FROM login_nonces
		 WHERE nonce_sha = $1 AND purpose = $2 AND expires_at > now()
		 RETURNING user_id`, codeDigest(code), purposeBind).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", ErrBindCodeInvalid
	}
	if err != nil {
		return 0, "", fmt.Errorf("привязка мессенджера: %w", err)
	}
	if err := bindableGuard(ctx, tx, userID); err != nil {
		return 0, "", err
	}
	// СОГЛАСИЕ СВЕРЯЕТСЯ ЗДЕСЬ, хотя записано оно было при выдаче кода: между
	// этими двумя минутами человек мог нажать «Отозвать», и строка identities,
	// появившаяся после отзыва, была бы обработкой без основания.
	ok, err := hasLiveConsent(ctx, tx, userID, ConsentBinding)
	if err != nil {
		return 0, "", err
	}
	if !ok {
		return 0, "", ErrBindCodeInvalid
	}
	external := strconv.FormatInt(messengerUserID, 10)
	// Чужую привязку не переписываем МОЛЧА: у того человека это способ входа, и
	// отнимать его тихо нельзя. Свою — обновляем отметкой времени: повторная
	// привязка того же аккаунта не ошибка, а «проверю, что работает».
	var owner int64
	err = tx.QueryRow(ctx,
		`SELECT user_id FROM identities WHERE kind = $1 AND external_id = $2 FOR UPDATE`,
		messenger, external).Scan(&owner)
	switch {
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return 0, "", fmt.Errorf("привязка мессенджера: %w", err)
	case err == nil && owner != userID:
		return 0, "", ErrMessengerTaken
	}
	// ВТОРОЙ РУБЕЖ, и он-то и держит гонку. SELECT ... FOR UPDATE выше по
	// ОТСУТСТВУЮЩЕЙ строке не блокирует ничего (gap-локов в Postgres нет),
	// поэтому две привязки одного мессенджера к разным записям проходят проверку
	// обе. Замок берёт сама вставка: условие на DO UPDATE не даёт тронуть чужую
	// строку, и тогда RETURNING не вернёт ничего — это и есть «занято».
	//
	// Без него проигравший получал бы УСПЕХ, не привязав ничего (user_id в
	// DO UPDATE не менялся), да ещё и сносил бы следом собственную прежнюю
	// привязку этой породы — то есть терял вход, думая, что его завёл.
	var bound int64
	err = tx.QueryRow(ctx, `
		INSERT INTO identities (kind, external_id, user_id, method)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (kind, external_id) DO UPDATE SET verified_at = now()
		 WHERE identities.user_id = $3
		RETURNING user_id`,
		messenger, external, userID, MethodBind).Scan(&bound)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", ErrMessengerTaken
	}
	if err != nil {
		return 0, "", fmt.Errorf("привязка мессенджера: %w", err)
	}
	// ОДНА привязка на мессенджер: прежний аккаунт той же породы отцепляется.
	// Иначе у человека копились бы забытые ключи — старый телеграм, из которого
	// он давно вышел, остался бы годным для входа навсегда, а на «Моей
	// странице» отличить их было бы нечем (номера мы не показываем).
	if _, err := tx.Exec(ctx, `
		DELETE FROM identities
		 WHERE kind = $1 AND user_id = $2 AND external_id <> $3`,
		messenger, userID, external); err != nil {
		return 0, "", fmt.Errorf("привязка мессенджера: %w", err)
	}
	var nick string
	if err := tx.QueryRow(ctx, `SELECT nick FROM users WHERE id = $1`, userID).Scan(&nick); err != nil {
		return 0, "", fmt.Errorf("привязка мессенджера: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, "", fmt.Errorf("привязка мессенджера: %w", err)
	}
	return userID, nick, nil
}

// MessengerLogin — кого впускать по привязке. Вторая половина двери: бот
// спрашивает её, когда живой сессии НГС у него нет, и дальше выдаёт обычный
// ключ входа (StartBotLogin), то есть путь дальше ровно тот же.
func (p *Platform) MessengerLogin(ctx context.Context, messenger string, messengerUserID int64) (int64, string, error) {
	if !knownMessenger(messenger) {
		return 0, "", ErrUnknownMessenger
	}
	var (
		userID     int64
		nick       string
		kind       Kind
		anonymized *time.Time
	)
	err := p.pool.QueryRow(ctx, `
		SELECT u.id, u.nick, u.kind, u.anonymized_at
		  FROM identities i JOIN users u ON u.id = i.user_id
		 WHERE i.kind = $1 AND i.external_id = $2`,
		messenger, strconv.FormatInt(messengerUserID, 10)).Scan(&userID, &nick, &kind, &anonymized)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", ErrNoBinding
	}
	if err != nil {
		return 0, "", fmt.Errorf("вход по привязке: %w", err)
	}
	// Отзыв общего согласия привязки удаляет (см. RevokeConsent), поэтому сюда
	// такой человек не дойдёт; проверка стоит второй страховкой на случай, если
	// вид записи поменяют иначе — впустить обезличенного нельзя ни по какой
	// двери.
	if anonymized != nil || kind != KindMember {
		return 0, "", ErrNoBinding
	}
	return userID, nick, nil
}

// UserBindings — привязки человека для «Моей страницы», свежие первыми.
func (p *Platform) UserBindings(ctx context.Context, userID int64) ([]Binding, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT kind, verified_at FROM identities
		 WHERE user_id = $1 AND kind = ANY($2)
		 ORDER BY verified_at DESC`,
		userID, []string{IdentityTelegram, IdentityMAX})
	if err != nil {
		return nil, fmt.Errorf("привязки %d: %w", userID, err)
	}
	defer rows.Close()
	var out []Binding
	for rows.Next() {
		var b Binding
		if err := rows.Scan(&b.Messenger, &b.VerifiedAt); err != nil {
			return nil, fmt.Errorf("привязки %d: %w", userID, err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Unbind снимает привязку. Гасит и согласие, если снята последняя: документ
// обещает, что отвязка «гасит это согласие», — а согласие, пережившее данные,
// это бумага, которой нечего покрывать.
func (p *Platform) Unbind(ctx context.Context, userID int64, messenger string) error {
	if !knownMessenger(messenger) {
		return ErrUnknownMessenger
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("отвязка мессенджера: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	tag, err := tx.Exec(ctx,
		`DELETE FROM identities WHERE user_id = $1 AND kind = $2`, userID, messenger)
	if err != nil {
		return fmt.Errorf("отвязка мессенджера: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoBinding
	}
	var left int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM identities WHERE user_id = $1 AND kind = ANY($2)`,
		userID, []string{IdentityTelegram, IdentityMAX}).Scan(&left); err != nil {
		return fmt.Errorf("отвязка мессенджера: %w", err)
	}
	if left == 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE consents SET revoked_at = now()
			 WHERE user_id = $1 AND kind = $2 AND revoked_at IS NULL`,
			userID, ConsentBinding); err != nil {
			return fmt.Errorf("отвязка мессенджера: %w", err)
		}
	}
	return wrapf(tx.Commit(ctx), "отвязка мессенджера")
}

// dropBindings — снять все привязки человека. Зовётся из RevokeConsent той же
// транзакцией, что и сам отзыв: живая строка identities после отзыва осталась
// бы действующим ключом от учётной записи.
func dropBindings(ctx context.Context, q querier, userID int64) error {
	if _, err := q.Exec(ctx, `
		DELETE FROM identities WHERE user_id = $1 AND kind = ANY($2)`,
		userID, []string{IdentityTelegram, IdentityMAX}); err != nil {
		return fmt.Errorf("снятие привязок %d: %w", userID, err)
	}
	return nil
}

// hasLiveConsent — есть ли действующая подпись нужной редакции по одному виду.
// Тот же приём, что у consentGuard (последняя запись по виду), но про один вид
// и без перебора обязательных: необязательный документ ничего не обязан.
func hasLiveConsent(ctx context.Context, q querier, userID int64, kind string) (bool, error) {
	want, err := ConsentDocOf(Operator{}, kind)
	if err != nil {
		return false, err
	}
	var (
		version int
		revoked *time.Time
	)
	err = q.QueryRow(ctx, `
		SELECT version, revoked_at FROM consents
		 WHERE user_id = $1 AND kind = $2
		 ORDER BY granted_at DESC LIMIT 1`, userID, kind).Scan(&version, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("согласие %s у %d: %w", kind, userID, err)
	}
	return revoked == nil && version >= want.Version, nil
}
