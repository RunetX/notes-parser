package dmbot

// Внутренние новости проекта: /news у админа. Сайт в этом не участвует —
// текст уходит прямо в каналы мессенджеров (пакет news), заметки на
// love.ngs.ru не появляется. Диалог в два шага: текст → подтверждение;
// черновик живёт в состоянии диалога, поэтому переживает рестарт демона.

import (
	"context"
	"strings"
	"time"

	"lovegw/internal/kbd"
	"lovegw/internal/news"
)

const msgNewsOff = "Публикация новостей сейчас недоступна."

// yesWords — подтверждение публикации. Кнопки появятся вместе с эпиком
// callback'ов; пока это слово.
var yesWords = map[string]bool{
	"да": true, "ага": true, "ок": true, "окей": true,
	"публикуй": true, "опубликовать": true, "+": true, "yes": true,
}

// isNewsAdmin — можно ли этому пользователю публиковать новости.
func (l *Logic) isNewsAdmin(userID int64) bool {
	return l.news != nil && l.adminID != 0 && userID == l.adminID
}

// handleNews открывает диалог публикации новости (/news). Посторонним команда
// отвечает как несуществующая: она админская и в списке команд не значится.
func (l *Logic) handleNews(ctx context.Context, userID int64) {
	if !l.isNewsAdmin(userID) {
		l.tr.Send(ctx, userID, msgUnknownCommand)
		return
	}
	l.setState(ctx, userID, stateAwaitNews)
	l.tr.SendKeyboard(ctx, userID, "Отправьте текст новости проекта — я опубликую его в каналах.\n"+
		`Разметка: <b>жирный</b>, <i>наклонный</i>, <a href="ссылка">текст</a>; `+
		"остальные < и > экранируйте как &lt; и &gt;.", cancelKeyboard())
}

// draftNews принимает текст новости: проверяет разметку, показывает, как
// новость уйдёт в канал, и просит подтверждения. Ошибка разметки состояние не
// сбрасывает — админ присылает исправленный текст.
func (l *Logic) draftNews(ctx context.Context, userID int64, text string) {
	if !l.isNewsAdmin(userID) {
		l.dropNewsState(ctx, userID)
		return
	}
	html, err := news.Prepare(text)
	if err != nil {
		// Состояние не сбрасываем — админ пришлёт исправленный текст.
		l.tr.SendKeyboard(ctx, userID, "Так публиковать нельзя: "+err.Error()+
			"\nПришлите исправленный текст или отмените.", cancelKeyboard())
		return
	}
	id := news.NewID(time.Now())
	l.setState(ctx, userID, stateNewsPrefix+id+"\n"+html)
	l.tr.SendKeyboard(ctx, userID, "Новость "+id+", в каналы уйдёт так:\n\n"+html+
		"\n\nПубликуем?", newsKeyboard(id))
}

// confirmNews публикует подтверждённую словом новость. При сбое части каналов
// состояние остаётся: повторное «да» досылает только те, куда не ушло (id
// новости тот же, уже опубликованные каналы пропускаются).
func (l *Logic) confirmNews(ctx context.Context, userID int64, id, html, answer string) {
	if !l.isNewsAdmin(userID) {
		l.dropNewsState(ctx, userID)
		return
	}
	if !yesWords[strings.ToLower(strings.TrimSpace(answer))] {
		_ = l.st.ClearDialogState(ctx, l.stateNS, userID)
		l.tr.Send(ctx, userID, "Отменил, новость не опубликована.")
		return
	}
	report, done := l.publishNews(ctx, userID, id, html)
	if !done {
		report += "\n\nОтветьте «да» ещё раз, чтобы дослать оставшееся, или /cancel."
	}
	l.tr.Send(ctx, userID, report)
}

// cbNews публикует новость нажатием «Опубликовать». Черновик берём из
// состояния, а не из payload: в кнопке едет только id, и он должен совпасть —
// иначе это нажатие на протухшую кнопку от прошлого черновика.
func (l *Logic) cbNews(ctx context.Context, userID int64, cb kbd.Callback, id string) {
	if !l.isNewsAdmin(userID) {
		l.replace(ctx, userID, cb, msgNewsOff, nil)
		return
	}
	state, err := l.st.DialogState(ctx, l.stateNS, userID)
	if err != nil {
		l.log.Error("чтение состояния диалога", "user", userID, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	stateID, html, ok := parseNewsState(state)
	if !ok || stateID != id {
		// Уже опубликовали (состояние снято) или кнопка от прошлого черновика.
		l.replace(ctx, userID, cb, "Этот черновик уже неактуален.", nil)
		return
	}
	report, done := l.publishNews(ctx, userID, id, html)
	if !done {
		// Кнопку оставляем: это и есть механизм повтора — досылаем каналы,
		// куда новость не ушла.
		l.replace(ctx, userID, cb, report+"\n\nНажмите «Опубликовать» ещё раз, чтобы дослать оставшееся.",
			newsKeyboard(id))
		return
	}
	l.replace(ctx, userID, cb, report, nil)
}

// publishNews — общий кусок обеих дорог (слово «да» и кнопка). done == false
// означает, что часть каналов не приняла новость и черновик оставлен под
// повтор: id тот же, уже опубликованные каналы пропускаются.
func (l *Logic) publishNews(ctx context.Context, userID int64, id, html string) (report string, done bool) {
	results := l.news.Publish(ctx, id, html)
	report = news.Report(results)
	if news.Failed(results) {
		return report, false
	}
	_ = l.st.ClearDialogState(ctx, l.stateNS, userID)
	return "Готово:\n" + report, true
}

// dropNewsState снимает зависший черновик: новости успели выключить (или
// сменился админ), пока состояние ждало подтверждения.
func (l *Logic) dropNewsState(ctx context.Context, userID int64) {
	_ = l.st.ClearDialogState(ctx, l.stateNS, userID)
	l.tr.Send(ctx, userID, msgNewsOff)
}

// parseNewsState разбирает состояние «news:<id>\n<html>».
func parseNewsState(state string) (id, html string, ok bool) {
	if !strings.HasPrefix(state, stateNewsPrefix) {
		return "", "", false
	}
	id, html, ok = strings.Cut(state[len(stateNewsPrefix):], "\n")
	if !ok || id == "" || html == "" {
		return "", "", false
	}
	return id, html, true
}
