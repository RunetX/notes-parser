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
// страница заметки на сайте (есть всегда).
func ResolveLinks(ctx context.Context, st *store.Store, d Draft, p Publisher, siteBase string) ([]Block, error) {
	siteBase = strings.TrimSuffix(siteBase, "/")
	var blocks []Block
	var resolveErr error
	for _, sec := range d.Sections {
		for i, el := range sec {
			text := noteMarkerRe.ReplaceAllStringFunc(el, func(marker string) string {
				sub := noteMarkerRe.FindStringSubmatch(marker)
				id, label := sub[1], sub[2]
				href := siteBase + "/notes/" + id + "/"
				_, threadID, found, err := st.Target(ctx, p.Name(), store.TargetNoteThread, id)
				switch {
				case err != nil:
					resolveErr = err
				case found && threadID != "":
					if l := p.ThreadLink(threadID); l != "" {
						href = l
					}
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
