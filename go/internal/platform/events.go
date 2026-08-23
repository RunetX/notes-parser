package platform

// Шина событий: факт, повод, канал.
//
// Три слова названы отдельно, потому что путать их дорого. ФАКТ — что
// случилось; он один, у него нет адресата и он остаётся фактом, даже если
// сказать о нём некому. ПОВОД — почему это интересно конкретному человеку;
// поводов у одного факта бывает несколько или ни одного, и считаются они
// правилами (fanOutRules), а не записываются пишущим. КАНАЛ — где человек это
// увидит; каналов у площадки два (страница событий и живое обновление), и ядру
// о них не известно ничего.
//
// ГЛАВНОЕ ПРАВИЛО ЖУРНАЛА — то же, что в шапке миграции 0012, и повторено здесь
// намеренно: в events НЕТ КОЛОНКИ СО СВОБОДНЫМ ТЕКСТОМ ОТ УЧАСТНИКА. Событие
// ссылается на публикацию, открытую всем и так, а details содержит только наше
// служебное (код реакции, категория скрытия, срок запрета). Стоит появиться
// колонке, куда один человек пишет другому произвольные слова, — и площадка
// становится сервисом обмена сообщениями, то есть 149-ФЗ и реестром ОРИ. Это не
// вопрос вкуса и не задача на будущее: отсутствие такой колонки и есть то, чем
// граница держится.
//
// Почему запись факта и раздача поводов разведены. Факт пишется ТОЙ ЖЕ
// транзакцией, что и действие (см. recordEvent рядом с enqueueCheck и audit) —
// «ответили, а повода нет» это состояние, которого не должно существовать.
// Раздача идёт ФОНОМ, потому что правил адресации больше одного и они дорожают:
// упоминание уже требует обхода участников треда, а держать растущий SELECT
// внутри транзакции пишущего значит платить его временем за удобство читающих.
//
// Отношение к audit_log. Записи похожи, и слить их соблазнительно, но у них
// разная аудитория и разное правило отбора: журнал модерации пишет ВСЁ и
// адресован владельцу, шина — только то, о чём уместно сказать участнику. Слив
// их, мы получили бы либо уведомления о смене чужой роли, либо аудит с дырами.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// EventKind — вид факта. Список закрытый: событие, о котором нельзя сказать, как
// его показать человеку, шине не нужно.
type EventKind int16

const (
	EventNote     EventKind = 1 // опубликована заметка
	EventComment  EventKind = 2 // опубликован комментарий
	EventHidden   EventKind = 3 // публикацию скрыли (модератор или автомат)
	EventRestored EventKind = 4 // публикацию вернули
	EventBanned   EventKind = 5 // человеку запретили публиковать
	EventUnbanned EventKind = 6 // запрет снят
	EventReaction EventKind = 7 // на публикацию поставили реакции
)

// Reason — почему факт стал поводом ИМЕННО ДЛЯ ЭТОГО человека.
//
// Порядок значений — это ещё и старшинство: у одного факта поводов бывает
// несколько (ответили и упомянули одновременно), а строка на человека и факт
// ровно одна, поэтому раздача идёт от самого точного повода к самому общему, а
// ключ (user_id, event_id) оставляет первый. То же правило, по которому в
// мессенджерах подписка на заметку бьёт совпадение по слову: точное намерение
// человека важнее случайного совпадения.
type Reason int16

const (
	ReasonReplyToComment Reason = 1 // ответили на вашу реплику
	ReasonReplyToNote    Reason = 2 // ответили на вашу заметку
	ReasonMention        Reason = 3 // вас упомянули по нику
	ReasonAboutYou       Reason = 4 // решение о вас или о вашей публикации
	ReasonReaction       Reason = 5 // вашу публикацию отметили
)

