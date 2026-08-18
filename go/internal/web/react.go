package web

// Реакции на странице заметки.
//
// Работают БЕЗ скрипта, как и всё остальное: каждая кнопка — форма с POST, а
// после неё возврат на ту же страницу к тому же месту. Иначе реакции стали бы
// первой вещью на площадке, которая без JS не работает, — а тогда и «Выход»
// однажды окажется такой же.
//
// Набор кнопок не рисуется под каждой репликой: в дереве их бывает под
// девятьсот, и шесть кнопок на каждую — это пять тысяч форм на странице. Вместо
// этого под репликой стоят ТОЛЬКО уже нажатые реакции, а выбор открывается по
// «+» на одну реплику разом — тем же приёмом, что и форма ответа (?react=<id>,
// как ?reply=<id>).

import (
	"net/http"
	"strconv"

	"lovegw/internal/platform"
)

// reactParam — какой объект показывает выбиралку. «note» — сама заметка, иначе
// id комментария. Слово вместо нуля, потому что адрес читают люди.
const reactParam = "react"

const reactNote = "note"

func (s *Server) handleReact(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.writer(w, r)
	if !ok {
		return
	}
	noteID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || noteID <= 0 {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return
	}
	commentID, _ := strconv.ParseInt(r.FormValue("comment"), 10, 64)

	err = s.wr.React(r.Context(), platform.NewReaction{
		UserID:    u.ID,
		NoteID:    noteID,
		CommentID: commentID,
		Code:      r.FormValue("code"),
	})
	if err != nil {
		status, problem := writeProblem(err)
		if problem == "" {
			s.oops(w, r, "реакция", err)
			return
		}
		// Отказ рисуется страницей заметки, а не отдельной: человек нажал кнопку
		// посреди чтения, и уводить его с треда ради одной строки жестоко.
		s.showNote(w, r, noteID, status, compose{Problem: problem})
		return
	}
	http.Redirect(w, r, noteURL(noteID, s.threadLinear(w, r), pageParam(r.FormValue("page")))+
		anchorOf(commentID), http.StatusSeeOther)
}

// anchorOf — куда вернуть человека после нажатия: к своей реплике, а не к началу
// длинной страницы.
func anchorOf(commentID int64) string {
	if commentID == 0 {
		return ""
	}
	return "#c" + strconv.FormatInt(commentID, 10)
}

// reactTarget разбирает ?react=… — чей выбор реакций сейчас раскрыт. Возвращает
// id комментария, 0 для самой заметки и −1, если не раскрыт ничей.
func reactTarget(r *http.Request) int64 {
	switch v := r.URL.Query().Get(reactParam); v {
	case "":
		return -1
	case reactNote:
		return 0
	default:
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return -1
		}
		return id
	}
}

// reactURL — адрес «раскрыть выбор вот здесь». Собирается в Go по той же
// причине, что и replyURL: «?» и «&» в шаблоне уехали бы в %3f и %26.
func reactURL(base string, commentID int64) string {
	target := reactNote
	if commentID != 0 {
		target = strconv.FormatInt(commentID, 10)
	}
	return base + "&" + reactParam + "=" + target + anchorOf(commentID)
}

// reactBox — всё, что нужно нарисовать под одним объектом. Собирается в Go, а
// не выражением в шаблоне: у шаблона нет словарей, а протаскивать пять значений
// через один аргумент — это тот самый случай, когда «немного логики в разметке»
// заканчивается разметкой в логике.
type reactBox struct {
	NoteID   int64
	Target   int64 // 0 — сама заметка
	List     []platform.Reaction
	Codes    []string
	Open     bool // раскрыт выбор реакции
	CanWrite bool
	CSRF     string
	Page     int
	PickURL  string
}

// reactBoxOf — коробка реакций для объекта target (0 — заметка).
func reactBoxOf(p notePage, target int64) reactBox {
	return reactBox{
		NoteID:   p.Note.ID,
		Target:   target,
		List:     p.Reactions[target],
		Codes:    platform.ReactionCodes,
		Open:     p.ReactOpen == target && p.ReactOpen >= 0,
		CanWrite: p.CanWrite,
		CSRF:     p.CSRF,
		Page:     p.PageNum,
		PickURL:  reactURL(p.ReplyBase, target),
	}
}

// reactionLabels — как называется кнопка словами. Нужны они не для красоты:
// смайл НГС говорит здешнему человеку сам, а вот подпись читает голосовой
// движок и всплывающая подсказка тому, кто этих значков в глаза не видел.
//
// Коды остаются кодами сайта (agree, boogi), слова — наши: перевода у них нет
// и не было, картинку выбирали по виду, а не по названию файла.
// Слова сверены с САМИМИ картинками, а не с именами файлов: «live» это палец
// вверх, а «str» — рожица с разъехавшимися глазами. Имена в наборе НГС говорят
// о картинке далеко не всегда, и подпись, выведенная из имени, врала бы.
var reactionLabels = map[string]string{
	"crazy2":  "С ума сойти",
	"agree":   "Согласен",
	"popcorn": "Наблюдаю",
	"flowers": "Спасибо",
	"shuffle": "Отжигает",
	"boogi":   "Веселье",
	"sad2":    "Грустно",
	"wow":     "Ого",
	"str":     "Озадачен",
	"live":    "Одобряю",
	"respect": "Уважение",
	"bottle":  "Повод",
}

// reactionLabel — подпись кнопки. Незнакомому коду достаётся он сам: пусто было
// бы кнопкой без имени, а выдумывать название по имени файла — врать.
func reactionLabel(code string) string {
	if s, ok := reactionLabels[code]; ok {
		return s
	}
	return code
}
