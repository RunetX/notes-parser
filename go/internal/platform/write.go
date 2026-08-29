package platform

// Запись: правила, общие для заметки и комментария.
//
// Главное правило площадки — УЧАСТНИК ТОЛЬКО ПИШЕТ. Удаление живёт у модератора,
// правка чужого текста — у администратора (EditNoteAsAdmin в moderation.go: она
// меняет чужие слова, а не убирает их из разговора, и потому дверь у неё выше),
// и единственное исключение — своя заметка первые десять минут и один раз.
// Причина не в экономии работы: тред это чужие ответы на твои слова, правка
// задним числом делает их бессмысленными, а удаление оставляет в ветке дыру. На
// НГС снос реплики — тоже действие МОДЕРАТОРА, и это часть той преемственности,
// ради которой всё затевается.
//
// «Убрать своё» у человека при этом есть, но рычаг другой — отзыв согласия на
// распространение (consent.go): он разом обезличивает все свои заметки, оставляя
// их вместе с тредами на виду. Это право субъекта по 152-ФЗ, а не редакторское
// удаление, и путать их нельзя: первое обязано работать самообслуживанием,
// второе — нет.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// EditWindow — сколько живёт окно правки своей заметки. Окно на опечатку, а не
// на смену позиции.
const EditWindow = 10 * time.Minute

// MaxNickRunes — потолок длины ника. На НГС длиннее не встречается, а подпись
// рисуется в узкой колонке автора.
const MaxNickRunes = 40

var (
	// ErrNotMember — писать может только вошедший участник. Тень (её завело
	// зеркало) автором у нас не считается: за ней никто не доказал владения.
	ErrNotMember = errors.New("публиковать может только участник")
	// ErrBanned — срок запрета ещё идёт.
	ErrBanned = errors.New("публикации запрещены до конца срока")
	// ErrConsentRevoked — человек сам отозвал согласие. Отличается от
	// ErrConsentOutdated намеренно: «подпишите новую редакцию» тому, кто нажал
	// «Отозвать» пять минут назад, звучит как поломка, а не как ответ.
	ErrConsentRevoked = errors.New("согласие отозвано: верните его, чтобы писать снова")
	// ErrConsentOutdated — согласия человека остались в прежней редакции. Не
	// «нет согласия», а «согласие не на те условия»: редакция меняется, когда
	// меняется обещание (23.08.2026 — открытие страниц поисковикам), и
	// публиковать под новыми условиями по старой подписи нельзя.
	ErrConsentOutdated = errors.New("согласия обновились: подпишите новую редакцию")
	// ErrRateLimited — слишком часто.
	ErrRateLimited = errors.New("слишком часто")
	// ErrThreadLocked — обсуждение закрыл НАШ модератор (а не отметка НГС).
	ErrThreadLocked = errors.New("обсуждение закрыто")
	// ErrEditWindowClosed — заметку править уже нельзя.
	ErrEditWindowClosed = errors.New("окно правки закрыто")
	// ErrNotYours — чужая запись.
	ErrNotYours = errors.New("это не ваша запись")
	// ErrBadNick — ник не годится.
	ErrBadNick = errors.New("ник не годится")
	// ErrStageClosed — заметка-песочница: в ней говорят жители (эпик «народ»), а
	// участник её только читает. От ErrThreadLocked отличается тем, что там
	// разговор КОНЧЕН для всех, а здесь он идёт — просто не с нами.
	ErrStageClosed = errors.New("в этой заметке пишут только её жители")
	// ErrPersonaOffStage — обратная сторона того же правила: житель пишет ТОЛЬКО
	// в песочнице. Человеку эта ошибка не показывается никогда — её видит служба
	// народа, и означает она сбой в ней, а не отказ пользователю.
	ErrPersonaOffStage = errors.New("житель пишет только в песочнице")
)

// Виды объектов очереди проверки. Строками, а не числами: очередь читают люди и
// SQL, и «note» понятнее нуля.
const (
	SubjectNote    = "note"
	SubjectComment = "comment"
)

// rateRule — не больше Max публикаций за Window.
type rateRule struct {
	Window time.Duration
	Max    int
}

