package platform

// Личная переписка (эпик L, 11.09.2026). ЗОНА 149-ФЗ.
//
// Правило именования у эпика одно и служебное: всё, что касается переписки,
// зовётся mail — таблицы, пакеты, маршруты, этот файл. Префикс не для красоты.
// Слова, сказанные одним человеком другому наедине, живут по другому закону, чем
// всё остальное на площадке, и место, где они лежат, обязано находиться ОДНИМ
// GREP'ОМ — тем, кто через год станет дописывать выгрузку, уборку или выдачу.
// (Имя talks занято личкой НГС: internal/talks носит ЧУЖИЕ письма, где оператор
// не мы. Путать их нельзя тем более.)
//
// ЧТО ЗДЕСЬ ПРИНЯТО, помимо кода. Заведя переписку, площадка стала организатором
// распространения информации: содержание хранится шесть месяцев, сведения о
// приёме-передаче год, и то и другое выдаётся уполномоченным органам по
// мотивированному запросу. Сквозного шифрования нет, и это не недоделка —
// переписка, которую оператор прочесть не умеет, от обязанности выдать не
// освобождает, а делает её невыполнимой. Всё это сказано людям прямо в тексте
// согласия (consents/talks.v1.txt), потому что документ, обещающий больше, чем
// площадка делает, хуже отсутствующего.
//
// ЧЕГО ЗДЕСЬ НЕТ И НЕ БУДЕТ, и это тоже решения, а не недоделки:
//
//   - письмо НЕ идёт в moderation_queue. Автомат модерации — платный запрос к
//     сторонней модели, и посылать ему личную переписку значит отдать чужие
//     слова третьей стороне ради проверки, которой никто не просил. Единственная
//     дверь модератора сюда — жалоба ПОЛУЧАТЕЛЯ с процитированным им письмом,
//     и способа открыть чужую переписку целиком не заводится ни в одном
//     интерфейсе;
//   - письмо НЕ идёт в ngs_outbox. Никогда и ни по какой галочке: на НГС оно
//     легло бы публичной репликой;
//   - в шину не попадает ни слова письма. Событие несёт dialog_id — ссылку, как
//     note_id, — а выражение выдержки (notificationColumns) берёт текст у
//     заметки или реплики, которых у письма нет вовсе, и потому отдаёт пустую
//     строку САМО. Закреплено тестом, ищущим подстроку письма в events и
//     notifications;
//   - publishGuard здесь НЕ зовётся. Внутри него consentGuard, сверяющий
//     ОБЯЗАТЕЛЬНЫЕ редакции, то есть согласие на РАСПРОСТРАНЕНИЕ; письмо одному
//     адресату не распространяется, а позови мы его — выпуск новой редакции
//     распространения обрывал бы всем идущие переписки до переподписи. Вместо
//     него writeGuard (бан и «вообще ли этот человек вправе писать») плюс
//     talkGuard (своё, четвёртое согласие).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
)

// Потолки переписки. Названы ПОИМЁННО по тому же доводу, что NoteWindow и
// соседи: читает их не только enforceRate — справка обязана называть те же
// числа, что и отказ формы, а написанные в ней словами они разъезжаются молча.
const (
	// MessageWindow — не чаще одного письма за это время. Столько же, сколько у
	// реплики: переписка — тот же разговор, только вдвоём.
	MessageWindow = 10 * time.Second
	// MessagesPerHour — и не больше стольких за час. Тоже как у реплик: замер
	// архива дал рекорд одного автора 84 реплики в час, и отсекать здесь надо
	// шторм, а не разговорчивость.
	MessagesPerHour = 90
	// FirstWindow — не чаще одного НОВОГО собеседника за это время.
	FirstWindow = 5 * time.Minute
	// FirstsPerDay — и не больше стольких за сутки.
	//
	// Потолок первых писем считает ПЕРЕПИСКИ, а не письма, и в этом вся его
	// суть: рассылка отличается от разговорчивости не числом сказанного, а
	// числом новых собеседников. Человек, переписывающийся с тремя, упирается в
	// MessagesPerHour и никогда — в этот.
	FirstsPerDay = 10
	// UnansweredMax — сколько писем подряд можно отправить тому, кто не ответил
	// НИ РАЗУ. Четвёртое письмо в молчание — это уже не разговор.
	UnansweredMax = 3
)

// Сроки хранения переписки. ЗАКОН, а не наша осторожность, и единственное место
// у площадки, где срок одновременно и пол, и потолок: раньше не даёт 149-ФЗ,
// дольше не даёт 152-ФЗ (обработка в объёме и сроке, соразмерных цели).
//
// Стоят они здесь, а не рядом с KeepRead и KeepEvents в events.go, ровно по
// правилу из шапки: зона 149-ФЗ обязана находиться одним grep'ом по mail.
const (
	// KeepMessageBody — содержание письма (п. 3 ч. 1 ст. 10.1 149-ФЗ).
	KeepMessageBody = 180 * 24 * time.Hour
	// KeepMessageMeta — сведения о приёме и передаче: кто, кому, когда. Строка
	// письма живёт дольше своего текста, и потому таблица одна: вторая таблица
	// метаданных была бы вторым источником правды о том, кто кому писал.
	KeepMessageMeta = 365 * 24 * time.Hour
)

// noIDBand — нижний край полосы идентификаторов для писем, то есть его
// отсутствие. У mail_messages и mail_dialogs свои последовательности с единицы:
// полосы, разделяющей зеркальное и нативное, здесь нет и быть не может.
//
// Ноль передаётся ЯВНО, а не подразумевается, потому что до 11.09.2026
// enforceRate подставляла NativeIDBase сама — и условие `id >= 1e11` вышло бы
// ложным для каждой строки, счёт всегда нулевым, а потолок писем МОЛЧА не
// работал бы вовсе.
const noIDBand int64 = 0

var (
	// ErrNoRecipient — письмо этому адресату не отправить. Ответ ОДИН на все
	// причины (тень, житель, служебная анкета, обезличенный, отозвавший
	// согласие) намеренно: перебором номеров иначе выясняется, кто на площадке
	// завёл переписку, а кто её отключил.
	ErrNoRecipient = errors.New("этот участник не принимает личных писем")
	// ErrSelfMessage — переписка с самим собой. Отправить такое письмо нельзя и
	// без проверки (CHECK lo_id < hi_id), но отказ базы человеку не покажешь.
	ErrSelfMessage = errors.New("переписка с самим собой невозможна")
	// ErrNoTalkConsent — не подписано четвёртое согласие. Отдельной ошибкой, а
	// не общим отказом: морда по ней ведёт на экран документа, а не прячет
	// кнопку, — спрятанная кнопка ничего не объясняет.
	ErrNoTalkConsent = errors.New("личная переписка требует отдельного согласия")
	// ErrBlockedByPeer — собеседник закрыл от вас переписку. Говорится ПРЯМО
	// (решение владельца 11.09.2026): молчаливая блокировка копит у отправителя
	// переписку, которой никто не читает, — а это и есть травля, только
	// невидимая, и получателю она обходится дороже честного отказа.
	ErrBlockedByPeer = errors.New("этот участник закрыл от вас личную переписку")
	// ErrBlockedByYou — обратная сторона: закрыли вы. Ошибки разные, потому что
	// действия разные: одну снимает кнопка у вас, другую не снимает ничто.
	ErrBlockedByYou = errors.New("вы закрыли переписку с этим участником")
	// ErrUnanswered — подряд, в молчание, больше нельзя.
	ErrUnanswered = fmt.Errorf(
		"можно написать не больше %d писем подряд тому, кто ни разу не ответил", UnansweredMax)
)

