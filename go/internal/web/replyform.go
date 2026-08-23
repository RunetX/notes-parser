package web

// Форма ответа, открывающаяся НА МЕСТЕ.
//
// До этого нажатие «Ответить» перезагружало страницу с ?reply=<id>, и цена была
// не в удобстве, а в работе: чтобы показать форму под репликой, сервер заново
// собирал ВЕСЬ тред — до 5000 строк, — а человек ждал и терял место, на котором
// читал. Здесь страница просит одну готовую строку, и это в разы дешевле того,
// что было (см. isReplyForm в guard.go).
//
// Строку рисует ТОТ ЖЕ шаблон, что и страницу (parts/replyrow.gohtml вокруг
// parts/reply.gohtml) — по той же причине, по которой так устроен живой добор:
// второго способа превратить наш текст в разметку не заводится, а собери
// скрипт форму сам — у площадки появилась бы вторая поверхность для XSS и
// второе место, где однажды забудут про «кому отвечаете» и про тень.
//
// Именно поэтому здесь ходят в базу за адресатом, а не обходятся тем, что и так
// нарисовано на странице. Форме нужно то, чего в разметке реплики нет: тень её
// автор или участник. Метка «ещё не переехал сюда с НГС» — не украшение: она
// отвечает на единственный вопрос отвечающего, дойдёт ли его ответ до того, кому
// он пишет.
//
// Без скрипта ничего не меняется: «Ответить» остаётся обычной ссылкой на
// ?reply=<id>, и страница по-прежнему работает перезагрузкой.

import (
	"bytes"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"lovegw/internal/platform"
)

// handleReplyForm — форма ответа на реплику, куском разметки. Отказ здесь
// честный (404, а не пустой ответ, как у добора): скрипт на нём уходит по той же
// ссылке обычным переходом, и человек видит настоящую причину — снесённую
// реплику, закрытый тред, истёкшую сессию.
func (s *Server) handleReplyForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return
	}
	to, err := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
	if err != nil || to <= 0 {
		s.fail(w, r, http.StatusBadRequest, "Не сказано, кому отвечать.")
		return
	}

	ctx, v := r.Context(), s.viewer(r)
	note, err := s.st.NoteViewByID(ctx, v, id)
	if errors.Is(err, platform.ErrNotFound) {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return
	}
	if err != nil {
		s.oops(w, r, "чтение заметки", err)
		return
	}
	canMod := v.CanModerate() && s.mod != nil
	if note.Status != platform.StatusVisible && !(canMod && note.Status == platform.StatusHiddenMod) {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return
	}

	target, err := s.st.CommentViewByID(ctx, v, id, to)
	if errors.Is(err, platform.ErrNotFound) {
		// Реплику успели снести или скрыть. Ответить ей больше нельзя, и
		// показывать форму «в никуда» хуже, чем отправить человека на страницу:
		// там он увидит тред таким, какой он есть.
		s.fail(w, r, http.StatusNotFound, "Этой реплики больше нет.")
		return
	}
	if err != nil {
		s.oops(w, r, "чтение реплики", err)
		return
	}

	me, signedIn := s.me(r)
	linear := strings.EqualFold(r.URL.Query().Get("view"), "linear")
	// Контекст собирается такой же, как у настоящей страницы, — иначе форма,
	// приехавшая скриптом, отличалась бы от нарисованной обновлением, то есть
	// ровно тем, чего мы и избегаем. Общей части страницы (шапка, тема,
	// колокольчик) здесь нет: во фрагменте её некуда девать.
	p := notePage{
		Note: note,
		CanWrite: signedIn && me.Kind == platform.KindMember && s.wr != nil &&
			!note.Locked && note.Status == platform.StatusVisible,
		Linear:    linear,
		ReplyBase: noteURL(id, linear, pageParam(r.URL.Query().Get("page"))),
		Compose:   compose{ReplyTo: to},
		ReplyTo:   &target,
	}
	p.SignedIn = signedIn
	if signedIn {
		p.CSRF = csrfToken(s.session(r))
	}

	var buf bytes.Buffer
	if err := s.renderPart(&buf, "replyrow", p); err != nil {
		http.Error(w, "внутренняя ошибка", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Vary", "Cookie")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
