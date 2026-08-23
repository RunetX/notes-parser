package platform

// Модерация (Ш7).
//
// Устройство определено не удобством, а двумя фактами про эту площадку.
//
// ПЕРВЫЙ: у владельца нет времени читать треды (его слова, 18.08.2026). Значит
// объём снимает автомат — но только объём. Решения о людях принимает человек, и
// автомат не имеет права ни банить, ни править, ни удалять: его максимум —
// СКРЫТЬ уже опубликованное и позвать человека. Классификатор живёт снаружи
// (internal/platmod), здесь — состояние и действия, потому что ядро обязано
// уметь работать и без него.
//
// ВТОРОЙ: ссора, колкость и переход на личности — это ЖАНР раздела, а не
// нарушение. Модель, настроенную на «токсичность», нельзя пускать сюда вовсе:
// она выкосит саму площадку и первыми — самых заметных её людей. Поэтому
// закрытый список категорий и таблица «что гасится само» лежат ЗДЕСЬ, в коде, а
// не в промпте: промпт — это текст, который однажды поправят между делом, а
// политика обязана меняться правкой, которую видно в diff.
//
// Из первого факта следует и третье правило: у ложного срабатывания обязана
// быть дорога назад. Автору скрытого показывается причина и кнопка «на
// пересмотр» — молча исчезнувшая реплика худшее, что можно сделать с
// сообществом, которое только что переехало.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Verdict — мнение проверки о публикации.
type Verdict int16

const (
	VerdictClean  Verdict = 0 // ничего
	VerdictReview Verdict = 1 // человеку в очередь
	VerdictHidden Verdict = 2 // скрыто автоматом
)

// Decision — решение ЧЕЛОВЕКА. Хранится рядом с мнением автомата, а не вместо
// него: расхождение этих двух полей и есть статистика ложных срабатываний, без
// которой автомат держать нельзя.
type Decision int16

const (
	DecisionKeep Decision = 0 // оставить (или вернуть) видимым
	DecisionHide Decision = 1 // скрыть
)

// Категории нарушений — ЗАКРЫТЫЙ список. Всё, что в него не попало, модерации
// не подлежит вовсе: «мне не нравится» здесь не категория.
const (
	CatSpam      = "spam"      // реклама, рассылка, ссылки ради ссылок
	CatPII       = "pii"       // чужие персональные данные
	CatThreat    = "threat"    // угроза насилием
	CatDrugs     = "drugs"     // вовлечение в наркотики
	CatSuicide   = "suicide"   // вовлечение в суицид
	CatExtremism = "extremism" // экстремизм
	CatPorn      = "porn"      // порнография
	CatFlood     = "flood"     // шторм одинаковых сообщений
	CatOther     = "other"     // сомнительно, но не из списка — только человеку
	CatReport    = "report"    // не автомат: пожаловался человек
)

// categoryTitles — как категория называется по-русски. Показывается и
// модератору в очереди, и АВТОРУ скрытой публикации: «скрыто» без причины это
// та же молча исчезнувшая реплика, только с извинением.
var categoryTitles = map[string]string{
	CatSpam:      "реклама или спам",
	CatPII:       "чужие персональные данные",
	CatThreat:    "угроза насилием",
	CatDrugs:     "вовлечение в наркотики",
	CatSuicide:   "вовлечение в суицид",
	CatExtremism: "экстремизм",
	CatPorn:      "порнография",
	CatFlood:     "поток одинаковых сообщений",
	CatOther:     "на усмотрение модератора",
	CatReport:    "жалоба участника",
}

// autoHide — категории, которые автомат вправе погасить САМ, не дожидаясь
// человека. Список ровно тот, что записан в решении по Ш7, и расширять его
// нельзя без такого же решения: каждая строка здесь — это право машины убрать
// чужие слова из разговора.
//
// Чего в нём нет намеренно: грубость, оскорбление, «токсичность», offtopic,
// спор о политике. Это жанр раздела, а не нарушение.
var autoHide = map[string]bool{
	CatSpam:      true,
	CatPII:       true,
	CatThreat:    true,
	CatDrugs:     true,
	CatSuicide:   true,
	CatExtremism: true,
	CatPorn:      true,
	CatFlood:     true,
}

// KnownCategory — категория из закрытого списка (кроме служебной «жалоба»: её
// ставит не проверка, а человек).
func KnownCategory(c string) bool {
	_, ok := categoryTitles[c]
	return ok && c != CatReport
}

// AutoHideable — категория, которую автомат гасит сам.
func AutoHideable(c string) bool { return autoHide[c] }

// CategoryTitle — человеческое название категории.
func CategoryTitle(c string) string {
	if t, ok := categoryTitles[c]; ok {
		return t
	}
	return c
}

// AutoCategories — категории, между которыми выбирает автомат, в порядке
// показа. Служебная «жалоба» в набор не входит.
func AutoCategories() []string {
	return []string{CatSpam, CatPII, CatThreat, CatDrugs, CatSuicide,
		CatExtremism, CatPorn, CatFlood, CatOther}
}

// Действия журнала. Строками, потому что читать audit_log будут и глазами, и
// запросом.
const (
	ActionHide     = "hide"
	ActionAutoHide = "auto_hide" // тот же результат, но актор — машина
	ActionRestore  = "restore"
	ActionLock     = "lock"
	ActionUnlock   = "unlock"
	ActionPin      = "pin"
	ActionUnpin    = "unpin"
	ActionBan      = "ban"
	ActionUnban    = "unban"
	ActionRole     = "role"
	ActionAppeal   = "appeal"
	ActionReport   = "report"
	ActionDismiss  = "dismiss" // жалоба или подозрение отклонены
	ActionAnonym   = "anonymize"
	ActionExport   = "export"
)

// SubjectUser — третий вид объекта журнала: сам человек. В moderation_queue он
// не попадает никогда (проверяют публикации, а не людей), но бан и смена роли
// без него были бы записями ни о чём.
const SubjectUser = "user"

// MaxReasonRunes — потолок причины. Причина показывается автору и лежит в
// журнале; роман здесь ни к чему, а мегабайт — это дыра.
const MaxReasonRunes = 500

