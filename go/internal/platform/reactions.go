package platform

// Реакции: короткий ответ, на который иначе уходит целая реплика.
//
// Заведены решением владельца 18.08.2026 и НОВЫМИ здесь являются честно: на НГС
// кнопки не было, а смайл жил внутри реплики — из 1 642 652 комментариев замера
// смайл несут 22 948, а состоят из одного кода и ничего больше ровно 66. То
// есть это не перенос практики сайта, а замена ей: в 2026-м отдельное сообщение
// ради смайла избыточно.
//
// Показываются реакции смайлами САМОГО НГС (web/smiles.go): набор уже перенесён
// с сайта и узнаётся здешними людьми, а заводить второй язык знаков рядом с ним
// значило бы объяснять человеку разницу, которой нет.
//
// Кто нажал — не показывается никому и нигде, наружу идёт только счётчик. Это
// не забывчивость: список «кто согласился с этой репликой» — новые сведения о
// человеке, которых на сайте не было, и собирать их ради украшения страницы мы
// не станем. В базе автор нажатия, конечно, есть — иначе не отличить своё
// нажатие от чужого и не дать его снять.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReactionCodes — что предлагается нажать. Порядок кнопок — этот, и он не
// случаен: сперва согласие и благодарность (самое частое человеческое), потом
// смех и наблюдение. Коды взяты из верхушки архива по частоте, кроме respect —
// он реже, но занимает место, которого больше нечем занять.
var ReactionCodes = []string{"agree", "flowers", "respect", "boogi", "crazy2", "popcorn"}

// ErrBadReaction — кода нет в наборе. Молча проглатывать нельзя: набор задаём
// мы, и чужой код в базе означал бы кнопку, которую нечем нарисовать.
var ErrBadReaction = errors.New("такой реакции нет")

// reactionRates — потолок частоты. Считается по строкам, которые сейчас есть у
// человека, и этого ХВАТАЕТ не для всего: тот, кто жмёт одну и ту же кнопку
// туда-сюда, счётчик не растит и сюда не упрётся. Его ловит потолок частоты по
// клиенту (web/guard.go, POST стоит дороже страницы) — здесь же защита от
// другого: от прохода по треду с проставлением реакции всем подряд.
var reactionRates = []rateRule{{time.Hour, 120}}

// Reaction — одна кнопка под объектом: сколько нажали и нажимал ли ты.
type Reaction struct {
	Code  string
	Count int
	Mine  bool
}

// NewReaction — что нажали. CommentID == 0 означает саму заметку.
type NewReaction struct {
	UserID    int64
	NoteID    int64
	CommentID int64
	Code      string
}