// Пороги частоты. Меряются по НАТИВНЫМ публикациям автора: зеркальный след
// прошлых лет к тому, как часто человек пишет здесь, отношения не имеет.
//
// Числа взяты из плана эпика E и защищают от шторма, а не от разговорчивости:
// тридцать реплик в час — это вдвое больше, чем пишет самый быстрый комментатор
// зеркала в свой самый людный час.
var (
	noteRates    = []rateRule{{5 * time.Minute, 1}, {24 * time.Hour, 5}}
	commentRates = []rateRule{{10 * time.Second, 1}, {time.Hour, 30}}
)

const (
	notesRecentQuery    = `SELECT count(*) FROM notes    WHERE author_id = $1 AND id >= $2 AND published_at > $3`
	commentsRecentQuery = `SELECT count(*) FROM comments WHERE author_id = $1 AND id >= $2 AND published_at > $3`
)

// writeGuard — общая проверка «этому человеку сейчас можно публиковать».
//
// Читается ВНУТРИ той же транзакции, что и вставка: иначе между проверкой и
// записью успевает пройти бан или отзыв согласия, и запрет исполнится с
// задержкой в одну публикацию.
func writeGuard(ctx context.Context, q querier, userID int64) error {
	var (
		kind       Kind
		banned     *time.Time
		anonymized *time.Time
	)
	err := q.QueryRow(ctx,
		`SELECT kind, banned_until, anonymized_at FROM users WHERE id = $1`, userID).
		Scan(&kind, &banned, &anonymized)
	if err != nil {
		return fmt.Errorf("проверка автора %d: %w", userID, err)
	}
	switch {
	case anonymized != nil, kind != KindMember:
		return ErrNotMember
	case banned != nil && banned.After(time.Now()):
		return ErrBanned
	}
	return nil
}

// publishGuard — writeGuard плюс действующая редакция согласий. Стоит он на
// путях, где появляется ЧУЖОЙ ТЕКСТ на виду у всех: заметка, комментарий,
// правка своей заметки.
//
// Реакция и жалоба через него не идут намеренно. Реакцию не видит никто, кроме
// счётчика («кто нажал» не показывается вовсе), а жалоба — обращение к
// модератору, а не публикация; требовать за них новую подпись значило бы
// закрыть человеку жалобу на то самое изменение, о котором его и просят
// подписаться.
func publishGuard(ctx context.Context, q querier, userID int64) error {
	if err := writeGuard(ctx, q, userID); err != nil {
		return err
	}
	return consentGuard(ctx, q, userID)
}

// stageGuard — ПРАВИЛО ПЕСОЧНИЦЫ, обе его стороны разом.
//
// Сторон две, и держать их вместе важнее, чем каждую по отдельности:
//
//   - в песочнице пишут только ЖИТЕЛЬ и АДМИНИСТРАТОР. Второй нужен не для
//     полноты — садовник обязан уметь войти в разговор своих жителей, не
//     открывая песочницу всем. Модератору при этом не позволено: он решает про
//     СЛОВА, а участвовать в разговоре — не его роль;
//   - житель пишет ТОЛЬКО в песочнице.
//
// Вторая половина и есть та, ради которой правило собрано в одном месте.
// Благодаря ей «машинная реплика» и «песочница» перестают быть двумя разными
// вопросами: где нет песочницы, там нет и машинных реплик, — и всё, что стоит
// ниже по течению (исходящий обход в каналы, недельная сводка, поводы шины),
// вправе спрашивать про ОДНУ заметку вместо того, чтобы join'ить users к
// десяти миллионам комментариев ради условия, которое почти всегда истинно.
//
// Разблокировка песочницы для аудитории — это снятие ПЕРВОЙ половины. Вторая
// остаётся: она про то, где живут жители, а не про то, кого туда пускают.
func stageGuard(ctx context.Context, q querier, userID int64, stage bool) error {
	var (
		persona bool
		role    Role
	)
	if err := q.QueryRow(ctx,
		`SELECT persona, role FROM users WHERE id = $1`, userID).Scan(&persona, &role); err != nil {
		return fmt.Errorf("право писать в песочнице %d: %w", userID, err)
	}
	switch {
	case stage && !persona && role != RoleAdmin:
		return ErrStageClosed
	case !stage && persona:
		return ErrPersonaOffStage
	}
	return nil
}

