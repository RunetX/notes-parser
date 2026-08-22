package digest

// Выпуск на ПЛОЩАДКЕ — основная публикация, а каналы получают его сами.
//
// До 22.08.2026 выпуск существовал только в каналах: `Publish` резал его на
// части по бюджету мессенджера и подставлял ссылки на треды. С появлением
// площадки это стало неправильным местом: сводка про сообщество не приходила
// туда, где сообщество живёт, у неё не было ни адреса, ни треда — пост в канале
// обсуждался в автофорварде, которого мост не опознаёт ни как заметку, ни как
// комментарий, и обсуждение выпуска не доходило никуда.
//
// Теперь выпуск — НАТИВНАЯ ЗАМЕТКА, а в Telegram и MAX её несёт `platout`, как
// всякую написанную здесь. Плата названа и принята: сплита серии больше нет
// (длинный выпуск в канале обрежется со ссылкой на оригинал), и ссылки ведут на
// площадку, а не в треды мессенджеров, — для эпика E это скорее цель.
//
// Своей страницы `/digest/<неделя>` мы не заводим намеренно: заметка даёт то же
// самое даром — страницу, тред с ответами в любую точку дерева, реакции,
// закрепление наверху ленты и архив выпусков, которым служит сама лента.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"lovegw/internal/chantext"
	"lovegw/internal/store"
)

// Site — что дайджест умеет делать с площадкой. Интерфейс узкий и объявлен
// здесь, а не взят из `platform`, по той же причине, по какой у веб-морды свои
// четыре: список — это исчерпывающий ответ на вопрос «что выпуск делает с
// площадкой». Опубликовать и закрепить; ни читать, ни править он не вправе.
type Site interface {
	PublishNote(ctx context.Context, body string) (noteID int64, err error)
	PinNote(ctx context.Context, noteID int64, pinned bool) error
}

// pinnedRef — под каким ref в message_targets помним ЗАКРЕПЛЁННЫЙ сейчас
// выпуск. Помним потому, что закрепление у площадки — состояние с потолком
// (`MaxPinned` = 5), и новый выпуск обязан снимать прошлый: иначе за месяц
// закрепы кончатся и место наверху займут одни дайджесты.
const pinnedRef = "pinned"

// RenderPlatform собирает тело заметки-выпуска.
//
// Отличий от канального рендера два, и оба вынужденные. Маркер {note:ID|текст}
// ведёт на страницу ПЛОЩАДКИ: у зеркальной заметки id строки равен id на НГС,
// поэтому адрес получается прямой подстановкой, и это единственный приёмник,
// где ссылка ведёт в живой разговор. А ссылки с подписью в теле заметки не
// существует вовсе (своего [url] площадка не заводит), поэтому подпись и адрес
// печатаются рядом — `chantext.ToSiteMarkup`.
func RenderPlatform(d Draft, baseURL string) string {
	baseURL = strings.TrimSuffix(baseURL, "/")
	var paras []string
	for _, sec := range d.Sections {
		for _, el := range sec {
			s := noteMarkerRe.ReplaceAllStringFunc(el, func(marker string) string {
				sub := noteMarkerRe.FindStringSubmatch(marker)
				return `<a href="` + baseURL + "/n/" + sub[1] + `">` + sub[2] + `</a>`
			})
			paras = append(paras, chantext.ToSiteMarkup(s))
		}
	}
	return strings.Join(paras, "\n\n")
}

// PublishPlatform публикует выпуск заметкой. Идемпотентна по message_targets
// (приёмник `platform`, kind `digest`, ref — неделя): повторный запуск ничего
// не дублирует и возвращает id уже опубликованной заметки.
//
// Отметка ставится ПОСЛЕ вставки, как и у канальной публикации: обрыв между
// ними стоил бы второй заметки в ленте (её уберёт модератор), а обратный
// порядок стоил бы выпуска целиком — планировщик счёл бы неделю закрытой.
func PublishPlatform(ctx context.Context, st *store.Store, site Site, d Draft, weekID, baseURL string) (noteID int64, created bool, err error) {
	if id, _, done, err := st.Target(ctx, store.MessengerPlatform, store.TargetDigest, weekID); err != nil {
		return 0, false, err
	} else if done {
		n, _ := strconv.ParseInt(id, 10, 64)
		return n, false, nil
	}
	noteID, err = site.PublishNote(ctx, RenderPlatform(d, baseURL))
	if err != nil {
		return 0, false, fmt.Errorf("выпуск %s на площадке: %w", weekID, err)
	}
	if err := st.SetTarget(ctx, store.MessengerPlatform, store.TargetDigest, weekID,
		strconv.FormatInt(noteID, 10), ""); err != nil {
		return noteID, true, err
	}
	return noteID, true, nil
}

// PinIssue закрепляет свежий выпуск наверху ленты и снимает прошлый.
//
// Отдельным шагом от публикации намеренно: закрепление — украшение, а выпуск —
// нет. Сорвавшийся закреп не должен ни отменять публикацию, ни заставлять её
// повториться, поэтому зовут его ПОСЛЕ отметки об успехе и об отказе только
// сообщают.
func PinIssue(ctx context.Context, st *store.Store, site Site, noteID int64) error {
	id := strconv.FormatInt(noteID, 10)
	prev, _, found, err := st.Target(ctx, store.MessengerPlatform, store.TargetDigest, pinnedRef)
	if err != nil {
		return err
	}
	if found && prev != "" && prev != id {
		if n, err := strconv.ParseInt(prev, 10, 64); err == nil {
			if err := site.PinNote(ctx, n, false); err != nil {
				// Прошлый выпуск мог быть откреплён руками — это не повод не
				// закреплять новый.
				return fmt.Errorf("прошлый выпуск %s не откреплён: %w", prev, err)
			}
		}
	}
	if err := site.PinNote(ctx, noteID, true); err != nil {
		return fmt.Errorf("выпуск %d не закреплён: %w", noteID, err)
	}
	return st.SetTarget(ctx, store.MessengerPlatform, store.TargetDigest, pinnedRef, id, "")
}
