package digest

// Рендер выпуска per-sink: у каждого мессенджера свои id тредов и свой
// формат ссылок, поэтому общий черновик превращается в HTML для конкретного
// приёмника резолвом маркеров {note:ID|текст}.

import (
	"context"
	"strings"

	"lovegw/internal/store"
)

// Publisher — приёмник выпуска дайджеста. Реализуют tgx.Mirror и maxx.Mirror;
// опциональный интерфейс по образцу mirror.ThreadStarter, mirror.Sink не
// трогаем.
type Publisher interface {
	Name() string // store.MessengerTelegram / store.MessengerMax
	PostChannelHTML(ctx context.Context, html string) (msgID string, err error)
	ThreadLink(threadID string) string // "" — deep-link невозможен
}

// Block — атом сплита: один абзац черновика с готовым HTML.
type Block struct {
	Text       string
	NewSection bool // абзац открывает рубрику (первый в секции)
}

// ResolveLinks превращает маркеры {note:ID|текст} в <a href>: ссылка на тред
// заметки в мессенджере приёмника (message_targets → ThreadLink), фолбэк —
// страница заметки НА ПЛОЩАДКЕ (у зеркальной строки id равен id на НГС, поэтому
// адрес получается прямой подстановкой — тот же приём, что в RenderPlatform).
//
// Фолбэком была страница заметки на love.ngs.ru: она «есть всегда», но ссылок
// на НГС проект больше не ставит нигде (решение владельца 27.08.2026). Дорога
// эта — публикация выпуска прямо в каналы — работает только БЕЗ площадки, и
// тогда своей страницы у заметки нет вовсе: подпись остаётся текстом. Ссылка
// без адреса хуже её отсутствия.
func ResolveLinks(ctx context.Context, st *store.Store, d Draft, p Publisher, ourBase string) ([]Block, error) {
	ourBase = strings.TrimSuffix(ourBase, "/")
	var blocks []Block
	var resolveErr error
	for _, sec := range d.Sections {
		for i, el := range sec {
			text := noteMarkerRe.ReplaceAllStringFunc(el, func(marker string) string {
				sub := noteMarkerRe.FindStringSubmatch(marker)
				id, label := sub[1], sub[2]
				href := ""
				if ourBase != "" {
					href = ourBase + "/n/" + id
				}
				_, threadID, found, err := st.Target(ctx, p.Name(), store.TargetNoteThread, id)
				switch {
				case err != nil:
					resolveErr = err
				case found && threadID != "":
					if l := p.ThreadLink(threadID); l != "" {
						href = l
					}
				}
				if href == "" {
					return label
				}
				return `<a href="` + href + `">` + label + `</a>`
			})
			blocks = append(blocks, Block{Text: text, NewSection: i == 0})
		}
	}
	if resolveErr != nil {
		return nil, resolveErr
	}
	return blocks, nil
}