const (
	// EventHorizon — насколько свежей должна быть ЗЕРКАЛЬНАЯ публикация, чтобы о
	// ней имело смысл говорить.
	//
	// Правило про ДАННЫЕ, а не про вызывающего, и это принципиально. Один и тот
	// же приём (IngestComment) обслуживает живой поток зеркала, догоняющую
	// сверку и — на пустой площадке — перенос всего зеркала целиком. Флаг
	// «я живой» пришлось бы верно проставить в каждой точке вызова, включая ту,
	// которую напишут через год; свежесть же сама отсекает перенос реплик 2014
	// года и сама пропускает сверку, догоняющую двадцатиминутный отказ Postgres.
	EventHorizon = time.Hour

	// excerptRunes — длина выдержки в списке событий. Строка списка, а не
	// цитата: чтобы понять, о чём речь, и решить, идти ли в тред.
	excerptRunes = 200

	// mentionMinRunes — короче этого ник в упоминание не считается. Ник из
	// одной-двух букв совпадает со случайным словом в каждом втором тексте, и
	// его владелец получал бы повод от всей площадки.
	mentionMinRunes = 3

	// reactionMergeWindow — в течение какого срока новые реакции на тот же
	// объект дописываются в уже стоящий повод, а не заводят новый.
	reactionMergeWindow = 24 * time.Hour
)

// Сроки хранения. Журнал о том, кто кому отвечал, — сведения о людях, и держать
// их дольше нужного не разрешает уже не вкус, а 152-ФЗ (обработка в объёме и
// сроке, соразмерных цели). Цель здесь короткая: сказать человеку, что
// произошло, пока это ему интересно.
const (
	KeepRead   = 30 * 24 * time.Hour  // прочитанный повод
	KeepUnread = 180 * 24 * time.Hour // непрочитанный: полгода — это «не заходил»
	KeepEvents = 90 * 24 * time.Hour  // факт, поводов по которому не осталось
)

// newEvent — факт на запись.
type newEvent struct {
	Kind      EventKind
	ActorID   int64 // 0 — машина, аноним или зеркальный комментатор без анкеты
	SubjectID int64 // о ком факт; 0 у тредовых
	NoteID    int64
	CommentID int64
	Details   map[string]any
}

// errNoEventKind — страховка на случай, если вид факта забыли назвать. Отказ
// здесь честнее, чем строка журнала с нулевым видом: её никто никогда не
// покажет, а найдётся она через год.
var errNoEventKind = errors.New("событие без вида")