// Стёртое по сроку содержание ошибкой НЕ считается и своей ошибки не имеет: это
// не поломка и не потеря, а состояние строки, и несёт его MessageView.Purged.
// Ошибка на его месте заставила бы страницу объяснять законом отказ, которого
// не было.

// mailRates и mailFirstRates — те же два правила, что у заметок и реплик:
// частое и суточное.
var (
	mailRates      = []rateRule{{MessageWindow, 1}, {time.Hour, MessagesPerHour}}
	mailFirstRates = []rateRule{{FirstWindow, 1}, {24 * time.Hour, FirstsPerDay}}
)

// messagesRate — частота писем, считается по отправителю за окно; вход —
// mail_messages_rate.
//
// firstsRate — частота НОВЫХ СОБЕСЕДНИКОВ, и считает она переписки
// (mail_dialogs.started_by), а не письма: см. FirstsPerDay.
var (
	messagesRate = rateQuery{
		count: `SELECT count(*) FROM mail_messages
		         WHERE sender_id = $1 AND id >= $2 AND sent_at > $3`,
		nth: `SELECT sent_at FROM mail_messages
		       WHERE sender_id = $1 AND id >= $2 AND sent_at > $3
		       ORDER BY sent_at LIMIT 1 OFFSET $4`,
	}
	firstsRate = rateQuery{
		count: `SELECT count(*) FROM mail_dialogs
		         WHERE started_by = $1 AND id >= $2 AND created_at > $3`,
		nth: `SELECT created_at FROM mail_dialogs
		       WHERE started_by = $1 AND id >= $2 AND created_at > $3
		       ORDER BY created_at LIMIT 1 OFFSET $4`,
	}
)

// ------------------------------------------------------------------ стражи

// talkGuard — у этого человека есть действующая подпись под документом о
// переписке. Обёртка над hasLiveConsent (binding.go), которая и заводилась
// общей по виду: второй способ спросить «подписано ли» разошёлся бы с первым.
func talkGuard(ctx context.Context, q querier, userID int64) error {
	ok, err := hasLiveConsent(ctx, q, userID, ConsentTalks)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoTalkConsent
	}
	return nil
}

// consentRevoked — последняя подпись по этому виду ОТОЗВАНА. Отличается от
// «не подписывал» (там строки нет вовсе), и различие здесь несущее: отзыв —
// осознанное действие человека, и по решению владельца новых писем отозвавшему
// мы не принимаем; молчание же согласием не считается, но и запретом не
// является — письмо такому адресату ляжет непоказанным, о чём документ говорит
// прямо.
func consentRevoked(ctx context.Context, q querier, userID int64, kind string) (bool, error) {
	var revoked *time.Time
	err := q.QueryRow(ctx, `
		SELECT revoked_at FROM consents
		 WHERE user_id = $1 AND kind = $2
		 ORDER BY granted_at DESC LIMIT 1`, userID, kind).Scan(&revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("согласие %s у %d: %w", kind, userID, err)
	}
	return revoked != nil, nil
}

// recipientGuard — этому адресату письмо отправить можно.
//
// Живой участник: не тень (за ней никто не входил и согласия не давал), не
// житель (у персонажа нет ни почтового ящика, ни субъекта), не служебная анкета
// площадки (писать оператору — это «Как связаться», а не личка), не обезличенный
// (его больше нет) и не отозвавший согласие на переписку.
func recipientGuard(ctx context.Context, q querier, recipientID int64) error {
	var (
		kind       Kind
		persona    bool
		anonymized *time.Time
	)
	err := q.QueryRow(ctx,
		`SELECT kind, persona, anonymized_at FROM users WHERE id = $1`, recipientID).
		Scan(&kind, &persona, &anonymized)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoRecipient
	}
	if err != nil {
		return fmt.Errorf("проверка адресата %d: %w", recipientID, err)
	}
	if anonymized != nil || persona || kind != KindMember {
		return ErrNoRecipient
	}
	revoked, err := consentRevoked(ctx, q, recipientID, ConsentTalks)
	if err != nil {
		return err
	}
	if revoked {
		return ErrNoRecipient
	}
	return nil
}

// blocksQuery — обе стороны чёрного списка одним запросом, двумя точными
// попаданиями в первичный ключ. Константой, потому что её план проверяется
// тестом: спрашивается она на каждое письмо и на каждую кнопку «Написать».
const blocksQuery = `
	SELECT coalesce(bool_or(user_id = $1), false),
	       coalesce(bool_or(user_id = $2), false)
	  FROM mail_blocks
	 WHERE (user_id = $1 AND blocked_id = $2)
	    OR (user_id = $2 AND blocked_id = $1)`

// dialogPairQuery — переписка пары по нормализованному ключу. Тоже константой и
// по той же причине: спрашивает её каждая кнопка «Написать» на странице
// участника.
const dialogPairQuery = `SELECT id FROM mail_dialogs WHERE lo_id = $1 AND hi_id = $2`

// blockGuard — обе стороны чёрного списка разом, одним запросом по префиксу
// первичного ключа. Ответы РАЗНЫЕ: своё закрытие снимает кнопка, чужое не
// снимает ничто, и одна ошибка на два случая отправила бы человека жать кнопку,
// которой у него нет.
//
// Своё проверяется первым: оно действеннее, а узнав только про чужое, человек
// снял бы своё и упёрся во второй отказ.
func blockGuard(ctx context.Context, q querier, from, to int64) error {
	var byMe, byPeer bool
	err := q.QueryRow(ctx, blocksQuery, from, to).Scan(&byMe, &byPeer)
	if err != nil {
		return fmt.Errorf("чёрный список %d и %d: %w", from, to, err)
	}
	switch {
	case byMe:
		return ErrBlockedByYou
	case byPeer:
		return ErrBlockedByPeer
	}
	return nil
}

// ------------------------------------------------------------------ отправка

// MessageSent — что вышло из отправки.
type MessageSent struct {
	DialogID  int64
	MessageID int64
	// First — этим письмом переписка и заведена. Нужно вызывающему, чтобы
	// отвести человека на страницу переписки, адреса которой до нажатия не
	// существовало.
	First bool
}

