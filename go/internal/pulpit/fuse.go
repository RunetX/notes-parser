package pulpit

// Предохранитель «меня забанили». Запрет писать в раздел «Заметки» — самое
// частое наказание на площадке, и он невидим: анкета жива, страницы читаются,
// сайт отвечает 200. Единственный след — реплика не появляется в треде.
//
// Отсюда правила счёта, каждое оплачено разбором чужих случаев:
//   - промах засчитывается ТОЛЬКО при успешно прочитанной странице; 5xx, 403 и
//     дрейф вёрстки счётчик не трогают (у сайта бывают короткие 5xx-штормы);
//   - то же и про запись: не дошедший POST (reasonSendFailed) — не промах,
//     хотя тред после него читается прекрасно и реплики в нём нет. 17.08.2026
//     сайт отвечал 500 на любой комментарий, включая чужие, и без этого
//     правила шторм гасил бы фичу за три заметки, диагностировав запрет там,
//     где анкета цела;
//   - исчезнувшая заметка — не промах: снесли её, а не нас, иначе первый же
//     снос модератором выключил бы фичу;
//   - полоса считается ИЗ БД, а не из счётчика в памяти: краш-луп не должен
//     сбрасывать предохранитель;
//   - при достижении порога состояние подтверждается по анкете
//     (love.ProfileControl): гостевой ответ значит «сессия мертва», Blocked —
//     «анкета закрыта», и только «всё в порядке» означает именно запрет писать.
//
// Автовосстановления нет: срок запрета неизвестен, а самовольное возвращение —
// прямая дорога ко второму бану. Обратно включает только человек.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// fuseDeletions — сколько подтверждённых реплик должно пропасть, чтобы счесть
// это чисткой, а не случайностью.
const fuseDeletions = 2

// fuseWindow — сколько последних строк смотрим. С запасом: между промахами
// попадаются пропуски и исчезнувшие заметки, а они полосу не разрывают.
const fuseWindow = 30

// outcome — исход одной реплики глазами предохранителя.
type outcome int

const (
	outcomeNeutral   outcome = iota // пропуск, ошибка генерации, ещё не проверено
	outcomeConfirmed                // реплика на месте: полоса разорвана
	outcomeMissing                  // страница прочиталась, реплики нет
	outcomeVanished                 // заметка исчезла — не про нас
	outcomeDeleted                  // подтверждённую реплику вычистили
)

// profileState — что сказала анкета.
type profileState int

const (
	profileUnknown profileState = iota // не спрашивали или сайт не ответил
	profileOK
	profileUnauthorized
	profileBlocked
)

type fuseInput struct {
	Recent  []outcome // от новых к старым
	Profile profileState
}

// fuseVerdict — выключаться ли. Чистая: сеть сюда не заходит.
func fuseVerdict(in fuseInput, maxMisses int) (off bool, reason string) {
	switch in.Profile {
	case profileUnauthorized:
		return true, "сессия сайта недействительна — нужен повторный вход (/login)"
	case profileBlocked:
		return true, "анкета на сайте заблокирована"
	}
	misses, deleted := streakOf(in.Recent)
	if maxMisses > 0 && misses >= maxMisses {
		return true, fmt.Sprintf("%d реплики подряд не появились в тредах — похоже на запрет писать в «Заметки»", misses)
	}
	if deleted >= fuseDeletions {
		return true, fmt.Sprintf("%d подтверждённые реплики вычистили из тредов", deleted)
	}
	return false, ""
}

// streakOf считает полосу с самого свежего исхода. Подтверждённая реплика
// полосу разрывает, исчезнувшая заметка и пропуски — нет.
func streakOf(recent []outcome) (misses, deleted int) {
	for _, o := range recent {
		switch o {
		case outcomeConfirmed:
			return misses, deleted
		case outcomeMissing:
			misses++
		case outcomeDeleted:
			deleted++
		}
	}
	return misses, deleted
}