// recordEvent пишет факт. Сосед enqueueCheck и audit: всегда той же
// транзакцией, что и само действие.
func recordEvent(ctx context.Context, q querier, e newEvent) error {
	if e.Kind == 0 {
		return errNoEventKind
	}
	raw := []byte("{}")
	if len(e.Details) > 0 {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return fmt.Errorf("событие %d: %w", e.Kind, err)
		}
		raw = b
	}
	_, err := q.Exec(ctx, `
		INSERT INTO events (kind, actor_id, subject_user_id, note_id, comment_id, details)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		e.Kind, nullID(e.ActorID), nullID(e.SubjectID),
		nullID(e.NoteID), nullID(e.CommentID), raw)
	return wrapf(err, "событие %d", e.Kind)
}

// worthTelling — о зеркальной публикации этого возраста ещё имеет смысл
// говорить. См. EventHorizon: правило о данных, а не о вызывающем.
func worthTelling(publishedAt time.Time) bool {
	return time.Since(publishedAt) < EventHorizon
}

// dropUnreadAbout убирает НЕПРОЧИТАННЫЕ поводы прийти и прочитать публикацию,
// которую только что скрыли.
//
// Исполнять, а не проверять на показе. Проверка join'ом к публикации тоже
// работает и стоит недорого — но она из того рода, который однажды убирают «для
// скорости», и площадка начинает звать людей читать скрытое. Прочитанные поводы
// при этом остаются: человек их уже видел, и переписывать его прошлое хуже, чем
// оставить ссылку, которая честно скажет «запись скрыта».
//
// Виды перечислены явно: убираются приглашения ЧИТАТЬ, а не сообщения о
// решениях. Событие о самом скрытии (EventHidden) ссылается на ту же публикацию
// и попало бы под тот же DELETE — а это ровно тот повод, ради которого всё и
// затевалось.
func dropUnreadAbout(ctx context.Context, q querier, s Subject) error {
	kinds := []EventKind{EventComment, EventNote, EventReaction}
	sql := `DELETE FROM notifications n USING events e
	         WHERE n.event_id = e.id AND n.read_at IS NULL
	           AND e.kind = ANY($2) AND e.comment_id = $1`
	if s.IsNote() {
		sql = `DELETE FROM notifications n USING events e
		        WHERE n.event_id = e.id AND n.read_at IS NULL
		          AND e.kind = ANY($2) AND e.note_id = $1 AND e.comment_id IS NULL`
	}
	_, err := q.Exec(ctx, sql, s.ID, kinds)
	return wrapf(err, "снятие поводов о %s", s)
}

// dropUserEvents убирает из шины всё, что связано с человеком. Зовётся
// обезличиванием.
//
// Удаляется, а не переезжает на могилу, как публикации, и разница
// принципиальна: событие «X ответил Y» — это связь между двумя людьми, и
// перенеся её на могилу, мы своей же рукой сохранили бы ту дополнительную
// информацию, отсутствие которой и делает обезличивание обезличиванием.
func dropUserEvents(ctx context.Context, q querier, userID int64) error {
	if _, err := q.Exec(ctx,
		`DELETE FROM notifications WHERE user_id = $1`, userID); err != nil {
		return wrapf(err, "снятие поводов %d", userID)
	}
	_, err := q.Exec(ctx,
		`DELETE FROM events WHERE actor_id = $1 OR subject_user_id = $1`, userID)
	return wrapf(err, "снятие событий %d", userID)
}

// subjectRefs — на что ссылается событие о публикации. У заметки комментария
// нет, у комментария заметка есть всегда: по ней страница событий строит ссылку,
// не заглядывая в comments.
func subjectRefs(s Subject, noteID int64) (note, comment int64) {
	if s.IsNote() {
		return s.ID, 0
	}
	return noteID, s.ID
}

// hideEvent собирает факт о решении модерации по публикации: скрыли или
// вернули. Одна сборка на три места (решение модератора, решение по очереди,
// автоскрытие), потому что расходиться им нельзя: автор обязан узнать об одном и
// том же одинаково, кто бы ни нажал.
//
// Причина модератора попадает в details — и это ЕДИНСТВЕННОЕ исключение из
// правила «в журнале нет чужого текста». Оправдано оно тем, что текст этот наш
// служебный (его пишет модератор площадки, а не участник участнику) и автору он
// и так показывается на /me: скрыть реплику, не сказав за что, — это то же
// молчаливое исчезновение, против которого заведена кнопка пересмотра.
func hideEvent(kind EventKind, actor int64, s Subject, f subjectFacts, category, reason string) newEvent {
	note, comment := subjectRefs(s, f.NoteID)
	details := map[string]any{}
	if reason != "" {
		details["reason"] = reason
	}
	if category != "" {
		details["category"] = category
	}
	return newEvent{
		Kind: kind, ActorID: actor, SubjectID: f.AuthorID,
		NoteID: note, CommentID: comment, Details: details,
	}
}

// ---------------------------------------------------------------- раздача

// fanRule — одно правило адресации: кому этот факт повод.
//
// Правила живут SQL'ем и одним списком, потому что «кто узнает о событии» — это
// и есть содержание шины, и читаться оно должно целиком, за один взгляд.
type fanRule struct {
	name   string
	reason Reason
	// kinds — виды фактов, на которые смотрит правило. Списком, а не одним
	// значением: «решение о вас» покрывает сразу четыре, и перечислить их явно
	// честнее, чем не спрашивать вид вовсе, — так видно, о чём человеку скажут.
	kinds []EventKind
	sql   string
}

// fanOutRules — все правила v1, В ПОРЯДКЕ СТАРШИНСТВА (см. Reason).
//
// $1 — список фактов, $2 — повод, $3 — виды фактов, $4 — KindMember.
//
// Общие оговорки, повторяющиеся в каждом правиле и потому названные здесь:
//   - получатель обязан быть УЧАСТНИКОМ (kind = member): тень уведомлять некуда,
//     она сюда не входила и согласия не давала;
//   - обезличенному не говорят ничего: его больше нет;
//   - сам себе повода не бывает — ответивший на свою же реплику молчания и ждёт;
//   - скрытая публикация поводов не порождает вовсе (status = 0), поэтому
//     автомат модерации, успевший сработать до раздачи, гасит и уведомление.
var fanOutRules = []fanRule{{
	name:   "ответ на реплику",
	reason: ReasonReplyToComment,
	kinds:  []EventKind{EventComment},
	sql: `INSERT INTO notifications (user_id, event_id, reason)
	      SELECT p.author_id, e.id, $2::smallint
	        FROM events e
	        JOIN comments c ON c.id = e.comment_id
	        JOIN comments p ON p.id = c.reply_to_id
	        JOIN users    u ON u.id = p.author_id
	       WHERE e.id = ANY($1) AND e.kind = ANY($3) AND c.status = 0
	         AND p.author_id IS DISTINCT FROM c.author_id
	         AND u.kind = $4 AND u.anonymized_at IS NULL
	      ON CONFLICT DO NOTHING`,
}, {
	// Автору заметки — только про ответы САМОЙ ЗАМЕТКЕ, а не про весь тред.
	// «Мне ответили» и «в моём треде кто-то говорит» — разные вопросы: под
	// заметкой с девятью сотнями реплик второе это не уведомление, а рассылка.
	// Захотевшему следить за тредом целиком нужна подписка, а не повод.
	name:   "ответ на заметку",
	reason: ReasonReplyToNote,
	kinds:  []EventKind{EventComment},
	sql: `INSERT INTO notifications (user_id, event_id, reason)
	      SELECT n.author_id, e.id, $2::smallint
	        FROM events e
	        JOIN comments c ON c.id = e.comment_id
	        JOIN notes    n ON n.id = c.note_id
	        JOIN users    u ON u.id = n.author_id
	       WHERE e.id = ANY($1) AND e.kind = ANY($3) AND c.status = 0
	         AND c.reply_to_id IS NULL
	         AND n.author_id IS DISTINCT FROM c.author_id
	         AND u.kind = $4 AND u.anonymized_at IS NULL
	      ON CONFLICT DO NOTHING`,
}, {
	// Упоминание. Ник ищется СЛОВОМ (текст разбирает Postgres), а не подстрокой:
	// подстрокой ник «Ян» находится в «январе».
	//
	// Круг сужен дважды, и оба раза по делу. Во-первых, откликается только
	// УЧАСТНИК — их десятки, и поиск идёт по готовому индексу users_nick_lower,
	// а не по десяти миллионам строк. Во-вторых, участник обязан быть в этом
	// треде своим: иначе слово «лампочка» в чужом разговоре дёргало бы человека
	// с таким ником через всю площадку.
	//
	// Ник с пробелом внутри так не находится, и это принятая цена: разбор на
	// слова стоит одного выражения, а сшивание словосочетаний обратно — своего
	// разбора со своими ошибками. Обращаются же на площадке кнопкой «ответить»,
	// и у такого обращения есть повод точнее (ReasonReplyToComment).
	name:   "упоминание",
	reason: ReasonMention,
	kinds:  []EventKind{EventComment},
	sql: `INSERT INTO notifications (user_id, event_id, reason)
	      SELECT DISTINCT u.id, e.id, $2::smallint
	        FROM events e
	        JOIN comments c ON c.id = e.comment_id
	        CROSS JOIN LATERAL regexp_split_to_table(lower(c.body), '[^[:alnum:]_]+') AS w(word)
	        JOIN users u ON lower(u.nick) = w.word
	       WHERE e.id = ANY($1) AND e.kind = ANY($3) AND c.status = 0
	         AND u.id IS DISTINCT FROM c.author_id
	         AND u.kind = $4 AND u.anonymized_at IS NULL
	         AND char_length(w.word) >= $5
	         AND (EXISTS (SELECT 1 FROM comments q
	                       WHERE q.note_id = c.note_id AND q.author_id = u.id AND q.status = 0)
	           OR EXISTS (SELECT 1 FROM notes nn
	                       WHERE nn.id = c.note_id AND nn.author_id = u.id))
	      ON CONFLICT DO NOTHING`,
}, {
	// Решения о человеке: скрытие, возврат, запрет, снятие запрета. Вид факта
	// здесь не спрашивается — адресат назван в самой строке, и правило одно на
	// все четыре. Себе решение поводом не становится: модератор, скрывший
	// собственную реплику, помнит об этом и так.
	name:   "решение о вас",
	reason: ReasonAboutYou,
	kinds:  []EventKind{EventHidden, EventRestored, EventBanned, EventUnbanned},
	sql: `INSERT INTO notifications (user_id, event_id, reason)
	      SELECT e.subject_user_id, e.id, $2::smallint
	        FROM events e JOIN users u ON u.id = e.subject_user_id
	       WHERE e.id = ANY($1) AND e.kind = ANY($3) AND e.subject_user_id IS NOT NULL
	         AND e.actor_id IS DISTINCT FROM e.subject_user_id
	         AND u.kind = $4 AND u.anonymized_at IS NULL
	      ON CONFLICT DO NOTHING`,
}, {
	name:   "реакция",
	reason: ReasonReaction,
	kinds:  []EventKind{EventReaction},
	sql: `INSERT INTO notifications (user_id, event_id, reason)
	      SELECT coalesce(c.author_id, n.author_id), e.id, $2::smallint
	        FROM events e
	        LEFT JOIN comments c ON c.id = e.comment_id
	        LEFT JOIN notes    n ON n.id = e.note_id
	        JOIN users u ON u.id = coalesce(c.author_id, n.author_id)
	       WHERE e.id = ANY($1) AND e.kind = ANY($3)
	         AND coalesce(c.status, n.status, 1) = 0
	         AND u.kind = $4 AND u.anonymized_at IS NULL
	      ON CONFLICT DO NOTHING`,
}}

// FanOut раздаёт поводы по нерозданным фактам и возвращает, сколько фактов
// обработал.
//
// Одной транзакцией на пачку: либо факт роздан всем, кому следовало, либо не
// роздан никому и будет взят следующим проходом. Повтор безопасен — ключ
// (user_id, event_id) не даёт удвоить строку, — поэтому обрыв посередине не
// требует ни разбора, ни ручной починки.
func (p *Platform) FanOut(ctx context.Context, limit int) (int, error) {
	ids, err := p.pendingEvents(ctx, clampLimit(limit))
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, wrapf(err, "раздача поводов")
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	for _, r := range fanOutRules {
		args := []any{ids, r.reason, r.kinds, KindMember}
		if r.reason == ReasonMention {
			args = append(args, mentionMinRunes)
		}
		if _, err := tx.Exec(ctx, r.sql, args...); err != nil {
			return 0, wrapf(err, "раздача поводов (%s)", r.name)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE events SET fanned_at = now() WHERE id = ANY($1)`, ids); err != nil {
		return 0, wrapf(err, "отметка раздачи")
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, wrapf(err, "раздача поводов")
	}
	return len(ids), nil
}