// SendMessage отправляет письмо.
//
// Порядок внутри транзакции — тот же, что у CreateComment, и по тому же доводу:
// всё, что решает «можно ли», читается ТОЙ ЖЕ транзакцией, что и вставка, иначе
// между проверкой и записью успевает пройти бан или отзыв согласия.
//
// ЗАМОК НА ПЕРЕПИСКЕ БЕРЁТСЯ ПЕРВЫМ из всего, что трогает её строки, и это не
// придирка. Два встречных письма одной пары иначе обновляли бы свою и чужую
// сторону в ОБРАТНОМ порядке и встали бы в классический взаимный замок; строка
// mail_dialogs, взятая FOR UPDATE, выстраивает их в очередь целиком.
func (p *Platform) SendMessage(ctx context.Context, senderID, recipientID int64, body string) (MessageSent, error) {
	var out MessageSent
	// Своего потолка длины у письма нет: MaxBodyRunes один на площадку, и
	// второе число рядом с первым разъехалось бы на первой же правке.
	body, err := cleanBody(body)
	if err != nil {
		return out, err
	}
	switch {
	case senderID == 0:
		return out, ErrNotMember
	case senderID == recipientID:
		return out, ErrSelfMessage
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return out, wrapf(err, "отправка письма")
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	if err := writeGuard(ctx, tx, senderID); err != nil {
		return out, err
	}
	// ВТОРОЙ РУБЕЖ согласия, буквально как в BindMessenger: подпись стоит на
	// своём экране до первого письма, но между экраном и отправкой человек мог
	// нажать «Отозвать», и письмо, записанное после отзыва, было бы обработкой
	// без основания.
	if err := talkGuard(ctx, tx, senderID); err != nil {
		return out, err
	}
	if err := recipientGuard(ctx, tx, recipientID); err != nil {
		return out, err
	}
	if err := blockGuard(ctx, tx, senderID, recipientID); err != nil {
		return out, err
	}
	now := time.Now()
	if err := enforceRate(ctx, tx, messagesRate, senderID, noIDBand, now, mailRates); err != nil {
		return out, err
	}

	lo, hi := senderID, recipientID
	if lo > hi {
		lo, hi = hi, lo
	}
	dialogID, mySent, peerSent, err := lockDialog(ctx, tx, lo, hi, senderID)
	if err != nil {
		return out, err
	}
	if dialogID == 0 {
		// Первое письмо незнакомцу: свой потолок, своя единица счёта.
		if err := enforceRate(ctx, tx, firstsRate, senderID, noIDBand, now, mailFirstRates); err != nil {
			return out, err
		}
		if dialogID, err = openDialog(ctx, tx, lo, hi, senderID); err != nil {
			return out, err
		}
		out.First = dialogID != 0
		if dialogID == 0 {
			// Гонку выиграло встречное первое письмо: переписка уже есть, и
			// наше письмо ложится в неё обычным порядком.
			if dialogID, mySent, peerSent, err = lockDialog(ctx, tx, lo, hi, senderID); err != nil {
				return out, err
			}
		}
	}
	// Молчание в ответ — не отказ, но и не приглашение. Считается по СТОРОНАМ:
	// ноль у собеседника означает «я ему до сих пор незнаком».
	if peerSent == 0 && mySent >= UnansweredMax {
		return out, ErrUnanswered
	}
	if err := ensureSides(ctx, tx, dialogID, senderID, recipientID); err != nil {
		return out, err
	}

	var messageID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO mail_messages (dialog_id, sender_id, body, sent_at)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		dialogID, senderID, body, now).Scan(&messageID); err != nil {
		return out, wrapf(err, "отправка письма")
	}
	if err := bumpSides(ctx, tx, dialogID, senderID, recipientID, now); err != nil {
		return out, err
	}
	// Факт — той же транзакцией, что и письмо, по общему правилу шины. Текста в
	// нём нет: событие несёт ССЫЛКУ на переписку, а кому это повод, решается
	// потом и фоном.
	if err := recordEvent(ctx, tx, newEvent{
		Kind: EventMessage, ActorID: senderID, SubjectID: recipientID, DialogID: dialogID,
	}); err != nil {
		return out, err
	}
	if err := tx.Commit(ctx); err != nil {
		return out, wrapf(err, "отправка письма")
	}
	out.DialogID, out.MessageID = dialogID, messageID
	return out, nil
}

// lockDialog берёт переписку пары под замок и заодно отвечает, сколько писем
// написала каждая сторона. Ноль в dialogID означает «переписки ещё нет».
// Запросов ДВА, а не один со счётом сторон в том же SELECT: Postgres не
// разрешает FOR UPDATE вместе с GROUP BY, а замок здесь важнее экономии
// round-trip'а — он и есть то, что выстраивает встречные письма в очередь.
func lockDialog(ctx context.Context, q querier, lo, hi, meID int64) (dialogID, mySent, peerSent int64, err error) {
	err = q.QueryRow(ctx,
		`SELECT id FROM mail_dialogs WHERE lo_id = $1 AND hi_id = $2 FOR UPDATE`, lo, hi).Scan(&dialogID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, 0, nil
	}
	if err != nil {
		return 0, 0, 0, wrapf(err, "переписка %d и %d", lo, hi)
	}
	// Стороны читаются УЖЕ ПОД ЗАМКОМ, поэтому увидеть их наполовину
	// обновлёнными нельзя: встречное письмо ждёт на строке переписки.
	err = q.QueryRow(ctx, `
		SELECT coalesce(max(sent) FILTER (WHERE user_id = $2), 0),
		       coalesce(max(sent) FILTER (WHERE user_id <> $2), 0)
		  FROM mail_sides WHERE dialog_id = $1`, dialogID, meID).Scan(&mySent, &peerSent)
	if err != nil {
		return 0, 0, 0, wrapf(err, "стороны переписки %d", dialogID)
	}
	return dialogID, mySent, peerSent, nil
}