// isPersona — житель ли это (эпик «народ»). Отдельным запросом, а не полем в
// writeGuard: спрашивают про него ровно два места, и оба — про согласия и про
// песочницу, а не про право писать вообще.
func isPersona(ctx context.Context, q querier, userID int64) (bool, error) {
	var persona bool
	if err := q.QueryRow(ctx, `SELECT persona FROM users WHERE id = $1`, userID).Scan(&persona); err != nil {
		return false, fmt.Errorf("признак жителя %d: %w", userID, err)
	}
	return persona, nil
}

// consentGuard — публиковать можно только по ДЕЙСТВУЮЩЕЙ редакции согласий.
//
// Проверка появилась вместе с открытием площадки поисковикам (23.08.2026):
// выпустить новую редакцию и не спросить её — значит выпустить бумагу, которая
// ничего не меняет, а условия распространения тем временем стали другими. Стоит
// она в ядре, а не в форме, ровно потому, что писать можно не только с сайта:
// ответ из телеграма приходит тем же путём (bridge), и второй список правил там
// однажды разошёлся бы с этим.
//
// Читается той же транзакцией и тем же приёмом, что и остальной writeGuard:
// последняя запись по каждому виду.
//
// Отозванное и устаревшее различаются, и это не косметика. Отзыв согласия
// человек нажимает САМ и знает, что нажал, — а раньше про него отвечал
// writeGuard («ваши публикации скрыты»), потому что отзыв поднимал рубильник
// hide_all. Рубильника с 25.08.2026 нет вовсе (отзыв обезличивает заметки, а не
// прячет их), и различать эти два случая стало некому, кроме как здесь.
func consentGuard(ctx context.Context, q querier, userID int64) error {
	// ЖИТЕЛЬ СОГЛАСИЙ НЕ ПОДПИСЫВАЕТ, и это не послабление, а единственный
	// возможный ответ. Согласие на обработку персональных данных даёт СУБЪЕКТ —
	// живой человек, чьи данные обрабатывают; у персонажа персональных данных
	// нет, и строка от его имени в `consents` была бы записью в доказательственной
	// таблице о согласии, которого никто не давал, то есть подделкой ровно там,
	// где площадка обязана быть безупречной.
	//
	// Право писать ему даёт не подпись, а ПРИЗНАК, введённый оператором
	// сознательно (users.persona, миграция 0018): за всё, что житель напишет,
	// отвечает оператор, а не житель.
	//
	// Исключение стоит здесь, а не в вызывающих: путей публикации несколько
	// (форма, мост из мессенджера, служба народа), и второй список правил рядом
	// с этим однажды разошёлся бы — тот же довод, по которому сюда переехала и
	// сама проверка редакции.
	persona, err := isPersona(ctx, q, userID)
	if err != nil {
		return err
	}
	if persona {
		return nil
	}
	want, err := currentConsentVersions()
	if err != nil {
		return err
	}
	rows, err := q.Query(ctx, `
		SELECT DISTINCT ON (kind) kind, version, revoked_at IS NOT NULL
		  FROM consents WHERE user_id = $1
		 ORDER BY kind, granted_at DESC`, userID)
	if err != nil {
		return fmt.Errorf("согласия автора %d: %w", userID, err)
	}
	have := make(map[string]int, 2)
	revoked := make(map[string]bool, 2)
	for rows.Next() {
		var kind string
		var version int
		var isRevoked bool
		if err := rows.Scan(&kind, &version, &isRevoked); err != nil {
			rows.Close()
			return fmt.Errorf("согласия автора %d: %w", userID, err)
		}
		if isRevoked {
			revoked[kind] = true
			continue
		}
		have[kind] = version
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("согласия автора %d: %w", userID, err)
	}
	// Отзыв спрашивается первым проходом по ВСЕМУ списку, а не по ходу дела:
	// порядок обхода map случаен, и человек с отозванным одним согласием и
	// устаревшим другим получал бы то один ответ, то другой.
	for kind := range want {
		if revoked[kind] {
			return ErrConsentRevoked
		}
	}
	for kind, version := range want {
		if have[kind] < version {
			return ErrConsentOutdated
		}
	}
	return nil
}