// pendingQuery — очередь раздачи. Константой, потому что её план проверяется
// тестом: раздача идёт каждые пять секунд, и полный перебор журнала в ней
// однажды съел бы то самое ядро, ради которого всё и считается.
const pendingQuery = `SELECT id FROM events WHERE fanned_at IS NULL ORDER BY id LIMIT $1`

func (p *Platform) pendingEvents(ctx context.Context, limit int) ([]int64, error) {
	rows, err := p.pool.Query(ctx, pendingQuery, limit)
	if err != nil {
		return nil, wrapf(err, "очередь раздачи")
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, wrapf(err, "очередь раздачи")
		}
		ids = append(ids, id)
	}
	return ids, wrapf(rows.Err(), "очередь раздачи")
}

// ---------------------------------------------------------------- реакции

// reactionGroup — объект и знак, которым его отметили.
type reactionGroup struct {
	NoteID    int64
	CommentID int64
	Code      string
}

// NoticeReactions превращает поставленные реакции в события и возвращает, о
// скольких объектах сказано.
//
// Событие на каждое нажатие завести нельзя: реакция стоит одного движения, и
// автор реплики получил бы десяток поводов за минуту. Поэтому проход идёт своим,
// медленным тактом и схлопывает нажатия в одно событие на объект и знак, а если
// повод об этом объекте у человека ещё не прочитан — дописывает число прямо в
// него. Ровно так и ведёт себя счётчик под сообщением в мессенджере.
//
// Имени нажавшего здесь нет нигде, и это не «не показываем», а «положить некуда»:
// у события реакции actor_id не заполняется вовсе. Правило Ш5г («кто нажал, не
// показывается никому и нигде») остаётся в силе буквально — модератор здесь
// такой же посторонний, как все.
func (p *Platform) NoticeReactions(ctx context.Context, limit int) (int, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, wrapf(err, "разбор реакций")
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	// ctid, потому что ключ таблицы содержит NULL (реакция на заметку), а
	// сравнение кортежей с NULL не работает. Внутри транзакции ctid устойчив.
	rows, err := tx.Query(ctx, `
		UPDATE reactions SET noticed_at = now()
		 WHERE ctid IN (SELECT ctid FROM reactions WHERE noticed_at IS NULL
		                 ORDER BY created_at LIMIT $1)
		RETURNING note_id, coalesce(comment_id, 0), code`, clampLimit(limit))
	if err != nil {
		return 0, wrapf(err, "разбор реакций")
	}
	groups := map[reactionGroup]int{}
	for rows.Next() {
		var g reactionGroup
		if err := rows.Scan(&g.NoteID, &g.CommentID, &g.Code); err != nil {
			rows.Close()
			return 0, wrapf(err, "разбор реакций")
		}
		groups[g]++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, wrapf(err, "разбор реакций")
	}
	for g, n := range groups {
		merged, err := mergeReaction(ctx, tx, g, n)
		if err != nil {
			return 0, err
		}
		if merged {
			continue
		}
		if err := recordEvent(ctx, tx, newEvent{
			Kind: EventReaction, NoteID: g.NoteID, CommentID: g.CommentID,
			Details: map[string]any{"code": g.Code, "count": n},
		}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, wrapf(err, "разбор реакций")
	}
	return len(groups), nil
}

// mergeReaction дописывает число в уже стоящий НЕПРОЧИТАННЫЙ повод об этом же
// объекте и знаке. Прочитанный не трогаем: человек его уже видел, и молча
// подросшее число он не заметит — про новые реакции ему нужен новый повод.
func mergeReaction(ctx context.Context, q querier, g reactionGroup, n int) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE events SET at = now(),
		       details = jsonb_set(details, '{count}',
		                 to_jsonb(coalesce((details->>'count')::int, 0) + $4))
		 WHERE id = (SELECT e.id FROM events e
		               JOIN notifications t ON t.event_id = e.id AND t.read_at IS NULL
		              WHERE e.kind = $5
		                AND e.note_id = $1
		                AND coalesce(e.comment_id, 0) = $2
		                AND e.details->>'code' = $3
		                AND e.at > now() - $6::interval
		              ORDER BY e.id DESC LIMIT 1)`,
		g.NoteID, g.CommentID, g.Code, n, EventReaction, reactionMergeWindow.String())
	if err != nil {
		return false, wrapf(err, "дописывание реакций к заметке %d", g.NoteID)
	}
	return tag.RowsAffected() > 0, nil
}

// ---------------------------------------------------------------- уборка

// BusPruned — что убрала уборка.
type BusPruned struct {
	Read   int // прочитанных поводов
	Unread int // непрочитанных, доживших до срока
	Events int // фактов, поводов по которым не осталось
}

// Any — уборка что-то сделала. Нужна вызывающему, чтобы не писать в лог строку
// о том, что убирать было нечего.
func (b BusPruned) Any() bool { return b.Read+b.Unread+b.Events > 0 }

// PruneBus убирает состарившееся. Не «когда-нибудь дойдут руки», а часть
// устройства: журнал о том, кто кому отвечал, — сведения о людях, и хранить их
// дольше цели закон не разрешает, а цель здесь короткая.
func (p *Platform) PruneBus(ctx context.Context, limit int) (BusPruned, error) {
	var out BusPruned
	steps := []struct {
		to  *int
		sql string
		age string
	}{
		{&out.Read, `DELETE FROM notifications WHERE ctid IN (
		               SELECT ctid FROM notifications
		                WHERE read_at IS NOT NULL AND read_at < now() - $1::interval
		                LIMIT $2)`, KeepRead.String()},
		{&out.Unread, `DELETE FROM notifications WHERE ctid IN (
		                 SELECT ctid FROM notifications
		                  WHERE read_at IS NULL AND created_at < now() - $1::interval
		                  LIMIT $2)`, KeepUnread.String()},
		// Факты уходят последними и только те, по которым поводов уже нет:
		// пока повод жив, страница событий обязана уметь его показать.
		{&out.Events, `DELETE FROM events WHERE ctid IN (
		                 SELECT e.ctid FROM events e
		                  WHERE e.fanned_at IS NOT NULL AND e.at < now() - $1::interval
		                    AND NOT EXISTS (SELECT 1 FROM notifications n WHERE n.event_id = e.id)
		                  LIMIT $2)`, KeepEvents.String()},
	}
	for _, s := range steps {
		tag, err := p.pool.Exec(ctx, s.sql, s.age, clampLimit(limit))
		if err != nil {
			return out, wrapf(err, "уборка шины")
		}
		*s.to = int(tag.RowsAffected())
	}
	return out, nil
}

// ---------------------------------------------------------------- чтение

// NotificationView — повод, как его показывают человеку.
//
// Полей «кто нажал реакцию» и «настоящий автор анонимной заметки» здесь нет по
// той же причине, по которой их нет в NoteView: пока поля нет, показать его
// нельзя даже по небрежности.
type NotificationView struct {
	EventID   int64
	Kind      EventKind
	Reason    Reason
	At        time.Time
	Read      bool
	ActorNick string // пусто у машины, у реакций и у анонимной заметки
	NoteID    int64
	CommentID int64
	Excerpt   string // выдержка из публичного текста; пусто, если он скрыт
	Code      string // знак реакции
	Count     int    // сколько реакций схлопнулось
	Detail    string // наше служебное: причина модератора
	Hidden    bool   // публикация сейчас скрыта — вести на неё некуда
}

const notificationColumns = `
	e.id, e.kind, n.reason, e.at, n.read_at IS NOT NULL,
	coalesce(a.nick, ''), coalesce(e.note_id, 0), coalesce(e.comment_id, 0),
	CASE WHEN coalesce(c.status, nt.status, 0) = 0
	     THEN left(coalesce(c.body, nt.body, ''), $4) ELSE '' END,
	coalesce(e.details->>'code', ''), coalesce((e.details->>'count')::int, 0),
	coalesce(e.details->>'reason', ''),
	coalesce(c.status, nt.status, 0) <> 0
  FROM notifications n
  JOIN events   e  ON e.id = n.event_id
  LEFT JOIN users    a  ON a.id = e.actor_id
  LEFT JOIN comments c  ON c.id = e.comment_id
  LEFT JOIN notes    nt ON nt.id = e.note_id`

// Notifications — поводы человека, от свежих к старым.
//
// Порядок по event_id, а не по времени: id монотонен, лежит в ключе таблицы, и
// страница читается range-scan'ом без единого лишнего индекса. Разойтись со
// временем он может ровно в одном месте — у дописанной реакции, где at
// обновляется; там повод и должен остаться на своём месте в списке, иначе
// каждое нажатие таскало бы старую строку наверх.
const noticesQuery = `SELECT ` + notificationColumns + `
	 WHERE n.user_id = $1 ORDER BY n.event_id DESC LIMIT $2 OFFSET $3`

func (p *Platform) Notifications(ctx context.Context, userID int64, offset, limit int) ([]NotificationView, error) {
	rows, err := p.pool.Query(ctx, noticesQuery,
		userID, clampLimit(limit), max(0, offset), excerptRunes)
	if err != nil {
		return nil, wrapf(err, "события участника %d", userID)
	}
	defer rows.Close()
	var out []NotificationView
	for rows.Next() {
		var v NotificationView
		if err := rows.Scan(&v.EventID, &v.Kind, &v.Reason, &v.At, &v.Read,
			&v.ActorNick, &v.NoteID, &v.CommentID, &v.Excerpt,
			&v.Code, &v.Count, &v.Detail, &v.Hidden); err != nil {
			return nil, wrapf(err, "события участника %d", userID)
		}
		out = append(out, v)
	}
	return out, wrapf(rows.Err(), "события участника %d", userID)
}

// CountNotifications — сколько всего поводов у человека (под нумерованную
// постраничку, как в ленте).
func (p *Platform) CountNotifications(ctx context.Context, userID int64) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1`, userID).Scan(&n)
	return n, wrapf(err, "счёт событий участника %d", userID)
}

