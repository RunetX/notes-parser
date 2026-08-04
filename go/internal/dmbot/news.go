package dmbot

// Внутренние новости проекта: /news у админа. Сайт в этом не участвует —
// текст уходит прямо в каналы мессенджеров (пакет news), заметки на
// love.ngs.ru не появляется. Диалог в два шага: текст → подтверждение;
// черновик живёт в состоянии диалога, поэтому переживает рестарт демона.

import (
	"context"
	"strings"
	"time"

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
	l.tr.Send(ctx, userID, "Отправьте текст новости проекта — я опубликую его в каналах.\n"+
		`Разметка: <b>жирный</b>, <i>наклонный</i>, <a href="ссылка">текст</a>; `+
		"остальные < и > экранируйте как &lt; и &gt;.")
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
		l.tr.Send(ctx, userID, "Так публиковать нельзя: "+err.Error()+
			"\nПришлите исправленный текст или /cancel.")
		return
	}
	id := news.NewID(time.Now())
	l.setState(ctx, userID, stateNewsPrefix+id+"\n"+html)
	l.tr.Send(ctx, userID, "Новость "+id+", в каналы уйдёт так:\n\n"+html+
		"\n\nПубликуем? Ответьте «да» — или /cancel, чтобы отменить.")
}

// confirmNews публикует подтверждённую новость. При сбое части каналов
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
	results := l.news.Publish(ctx, id, html)
	report := news.Report(results)
	if news.Failed(results) {
		l.tr.Send(ctx, userID, report+
			"\n\nОтветьте «да» ещё раз, чтобы дослать оставшееся, или /cancel.")
		return
	}
	_ = l.st.ClearDialogState(ctx, l.stateNS, userID)
	l.tr.Send(ctx, userID, "Готово:\n"+report)
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
