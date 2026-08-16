package digest

// Публикация выпуска: идемпотентная и резюмируемая через message_targets.
// Части выпуска — kind digest, ref_id «<неделя>#<n>»; головная запись
// «<неделя>» ставится после всех частей: её наличие означает «выпуск в этом
// мессенджере опубликован целиком». Правка черновика после частичной
// публикации не подхватится уже отправленными частями — докатываются только
// недостающие.

import (
	"context"
	"fmt"

	"lovegw/internal/store"
)

// Publish постит выпуск weekID в мессенджер приёмника. Возвращает число
// отправленных этим вызовом частей; (0, nil) — выпуск уже был опубликован.
func Publish(ctx context.Context, st *store.Store, p Publisher, d Draft, weekID, siteBase string) (int, error) {
	if _, _, done, err := st.Target(ctx, p.Name(), store.TargetDigest, weekID); err != nil {
		return 0, err
	} else if done {
		return 0, nil
	}
	blocks, err := ResolveLinks(ctx, st, d, p, siteBase)
	if err != nil {
		return 0, fmt.Errorf("резолв ссылок (%s): %w", p.Name(), err)
	}
	msgs := SplitMessages(blocks, BudgetFor(p))

	sent := 0
	firstID := ""
	for i, msg := range msgs {
		ref := fmt.Sprintf("%s#%d", weekID, i+1)
		msgID, _, found, err := st.Target(ctx, p.Name(), store.TargetDigest, ref)
		if err != nil {
			return sent, err
		}
		if !found { // часть ещё не уходила (или докат после сбоя)
			msgID, err = p.PostChannelHTML(ctx, msg)
			if err != nil {
				return sent, fmt.Errorf("часть %d/%d (%s): %w", i+1, len(msgs), p.Name(), err)
			}
			if err := st.SetTarget(ctx, p.Name(), store.TargetDigest, ref, msgID, ""); err != nil {
				return sent, err
			}
			sent++
		}
		if i == 0 {
			firstID = msgID
		}
	}
	return sent, st.SetTarget(ctx, p.Name(), store.TargetDigest, weekID, firstID, "")
}