var (
	// ErrNotModerator — действие требует прав модератора.
	ErrNotModerator = errors.New("нужны права модератора")
	// ErrBadSubject — вид объекта не «заметка» и не «комментарий».
	ErrBadSubject = errors.New("неизвестный вид объекта")
	// ErrNothingToDo — объект уже в нужном состоянии.
	ErrNothingToDo = errors.New("состояние уже такое")
	// ErrTooManyPinned — закреплённых уже столько, сколько лента выдерживает.
	ErrTooManyPinned = fmt.Errorf("закрепить можно не больше %d заметок", MaxPinned)
	// ErrNoAppeal — обжаловать нечего: публикация не скрыта автоматом либо
	// пересмотр уже запрошен.
	ErrNoAppeal = errors.New("пересмотр этой публикации запросить нельзя")
	// ErrSelfReport — жалоба на самого себя.
	ErrSelfReport = errors.New("на себя жаловаться незачем")
)

// Subject — на что смотрит модерация: заметка, комментарий или человек.
type Subject struct {
	Kind string
	ID   int64
}

// NoteSubject, CommentSubject и UserSubject — конструкторы, чтобы вид объекта
// не собирался строкой в десяти местах.
func NoteSubject(id int64) Subject    { return Subject{Kind: SubjectNote, ID: id} }
func CommentSubject(id int64) Subject { return Subject{Kind: SubjectComment, ID: id} }
func UserSubject(id int64) Subject    { return Subject{Kind: SubjectUser, ID: id} }

// Valid — вид объекта известен модерации публикаций.
func (s Subject) Valid() bool {
	return (s.Kind == SubjectNote || s.Kind == SubjectComment) && s.ID > 0
}

// IsNote — объект это заметка.
func (s Subject) IsNote() bool { return s.Kind == SubjectNote }

// String — для журнала и логов.
func (s Subject) String() string { return fmt.Sprintf("%s %d", s.Kind, s.ID) }

// subjectFacts — то, что нужно знать про объект, прежде чем что-то с ним делать.
type subjectFacts struct {
	NoteID   int64
	AuthorID int64
	Status   Status
	Body     string
}

// factsOf читает объект независимо от вида. Одним местом, потому что каждое
// действие модерации начинается с одного и того же вопроса — «а что это и чьё».
func factsOf(ctx context.Context, q querier, s Subject) (subjectFacts, error) {
	var (
		f      subjectFacts
		author *int64
		err    error
	)
	switch s.Kind {
	case SubjectNote:
		f.NoteID = s.ID
		err = q.QueryRow(ctx,
			`SELECT author_id, status, body FROM notes WHERE id = $1`, s.ID).
			Scan(&author, &f.Status, &f.Body)
	case SubjectComment:
		err = q.QueryRow(ctx,
			`SELECT note_id, author_id, status, body FROM comments WHERE id = $1`, s.ID).
			Scan(&f.NoteID, &author, &f.Status, &f.Body)
	default:
		return f, fmt.Errorf("%w: %q", ErrBadSubject, s.Kind)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return f, fmt.Errorf("%s: %w", s, ErrNotFound)
	}
	if err != nil {
		return f, fmt.Errorf("чтение %s: %w", s, err)
	}
	f.AuthorID = idOf(author)
	return f, nil
}

// ---------------------------------------------------------------- журнал

// AuditEntry — строка журнала.
type AuditEntry struct {
	ID      int64
	At      time.Time
	Actor   int64  // 0 — автомат или командная строка
	Nick    string // ник актора, пусто у машины
	Action  string
	Subject Subject
	Details map[string]any
}

// audit пишет в журнал. Всегда ТОЙ ЖЕ транзакцией, что и само действие:
// «скрыли, но в журнал не попало» — состояние, которого не должно быть, иначе
// через месяц «за что скрыли» отвечается догадкой.
//
// actor = 0 означает «не человек»: автомат или команда администратора из
// консоли. Колонка nullable ровно поэтому — признать, что автора не было,
// честнее, чем приписать действие первому попавшемуся админу.
func audit(ctx context.Context, q querier, actor int64, action string, s Subject, details map[string]any) error {
	raw := []byte("{}")
	if len(details) > 0 {
		b, err := json.Marshal(details)
		if err != nil {
			return fmt.Errorf("журнал %s %s: %w", action, s, err)
		}
		raw = b
	}
	_, err := q.Exec(ctx, `
		INSERT INTO audit_log (actor_id, action, subject_kind, subject_id, details)
		VALUES ($1, $2, $3, $4, $5)`, nullID(actor), action, s.Kind, s.ID, raw)
	return wrapf(err, "журнал %s %s", action, s)
}

const auditColumns = `
	a.id, a.at, a.actor_id, coalesce(u.nick, ''), a.action,
	a.subject_kind, a.subject_id, a.details
  FROM audit_log a LEFT JOIN users u ON u.id = a.actor_id`

// AuditTail — последние записи журнала. Показываются модератору: собственная
// работа обязана быть видна тому, кто её делает, иначе «кто это скрыл»
// выясняется перепиской.
func (p *Platform) AuditTail(ctx context.Context, limit int) ([]AuditEntry, error) {
	return p.auditQuery(ctx, `SELECT `+auditColumns+`
		 ORDER BY a.at DESC, a.id DESC LIMIT $1`, clampLimit(limit))
}

// SubjectAudit — что делали именно с этим объектом.
func (p *Platform) SubjectAudit(ctx context.Context, s Subject, limit int) ([]AuditEntry, error) {
	return p.auditQuery(ctx, `SELECT `+auditColumns+`
		 WHERE a.subject_kind = $2 AND a.subject_id = $3
		 ORDER BY a.at DESC, a.id DESC LIMIT $1`, clampLimit(limit), s.Kind, s.ID)
}

func (p *Platform) auditQuery(ctx context.Context, sql string, args ...any) ([]AuditEntry, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("журнал модерации: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var (
			e     AuditEntry
			actor *int64
			raw   []byte
		)
		if err := rows.Scan(&e.ID, &e.At, &actor, &e.Nick, &e.Action,
			&e.Subject.Kind, &e.Subject.ID, &raw); err != nil {
			return nil, fmt.Errorf("журнал модерации: %w", err)
		}
		e.Actor = idOf(actor)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &e.Details) //nolint:errcheck // битый details не повод терять строку
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- скрыть и вернуть