// openDialog заводит переписку. Ноль означает «кто-то успел первым» — не ошибку:
// встречное первое письмо той же пары это обычное дело, а не гонка, которую надо
// разбирать руками.
func openDialog(ctx context.Context, q querier, lo, hi, starter int64) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO mail_dialogs (lo_id, hi_id, started_by)
		VALUES ($1, $2, $3)
		ON CONFLICT (lo_id, hi_id) DO NOTHING
		RETURNING id`, lo, hi, starter).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, wrapf(err, "заведение переписки %d и %d", lo, hi)
}

// ensureSides заводит обе стороны. Одним запросом на двоих: сторона без
// собеседника бессмысленна, и заводиться они обязаны вместе.
func ensureSides(ctx context.Context, q querier, dialogID, meID, peerID int64) error {
	_, err := q.Exec(ctx, `
		INSERT INTO mail_sides (dialog_id, user_id, peer_id)
		VALUES ($1, $2, $3), ($1, $3, $2)
		ON CONFLICT (dialog_id, user_id) DO NOTHING`, dialogID, meID, peerID)
	return wrapf(err, "стороны переписки %d", dialogID)
}

// bumpSides двигает счётчики. Явными UPDATE, а не триггером: правит их та же
// транзакция, что пишет письмо, и другого места, где они меняются, нет — тот же
// приём, что у notes.comment_count.
//
// Каждой стороне свой запрос, хотя одним с CASE было бы короче: пара
// «CASE WHEN user_id = $me» и есть тот вид кода, ради отсутствия которого
// стороны и разведены по строкам.
//
// hidden_at гасится У ОБОИХ, и это решение с ценой: убранная переписка всплывает
// от нового письма. Иначе «убрать у себя» молча стало бы блокировкой, о которой
// отправитель не знает, — а от навязчивости здесь бережёт чёрный список, про
// который человеку говорят прямо.
func bumpSides(ctx context.Context, q querier, dialogID, senderID, recipientID int64, now time.Time) error {
	if _, err := q.Exec(ctx,
		`UPDATE mail_dialogs SET last_message_at = $2 WHERE id = $1`, dialogID, now); err != nil {
		return wrapf(err, "переписка %d", dialogID)
	}
	if _, err := q.Exec(ctx, `
		UPDATE mail_sides SET sent = sent + 1, last_message_at = $3, hidden_at = NULL
		 WHERE dialog_id = $1 AND user_id = $2`, dialogID, senderID, now); err != nil {
		return wrapf(err, "сторона отправителя %d", senderID)
	}
	if _, err := q.Exec(ctx, `
		UPDATE mail_sides SET unread = unread + 1, last_message_at = $3, hidden_at = NULL
		 WHERE dialog_id = $1 AND user_id = $2`, dialogID, recipientID, now); err != nil {
		return wrapf(err, "сторона адресата %d", recipientID)
	}
	return nil
}

// CanWriteTo — можно ли этому человеку написать тому, и есть ли у них уже
// переписка. ОДНО правило на ядро и на кнопку: второй список условий в шаблоне
// однажды нарисовал бы кнопку, отвечающую отказом.
//
// Возвращает номер существующей переписки (ноль — её ещё нет) и типизированную
// ошибку: по ErrNoTalkConsent морда ведёт на экран документа, а не прячет
// кнопку.
func (p *Platform) CanWriteTo(ctx context.Context, meID, peerID int64) (int64, error) {
	switch {
	case meID == 0:
		return 0, ErrNotMember
	case meID == peerID:
		return 0, ErrSelfMessage
	}
	if err := writeGuard(ctx, p.pool, meID); err != nil {
		return 0, err
	}
	if err := talkGuard(ctx, p.pool, meID); err != nil {
		return 0, err
	}
	if err := recipientGuard(ctx, p.pool, peerID); err != nil {
		return 0, err
	}
	if err := blockGuard(ctx, p.pool, meID, peerID); err != nil {
		return 0, err
	}
	lo, hi := meID, peerID
	if lo > hi {
		lo, hi = hi, lo
	}
	var id int64
	err := p.pool.QueryRow(ctx,
		`SELECT id FROM mail_dialogs WHERE lo_id = $1 AND hi_id = $2`, lo, hi).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, wrapf(err, "переписка %d и %d", lo, hi)
}

// ------------------------------------------------------------------ чтение

// mailPeerColumns — собеседник, как его показывают. Те же колонки и в том же
// порядке, что у автора заметки и реплики: третьего представления человека на
// площадке не заводится.
const mailPeerColumns = `
	p.id, p.nick, p.avatar_sha, pm.mime, coalesce(p.gender, 0),
	p.kind = 0, p.persona, p.kind = 2, coalesce(p.age, 0)`

// mailPeer — приёмник этих колонок.
type mailPeer struct {
	id      int64
	nick    string
	sha     []byte
	mime    *string
	gender  Gender
	shadow  bool
	persona bool
	system  bool
	age     int16
}

func (m *mailPeer) dest() []any {
	return []any{&m.id, &m.nick, &m.sha, &m.mime, &m.gender,
		&m.shadow, &m.persona, &m.system, &m.age}
}

func (m mailPeer) author() Author {
	return Author{
		ID: m.id, Nick: m.nick, AvatarURL: MediaURL(m.sha, strOf(m.mime)),
		Gender: m.gender, Shadow: m.shadow, Persona: m.persona,
		System: m.system, Age: int(m.age),
	}
}

// DialogView — строка списка переписок.
type DialogView struct {
	DialogID int64
	Peer     Author
	Unread   int
	Sent     int
	LastAt   time.Time
	// Excerpt — начало последнего письма. Показывается УЧАСТНИКУ переписки, то
	// есть тому, кто и так вправе прочесть его целиком; у стёртого по сроку
	// письма выдержка пуста сама.
	Excerpt    string
	LastFromMe bool
}

// dialogsQuery — список переписок. Константой, потому что его план проверяется
// тестом: вход — частичный индекс mail_sides_inbox, и молчаливый переезд на
// перебор сторон стоил бы страницы у каждого.
const dialogsQuery = `
	SELECT s.dialog_id, s.unread, s.sent, s.last_message_at, ` + mailPeerColumns + `,
	       coalesce(left(lm.body, $4), ''), coalesce(lm.sender_id = $1, false)
	  FROM mail_sides s
	  JOIN users p ON p.id = s.peer_id
	  LEFT JOIN media pm ON pm.sha256 = p.avatar_sha
	  LEFT JOIN LATERAL (SELECT body, sender_id FROM mail_messages m
	                      WHERE m.dialog_id = s.dialog_id ORDER BY m.id DESC LIMIT 1) lm ON true
	 WHERE s.user_id = $1 AND s.hidden_at IS NULL
	 ORDER BY s.last_message_at DESC
	 LIMIT $2 OFFSET $3`

// Dialogs — переписки человека, от свежих к старым.
func (p *Platform) Dialogs(ctx context.Context, userID int64, offset, limit int) ([]DialogView, error) {
	rows, err := p.pool.Query(ctx, dialogsQuery, userID, clampLimit(limit), max(0, offset), excerptRunes)
	if err != nil {
		return nil, wrapf(err, "переписки участника %d", userID)
	}
	defer rows.Close()
	var out []DialogView
	for rows.Next() {
		var (
			v    DialogView
			peer mailPeer
		)
		dest := []any{&v.DialogID, &v.Unread, &v.Sent, &v.LastAt}
		dest = append(dest, peer.dest()...)
		dest = append(dest, &v.Excerpt, &v.LastFromMe)
		if err := rows.Scan(dest...); err != nil {
			return nil, wrapf(err, "переписки участника %d", userID)
		}
		v.Peer = peer.author()
		out = append(out, v)
	}
	return out, wrapf(rows.Err(), "переписки участника %d", userID)
}

// CountDialogs — сколько у человека переписок (под постраничку).
func (p *Platform) CountDialogs(ctx context.Context, userID int64) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM mail_sides WHERE user_id = $1 AND hidden_at IS NULL`, userID).Scan(&n)
	return n, wrapf(err, "счёт переписок участника %d", userID)
}

// DialogHead — шапка переписки.
type DialogHead struct {
	DialogID    int64
	Peer        Author
	Total       int
	Unread      int
	LastReadID  int64
	BlockedByMe bool
	BlockedMe   bool
}

const dialogHeadQuery = `
	SELECT s.dialog_id, ` + mailPeerColumns + `,
	       (SELECT count(*) FROM mail_messages m WHERE m.dialog_id = s.dialog_id),
	       s.unread, s.last_read_id,
	       EXISTS (SELECT 1 FROM mail_blocks b WHERE b.user_id = $1 AND b.blocked_id = s.peer_id),
	       EXISTS (SELECT 1 FROM mail_blocks b WHERE b.user_id = s.peer_id AND b.blocked_id = $1)
	  FROM mail_sides s
	  JOIN users p ON p.id = s.peer_id
	  LEFT JOIN media pm ON pm.sha256 = p.avatar_sha
	 WHERE s.user_id = $1 AND s.dialog_id = $2`

// MessageView — письмо, как его показывают.
type MessageView struct {
	ID     int64
	FromMe bool
	Body   string
	SentAt time.Time
	// Purged — содержание стёрто по сроку хранения. Строка остаётся, и на
	// странице на её месте стоит объяснение, а не пустота: молча исчезнувшее
	// письмо читается как потеря, а это закон.
	Purged bool
}

const dialogMessagesQuery = `
	SELECT id, sender_id = $2, body, sent_at, purged_at IS NOT NULL
	  FROM mail_messages WHERE dialog_id = $1 ORDER BY id LIMIT $3 OFFSET $4`

// Dialog — шапка переписки и письма в ней, в порядке разговора.
//
// Чужую переписку не отдаёт: строки в mail_sides у постороннего нет, и ответ ему
// — ErrNotFound, а не «это не ваша переписка». Существование чужой переписки —
// само по себе сведения, тот же довод, что у /mod для постороннего.
//
// Читать письма тоже можно только по подписи: согласие здесь не разрешение
// писать, а основание обрабатывать переписку вообще.
func (p *Platform) Dialog(ctx context.Context, userID, dialogID int64, offset, limit int) (DialogHead, []MessageView, error) {
	var head DialogHead
	if err := talkGuard(ctx, p.pool, userID); err != nil {
		return head, nil, err
	}
	var peer mailPeer
	dest := []any{&head.DialogID}
	dest = append(dest, peer.dest()...)
	dest = append(dest, &head.Total, &head.Unread, &head.LastReadID, &head.BlockedByMe, &head.BlockedMe)
	err := p.pool.QueryRow(ctx, dialogHeadQuery, userID, dialogID).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return head, nil, ErrNotFound
	}
	if err != nil {
		return head, nil, wrapf(err, "переписка %d", dialogID)
	}
	head.Peer = peer.author()

	rows, err := p.pool.Query(ctx, dialogMessagesQuery, dialogID, userID, clampLimit(limit), max(0, offset))
	if err != nil {
		return head, nil, wrapf(err, "письма переписки %d", dialogID)
	}
	defer rows.Close()
	var out []MessageView
	for rows.Next() {
		var m MessageView
		if err := rows.Scan(&m.ID, &m.FromMe, &m.Body, &m.SentAt, &m.Purged); err != nil {
			return head, nil, wrapf(err, "письма переписки %d", dialogID)
		}
		out = append(out, m)
	}
	return head, out, wrapf(rows.Err(), "письма переписки %d", dialogID)
}