// UnreadCap — потолок счётчика в шапке. Выше него точное число ничего не
// добавляет («99+» читается так же), а запрос перестаёт зависеть от того,
// сколько человек не заходил.
const UnreadCap = 100

// unreadQuery — счётчик в шапке. Спрашивается на КАЖДОЙ странице вошедшего,
// поэтому его план — часть договора и проверяется тестом.
const unreadQuery = `
	SELECT count(*) FROM (
	    SELECT 1 FROM notifications
	     WHERE user_id = $1 AND read_at IS NULL LIMIT $2) t`

// Unread — сколько непрочитанного, не больше UnreadCap.
func (p *Platform) Unread(ctx context.Context, userID int64) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx, unreadQuery, userID, UnreadCap).Scan(&n)
	return n, wrapf(err, "непрочитанное участника %d", userID)
}

// MarkRead отмечает прочитанным всё до upto включительно; upto = 0 — всё.
//
// «До границы», а не «вот эти строки»: человек читает список сверху, и отметить
// он хочет то, что видел. Граница вдобавок делает повтор безобидным.
func (p *Platform) MarkRead(ctx context.Context, userID, upto int64) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE notifications SET read_at = now()
		 WHERE user_id = $1 AND read_at IS NULL AND ($2 = 0 OR event_id <= $2)`, userID, upto)
	return wrapf(err, "отметка прочитанного %d", userID)
}

// ---------------------------------------------------------------- живой канал

// LiveEvent — факт для живой страницы. Ни текста, ни имени: страница получает
// СИГНАЛ «в этом треде новое», а содержимое читает обычным переходом.
type LiveEvent struct {
	ID        int64
	Kind      EventKind
	NoteID    int64
	CommentID int64
}

// LiveSince — факты после afterID. Один запрос на такт хаба независимо от числа
// открытых страниц: стоимость живого канала не должна расти вместе с публикой.
func (p *Platform) LiveSince(ctx context.Context, afterID int64, limit int) ([]LiveEvent, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, kind, coalesce(note_id, 0), coalesce(comment_id, 0)
		  FROM events WHERE id > $1 ORDER BY id LIMIT $2`, afterID, clampLimit(limit))
	if err != nil {
		return nil, wrapf(err, "живая лента после %d", afterID)
	}
	defer rows.Close()
	var out []LiveEvent
	for rows.Next() {
		var e LiveEvent
		if err := rows.Scan(&e.ID, &e.Kind, &e.NoteID, &e.CommentID); err != nil {
			return nil, wrapf(err, "живая лента после %d", afterID)
		}
		out = append(out, e)
	}
	return out, wrapf(rows.Err(), "живая лента после %d", afterID)
}