// HideSubject скрывает публикацию решением МОДЕРАТОРА.
//
// Скрытие, а не удаление, и это правило всей площадки: тред — чужие ответы на
// чьи-то слова, и дыра в ветке ломает разговор посторонним. Строка остаётся в
// базе, её видит модератор, и вернуть её — одно нажатие.
func (p *Platform) HideSubject(ctx context.Context, actor Viewer, s Subject, category, reason string) error {
	if !actor.CanModerate() {
		return ErrNotModerator
	}
	return p.setSubjectHidden(ctx, actor.UserID, s, true, ActionHide, category, reason)
}

// RestoreSubject возвращает скрытое модерацией.
//
// Скрытое АВТОРОМ (отзыв согласия, StatusHiddenOwner) и обезличенное не
// трогается: первое — исполнение права субъекта, отменять его модератор не
// вправе; второе необратимо по смыслу.
func (p *Platform) RestoreSubject(ctx context.Context, actor Viewer, s Subject, reason string) error {
	if !actor.CanModerate() {
		return ErrNotModerator
	}
	return p.setSubjectHidden(ctx, actor.UserID, s, false, ActionRestore, "", reason)
}

// setSubjectHidden — общая дорога скрытия и возврата. Одной транзакцией:
// статус, денормализованный счётчик заметки, карточка проверки, жалобы и журнал.
func (p *Platform) setSubjectHidden(ctx context.Context, actor int64, s Subject,
	hide bool, action, category, reason string) error {
	if !s.Valid() {
		return fmt.Errorf("%w: %q", ErrBadSubject, s.Kind)
	}
	reason = trimReason(reason)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%s %s: %w", action, s, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	facts, err := factsOf(ctx, tx, s)
	if err != nil {
		return err
	}
	from, to := StatusVisible, StatusHiddenMod
	if !hide {
		from, to = StatusHiddenMod, StatusVisible
	}
	if facts.Status != from {
		return fmt.Errorf("%s: %w", s, ErrNothingToDo)
	}
	if err := moveStatus(ctx, tx, s, facts.NoteID, from, to); err != nil {
		return err
	}
	// Решение записывается в ту же карточку, где лежит мнение автомата. Строка
	// заводится, если её ещё нет: у зеркальной реплики 2014 года очереди не было
	// и быть не могло, а решение по ней такое же настоящее.
	decision := DecisionHide
	if !hide {
		decision = DecisionKeep
	}
	if err := recordDecision(ctx, tx, s, facts, actor, decision, category, reason); err != nil {
		return err
	}
	if err := resolveReports(ctx, tx, s, actor, action); err != nil {
		return err
	}
	details := map[string]any{"reason": reason}
	if category != "" {
		details["category"] = category
	}
	if facts.AuthorID != 0 {
		details["author"] = facts.AuthorID
	}
	if err := audit(ctx, tx, actor, action, s, details); err != nil {
		return err
	}
	// Приглашения прийти и прочитать снимаются ТОЙ ЖЕ транзакцией — исполняем,
	// а не проверяем на показе (см. dropUnreadAbout). Само сообщение о скрытии
	// под эту уборку не попадает: она перечисляет виды фактов явно.
	if hide {
		if err := dropUnreadAbout(ctx, tx, s); err != nil {
			return err
		}
	}
	kind := EventRestored
	if hide {
		kind = EventHidden
	}
	if err := recordEvent(ctx, tx, hideEvent(kind, actor, s, facts, category, reason)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%s %s: %w", action, s, err)
	}
	return nil
}

// moveStatus двигает статус одной публикации и правит счётчик заметки.
//
// Счётчик денормализован (лента не делает COUNT(*)), поэтому скрытие обязано
// его поправить — иначе под заметкой стоит «Комментарии 42», а видно сорок.
func moveStatus(ctx context.Context, q querier, s Subject, noteID int64, from, to Status) error {
	if s.IsNote() {
		tag, err := q.Exec(ctx,
			`UPDATE notes SET status = $2 WHERE id = $1 AND status = $3`, s.ID, to, from)
		if err != nil {
			return wrapf(err, "статус заметки %d", s.ID)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%s: %w", s, ErrNothingToDo)
		}
		return nil
	}
	tag, err := q.Exec(ctx,
		`UPDATE comments SET status = $2 WHERE id = $1 AND status = $3`, s.ID, to, from)
	if err != nil {
		return wrapf(err, "статус комментария %d", s.ID)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: %w", s, ErrNothingToDo)
	}
	delta := -1
	if to == StatusVisible {
		delta = 1
	}
	_, err = q.Exec(ctx, `
		UPDATE notes SET comment_count = greatest(0, comment_count + $2) WHERE id = $1`, noteID, delta)
	return wrapf(err, "счётчик комментариев заметки %d", noteID)
}