// unreadMailQuery — счётчик писем в шапке. Спрашивается на КАЖДОЙ странице
// вошедшего, рядом с колокольчиком, поэтому устроен так же: частичный индекс
// mail_sides_unread плюс потолок строк, чтобы запрос не зависел от того,
// сколько у человека переписок.
const unreadMailQuery = `
	SELECT coalesce(sum(unread), 0) FROM (
	    SELECT unread FROM mail_sides
	     WHERE user_id = $1 AND unread > 0 LIMIT $2) t`

// UnreadMail — сколько непрочитанных писем, считая не больше UnreadCap переписок.
func (p *Platform) UnreadMail(ctx context.Context, userID int64) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx, unreadMailQuery, userID, UnreadCap).Scan(&n)
	return n, wrapf(err, "непрочитанные письма участника %d", userID)
}

// MarkDialogRead отмечает переписку прочитанной до uptoID включительно;
// uptoID = 0 означает «всё, что есть».
//
// Одной транзакцией со снятием поводов шины: прочитанное письмо, оставляющее
// гореть колокольчик, — ровно то состояние, из-за которого счётчики и заводят
// денормализованными.
func (p *Platform) MarkDialogRead(ctx context.Context, userID, dialogID, uptoID int64) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return wrapf(err, "отметка переписки %d", dialogID)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	if uptoID == 0 {
		if err := tx.QueryRow(ctx,
			`SELECT coalesce(max(id), 0) FROM mail_messages WHERE dialog_id = $1`,
			dialogID).Scan(&uptoID); err != nil {
			return wrapf(err, "отметка переписки %d", dialogID)
		}
	}
	// Граница двигается только вперёд (greatest), поэтому повтор и возврат к
	// старому письму ничего не портят. Счётчик пересчитывается по этой же
	// границе, а не уменьшается на единицу: считать разностью значит однажды
	// разойтись с тем, что человек видит.
	tag, err := tx.Exec(ctx, `
		UPDATE mail_sides s
		   SET last_read_id = greatest(s.last_read_id, $3),
		       unread = (SELECT count(*) FROM mail_messages m
		                  WHERE m.dialog_id = s.dialog_id AND m.sender_id <> s.user_id
		                    AND m.id > greatest(s.last_read_id, $3))
		 WHERE s.dialog_id = $1 AND s.user_id = $2`, dialogID, userID, uptoID)
	if err != nil {
		return wrapf(err, "отметка переписки %d", dialogID)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := readMailPokes(ctx, tx, userID, dialogID); err != nil {
		return err
	}
	return wrapf(tx.Commit(ctx), "отметка переписки %d", dialogID)
}

// readMailPokes гасит поводы шины об этой переписке.
//
// Гасятся ВСЕ непрочитанные по ней, без оглядки на границу письма: в events
// лежит ссылка на переписку, а не на письмо, и точнее отметить нечем. Цена
// названа и мала — письмо, пришедшее между чтением страницы и нажатием, теряет
// колокольчик, но НЕ теряется само: счётчик mail_sides считается по своей
// границе и остаётся верным, а он здесь и есть главный.
func readMailPokes(ctx context.Context, q querier, userID, dialogID int64) error {
	_, err := q.Exec(ctx, `
		UPDATE notifications n SET read_at = now()
		  FROM events e
		 WHERE n.event_id = e.id AND n.user_id = $1
		   AND n.read_at IS NULL AND e.dialog_id = $2`, userID, dialogID)
	return wrapf(err, "снятие поводов о переписке %d", dialogID)
}

// RecountUnread пересчитывает счётчик непрочитанного по самим письмам и
// возвращает сумму. Образец — RecountComments: денормализованное число обязано
// иметь способ сойтись с правдой, иначе однажды разойдётся навсегда.
func (p *Platform) RecountUnread(ctx context.Context, userID int64) (int, error) {
	if _, err := p.pool.Exec(ctx, `
		UPDATE mail_sides s
		   SET unread = (SELECT count(*) FROM mail_messages m
		                  WHERE m.dialog_id = s.dialog_id AND m.sender_id <> s.user_id
		                    AND m.id > s.last_read_id)
		 WHERE s.user_id = $1`, userID); err != nil {
		return 0, wrapf(err, "пересчёт непрочитанного %d", userID)
	}
	return p.UnreadMail(ctx, userID)
}

// HideDialog убирает переписку со своей страницы.
//
// Не удаление и не блокировка: письма остаются лежать (раньше срока их стереть
// нельзя), а новое письмо возвращает переписку в список. Счётчик при этом
// обнуляется — иначе колокольчик горел бы о письмах, которых человек не видит.
func (p *Platform) HideDialog(ctx context.Context, userID, dialogID int64) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return wrapf(err, "скрытие переписки %d", dialogID)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	tag, err := tx.Exec(ctx, `
		UPDATE mail_sides s
		   SET hidden_at = now(), unread = 0,
		       last_read_id = greatest(s.last_read_id,
		           coalesce((SELECT max(m.id) FROM mail_messages m
		                      WHERE m.dialog_id = s.dialog_id), 0))
		 WHERE s.dialog_id = $1 AND s.user_id = $2`, dialogID, userID)
	if err != nil {
		return wrapf(err, "скрытие переписки %d", dialogID)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := readMailPokes(ctx, tx, userID, dialogID); err != nil {
		return err
	}
	return wrapf(tx.Commit(ctx), "скрытие переписки %d", dialogID)
}

// ------------------------------------------------------------ чёрный список