// React ставит, меняет или снимает реакцию — смотря что стоит сейчас.
//
// Нажатие ТОЙ ЖЕ кнопки снимает её, нажатие другой — заменяет: одна реакция на
// человека на объект. Иначе под репликой копится частокол из шести значков от
// одного и того же человека, а «сколько людей согласилось» перестаёт читаться.
func (p *Platform) React(ctx context.Context, in NewReaction) error {
	if !slices.Contains(ReactionCodes, in.Code) {
		return ErrBadReaction
	}
	if in.UserID == 0 || in.NoteID == 0 {
		return errors.New("реакция без автора или без заметки")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("реакция: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	// Те же ворота, что и у публикации: тень не участник, забаненный молчит, а
	// отозвавший согласие на распространение не публикует ничего нового — в том
	// числе и своего согласия с чужой репликой.
	if err := writeGuard(ctx, tx, in.UserID); err != nil {
		return err
	}
	// Свой цикл вместо enforceRate: тот считает ПУБЛИКАЦИИ и потому передаёт в
	// запрос границу нативной полосы — у реакций своей полосы нет и быть не может.
	for _, rule := range reactionRates {
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM reactions WHERE user_id = $1 AND created_at > $2`,
			in.UserID, time.Now().Add(-rule.Window)).Scan(&n); err != nil {
			return fmt.Errorf("частота реакций: %w", err)
		}
		if n >= rule.Max {
			return ErrRateLimited
		}
	}
	if err := reactionTarget(ctx, tx, in); err != nil {
		return err
	}

	// Снять или поставить — решает то, что уже стоит. Один DELETE вместо
	// «сначала спросим»: между вопросом и ответом человек успевает нажать
	// дважды, и второе нажатие потеряло бы смысл.
	tag, err := tx.Exec(ctx, `
		DELETE FROM reactions
		 WHERE note_id = $1 AND comment_id IS NOT DISTINCT FROM $2
		   AND user_id = $3 AND code = $4`,
		in.NoteID, nullID(in.CommentID), in.UserID, in.Code)
	if err != nil {
		return fmt.Errorf("снятие реакции: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO reactions (note_id, comment_id, user_id, code)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT ON CONSTRAINT reactions_one
			DO UPDATE SET code = EXCLUDED.code, created_at = now()`,
			in.NoteID, nullID(in.CommentID), in.UserID, in.Code); err != nil {
			return fmt.Errorf("реакция: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("реакция: %w", err)
	}
	return nil
}

// reactionTarget проверяет, что нажимать есть на что и что тред открыт.
//
// Скрытое модерацией для нажимающего просто отсутствует — то же правило, что и
// при публикации комментария: рассказывать «оно есть, но спрятано» значит
// показывать работу модерации посторонним.
func reactionTarget(ctx context.Context, q querier, in NewReaction) error {
	var (
		locked bool
		status Status
	)
	err := q.QueryRow(ctx, `SELECT locked, status FROM notes WHERE id = $1`, in.NoteID).
		Scan(&locked, &status)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != StatusVisible) {
		return fmt.Errorf("заметка %d: %w", in.NoteID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("реакция на заметку %d: %w", in.NoteID, err)
	}
	if locked {
		return ErrThreadLocked
	}
	if in.CommentID == 0 {
		return nil
	}
	// Комментарий проверяется ВМЕСТЕ с заметкой: нажатие приходит формой, а в
	// форме можно поменять что угодно — и реакция к чужому треду разошлась бы с
	// note_id, по которому её потом читают.
	var cstatus Status
	err = q.QueryRow(ctx, `SELECT status FROM comments WHERE id = $1 AND note_id = $2`,
		in.CommentID, in.NoteID).Scan(&cstatus)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && cstatus != StatusVisible) {
		return fmt.Errorf("комментарий %d: %w", in.CommentID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("реакция на комментарий %d: %w", in.CommentID, err)
	}
	return nil
}

// NoteReactions — реакции заметки и всего её треда ОДНИМ запросом. Ключ 0 —
// сама заметка, остальные — id комментариев.
//
// Один запрос, а не по одному на реплику, — это и есть причина, по которой
// note_id стоит в каждой строке (см. миграцию 0008): на странице бывает под
// девятьсот комментариев, и девятьсот запросов положили бы её на том же
// единственном ядре, где живёт зеркало.
func (p *Platform) NoteReactions(ctx context.Context, viewerID, noteID int64) (map[int64][]Reaction, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT coalesce(comment_id, 0), code, count(*)::int, bool_or(user_id = $2)
		  FROM reactions
		 WHERE note_id = $1
		 GROUP BY 1, 2
		 ORDER BY 1, 3 DESC, 2`, noteID, viewerID)
	if err != nil {
		return nil, fmt.Errorf("реакции заметки %d: %w", noteID, err)
	}
	defer rows.Close()
	out := map[int64][]Reaction{}
	for rows.Next() {
		var (
			target int64
			r      Reaction
		)
		if err := rows.Scan(&target, &r.Code, &r.Count, &r.Mine); err != nil {
			return nil, fmt.Errorf("реакции заметки %d: %w", noteID, err)
		}
		out[target] = append(out[target], r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("реакции заметки %d: %w", noteID, err)
	}
	return out, nil
}