// MayPublishNote — можно ли этому человеку сейчас опубликовать заметку.
//
// Тот же набор правил, что и внутри CreateNote, но ЧТЕНИЕМ и ЗАРАНЕЕ. Заводится
// не ради экономии запросов: настоящая проверка всё равно стоит в транзакции и
// остаётся единственной, которой можно верить.
//
// Нужна она из-за картинок. Байты ложатся на диск ДО транзакции (иначе строка
// note_images ссылалась бы на файл, которого нет), а уборки каталога у площадки
// нет вовсе — значит каждая ОТКЛОНЁННАЯ публикация оставляла бы файл навсегда.
// Порог частоты у заметки — одна в пять минут, то есть отклонённых попыток может
// быть сколько угодно: потолок морды пропускает примерно одну в десять секунд, и
// это до двух с половиной гигабайт мусора в сутки с одного адреса. Не
// теоретическая дыра, а способ забить диск.
//
// Гонка с настоящей проверкой остаётся: человека могли забанить в те сто
// миллисекунд, что идут между ними. Один осиротевший файл — приемлемая цена,
// две с половиной тысячи в сутки — нет.
func (p *Platform) MayPublishNote(ctx context.Context, userID int64) error {
	if userID == 0 {
		return ErrNotMember
	}
	if err := publishGuard(ctx, p.pool, userID); err != nil {
		return err
	}
	return enforceRate(ctx, p.pool, notesRecentQuery, userID, time.Now(), noteRates)
}

// enforceRate проверяет пороги частоты по нативным публикациям автора.
func enforceRate(ctx context.Context, q querier, query string, authorID int64, now time.Time, rules []rateRule) error {
	for _, r := range rules {
		var n int
		if err := q.QueryRow(ctx, query, authorID, NativeIDBase, now.Add(-r.Window)).Scan(&n); err != nil {
			return fmt.Errorf("частота публикаций автора %d: %w", authorID, err)
		}
		if n >= r.Max {
			return ErrRateLimited
		}
	}
	return nil
}

// enqueueCheck ставит публикацию в очередь проверки. Той же транзакцией, что и
// сама публикация: «опубликовано, но в очередь не попало» — состояние, которого
// не должно быть вовсе.
//
// noteID хранится рядом, хотя у комментария он выводится из самой строки: по
// нему очередь строит ссылку на место в треде, а лезть за ней в comments на
// каждую строку показа незачем — заметка у комментария не меняется никогда.
//
// Повторная постановка (правка заметки в окне) СБРАСЫВАЕТ и решение человека:
// текст стал другим, и прежний вердикт относился не к нему. Обжалование при
// этом тоже снимается — обжаловать нечего, публикация снова видима.
//
// ПУБЛИКАЦИИ АДМИНИСТРАЦИИ в очередь не идут вовсе (решение владельца
// 23.08.2026). Довод не в доверии, а в том, что автомат над ними бессилен по
// устройству: единственное его право — скрыть, а модератор снимает скрытие
// одним нажатием, — значит очередь получала бы шум вместо надзора. Замер это и
// показал: из пятнадцати автоскрытий на 738 строках пять пришлись на объявления
// площадки о самой себе, включая реплики со ссылкой на нашу же справку
// `t3h.ru/help`, названные «ссылкой на сторонний сайт».
//
// Условие стоит В ЗАПРОСЕ, а не отдельным походом в users, потому что очередь
// заводится ТОЙ ЖЕ транзакцией, что и публикация: лишний round-trip здесь — это
// удлинение самой горячей транзакции площадки.
func enqueueCheck(ctx context.Context, q querier, kind string, id, noteID, authorID int64) error {
	_, err := q.Exec(ctx, `
		INSERT INTO moderation_queue (subject_kind, subject_id, note_id, author_id)
		SELECT $1, $2, $3, $4
		 WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = $4 AND u.role >= $5)
		ON CONFLICT (subject_kind, subject_id) DO UPDATE
		   SET queued_at = now(), checked_at = NULL, verdict = NULL, attempts = 0,
		       category = '', reason = '', quote = '', model = '', prompt_sha = NULL,
		       appealed_at = NULL, decided_at = NULL, decided_by = NULL, decision = NULL`,
		kind, id, nullID(noteID), nullID(authorID), RoleModerator)
	return wrapf(err, "очередь проверки %s %d", kind, id)
}