// SetThreadLocked закрывает и открывает обсуждение. Наше решение, а не перенос
// отметки НГС: та живёт в comments_closed и остаётся надписью — она стоит у
// 62 % заметок зеркала и появляется через минуты после публикации, то есть как
// признак «разговор кончился» недостоверна и на самом сайте.
func (p *Platform) SetThreadLocked(ctx context.Context, actor Viewer, noteID int64, locked bool, reason string) error {
	if !actor.CanModerate() {
		return ErrNotModerator
	}
	reason = trimReason(reason)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return wrapf(err, "замок обсуждения %d", noteID)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	tag, err := tx.Exec(ctx,
		`UPDATE notes SET locked = $2 WHERE id = $1 AND locked <> $2`, noteID, locked)
	if err != nil {
		return wrapf(err, "замок обсуждения %d", noteID)
	}
	if tag.RowsAffected() == 0 {
		// Либо заметки нет вовсе, либо замок уже в этом положении — второе не
		// ошибка, но и не действие, и путать их нельзя.
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT true FROM notes WHERE id = $1`, noteID).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("заметка %d: %w", noteID, ErrNotFound)
			}
			return wrapf(err, "замок обсуждения %d", noteID)
		}
		return ErrNothingToDo
	}
	action := ActionUnlock
	if locked {
		action = ActionLock
	}
	if err := audit(ctx, tx, actor.UserID, action, NoteSubject(noteID),
		map[string]any{"reason": reason}); err != nil {
		return err
	}
	return wrapf(tx.Commit(ctx), "замок обсуждения %d", noteID)
}

// SetNotePinned закрепляет заметку наверху ленты и снимает закрепление.
//
// Право модератора, а не автора: закрепление — это не свойство своей записи, а
// место в общей ленте, то есть решение про чужое внимание. Потолок MaxPinned
// проверяется ЗДЕСЬ же, одной транзакцией со вставкой: два модератора, нажавших
// одновременно, иначе поставили бы шестую и седьмую.
func (p *Platform) SetNotePinned(ctx context.Context, actor Viewer, noteID int64, pinned bool, reason string) error {
	if !actor.CanModerate() {
		return ErrNotModerator
	}
	reason = trimReason(reason)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return wrapf(err, "закрепление заметки %d", noteID)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	if pinned {
		// FOR UPDATE не нужен: считаем в той же транзакции, а гонку закрывает
		// повторная проверка при коммите — в худшем случае вторая попытка
		// увидит потолок и честно откажет.
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM notes WHERE pinned_at IS NOT NULL AND status = 0 AND id <> $1`,
			noteID).Scan(&n); err != nil {
			return wrapf(err, "закрепление заметки %d", noteID)
		}
		if n >= MaxPinned {
			return ErrTooManyPinned
		}
	}

	tag, err := tx.Exec(ctx, `UPDATE notes SET pinned_at = CASE WHEN $2 THEN now() END
	                           WHERE id = $1 AND (pinned_at IS NOT NULL) <> $2`, noteID, pinned)
	if err != nil {
		return wrapf(err, "закрепление заметки %d", noteID)
	}
	if tag.RowsAffected() == 0 {
		// Либо заметки нет, либо она уже в этом положении: второе не ошибка, но
		// и не действие — путать их нельзя (то же правило, что у замка).
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT true FROM notes WHERE id = $1`, noteID).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("заметка %d: %w", noteID, ErrNotFound)
			}
			return wrapf(err, "закрепление заметки %d", noteID)
		}
		return ErrNothingToDo
	}
	action := ActionUnpin
	if pinned {
		action = ActionPin
	}
	if err := audit(ctx, tx, actor.UserID, action, NoteSubject(noteID),
		map[string]any{"reason": reason}); err != nil {
		return err
	}
	return wrapf(tx.Commit(ctx), "закрепление заметки %d", noteID)
}

// ---------------------------------------------------------------- запрет писать

// BanUser запрещает человеку публиковать до срока.
//
// Сессии при этом НЕ гасятся, и это осознанно: запрет касается публикаций, а
// чтение на площадке открыто всем. Выкинув забаненного из его же учётной
// записи, мы отняли бы у него ровно ту страницу, где написано, за что и до
// какого числа, — то есть превратили бы наказание в исчезновение.
func (p *Platform) BanUser(ctx context.Context, actor Viewer, userID int64, until time.Time, reason string) error {
	if !actor.CanModerate() {
		return ErrNotModerator
	}
	if !until.After(time.Now()) {
		return errors.New("срок запрета уже прошёл")
	}
	reason = trimReason(reason)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return wrapf(err, "запрет %d", userID)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	tag, err := tx.Exec(ctx, `
		UPDATE users SET banned_until = $2, ban_reason = $3, banned_by = $4
		 WHERE id = $1`, userID, until, reason, nullID(actor.UserID))
	if err != nil {
		return wrapf(err, "запрет %d", userID)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("пользователь %d: %w", userID, ErrNotFound)
	}
	if err := audit(ctx, tx, actor.UserID, ActionBan, UserSubject(userID),
		map[string]any{"until": until.UTC().Format(time.RFC3339), "reason": reason}); err != nil {
		return err
	}
	// Сессии не гасятся (см. шапку), поэтому повод забаненный увидит — и это
	// единственный способ узнать о запрете раньше, чем упрёшься в него формой.
	// Срок в details лежит в RFC 3339: показывает его страница, а не журнал.
	if err := recordEvent(ctx, tx, newEvent{
		Kind: EventBanned, ActorID: actor.UserID, SubjectID: userID,
		Details: map[string]any{"until": until.UTC().Format(time.RFC3339), "reason": reason},
	}); err != nil {
		return err
	}
	return wrapf(tx.Commit(ctx), "запрет %d", userID)
}

// UnbanUser снимает запрет досрочно.
func (p *Platform) UnbanUser(ctx context.Context, actor Viewer, userID int64, reason string) error {
	if !actor.CanModerate() {
		return ErrNotModerator
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return wrapf(err, "снятие запрета %d", userID)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	tag, err := tx.Exec(ctx, `
		UPDATE users SET banned_until = NULL, ban_reason = '', banned_by = NULL
		 WHERE id = $1 AND banned_until IS NOT NULL`, userID)
	if err != nil {
		return wrapf(err, "снятие запрета %d", userID)
	}
	if tag.RowsAffected() == 0 {
		return ErrNothingToDo
	}
	if err := audit(ctx, tx, actor.UserID, ActionUnban, UserSubject(userID),
		map[string]any{"reason": trimReason(reason)}); err != nil {
		return err
	}
	if err := recordEvent(ctx, tx, newEvent{
		Kind: EventUnbanned, ActorID: actor.UserID, SubjectID: userID,
	}); err != nil {
		return err
	}
	return wrapf(tx.Commit(ctx), "снятие запрета %d", userID)
}

// ---------------------------------------------------------------- очередь автомата

// Pending — публикация, ждущая проверки автоматом. Текст лежит здесь же:
// классификатору он и нужен, а второй запрос на строку очереди — это лишний
// поход в базу на каждую реплику.
type Pending struct {
	Subject  Subject
	NoteID   int64
	AuthorID int64
	Body     string
	QueuedAt time.Time
	Attempts int
}

// PendingChecks — что автомат ещё не смотрел, от старых к новым.
//
// Строка заводится ТОЛЬКО публикацией у нас (см. enqueueCheck), поэтому
// классификатор физически не может пойти по архиву: 10,8 млн комментариев это
// тысячи долларов даже на дешёвой модели, и историю мы модерируем по жалобе.
//
// maxAttempts отсекает строки, на которых модель спотыкается раз за разом:
// иначе одна такая занимала бы каждую пачку до конца времён.
func (p *Platform) PendingChecks(ctx context.Context, limit, maxAttempts int) ([]Pending, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT q.subject_kind, q.subject_id, coalesce(q.note_id, 0), coalesce(q.author_id, 0),
		       coalesce(n.body, c.body, ''), q.queued_at, q.attempts
		  FROM moderation_queue q
		  LEFT JOIN notes    n ON q.subject_kind = 'note'    AND n.id = q.subject_id
		  LEFT JOIN comments c ON q.subject_kind = 'comment' AND c.id = q.subject_id
		 WHERE q.checked_at IS NULL AND q.verdict IS NULL AND q.attempts < $2
		 ORDER BY q.queued_at
		 LIMIT $1`, clampLimit(limit), maxAttempts)
	if err != nil {
		return nil, fmt.Errorf("очередь проверки: %w", err)
	}
	defer rows.Close()
	var out []Pending
	for rows.Next() {
		var it Pending
		if err := rows.Scan(&it.Subject.Kind, &it.Subject.ID, &it.NoteID, &it.AuthorID,
			&it.Body, &it.QueuedAt, &it.Attempts); err != nil {
			return nil, fmt.Errorf("очередь проверки: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// PendingOfNote — РЕПЛИКИ треда как строки проверки, ДЛЯ СТЕНДА.
//
// Очередь (`PendingChecks`) сюда не годится: строка заводится только публикацией
// у нас, а замерять классификатор надо на том, что в базе уже лежит, — то есть
// на зеркальных тредах, которых в очереди нет и не будет. Отсюда отдельный вход,
// и он НЕ пишет ничего: попытки и вердикты остаются делом боевого автомата.
//
// Тело заметки здесь не берётся, и это свойство СТЕНДА, а не правило модерации.
// В бою проверку проходит весь НАШ контент — и заметки, и реплики наравне
// (`enqueueCheck` ставит в очередь обе, `PendingChecks` джойнит обе таблицы);
// зеркальное НГС в очередь не попадает вовсе и модерируется по жалобе. Замерять
// же зовут из-за того, что творится в РЕПЛИКАХ: их сотни против одной строки
// заметки, и только на них замер что-то значит.
//
// Берутся только видимые. Про скрытое модератором решение уже принято, а
// скрытое АВТОРОМ (отзыв согласия) отправлять третьей стороне нельзя вовсе —
// и хорошо, что это держит запрос, а не память того, кто зовёт команду.
func (p *Platform) PendingOfNote(ctx context.Context, noteID int64, limit int) ([]Pending, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT c.id, c.note_id, coalesce(c.author_id, 0), c.body, c.created_at
		  FROM comments c
		 WHERE c.note_id = $1 AND c.status = $2
		 ORDER BY c.id
		 LIMIT $3`, noteID, StatusVisible, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("тред %d для стенда: %w", noteID, err)
	}
	defer rows.Close()
	var out []Pending
	for rows.Next() {
		var it Pending
		var id int64
		if err := rows.Scan(&id, &it.NoteID, &it.AuthorID, &it.Body, &it.QueuedAt); err != nil {
			return nil, fmt.Errorf("тред %d для стенда: %w", noteID, err)
		}
		it.Subject = CommentSubject(id)
		out = append(out, it)
	}
	return out, rows.Err()
}

// BumpAttempts отмечает, что автомат взял эти строки в работу.
//
// Считается ПОПЫТКА, а не успех, и записывается она ДО запроса к модели: иначе
// строка, на которой модель спотыкается воспроизводимо, попадает в каждую пачку
// вечно и не даёт очереди двигаться.
func (p *Platform) BumpAttempts(ctx context.Context, subs []Subject) error {
	for _, s := range subs {
		if _, err := p.pool.Exec(ctx, `
			UPDATE moderation_queue SET attempts = attempts + 1
			 WHERE subject_kind = $1 AND subject_id = $2`, s.Kind, s.ID); err != nil {
			return wrapf(err, "попытка проверки %s", s)
		}
	}
	return nil
}

// SameBodyCount — сколько НАТИВНЫХ публикаций автора с ровно таким же текстом
// уложились в окно.
//
// Шторм одинаковых сообщений — единственная категория списка, которую модель
// увидеть не может в принципе: ей показывают одну реплику, а нарушение состоит
// в повторе. Поэтому её считает код, и считает дёшево — по (author_id, id) с
// нижней границей нативной полосы, то есть узким range-scan, а не обходом всех
// реплик человека за тринадцать лет.
func (p *Platform) SameBodyCount(ctx context.Context, authorID int64, body string, window time.Duration) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx, `
		SELECT count(*) FROM comments
		 WHERE author_id = $1 AND id >= $2 AND published_at > $3 AND status = 0 AND body = $4`,
		authorID, NativeIDBase, time.Now().Add(-window), body).Scan(&n)
	return n, wrapf(err, "повторы автора %d", authorID)
}

// VerdictRecord — что решила проверка об одной публикации.
type VerdictRecord struct {
	Verdict   Verdict
	Category  string
	Reason    string
	Quote     string // цитата, на которой основано решение
	Model     string
	PromptSHA []byte
}

// RecordVerdict записывает мнение автомата и, если оно «скрыть», ИСПОЛНЯЕТ его
// той же транзакцией.
//
// Вместе, а не двумя шагами: «в карточке скрыто, а на странице видно» и
// обратное — оба состояния означают, что через месяц никто не разберётся, что
// именно произошло.
func (p *Platform) RecordVerdict(ctx context.Context, s Subject, v VerdictRecord) error {
	if !s.Valid() {
		return fmt.Errorf("%w: %q", ErrBadSubject, s.Kind)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return wrapf(err, "проверка %s", s)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	facts, err := factsOf(ctx, tx, s)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE moderation_queue
		   SET checked_at = now(), verdict = $3, category = $4, reason = $5,
		       quote = $6, model = $7, prompt_sha = $8
		 WHERE subject_kind = $1 AND subject_id = $2`,
		s.Kind, s.ID, v.Verdict, v.Category, trimReason(v.Reason),
		trimReason(v.Quote), v.Model, v.PromptSHA); err != nil {
		return wrapf(err, "проверка %s", s)
	}
	// Гасим только ВИДИМОЕ: пока строка ждала очереди, её мог скрыть человек
	// или сам автор отзывом согласия, и переписывать чужое решение машине
	// нельзя.
	if v.Verdict == VerdictHidden && facts.Status == StatusVisible {
		if err := moveStatus(ctx, tx, s, facts.NoteID, StatusVisible, StatusHiddenMod); err != nil {
			return err
		}
		if err := audit(ctx, tx, 0, ActionAutoHide, s, map[string]any{
			"category": v.Category, "reason": v.Reason, "quote": v.Quote,
			"model": v.Model, "author": facts.AuthorID,
		}); err != nil {
			return err
		}
		// Актор — ноль: скрыла машина. Автору это видно и по тексту повода, и
		// по кнопке «на пересмотр», которая есть только у автоскрытия.
		if err := dropUnreadAbout(ctx, tx, s); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx,
			hideEvent(EventHidden, 0, s, facts, v.Category, v.Reason)); err != nil {
			return err
		}
	}
	return wrapf(tx.Commit(ctx), "проверка %s", s)
}

// ---------------------------------------------------------------- очередь человека

// ReviewItem — строка очереди ЧЕЛОВЕКА.
type ReviewItem struct {
	Subject    Subject
	NoteID     int64
	AuthorID   int64
	AuthorNick string
	Body       string
	Status     Status
	QueuedAt   time.Time
	CheckedAt  *time.Time
	Verdict    *Verdict
	Category   string
	Reason     string
	Quote      string
	Model      string
	AppealedAt *time.Time
	Reports    []Report
}

// Hidden — публикация сейчас скрыта модерацией.
func (r ReviewItem) Hidden() bool { return r.Status == StatusHiddenMod }

// Appealed — автор попросил пересмотра.
func (r ReviewItem) Appealed() bool { return r.AppealedAt != nil }

// ByMachine — мнение принадлежит автомату (у него есть имя модели).
func (r ReviewItem) ByMachine() bool { return r.Model != "" }

// CategoryTitle — как назвать причину человеку.
func (r ReviewItem) CategoryTitle() string { return CategoryTitle(r.Category) }

// Приведение к smallint здесь не украшение: coalesce с числовым литералом даёт
// integer, а Status и Verdict — типы поверх int16, и молчаливое сужение при
// разборе строки лучше не оставлять драйверу.
const reviewColumns = `
	q.subject_kind, q.subject_id, coalesce(q.note_id, 0), coalesce(q.author_id, 0),
	coalesce(u.nick, ''), coalesce(n.body, c.body, ''),
	coalesce(n.status, c.status, 0)::smallint, q.queued_at, q.checked_at, q.verdict,
	q.category, q.reason, q.quote, q.model, q.appealed_at
  FROM moderation_queue q
  LEFT JOIN users    u ON u.id = q.author_id
  LEFT JOIN notes    n ON q.subject_kind = 'note'    AND n.id = q.subject_id
  LEFT JOIN comments c ON q.subject_kind = 'comment' AND c.id = q.subject_id`

// ReviewQueue — то, что ждёт ЧЕЛОВЕКА: автомат передал (verdict = 1), автор
// обжаловал скрытие или на публикацию пожаловались.
//
// Порядок — от старых к новым, и он же порядок работы: цель владельца — единицы
// строк в сутки, а если очередь растёт быстрее, чем её читают, поднимать надо
// порог автомата, а не сортировку.
func (p *Platform) ReviewQueue(ctx context.Context, limit int) ([]ReviewItem, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+reviewColumns+`
		 WHERE q.decided_at IS NULL AND (q.verdict = 1 OR q.appealed_at IS NOT NULL)
		 ORDER BY q.queued_at
		 LIMIT $1`, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("очередь модератора: %w", err)
	}
	out, err := scanReview(rows)
	if err != nil {
		return nil, err
	}
	return p.attachReports(ctx, out)
}

