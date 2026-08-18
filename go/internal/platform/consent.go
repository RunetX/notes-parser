package platform

// Согласия.
//
// Их ДВА, и это не педантизм: ч. 1 ст. 10.1 152-ФЗ требует брать согласие на
// распространение ОТДЕЛЬНО от общего (ст. 9) и прямо запрещает считать
// согласием молчание или заранее проставленную галочку. Отсюда две строки в
// consents, два документа и два отдельных нажатия — одним экраном с двумя
// галочками это собрать нельзя.
//
// Тексты версионируются, потому что через год «на что человек соглашался»
// придётся доказывать, а не вспоминать. Отсюда правило, которое стережёт
// EnsureConsentDocs: опубликованный текст неизменяем. Правка требует НОВОГО
// номера версии, и попытка тихо переписать выпущенную редакцию — ошибка старта,
// а не предупреждение в логе.
//
// Тексты лежат файлами рядом (consents/<вид>.v<N>.txt) и вшиты в бинарник:
// документ, который можно подменить на диске, доказательством не является.

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed consents/*.txt
var consentFS embed.FS

// Виды согласий — значения колонки consents.kind.
const (
	ConsentProcessing   = "processing"   // ст. 9: обработка вообще
	ConsentDistribution = "distribution" // ст. 10.1: распространение
)

// Operator — реквизиты того, кто обрабатывает. Подставляются в текст ДО
// публикации: в consent_docs обязан лежать финальный текст, иначе доказывать
// нечем. Поэтому смена реквизитов — это новая версия документа, и
// EnsureConsentDocs на такую попытку честно ругается.
type Operator struct {
	Name    string
	Contact string
}

func (o Operator) filled() Operator {
	if o.Name == "" {
		o.Name = "Владелец площадки"
	}
	if o.Contact == "" {
		o.Contact = "через бота РюмкинЪ в Telegram или MAX"
	}
	return o
}

// ConsentDoc — редакция документа.
type ConsentDoc struct {
	Kind    string
	Version int
	Title   string // первая строка файла
	Body    string // весь текст, включая заголовок
	SHA     []byte
}

// ConsentDocs — все редакции, вшитые в этот бинарник, с подставленными
// реквизитами оператора.
func ConsentDocs(op Operator) ([]ConsentDoc, error) {
	names, err := fs.Glob(consentFS, "consents/*.txt")
	if err != nil {
		return nil, fmt.Errorf("тексты согласий: %w", err)
	}
	op = op.filled()
	out := make([]ConsentDoc, 0, len(names))
	for _, n := range names {
		kind, version, err := parseConsentName(path.Base(n))
		if err != nil {
			return nil, err
		}
		raw, err := consentFS.ReadFile(n)
		if err != nil {
			return nil, fmt.Errorf("текст согласия %s: %w", n, err)
		}
		// text/template, а не html: документ показывается как текст, и
		// экранирование кавычек в реквизитах здесь только испортило бы его.
		t, err := template.New(n).Option("missingkey=error").Parse(string(raw))
		if err != nil {
			return nil, fmt.Errorf("текст согласия %s: %w", n, err)
		}
		var b strings.Builder
		if err := t.Execute(&b, op); err != nil {
			return nil, fmt.Errorf("текст согласия %s: %w", n, err)
		}
		body := b.String()
		sum := sha256.Sum256([]byte(body))
		title, _, _ := strings.Cut(body, "\n")
		out = append(out, ConsentDoc{
			Kind: kind, Version: version,
			Title: strings.TrimSpace(title), Body: body, SHA: sum[:],
		})
	}
	return out, nil
}

// CurrentConsentDocs — по одной, самой новой редакции каждого вида: именно их
// спрашивают у входящего.
func CurrentConsentDocs(op Operator) ([]ConsentDoc, error) {
	all, err := ConsentDocs(op)
	if err != nil {
		return nil, err
	}
	best := map[string]ConsentDoc{}
	for _, d := range all {
		if cur, ok := best[d.Kind]; !ok || d.Version > cur.Version {
			best[d.Kind] = d
		}
	}
	// Порядок фиксирован: сперва общее согласие, потом распространение —
	// согласиться на публикацию, не согласившись на обработку, бессмысленно.
	out := make([]ConsentDoc, 0, 2)
	for _, k := range []string{ConsentProcessing, ConsentDistribution} {
		if d, ok := best[k]; ok {
			out = append(out, d)
		}
	}
	return out, nil
}

func parseConsentName(name string) (string, int, error) {
	base := strings.TrimSuffix(name, ".txt")
	kind, ver, ok := strings.Cut(base, ".v")
	if !ok {
		return "", 0, fmt.Errorf("имя текста согласия %q: ожидалось <вид>.v<номер>.txt", name)
	}
	n, err := strconv.Atoi(ver)
	if err != nil || n < 1 {
		return "", 0, fmt.Errorf("имя текста согласия %q: номер версии не разобран", name)
	}
	if kind != ConsentProcessing && kind != ConsentDistribution {
		return "", 0, fmt.Errorf("имя текста согласия %q: неизвестный вид %q", name, kind)
	}
	return kind, n, nil
}

// EnsureConsentDocs публикует тексты в базу. Идемпотентна; расхождение текста
// при том же номере версии — ОТКАЗ, потому что молча переписанная редакция
// превращает все прежние согласия в бумажку без содержания.
func (p *Platform) EnsureConsentDocs(ctx context.Context, op Operator) error {
	docs, err := ConsentDocs(op)
	if err != nil {
		return err
	}
	for _, d := range docs {
		var stored []byte
		err := p.pool.QueryRow(ctx,
			`SELECT sha256 FROM consent_docs WHERE kind = $1 AND version = $2`, d.Kind, d.Version).Scan(&stored)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if _, err := p.pool.Exec(ctx, `
				INSERT INTO consent_docs (kind, version, sha256, body)
				VALUES ($1, $2, $3, $4)`, d.Kind, d.Version, d.SHA, d.Body); err != nil {
				return fmt.Errorf("публикация текста %s v%d: %w", d.Kind, d.Version, err)
			}
		case err != nil:
			return fmt.Errorf("проверка текста %s v%d: %w", d.Kind, d.Version, err)
		case string(stored) != string(d.SHA):
			return fmt.Errorf("текст согласия %s v%d изменён без смены номера версии: "+
				"опубликованная редакция неизменяема, заведите v%d", d.Kind, d.Version, d.Version+1)
		}
	}
	return nil
}

// ConsentRecord — что человек подписал.
type ConsentRecord struct {
	Kind      string
	Version   int
	GrantedAt time.Time
	RevokedAt *time.Time
}

// Live — согласие действует.
func (c ConsentRecord) Live() bool { return c.RevokedAt == nil }

// Consents — согласия человека по видам (последнее по каждому виду).
type Consents map[string]ConsentRecord

// Has — по этому виду есть действующее согласие нужной редакции.
func (c Consents) Has(kind string, version int) bool {
	r, ok := c[kind]
	return ok && r.Live() && r.Version >= version
}

// UserConsents читает согласия человека.
func (p *Platform) UserConsents(ctx context.Context, userID int64) (Consents, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT DISTINCT ON (kind) kind, version, granted_at, revoked_at
		  FROM consents WHERE user_id = $1
		 ORDER BY kind, granted_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("согласия %d: %w", userID, err)
	}
	defer rows.Close()
	out := Consents{}
	for rows.Next() {
		var r ConsentRecord
		if err := rows.Scan(&r.Kind, &r.Version, &r.GrantedAt, &r.RevokedAt); err != nil {
			return nil, fmt.Errorf("согласия %d: %w", userID, err)
		}
		out[r.Kind] = r
	}
	return out, rows.Err()
}

// MissingConsent — первый документ, которого человеку не хватает, чтобы
// пользоваться площадкой. Пустой Kind означает «всё подписано».
//
// Спрашивается по одному, а не списком: два документа на одном экране — ровно
// то, что закон и запрещает.
func (p *Platform) MissingConsent(ctx context.Context, userID int64, op Operator) (ConsentDoc, error) {
	docs, err := CurrentConsentDocs(op)
	if err != nil {
		return ConsentDoc{}, err
	}
	have, err := p.UserConsents(ctx, userID)
	if err != nil {
		return ConsentDoc{}, err
	}
	for _, d := range docs {
		if !have.Has(d.Kind, d.Version) {
			return d, nil
		}
	}
	return ConsentDoc{}, nil
}

// GrantConsent записывает согласие. Отдельной строкой на каждое нажатие:
// история согласий и отзывов — это и есть доказательство.
func (p *Platform) GrantConsent(ctx context.Context, userID int64, kind string, version int, ua string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("согласие %s: %w", kind, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	if _, err := tx.Exec(ctx, `
		INSERT INTO consents (user_id, kind, version, ua) VALUES ($1, $2, $3, $4)`,
		userID, kind, version, trimUA(ua)); err != nil {
		return fmt.Errorf("согласие %s: %w", kind, err)
	}
	// Согласие на распространение снимает рубильник «скрыть всё моё»: иначе
	// человек, отозвавший и вернувший согласие, остался бы невидимым молча.
	//
	// Сначала спрашиваем, был ли он поднят, и только потом идём по публикациям:
	// у входящего ВПЕРВЫЕ прятать нечего, а обход стоит перебора всех его
	// реплик. Разница не косметическая — 18.08.2026 трое участников не смогли
	// пройти экран согласий вовсе: у одного 138 тыс. реплик, и запрос не
	// укладывался в срок веб-морды (5 с). FOR UPDATE держит строку до конца
	// транзакции: между «был ли поднят» и «опускаем» не должен влезть отзыв.
	if kind == ConsentDistribution {
		var hidden bool
		if err := tx.QueryRow(ctx,
			`SELECT hide_all FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&hidden); err != nil {
			return fmt.Errorf("согласие %s: %w", kind, err)
		}
		if hidden {
			if _, err := tx.Exec(ctx,
				`UPDATE users SET hide_all = false WHERE id = $1`, userID); err != nil {
				return fmt.Errorf("согласие %s: %w", kind, err)
			}
			if err := setOwnVisibility(ctx, tx, userID, StatusVisible); err != nil {
				return fmt.Errorf("согласие %s: %w", kind, err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("согласие %s: %w", kind, err)
	}
	return nil
}

// RevokeConsent отзывает согласие, и отзыв ИСПОЛНЯЕТСЯ сразу, а не ставится в
// очередь к модератору:
//
//   - распространение → hide_all, и публикации исчезают со страниц в тот же
//     момент;
//   - обработка вообще → плюс к тому человек перестаёт быть участником (kind
//     возвращается в тень) и все его сессии гасятся: обрабатывать больше нечего.
//
// Тексты при этом остаются в базе скрытыми — стереть их целиком, не разрушив
// чужие разговоры, нельзя, и это делает обезличивание (Ш7), а не отзыв.
func (p *Platform) RevokeConsent(ctx context.Context, userID int64, kind string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("отзыв согласия %s: %w", kind, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	kinds := []string{kind}
	if kind == ConsentProcessing {
		// Распространение — частный случай обработки: отозвать общее и оставить
		// его действующим невозможно даже формально.
		kinds = []string{ConsentProcessing, ConsentDistribution}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE consents SET revoked_at = now()
		 WHERE user_id = $1 AND kind = ANY($2) AND revoked_at IS NULL`, userID, kinds); err != nil {
		return fmt.Errorf("отзыв согласия %s: %w", kind, err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE users SET hide_all = true WHERE id = $1`, userID); err != nil {
		return fmt.Errorf("отзыв согласия %s: %w", kind, err)
	}
	if err := setOwnVisibility(ctx, tx, userID, StatusHiddenOwner); err != nil {
		return fmt.Errorf("отзыв согласия %s: %w", kind, err)
	}
	if kind == ConsentProcessing {
		if _, err := tx.Exec(ctx,
			`UPDATE users SET kind = $2 WHERE id = $1`, userID, KindShadow); err != nil {
			return fmt.Errorf("отзыв согласия %s: %w", kind, err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE web_sessions SET revoked_at = now()
			  WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
			return fmt.Errorf("отзыв согласия %s: %w", kind, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("отзыв согласия %s: %w", kind, err)
	}
	return nil
}

// setOwnVisibility прячет или возвращает ВСЕ публикации человека.
//
// Рубильник исполняется записью статусов, а не проверкой на чтении, и это
// решение зафиксировано в комментарии к feedQuery: иначе отзыв согласия стоил
// бы соединения с users на каждой странице ленты — и однажды его бы оттуда
// убрали «для скорости», тихо сломав исполнение закона.
//
// Скрытое МОДЕРАЦИЕЙ (StatusHiddenMod) не трогается ни в ту, ни в другую
// сторону: возврат согласия не отменяет решения модератора, а его отзыв не
// должен присваивать модераторскому скрытию чужую причину.
func setOwnVisibility(ctx context.Context, q querier, userID int64, to Status) error {
	from, delta := StatusVisible, -1
	if to == StatusVisible {
		from, delta = StatusHiddenOwner, 1
	}
	if _, err := q.Exec(ctx,
		`UPDATE notes SET status = $2 WHERE author_id = $1 AND status = $3`, userID, to, from); err != nil {
		return err
	}
	// Счётчик комментариев денормализован (лента не делает COUNT(*)), поэтому
	// после скрытия его надо поправить — иначе под заметкой останется
	// «Комментарии 42», а видно будет сорок.
	//
	// Правится он РАЗНИЦЕЙ, тем же способом, что и растёт (bumpNote), а не
	// пересчётом: пересчёт брал count(*) в КАЖДОМ треде, где человек когда-либо
	// отвечал, — у участника с 14 тыс. таких тредов это минуты на таблице в
	// 10,7 млн строк, то есть отказ вместо согласия (18.08.2026). Разница
	// считается по тем же строкам, которые только что сдвинулись, поэтому
	// стоит ровно столько же, сколько сам сдвиг.
	//
	// Считаем по RETURNING, а не по самой таблице: снимок у всех частей запроса
	// общий, и count(*) по comments здесь увидел бы статусы ДО сдвига.
	// Разошедшийся счётчик чинится RecountComments — он для этого и есть.
	_, err := q.Exec(ctx, `
		WITH moved AS (
		    UPDATE comments SET status = $2
		     WHERE author_id = $1 AND status = $3
		    RETURNING note_id
		), touched AS (
		    SELECT note_id, count(*) AS n FROM moved GROUP BY note_id
		)
		UPDATE notes n
		   SET comment_count = greatest(0, n.comment_count + $4::int * t.n)
		  FROM touched t
		 WHERE n.id = t.note_id`, userID, to, from, delta)
	return err
}