// EditNote правит СВОЮ заметку в окне после публикации.
//
// Окно закрывают три вещи, и любая из них — насовсем: десять минут, первый
// комментарий и уже сделанная правка. Комментарий здесь важнее времени: текст,
// изменившийся под чужим ответом, выставляет ответившего дураком, и никакой
// таймер этого не исправляет. Однократность — из того же соображения: «поправить
// опечатку» это одно действие, а серия правок в окне есть та же смена позиции,
// только мелкими шагами.
//
// Правило целиком выражено состоянием строки, поэтому пережить рестарт и гонку
// ему нечем не помочь: edited_at и comment_count лежат в той же таблице и берутся
// под FOR UPDATE.
func (p *Platform) EditNote(ctx context.Context, userID int64, in NoteEdit) error {
	noteID := in.NoteID
	body, err := cleanBody(in.Body)
	if err != nil {
		return err
	}
	if userID == 0 {
		return ErrNotYours
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("правка заметки %d: %w", noteID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	var (
		author    *int64
		status    Status
		count     int
		published time.Time
		edited    *time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT author_id, status, comment_count, published_at, edited_at
		  FROM notes WHERE id = $1 FOR UPDATE`, noteID).
		Scan(&author, &status, &count, &published, &edited)
	if err != nil {
		return fmt.Errorf("правка заметки %d: %w", noteID, err)
	}
	// Автор проверяется по НАСТОЯЩЕМУ author_id, поэтому правило одинаково
	// работает и для анонимной заметки: она хранит своего автора.
	if idOf(author) != userID {
		return ErrNotYours
	}
	switch {
	case status != StatusVisible:
		return ErrNotFound
	case !IsNative(noteID):
		// Зеркальную заметку писали не у нас, и править её тут значило бы
		// расходиться с оригиналом молча.
		return ErrEditWindowClosed
	case count > 0, edited != nil, time.Since(published) >= EditWindow:
		return ErrEditWindowClosed
	}
	if _, err := tx.Exec(ctx,
		`UPDATE notes SET body = $2, edited_at = now() WHERE id = $1`, noteID, body); err != nil {
		return fmt.Errorf("правка заметки %d: %w", noteID, err)
	}
	// Снятие картинки — то же одно действие, что и правка текста, поэтому та же
	// транзакция. Строка media и файл на диске при этом остаются: хранилище
	// адресуемо содержимым, тот же файл может быть привязан к другой заметке
	// или стоять у кого-то аватаром, и «убрать своё» означало бы сломать чужое.
	if in.DropImage {
		if _, err := tx.Exec(ctx, `DELETE FROM note_images WHERE note_id = $1`, noteID); err != nil {
			return fmt.Errorf("снятие иллюстрации заметки %d: %w", noteID, err)
		}
	}
	// Текст стал другим — прежняя проверка к нему больше не относится.
	if err := enqueueCheck(ctx, tx, SubjectNote, noteID, noteID, userID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("правка заметки %d: %w", noteID, err)
	}
	return nil
}

// SetOwnNick — смена собственного ника участником.
//
// Отдельно от SetNick (та служебная, без проверок): здесь текст пришёл от
// человека, и вдобавок поднимается nick_custom — с этого момента вход по анкете
// НГС ник больше не переписывает. Без флага обещание «ник вы меняете сами» из
// текста согласия отменялось бы следующим же входом.
func (p *Platform) SetOwnNick(ctx context.Context, userID int64, nick string) error {
	nick, err := cleanNick(nick)
	if err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE users SET nick = $2, nick_custom = true
		 WHERE id = $1 AND kind = $3 AND anonymized_at IS NULL`, userID, nick, KindMember)
	if err != nil {
		return fmt.Errorf("смена ника %d: %w", userID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	return nil
}

// cleanNick приводит ник к виду, в котором его можно показать. Пробелы внутри
// разрешены (на НГС такие ники обычны), а вот управляющие знаки — нет: ими
// подпись ломается или подделывается под чужую.
func cleanNick(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "", fmt.Errorf("%w: пусто", ErrBadNick)
	}
	if utf8.RuneCountInString(s) > MaxNickRunes {
		return "", fmt.Errorf("%w: длиннее %d знаков", ErrBadNick, MaxNickRunes)
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", fmt.Errorf("%w: невидимые знаки", ErrBadNick)
		}
	}
	return s, nil
}
