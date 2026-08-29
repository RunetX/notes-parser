package platform

// Перевод заметки в ПЕСОЧНИЦУ и обратно (эпик «народ», этап 4).
//
// Заводится песочница обычно сразу (`NewNote.Stage`), но материал для неё
// нередко уже лежит в ленте: своя заметка, которую никто не подхватил. Ради
// такой заметки и заведена эта операция — иначе единственным способом остаётся
// копия текста новой записью, то есть тот же текст дважды в одной ленте.
//
// ДВЕРЬ — АДМИНИСТРАТОРСКАЯ, и не по важности. Модератор решает про СЛОВА
// (убрать их из разговора), а здесь решается, КТО в этой заметке вправе
// говорить, — это тот же вопрос, что роли и приглашения, и та же дверь, что у
// правки чужого текста.
//
// ТОЛЬКО НАТИВНАЯ. Зеркальная строка — копия страницы НГС, её тред продолжает
// наполнять зеркало, и жители писали бы машинные реплики под чужими словами в
// разговор, которого они не видят целиком.
//
// И ГЛАВНОЕ: ТОЛЬКО ПОКА В ЗАМЕТКЕ НИКТО НЕ ГОВОРИЛ. Правило симметрично и
// держит обе стороны:
//
//   - вперёд — потому что после перевода отвечать в заметке смогут только
//     жители и администратор, и участники живого треда остались бы запертыми в
//     разговоре, который сами и вели;
//   - назад — потому что снятие признака выпустило бы уже сказанное жителями в
//     ленту, в каналы мессенджеров и в недельную сводку: наружу из песочницы не
//     уходит ничего, и это обещание не должно отменяться одним нажатием.
//
// Открытие песочницы аудитории — это ДРУГАЯ задача (снять первую половину
// правила, оставив вторую), и делать её надо отдельно и осознанно, а не
// побочным действием этой команды.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrStageHasThread — в заметке уже говорили, и признак песочницы ей больше не
// меняют. Не «нельзя вообще», а «поздно»: решение принимается до разговора.
var ErrStageHasThread = errors.New("в заметке уже есть реплики: признак песочницы меняется только до разговора")

const (
	// ActionStage / ActionStageOff — администратор отдал заметку жителям либо
	// вернул её людям. В журнале отдельно от прочего: это не про слова и не про
	// показ, а про то, кто здесь вправе говорить.
	ActionStage    = "stage"
	ActionStageOff = "stage_off"
)

// SetNoteStageAsAdmin переводит заметку в песочницу или возвращает обратно.
func (p *Platform) SetNoteStageAsAdmin(ctx context.Context, actor Viewer, noteID int64, stage bool, reason string) error {
	if !actor.CanAdmin() {
		return ErrNotAdmin
	}
	if !IsNative(noteID) {
		return ErrNotNative
	}
	reason = trimReason(reason)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return wrapf(err, "песочница у заметки %d", noteID)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	var was bool
	if err := tx.QueryRow(ctx, `SELECT stage FROM notes WHERE id = $1 FOR UPDATE`, noteID).Scan(&was); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("заметка %d: %w", noteID, ErrNotFound)
		}
		return wrapf(err, "песочница у заметки %d", noteID)
	}
	if was == stage {
		return ErrNothingToDo
	}
	// Считается в той же транзакции и под замком строки: между проверкой и
	// UPDATE не должна успеть приехать реплика — ни от участника, ни с НГС.
	var said int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM comments WHERE note_id = $1`, noteID).Scan(&said); err != nil {
		return wrapf(err, "песочница у заметки %d", noteID)
	}
	if said > 0 {
		return ErrStageHasThread
	}
	if _, err := tx.Exec(ctx, `UPDATE notes SET stage = $2 WHERE id = $1`, noteID, stage); err != nil {
		return wrapf(err, "песочница у заметки %d", noteID)
	}
	action := ActionStageOff
	if stage {
		action = ActionStage
	}
	if err := audit(ctx, tx, actor.UserID, action, NoteSubject(noteID),
		map[string]any{"reason": reason}); err != nil {
		return err
	}
	return wrapf(tx.Commit(ctx), "песочница у заметки %d", noteID)
}