// outcomeOf переводит строку БД в исход. Всё, кроме проверенного, нейтрально:
// отправленная, но ещё не проверенная реплика — не промах.
func outcomeOf(row store.PulpitComment) outcome {
	switch row.State {
	case store.PulpitConfirmed:
		return outcomeConfirmed
	case store.PulpitVanished:
		return outcomeVanished
	case store.PulpitMissing:
		switch row.Reason {
		case reasonDeleted:
			return outcomeDeleted
		case reasonSendFailed:
			// POST не дошёл — реплики нет по известной невиновной причине.
			// Нейтрально, а не «подтверждено»: полосу это не разрывает,
			// потому что про запрет мы так ничего и не узнали.
			return outcomeNeutral
		}
		return outcomeMissing
	default:
		return outcomeNeutral
	}
}

// checkFuse считает полосу и, если порог взят, подтверждает диагноз анкетой.
func (s *Service) checkFuse(ctx context.Context) {
	rows, err := s.st.PulpitRecent(ctx, fuseWindow)
	if err != nil {
		s.log.Error("амвон: чтение истории для предохранителя", "err", err)
		return
	}
	outcomes := make([]outcome, 0, len(rows))
	for _, row := range rows {
		outcomes = append(outcomes, outcomeOf(row))
	}
	misses, deleted := streakOf(outcomes)
	if misses < s.cfg.FuseMisses && deleted < fuseDeletions {
		return
	}
	profile, detail := s.profileVerdict(ctx)
	off, reason := fuseVerdict(fuseInput{Recent: outcomes, Profile: profile}, s.cfg.FuseMisses)
	if !off {
		return
	}
	s.disable(ctx, reason, detail+" "+affectedNotes(rows))
}

// fuseStreak — текущая полоса для отчёта в ручке /pulpit.
func (s *Service) fuseStreak(ctx context.Context) (misses, deleted int, err error) {
	rows, err := s.st.PulpitRecent(ctx, fuseWindow)
	if err != nil {
		return 0, 0, err
	}
	outcomes := make([]outcome, 0, len(rows))
	for _, row := range rows {
		outcomes = append(outcomes, outcomeOf(row))
	}
	misses, deleted = streakOf(outcomes)
	return misses, deleted, nil
}

// profileVerdict спрашивает у сайта состояние анкеты — и только при взятом
// пороге: страница настроек к обходу ленты отношения не имеет, дёргать её
// каждый такт незачем.
func (s *Service) profileVerdict(ctx context.Context) (profileState, string) {
	cookies, err := s.cookies(ctx)
	if err != nil {
		return profileUnknown, "сессию проверить не удалось: " + err.Error() + "."
	}
	ctrl, err := s.site.ProfileControl(ctx, cookies)
	if errors.Is(err, love.ErrUnauthorized) {
		s.invalidateSession(ctx)
		return profileUnauthorized, "Сайт отвечает как гостю: сессия истекла."
	}
	if err != nil {
		return profileUnknown, "Анкету проверить не удалось: " + err.Error() + "."
	}
	if ctrl.Blocked {
		return profileBlocked, "Анкета закрыта (в том числе самим владельцем)."
	}
	return profileOK, "Анкета жива, сессия рабочая — значит закрыт именно раздел."
}

// invalidateSession гасит протухшую сессию: дальше её обновит /login, а до
// того ни мост, ни заметки, ни амвон под ней ничего не отправят.
func (s *Service) invalidateSession(ctx context.Context) {
	messenger, userID, err := s.st.SessionForProfile(ctx, s.cfg.OwnerProfileID)
	if err != nil {
		return
	}
	if err := s.st.SetSessionValid(ctx, messenger, userID, false, time.Now()); err != nil {
		s.log.Error("амвон: пометка сессии недействительной", "err", err)
	}
}

// affectedNotes — какие заметки попали в полосу: с ними админ идёт смотреть
// глазами.
func affectedNotes(rows []store.PulpitComment) string {
	var ids []string
	for _, row := range rows {
		o := outcomeOf(row)
		if o == outcomeConfirmed {
			break
		}
		if o == outcomeMissing || o == outcomeDeleted {
			ids = append(ids, row.NoteID)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	return "Заметки: " + strings.Join(ids, ", ") + "."
}