// BlockUser закрывает от человека личную переписку.
//
// В audit_log НЕ пишется ничего, и это решение: чёрный список — личное решение
// участника о том, с кем он разговаривает, а не работа модерации. Запись о нём в
// открытом всем модераторам журнале превратила бы её в сведения о людях, которых
// никто не просил собирать.
//
// Той же транзакцией переписка убирается со своей страницы: закрыть человека и
// продолжать видеть его письма в списке — не то, чего от кнопки ждут.
func (p *Platform) BlockUser(ctx context.Context, userID, blockedID int64) error {
	switch {
	case userID == 0:
		return ErrNotMember
	case userID == blockedID:
		return ErrSelfMessage
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return wrapf(err, "чёрный список %d", userID)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	if _, err := tx.Exec(ctx, `
		INSERT INTO mail_blocks (user_id, blocked_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, userID, blockedID); err != nil {
		return wrapf(err, "чёрный список %d", userID)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE mail_sides SET hidden_at = now(), unread = 0
		 WHERE user_id = $1 AND peer_id = $2`, userID, blockedID); err != nil {
		return wrapf(err, "чёрный список %d", userID)
	}
	// Поводы шины гасятся ТОЙ ЖЕ транзакцией — как у HideDialog и по той же
	// причине: письма ушли из списка и счётчик обнулён, а колокольчик горел бы
	// дальше и вёл в переписку, которую человек только что закрыл. Двери у
	// счётчика писем и у колокольчика разные, и закрывать их надо обе.
	lo, hi := userID, blockedID
	if lo > hi {
		lo, hi = hi, lo
	}
	var dialogID int64
	switch err := tx.QueryRow(ctx, dialogPairQuery, lo, hi).Scan(&dialogID); {
	case errors.Is(err, pgx.ErrNoRows):
		// Переписки нет вовсе — закрывают и того, кто ещё не написал. Гасить
		// нечего, и это не отказ.
	case err != nil:
		return wrapf(err, "чёрный список %d", userID)
	default:
		if err := readMailPokes(ctx, tx, userID, dialogID); err != nil {
			return err
		}
	}
	return wrapf(tx.Commit(ctx), "чёрный список %d", userID)
}

// UnblockUser снимает запрет. Повтор молчит: кнопка, отвечающая «состояние уже
// такое» на второе нажатие, объясняет человеку не то, о чём он спрашивал.
func (p *Platform) UnblockUser(ctx context.Context, userID, blockedID int64) error {
	_, err := p.pool.Exec(ctx,
		`DELETE FROM mail_blocks WHERE user_id = $1 AND blocked_id = $2`, userID, blockedID)
	return wrapf(err, "снятие из чёрного списка %d", userID)
}

// BlockedList — кого человек закрыл, от свежих к старым.
func (p *Platform) BlockedList(ctx context.Context, userID int64) ([]Author, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT `+mailPeerColumns+`
		  FROM mail_blocks b
		  JOIN users p ON p.id = b.blocked_id
		  LEFT JOIN media pm ON pm.sha256 = p.avatar_sha
		 WHERE b.user_id = $1 ORDER BY b.created_at DESC`, userID)
	if err != nil {
		return nil, wrapf(err, "чёрный список %d", userID)
	}
	defer rows.Close()
	var out []Author
	for rows.Next() {
		var peer mailPeer
		if err := rows.Scan(peer.dest()...); err != nil {
			return nil, wrapf(err, "чёрный список %d", userID)
		}
		out = append(out, peer.author())
	}
	return out, wrapf(rows.Err(), "чёрный список %d", userID)
}

const mailMessageQuery = `
	SELECT m.id, m.sender_id = $2, m.body, m.sent_at, m.purged_at IS NOT NULL, m.dialog_id
	  FROM mail_messages m
	  JOIN mail_sides s ON s.dialog_id = m.dialog_id AND s.user_id = $2
	 WHERE m.id = $1`

// MailMessage — ОДНО письмо своей переписки вместе с номером самой переписки.
//
// Нужен ровно затем, чтобы показать человеку, на что он жалуется, ДО нажатия:
// уходит модератору одно письмо и ни строкой больше, и увидеть это он обязан
// заранее. Право проверяет тот же JOIN по mail_sides, что и сама жалоба, —
// чужое письмо отвечает ErrNotFound, как и чужая переписка.
//
// Подпись спрашивается, как у чтения переписки: письмо читают, а не обжалуют
// вслепую. Значит отозвавший согласие формы не увидит — но он не видит и самого
// письма, и это одно и то же состояние, а не два разных.
func (p *Platform) MailMessage(ctx context.Context, userID, messageID int64) (MessageView, int64, error) {
	if err := talkGuard(ctx, p.pool, userID); err != nil {
		return MessageView{}, 0, err
	}
	var (
		m        MessageView
		dialogID int64
	)
	err := p.pool.QueryRow(ctx, mailMessageQuery, messageID, userID).
		Scan(&m.ID, &m.FromMe, &m.Body, &m.SentAt, &m.Purged, &dialogID)
	if errors.Is(err, pgx.ErrNoRows) {
		return MessageView{}, 0, ErrNotFound
	}
	return m, dialogID, wrapf(err, "письмо %d", messageID)
}

// ------------------------------------------------------------------ жалоба

// MailQuoteRunes — сколько знаков письма уходит модератору в цитате.
//
// Жалоба несёт СНИМОК текста, а не ссылку на письмо, и это главное решение L4.
// Не ради долговечности — цитата уходит вместе с содержанием оригинала
// (PruneMail) и жить дольше него не вправе, — а ради ГРАНИЦЫ: у модератора нет
// и не заводится способа открыть чужую переписку, поэтому цитата есть
// единственное, что он о письме увидит. Ссылка на письмо такой способ означала
// бы завести.
const MailQuoteRunes = 500

// SubjectMessage — пятый вид объекта журнала, и ТОЛЬКО для журнала: письмо.
// В moderation_queue оно не попадает никогда (автомат переписку не читает), а
// Subject.Valid() про него отвечает «нет» намеренно — factsOf и общие действия
// модерации к письму неприменимы, и применимыми им становиться нельзя.
const SubjectMessage = "message"

// ErrMessagePurged — жаловаться не на что: содержание стёрто по сроку хранения.
// Отдельной ошибкой, потому что человеку тут надо сказать правду — письмо было,
// а текста уже нет, — а не «такого письма нет».
var ErrMessagePurged = errors.New("содержание этого письма уже стёрто по сроку хранения")

// MailReport — жалоба на письмо, как её видит модератор.
//
// Ников тут два, а аватаров нет ни одного, и это не экономия: очередь обязана
// читаться за минуту, а лицо жалобщика решению не помогает ничем. Поля ровно
// те, по которым решают: кто, на кого, когда, что написано и что сказал сам
// жалобщик.
type MailReport struct {
	ID           int64
	At           time.Time
	ReporterID   int64
	ReporterNick string
	AuthorID     int64
	AuthorNick   string
	MessageID    int64
	DialogID     int64
	Quote        string
	Reason       string
}

// reportMessageQuery — письмо, на которое жалуются, вместе с доказательством,
// что жалобщик имеет к нему отношение.
//
// JOIN по mail_sides и есть эта проверка, и стои́т она В ЗАПРОСЕ, а не рядом с
// ним: пожаловаться на письмо из ЧУЖОЙ переписки нельзя по построению, а не по
// дисциплине вызывающего. Нет строки — ErrNotFound, тот же ответ, что у чтения
// чужой переписки.
const reportMessageQuery = `
	SELECT m.dialog_id, m.sender_id, left(m.body, $3), m.purged_at IS NOT NULL
	  FROM mail_messages m
	  JOIN mail_sides s ON s.dialog_id = m.dialog_id AND s.user_id = $2
	 WHERE m.id = $1`

// ReportMessage — пожаловаться модератору на письмо.
//
// Это ЕДИНСТВЕННАЯ дверь, через которую чужое письмо становится видно третьему
// человеку, и открывает её только получатель. Строки в moderation_queue она НЕ
// заводит: очередь читает автомат, а автомат переписку не видит вовсе — это
// обещано в подписанном согласии.
//
// Согласия на переписку здесь не спрашивается намеренно — тем же доводом, по
// которому его не спрашивают у реакции и у обычной жалобы: жалоба есть
// обращение к человеку, и требовать за неё подпись значило бы закрыть дорогу
// тому, кто жалуется как раз на происходящее.
func (p *Platform) ReportMessage(ctx context.Context, reporterID, messageID int64, reason string) error {
	reason = trimReason(reason)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return wrapf(err, "жалоба на письмо %d", messageID)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	// Тот же вход, что у публикации и у обычной жалобы: жалоба заводит работу
	// другому человеку, поэтому цена входа у неё та же, что у своих слов.
	if err := writeGuard(ctx, tx, reporterID); err != nil {
		return err
	}
	var (
		dialogID, senderID int64
		quote              string
		purged             bool
	)
	switch err := tx.QueryRow(ctx, reportMessageQuery, messageID, reporterID, MailQuoteRunes).
		Scan(&dialogID, &senderID, &quote, &purged); {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return wrapf(err, "жалоба на письмо %d", messageID)
	case senderID == reporterID:
		return ErrSelfReport
	case purged:
		return ErrMessagePurged
	}
	var open int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM mail_reports WHERE reporter_id = $1 AND resolved_at IS NULL`,
		reporterID).Scan(&open); err != nil {
		return wrapf(err, "жалоба на письмо %d", messageID)
	}
	if open >= MaxOpenReports {
		return ErrRateLimited
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO mail_reports (reporter_id, author_id, message_id, dialog_id, quote, reason)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
		reporterID, senderID, messageID, dialogID, quote, reason)
	if err != nil {
		return wrapf(err, "жалоба на письмо %d", messageID)
	}
	if tag.RowsAffected() == 0 {
		return ErrNothingToDo // уже жаловался, и решения ещё не было
	}
	// В ЖУРНАЛ идут ЧИСЛА. Ни цитаты, ни причины: журнал append-only, он
	// переживает и письмо, и его срок хранения, — а закон велит стереть
	// содержание через полгода. Копия текста в журнале сделала бы это стирание
	// ненастоящим ровно там, где оно обязано быть настоящим.
	if err := audit(ctx, tx, reporterID, ActionReport,
		Subject{Kind: SubjectMessage, ID: messageID},
		map[string]any{"dialog": dialogID}); err != nil {
		return err
	}
	return wrapf(tx.Commit(ctx), "жалоба на письмо %d", messageID)
}