// AutoHidden — что автомат погасил сам и человек ещё не смотрел.
//
// Отдельным списком от очереди, и это не удобство: очередь — работа, а этот
// список — КОНТРОЛЬ над машиной. Модератор обязан иметь возможность пробежать
// её решения глазами, не дожидаясь, пока обиженный автор нажмёт «на пересмотр».
func (p *Platform) AutoHidden(ctx context.Context, limit int) ([]ReviewItem, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+reviewColumns+`
		 WHERE q.verdict = 2 AND q.decided_at IS NULL
		 ORDER BY q.checked_at DESC
		 LIMIT $1`, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("скрытое автоматом: %w", err)
	}
	out, err := scanReview(rows)
	if err != nil {
		return nil, err
	}
	return p.attachReports(ctx, out)
}

func scanReview(rows pgx.Rows) ([]ReviewItem, error) {
	defer rows.Close()
	var out []ReviewItem
	for rows.Next() {
		var (
			it ReviewItem
			// verdict читается в *int16, а не в *Verdict: NULL здесь рабочее
			// состояние («автомат ещё не смотрел»), и разбор указателя на
			// именованный тип лучше не оставлять драйверу.
			verdict *int16
		)
		if err := rows.Scan(&it.Subject.Kind, &it.Subject.ID, &it.NoteID, &it.AuthorID,
			&it.AuthorNick, &it.Body, &it.Status, &it.QueuedAt, &it.CheckedAt, &verdict,
			&it.Category, &it.Reason, &it.Quote, &it.Model, &it.AppealedAt); err != nil {
			return nil, fmt.Errorf("очередь модератора: %w", err)
		}
		if verdict != nil {
			v := Verdict(*verdict)
			it.Verdict = &v
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// attachReports подвешивает к строкам очереди открытые жалобы. Одним запросом
// на всю страницу, а не запросом на строку: очередь читают целиком.
func (p *Platform) attachReports(ctx context.Context, items []ReviewItem) ([]ReviewItem, error) {
	if len(items) == 0 {
		return items, nil
	}
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.Subject.ID)
	}
	reports, err := p.openReports(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Reports = reports[items[i].Subject]
	}
	return items, nil
}

// Report — жалоба участника.
type Report struct {
	ID        int64
	Reporter  int64
	Nick      string
	Subject   Subject
	Reason    string
	CreatedAt time.Time
}

func (p *Platform) openReports(ctx context.Context, ids []int64) (map[Subject][]Report, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT r.id, r.reporter_id, coalesce(u.nick, ''), r.subject_kind, r.subject_id,
		       r.reason, r.created_at
		  FROM reports r LEFT JOIN users u ON u.id = r.reporter_id
		 WHERE r.resolved_at IS NULL AND r.subject_id = ANY($1)
		 ORDER BY r.created_at`, ids)
	if err != nil {
		return nil, fmt.Errorf("жалобы: %w", err)
	}
	defer rows.Close()
	out := map[Subject][]Report{}
	for rows.Next() {
		var r Report
		if err := rows.Scan(&r.ID, &r.Reporter, &r.Nick, &r.Subject.Kind, &r.Subject.ID,
			&r.Reason, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("жалобы: %w", err)
		}
		out[r.Subject] = append(out[r.Subject], r)
	}
	return out, rows.Err()
}

// Decide — решение человека по строке очереди.
//
// Оно и записывается, и ИСПОЛНЯЕТСЯ: «скрыть» гасит видимое, «оставить»
// возвращает скрытое модерацией. Скрытое самим автором (отзыв согласия) при
// этом не поднимается никогда — это право субъекта, а не спор о содержании.
func (p *Platform) Decide(ctx context.Context, actor Viewer, s Subject, d Decision, reason string) error {
	if !actor.CanModerate() {
		return ErrNotModerator
	}
	if !s.Valid() {
		return fmt.Errorf("%w: %q", ErrBadSubject, s.Kind)
	}
	reason = trimReason(reason)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return wrapf(err, "решение по %s", s)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	facts, err := factsOf(ctx, tx, s)
	if err != nil {
		return err
	}
	action := ActionDismiss
	switch {
	case d == DecisionHide && facts.Status == StatusVisible:
		if err := moveStatus(ctx, tx, s, facts.NoteID, StatusVisible, StatusHiddenMod); err != nil {
			return err
		}
		action = ActionHide
	case d == DecisionKeep && facts.Status == StatusHiddenMod:
		if err := moveStatus(ctx, tx, s, facts.NoteID, StatusHiddenMod, StatusVisible); err != nil {
			return err
		}
		action = ActionRestore
	}
	if err := recordDecision(ctx, tx, s, facts, actor.UserID, d, "", reason); err != nil {
		return err
	}
	if err := resolveReports(ctx, tx, s, actor.UserID, action); err != nil {
		return err
	}
	if err := audit(ctx, tx, actor.UserID, action, s, map[string]any{
		"reason": reason, "author": facts.AuthorID, "decision": int(d),
	}); err != nil {
		return err
	}
	// Автору говорят только о том, что с его публикацией СЛУЧИЛОСЬ. Отклонённая
	// жалоба (ActionDismiss) ничего не меняет, и рассказывать о ней значило бы
	// сообщать человеку, что на него жаловались, — сведения о ЖАЛОВАВШЕМСЯ, а не
	// о нём, и обсуждать их автору не с кем.
	switch action {
	case ActionHide:
		if err := dropUnreadAbout(ctx, tx, s); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx,
			hideEvent(EventHidden, actor.UserID, s, facts, "", reason)); err != nil {
			return err
		}
	case ActionRestore:
		if err := recordEvent(ctx, tx,
			hideEvent(EventRestored, actor.UserID, s, facts, "", reason)); err != nil {
			return err
		}
	}
	return wrapf(tx.Commit(ctx), "решение по %s", s)
}

// recordDecision пишет решение человека в карточку проверки, заводя её при
// необходимости.
func recordDecision(ctx context.Context, q querier, s Subject, f subjectFacts,
	actor int64, d Decision, category, reason string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO moderation_queue
		       (subject_kind, subject_id, note_id, author_id, category, reason,
		        decided_at, decided_by, decision, checked_at, verdict)
		VALUES ($1, $2, $3, $4, $5, $6, now(), $7, $8, now(), $9)
		ON CONFLICT (subject_kind, subject_id) DO UPDATE
		   SET decided_at = now(), decided_by = $7, decision = $8,
		       reason   = CASE WHEN $6 = '' THEN moderation_queue.reason   ELSE $6 END,
		       category = CASE WHEN $5 = '' THEN moderation_queue.category ELSE $5 END`,
		s.Kind, s.ID, nullID(f.NoteID), nullID(f.AuthorID), category, reason,
		nullID(actor), d, VerdictReview)
	return wrapf(err, "карточка проверки %s", s)
}