// Poke — «этому человеку стало о чём сказать». Ровно два числа: живому каналу
// нужно перерисовать счётчик, а не показать содержание.
type Poke struct {
	UserID  int64
	EventID int64
}

// PokesSince — поводы, появившиеся после afterID.
//
// Курсор у них СВОЙ, отдельный от LiveSince, и это не дублирование: повод
// появляется не тогда, когда случился факт, а тогда, когда его раздали, — то
// есть на такт-другой позже. Один курсор на двоих проскакивал бы мимо поводов
// по уже увиденным фактам.
func (p *Platform) PokesSince(ctx context.Context, afterID int64, limit int) ([]Poke, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT n.user_id, n.event_id
		  FROM notifications n JOIN events e ON e.id = n.event_id
		 WHERE n.event_id > $1 AND e.fanned_at IS NOT NULL
		 ORDER BY n.event_id LIMIT $2`, afterID, clampLimit(limit))
	if err != nil {
		return nil, wrapf(err, "поводы после %d", afterID)
	}
	defer rows.Close()
	var out []Poke
	for rows.Next() {
		var k Poke
		if err := rows.Scan(&k.UserID, &k.EventID); err != nil {
			return nil, wrapf(err, "поводы после %d", afterID)
		}
		out = append(out, k)
	}
	return out, wrapf(rows.Err(), "поводы после %d", afterID)
}

// LastEventID — верхний край журнала. Живому каналу нужен на старте: слушателю,
// открывшему страницу, прошлое не рассылают.
func (p *Platform) LastEventID(ctx context.Context) (int64, error) {
	var id int64
	err := p.pool.QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM events`).Scan(&id)
	return id, wrapf(err, "верх журнала событий")
}

// ---------------------------------------------------------------- сводка

// BusStats — наполнение шины. Ответ на единственный вопрос, который задают
// работающей службе: жива ли она и не растёт ли очередь быстрее раздачи.
type BusStats struct {
	Events    int
	Pending   int // фактов ждёт раздачи
	OldestAge time.Duration
	Notices   int
	Unread    int
	Reactions int // нажатий ждёт разбора
}

// BusStats считает наполнение шины.
func (p *Platform) BusStats(ctx context.Context) (BusStats, error) {
	var s BusStats
	var oldest *time.Time
	err := p.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM events),
		       (SELECT count(*) FROM events WHERE fanned_at IS NULL),
		       (SELECT min(at)   FROM events WHERE fanned_at IS NULL),
		       (SELECT count(*) FROM notifications),
		       (SELECT count(*) FROM notifications WHERE read_at IS NULL),
		       (SELECT count(*) FROM reactions WHERE noticed_at IS NULL)`).
		Scan(&s.Events, &s.Pending, &oldest, &s.Notices, &s.Unread, &s.Reactions)
	if err != nil {
		return s, wrapf(err, "сводка шины")
	}
	if oldest != nil {
		s.OldestAge = time.Since(*oldest)
	}
	return s, nil
}