const mailReportsQuery = `
	SELECT r.id, r.created_at, r.reporter_id, rp.nick, r.author_id, au.nick,
	       r.message_id, r.dialog_id, r.quote, r.reason
	  FROM mail_reports r
	  JOIN users rp ON rp.id = r.reporter_id
	  JOIN users au ON au.id = r.author_id
	 WHERE r.resolved_at IS NULL
	 ORDER BY r.created_at LIMIT $1`

// MailReports — нерассмотренные жалобы на письма.
//
// Отдаёт РОВНО то, что видит модератор: ники, время, цитату и причину. Метода,
// который отдал бы саму переписку или соседние письма, здесь нет — и его
// отсутствие есть единственная надёжная форма обещания «модератор видит только
// процитированное в жалобе».
func (p *Platform) MailReports(ctx context.Context, limit int) ([]MailReport, error) {
	rows, err := p.pool.Query(ctx, mailReportsQuery, clampLimit(limit))
	if err != nil {
		return nil, wrapf(err, "жалобы на письма")
	}
	defer rows.Close()
	var out []MailReport
	for rows.Next() {
		var r MailReport
		if err := rows.Scan(&r.ID, &r.At, &r.ReporterID, &r.ReporterNick,
			&r.AuthorID, &r.AuthorNick, &r.MessageID, &r.DialogID, &r.Quote, &r.Reason); err != nil {
			return nil, wrapf(err, "жалобы на письма")
		}
		out = append(out, r)
	}
	return out, wrapf(rows.Err(), "жалобы на письма")
}

// ResolveMailReport — «разобрано».
//
// Отдельным действием журнала (ActionMailDone), а не общим «отклонено»:
// разобрать жалобу можно и запретив автору писать, и не сделав ничего, — а
// запись о бане стои́т рядом своей строкой и сама говорит, что было. Резолюция в
// журнал НЕ идёт по той же причине, по какой не идёт цитата: журнал переживает
// содержание письма, а пишет резолюцию человек, который цитату только что
// видел.
func (p *Platform) ResolveMailReport(ctx context.Context, actor Viewer, reportID int64, resolution string) error {
	if !actor.CanModerate() {
		return ErrNotModerator
	}
	resolution = trimReason(resolution)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return wrapf(err, "разбор жалобы %d", reportID)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	var messageID int64
	switch err := tx.QueryRow(ctx, `
		UPDATE mail_reports SET resolved_at = now(), resolved_by = $2, resolution = $3
		 WHERE id = $1 AND resolved_at IS NULL
		RETURNING message_id`, reportID, actor.UserID, resolution).Scan(&messageID); {
	case errors.Is(err, pgx.ErrNoRows):
		// И «нет такой жалобы», и «её уже разобрали» — одно и то же для того,
		// кто нажал: работать больше не над чем.
		return ErrNothingToDo
	case err != nil:
		return wrapf(err, "разбор жалобы %d", reportID)
	}
	if err := audit(ctx, tx, actor.UserID, ActionMailDone,
		Subject{Kind: SubjectMessage, ID: messageID},
		map[string]any{"report": reportID}); err != nil {
		return err
	}
	return wrapf(tx.Commit(ctx), "разбор жалобы %d", reportID)
}

// ------------------------------------------------------------------ уборка

// MailPruned — что убрала уборка переписки.
type MailPruned struct {
	Bodies   int // писем, у которых стёрто содержание
	Reports  int // жалоб, переживших своё письмо
	Messages int // строк писем, доживших до года
	Dialogs  int // опустевших переписок
}

// Any — уборка что-то сделала.
func (m MailPruned) Any() bool { return m.Bodies+m.Reports+m.Messages+m.Dialogs > 0 }

// purgeBodiesQuery — первый шаг уборки: гашение СОДЕРЖАНИЯ по сроку. Вынесен
// константой, как pendingQuery у шины, потому что его план проверяется тестом:
// ходит он раз в сутки, но по всей таблице писем, и без своего индекса это
// ночной полный перебор растущей таблицы — а исполнять сроки хранения площадка
// обязана каждую ночь, а не когда успеет.
const purgeBodiesQuery = `
	UPDATE mail_messages SET body = '', purged_at = now()
	 WHERE ctid IN (SELECT ctid FROM mail_messages
	                 WHERE purged_at IS NULL AND sent_at < now() - $1::interval
	                 LIMIT $2)`

// PruneMail исполняет сроки хранения. Не «когда-нибудь дойдут руки», а прямая
// обязанность оператора: держать содержание дольше полугода 152-ФЗ не разрешает
// ровно так же, как стирать раньше не разрешает 149-ФЗ.
//
// Порядок шагов несущий. Сперва гаснет СОДЕРЖАНИЕ, а строка остаётся жить —
// сведения о приёме-передаче хранятся год, и вторая таблица под них была бы
// вторым источником правды о том, кто кому писал. Следом уходят ЖАЛОБЫ, чьё
// письмо стёрто: цитата-снимок не вправе жить дольше оригинала — ради него она и
// снимок, чтобы пережить разбирательство, а не закон. Через год уходит сама
// строка письма, и последними — опустевшие переписки вместе со сторонами и
// событиями шины (каскадом).
func (p *Platform) PruneMail(ctx context.Context, limit int) (MailPruned, error) {
	var out MailPruned
	n := clampLimit(limit)
	// У шага жалоб своего СРОКА нет: он привязан не к календарю, а к тому, что
	// стало с письмом, — поэтому аргументы у шагов списком, а не общей парой
	// «возраст и порция». Подставить ему возраст ради единообразия значило бы
	// завести второй срок хранения цитаты, расходящийся с первым молча.
	steps := []struct {
		to   *int
		sql  string
		args []any
	}{
		{&out.Bodies, purgeBodiesQuery, []any{KeepMessageBody.String(), n}},
		// Жалоба уходит, как только у её письма не стало содержания, — и по
		// тому же условию, если строка письма уже удалена вовсе.
		{&out.Reports, `DELETE FROM mail_reports WHERE ctid IN (
		                  SELECT r.ctid FROM mail_reports r
		                   WHERE NOT EXISTS (SELECT 1 FROM mail_messages m
		                                      WHERE m.id = r.message_id AND m.purged_at IS NULL)
		                   LIMIT $1)`, []any{n}},
		{&out.Messages, `DELETE FROM mail_messages WHERE ctid IN (
		                   SELECT ctid FROM mail_messages
		                    WHERE sent_at < now() - $1::interval LIMIT $2)`,
			[]any{KeepMessageMeta.String(), n}},
		// Переписка уходит, только когда писем в ней не осталось: пустая строка
		// пары — это всё ещё сведения о том, что эти двое переписывались.
		{&out.Dialogs, `DELETE FROM mail_dialogs WHERE ctid IN (
		                  SELECT d.ctid FROM mail_dialogs d
		                   WHERE d.last_message_at < now() - $1::interval
		                     AND NOT EXISTS (SELECT 1 FROM mail_messages m WHERE m.dialog_id = d.id)
		                   LIMIT $2)`, []any{KeepMessageMeta.String(), n}},
	}
	for _, s := range steps {
		tag, err := p.pool.Exec(ctx, s.sql, s.args...)
		if err != nil {
			return out, wrapf(err, "уборка переписки")
		}
		*s.to = int(tag.RowsAffected())
	}
	return out, nil
}