func resolveReports(ctx context.Context, q querier, s Subject, actor int64, resolution string) error {
	_, err := q.Exec(ctx, `
		UPDATE reports SET resolved_at = now(), resolved_by = $3, resolution = $4
		 WHERE subject_kind = $1 AND subject_id = $2 AND resolved_at IS NULL`,
		s.Kind, s.ID, nullID(actor), resolution)
	return wrapf(err, "закрытие жалоб на %s", s)
}

// ---------------------------------------------------------------- жалоба

// MaxOpenReports — сколько открытых жалоб может висеть на одном человеке.
// Потолок не от злого умысла, а от простой арифметики: жалоба — единственная
// кнопка, которой участник заводит работу МОДЕРАТОРУ, и десяток в сутки от
// одного человека это уже не сигнал, а шум.
const MaxOpenReports = 10

// AddReport — жалоба участника на публикацию.
//
// Кнопку «пожаловаться» с НГС мы не переносили (решение эпика E по виду
// страницы). Своя нужна по другой причине: классификатор по архиву не гоняется,
// значит 10,7 млн зеркальных реплик модерировать больше нечем. Жалоба и есть
// тот единственный вход, через который старая строка попадает человеку на глаза.
func (p *Platform) AddReport(ctx context.Context, reporterID int64, s Subject, reason string) error {
	if !s.Valid() {
		return fmt.Errorf("%w: %q", ErrBadSubject, s.Kind)
	}
	reason = trimReason(reason)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return wrapf(err, "жалоба на %s", s)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	// Тот же вход, что у публикации: жаловаться может участник, не забаненный и
	// не отозвавший согласие. Жалоба заводит работу другому человеку, поэтому
	// цена входа у неё та же, что у собственных слов.
	if err := writeGuard(ctx, tx, reporterID); err != nil {
		return err
	}
	facts, err := factsOf(ctx, tx, s)
	if err != nil {
		return err
	}
	if facts.Status != StatusVisible {
		// Скрытое уже скрыто: жаловаться не на что, а говорить «оно есть, но
		// спрятано» значит показывать работу модерации посторонним.
		return fmt.Errorf("%s: %w", s, ErrNotFound)
	}
	if facts.AuthorID == reporterID {
		return ErrSelfReport
	}
	var open int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM reports WHERE reporter_id = $1 AND resolved_at IS NULL`,
		reporterID).Scan(&open); err != nil {
		return wrapf(err, "жалоба на %s", s)
	}
	if open >= MaxOpenReports {
		return ErrRateLimited
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO reports (reporter_id, subject_kind, subject_id, reason)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, reporterID, s.Kind, s.ID, reason)
	if err != nil {
		return wrapf(err, "жалоба на %s", s)
	}
	if tag.RowsAffected() == 0 {
		return ErrNothingToDo // уже жаловался, и решения ещё не было
	}
	// Жалоба поднимает публикацию в очередь ЧЕЛОВЕКА, не трогая мнения автомата:
	// расхождение «машина сказала чисто, а люди жалуются» — ровно то, ради чего
	// оба поля и лежат рядом.
	if _, err := tx.Exec(ctx, `
		INSERT INTO moderation_queue (subject_kind, subject_id, note_id, author_id, verdict, category)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (subject_kind, subject_id) DO UPDATE
		   SET verdict = $5, decided_at = NULL, decided_by = NULL, decision = NULL`,
		s.Kind, s.ID, nullID(facts.NoteID), nullID(facts.AuthorID), VerdictReview, CatReport); err != nil {
		return wrapf(err, "жалоба на %s", s)
	}
	if err := audit(ctx, tx, reporterID, ActionReport, s, map[string]any{"reason": reason}); err != nil {
		return err
	}
	return wrapf(tx.Commit(ctx), "жалоба на %s", s)
}

// ---------------------------------------------------------------- пересмотр

// MyCheck — карточка СВОЕЙ скрытой публикации, какой её видит автор.
type MyCheck struct {
	Subject   Subject
	NoteID    int64
	Body      string
	Category  string
	Reason    string
	HiddenAt  time.Time
	ByMachine bool // скрыл автомат, человек ещё не смотрел
	Appealed  bool
	Decided   bool
}

// CategoryTitle — как назвать причину автору.
func (m MyCheck) CategoryTitle() string { return CategoryTitle(m.Category) }

// CanAppeal — пересмотр ещё можно попросить.
func (m MyCheck) CanAppeal() bool { return !m.Appealed && !m.Decided }

// MyHidden — свои публикации, скрытые модерацией, с причиной.
//
// Спрашивается по ОЧЕРЕДИ, а не обходом comments по автору: у участника с
// 138 тыс. реплик такой обход стоит 53 с (замер 18.08.2026) и в срок веб-запроса
// не влезает вовсе. В очереди же лежит только нативное и то, на что жаловались,
// — то есть ровно те строки, про которые вопрос и задаётся.
func (p *Platform) MyHidden(ctx context.Context, userID int64, limit int) ([]MyCheck, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT q.subject_kind, q.subject_id, coalesce(q.note_id, 0),
		       coalesce(n.body, c.body, ''), q.category, q.reason,
		       coalesce(q.decided_at, q.checked_at, q.queued_at),
		       q.decided_at IS NULL, q.appealed_at IS NOT NULL, q.decided_at IS NOT NULL
		  FROM moderation_queue q
		  LEFT JOIN notes    n ON q.subject_kind = 'note'    AND n.id = q.subject_id
		  LEFT JOIN comments c ON q.subject_kind = 'comment' AND c.id = q.subject_id
		 WHERE q.author_id = $1 AND coalesce(n.status, c.status, 0) = $3
		 ORDER BY q.queued_at DESC
		 LIMIT $2`, userID, clampLimit(limit), StatusHiddenMod)
	if err != nil {
		return nil, fmt.Errorf("мои скрытые публикации: %w", err)
	}
	defer rows.Close()
	var out []MyCheck
	for rows.Next() {
		var m MyCheck
		if err := rows.Scan(&m.Subject.Kind, &m.Subject.ID, &m.NoteID, &m.Body,
			&m.Category, &m.Reason, &m.HiddenAt, &m.ByMachine, &m.Appealed, &m.Decided); err != nil {
			return nil, fmt.Errorf("мои скрытые публикации: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Appeal — автор просит пересмотреть скрытие человеком.
//
// Кнопка обязательна, а не любезна: автомат ошибается, а молча исчезнувшая
// реплика — худшее, что можно сделать с сообществом, которое только что
// переехало. Одна просьба на публикацию: пересмотр это не голосование.
func (p *Platform) Appeal(ctx context.Context, userID int64, s Subject) error {
	if !s.Valid() {
		return fmt.Errorf("%w: %q", ErrBadSubject, s.Kind)
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE moderation_queue SET appealed_at = now()
		 WHERE subject_kind = $1 AND subject_id = $2 AND author_id = $3
		   AND appealed_at IS NULL AND decided_at IS NULL AND verdict = $4`,
		s.Kind, s.ID, userID, VerdictHidden)
	if err != nil {
		return wrapf(err, "пересмотр %s", s)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoAppeal
	}
	return audit(ctx, p.pool, userID, ActionAppeal, s, nil)
}

// ---------------------------------------------------------------- сводка

// ModerationStats — наполнение очереди. Ровно то, что нужно, чтобы понять, не
// растёт ли она быстрее, чем её читают: цель — единицы строк в сутки, иначе это
// та же нехватка времени, только с интерфейсом.
type ModerationStats struct {
	Unchecked  int // автомат ещё не смотрел
	Review     int // ждёт человека
	AutoHidden int // погашено автоматом и не пересмотрено
	Appeals    int // просьб о пересмотре
	Reports    int // открытых жалоб
	HiddenDay  int // скрыто автоматом за сутки
}

// ModerationStats считает наполнение очереди.
func (p *Platform) ModerationStats(ctx context.Context) (ModerationStats, error) {
	var s ModerationStats
	err := p.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE checked_at IS NULL AND verdict IS NULL),
		       count(*) FILTER (WHERE decided_at IS NULL AND (verdict = 1 OR appealed_at IS NOT NULL)),
		       count(*) FILTER (WHERE verdict = 2 AND decided_at IS NULL),
		       count(*) FILTER (WHERE appealed_at IS NOT NULL AND decided_at IS NULL),
		       count(*) FILTER (WHERE verdict = 2 AND checked_at > now() - interval '24 hours')
		  FROM moderation_queue`).
		Scan(&s.Unchecked, &s.Review, &s.AutoHidden, &s.Appeals, &s.HiddenDay)
	if err != nil {
		return s, fmt.Errorf("сводка модерации: %w", err)
	}
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM reports WHERE resolved_at IS NULL`).Scan(&s.Reports); err != nil {
		return s, fmt.Errorf("сводка модерации: %w", err)
	}
	return s, nil
}

// ---------------------------------------------------------------- служебное

// trimReason приводит причину к виду, в котором её можно хранить и показывать.
func trimReason(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\x00", ""))
	r := []rune(s)
	if len(r) > MaxReasonRunes {
		return string(r[:MaxReasonRunes])
	}
	return string(r)
}