// MailStats — наполнение переписки.
type MailStats struct {
	Dialogs  int
	Messages int
	Unread   int
	Blocks   int
	Reports  int // открытых жалоб
	// OldestBody — возраст самого старого письма, содержание которого ещё не
	// стёрто. Стоит в `platform doctor` не для красоты: уборка работает МОЛЧА, и
	// её молчаливый отказ означает нарушение срока хранения через полгода —
	// заметить это надо раньше, чем через полгода.
	OldestBody time.Duration
}

// MailStats считает наполнение переписки.
func (p *Platform) MailStats(ctx context.Context) (MailStats, error) {
	var s MailStats
	var oldest *time.Time
	err := p.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM mail_dialogs),
		       (SELECT count(*) FROM mail_messages),
		       (SELECT coalesce(sum(unread), 0) FROM mail_sides),
		       (SELECT count(*) FROM mail_blocks),
		       (SELECT count(*) FROM mail_reports WHERE resolved_at IS NULL),
		       (SELECT min(sent_at) FROM mail_messages WHERE purged_at IS NULL)`).
		Scan(&s.Dialogs, &s.Messages, &s.Unread, &s.Blocks, &s.Reports, &oldest)
	if err != nil {
		return s, wrapf(err, "сводка переписки")
	}
	if oldest != nil {
		s.OldestBody = time.Since(*oldest)
	}
	return s, nil
}

// ------------------------------------------------------------------ выдача

// ExportMail выгружает переписку человека потоком JSON — ДЛЯ ВЫДАЧИ ПО ЗАПРОСУ
// УПОЛНОМОЧЕННОГО ОРГАНА.
//
// Появляется вместе со сроками хранения, а не «когда понадобится»: обязанность
// ВЫДАТЬ и обязанность ХРАНИТЬ установлены одной статьёй и возникают в один
// день, а служба, умеющая только копить, — это площадка, которая в день первого
// запроса будет писать SQL руками и под давлением.
//
// Командой, а не кнопкой, и по двум причинам сразу. Во-первых, той же, что у
// ExportUser и AnonymizeUser: это поток на десятки мегабайт и действие
// администратора, а не страница. Во-вторых и главных: отвечает на запрос ЧЕЛОВЕК
// — он читает бумагу, решает, законна ли она, и отвечает своим именем. Команда
// лишь даёт ему, чем ответить.
//
// Отдаются ОБЕ стороны разговора: запрос всегда о переписке, а не о репликах
// одного, и половина её бессмысленна. Этим выдача и отличается от ExportUser,
// где текстов входящих писем нет намеренно.
//
// Пустые from и to означают «без границы»: сроки хранения и так держат нижнюю.
func (p *Platform) ExportMail(ctx context.Context, userID int64, from, to time.Time, w io.Writer) error {
	u, err := p.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	write := func(s string) error {
		_, err := io.WriteString(w, s)
		return err
	}
	if err := write("{\n\"выдано\": "); err != nil {
		return err
	}
	if err := enc.Encode(time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if err := write(",\n\"участник\": "); err != nil {
		return err
	}
	if err := enc.Encode(map[string]any{
		"id": u.ID, "ник": u.Nick, "вид": int(u.Kind), "заведён": u.CreatedAt,
	}); err != nil {
		return err
	}
	if err := write(",\n\"период\": "); err != nil {
		return err
	}
	if err := enc.Encode(map[string]any{"с": nullTime(from), "по": nullTime(to)}); err != nil {
		return err
	}
	// Аргументы у разделов РАЗНЫЕ: период сужает письма и не касается списка
	// переписок — сведения «эти двое переписывались» не делятся на месяцы.
	// Postgres считает параметры по наибольшему номеру в запросе и честно
	// отказывается принимать лишние, поэтому список, а не общая тройка.
	sections := []struct {
		name string
		sql  string
		args []any
	}{
		{"переписки", `SELECT to_jsonb(x) FROM (
			SELECT d.id AS переписка, s.peer_id AS собеседник, p2.nick AS ник_собеседника,
			       d.started_by AS первым_написал, d.created_at AS заведена,
			       d.last_message_at AS последнее_письмо, s.sent AS его_писем
			  FROM mail_sides s
			  JOIN mail_dialogs d ON d.id = s.dialog_id
			  JOIN users p2 ON p2.id = s.peer_id
			 WHERE s.user_id = $1 ORDER BY d.id) x`, []any{userID}},
		// Обе стороны: письма этого человека и письма ему. Содержание у стёртых
		// по сроку пусто, и об этом говорит отдельное поле, а не пустая строка,
		// — «письма нет» и «письмо было, срок вышел» для запроса не одно и то же.
		{"письма", `SELECT to_jsonb(x) FROM (
			SELECT m.id, m.dialog_id AS переписка, m.sender_id AS отправитель,
			       m.sent_at AS отправлено, m.body AS текст,
			       m.purged_at AS содержание_удалено_по_сроку
			  FROM mail_messages m
			  JOIN mail_sides s ON s.dialog_id = m.dialog_id AND s.user_id = $1
			 WHERE ($2::timestamptz IS NULL OR m.sent_at >= $2)
			   AND ($3::timestamptz IS NULL OR m.sent_at <= $3)
			 ORDER BY m.id) x`, []any{userID, nullTime(from), nullTime(to)}},
	}
	for _, s := range sections {
		if err := write(",\n\"" + s.name + "\": ["); err != nil {
			return err
		}
		if err := p.exportRows(ctx, w, s.sql, s.args...); err != nil {
			return fmt.Errorf("выдача переписки %d (%s): %w", userID, s.name, err)
		}
		if err := write("]"); err != nil {
			return err
		}
	}
	return write("\n}\n")
}

// nullTime — пустое время как отсутствие границы. Отдельной функцией, потому
// что то же значение уходит и в запрос, и в шапку выдачи: разойдись они,
// документ обещал бы период, которого не спрашивали.
func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// LogMailExport отмечает выдачу в журнале. ОБЯЗАТЕЛЬНО и отдельным вызовом, как
// LogExport: сама выдача — поток наружу, она может оборваться на середине, а
// запись «письма отданы» должна означать, что они действительно отданы.
//
// Действие своё (ActionMailExport), не общее с выгрузкой субъекту: журнал обязан
// отвечать на вопрос «кому и когда отдали ЧУЖИЕ письма» — единственный, с
// которым сюда придут.
func (p *Platform) LogMailExport(ctx context.Context, actor Viewer, userID int64, from, to time.Time) error {
	return audit(ctx, p.pool, actor.UserID, ActionMailExport, UserSubject(userID), map[string]any{
		"с": nullTime(from), "по": nullTime(to),
	})
}
