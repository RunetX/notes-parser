package dmbot

// /profile — управление своей анкетой на сайте: заблокировать и разблокировать.
//
// Состояние НЕ хранится в БД: заблокировать анкету можно и на самом сайте, и
// сохранённый флаг соврал бы при первом же таком заходе. Поэтому и показ, и
// применение читают состояние живьём — то же правило, что у доставки ЛС
// (delivery.go), только источник истины не своя БД, а сайт.
//
// Кнопка на экране — ровно та, которую предлагает сайт: её подпись и поле формы
// приезжают со страницы настроек (love.ProfileControl), а у сайта они в двух
// состояниях разные. Если кнопки там вдруг не окажется — скажем честно, а не
// подставим угаданную.

import (
	"context"
	"errors"
	"time"

	"lovegw/internal/kbd"
	"lovegw/internal/love"
)

const (
	btnProfile = "👤 Моя анкета"
	// btnProfileYes — подтверждение блокировки. Разблокировку не подтверждаем:
	// вернуть анкету — действие безобидное, лишний шаг только мешает.
	btnProfileYes  = "🔒 Да, заблокировать"
	msgProfileOff  = "Управление анкетой сейчас недоступно."
	msgProfileGone = "Сайт не предлагает такой кнопки — зайдите на love.ngs.ru."
	// msgProfileWarn — что человек теряет на время блокировки. Про личные
	// сообщения сказано отдельно: их носит этот же бот, и молчание после
	// блокировки выглядело бы поломкой.
	msgProfileWarn = "Заблокировать анкету? Пока она заблокирована, профиль не виден на сайте, " +
		"а личные сообщения не приходят и не отправляются.\n\nВернуть можно этой же командой."
)

// handleProfile показывает состояние анкеты и кнопку действия. cb == nil —
// пришли командой, шлём новое сообщение; иначе правим своё же сообщение.
func (l *Logic) handleProfile(ctx context.Context, userID int64, cb *kbd.Callback) {
	if l.profile == nil {
		l.tr.Send(ctx, userID, msgProfileOff)
		return
	}
	ctrl, ok := l.profileState(ctx, userID)
	if !ok {
		return
	}
	l.showProfile(ctx, userID, cb, "", ctrl)
}

// askProfileBlock — подтверждение блокировки. Состояние перечитываем: кнопка
// могла провисеть до того, как анкету заблокировали на сайте.
func (l *Logic) askProfileBlock(ctx context.Context, userID int64, cb kbd.Callback) {
	if l.profile == nil {
		l.tr.Send(ctx, userID, msgProfileOff)
		return
	}
	ctrl, ok := l.profileState(ctx, userID)
	if !ok {
		return
	}
	if ctrl.Blocked || !ctrl.Available {
		l.showProfile(ctx, userID, &cb, "", ctrl)
		return
	}
	l.replace(ctx, userID, cb, msgProfileWarn, kbd.New().Row(
		kbd.Button{Text: btnProfileYes, Payload: kbd.Pack(verbProfileSet, argProfileBlock)},
		kbd.Button{Text: btnCancel, Payload: kbd.Pack(verbCancel, "")},
	))
}

// setProfile нажимает кнопку сайта. arg — намерение той кнопки, которую нажал
// человек: пока сообщение висело, анкету могли переключить на самом сайте, и
// делать не то, что написано на кнопке, нельзя.
func (l *Logic) setProfile(ctx context.Context, userID int64, cb kbd.Callback, arg string) {
	if l.profile == nil {
		l.tr.Send(ctx, userID, msgProfileOff)
		return
	}
	cookies, ok := l.siteCookies(ctx, userID)
	if !ok {
		return
	}
	ctrl, err := l.profile.ProfileControl(ctx, cookies)
	if err != nil {
		l.reportProfileError(ctx, userID, err)
		return
	}
	wantBlock := arg == argProfileBlock
	if ctrl.Blocked == wantBlock || !ctrl.Available {
		// Состояние уже такое, какого добивались, либо нажимать нечего.
		l.showProfile(ctx, userID, &cb, "", ctrl)
		return
	}
	if err := l.profile.SubmitProfileControl(ctx, cookies, ctrl); err != nil {
		l.reportProfileError(ctx, userID, err)
		return
	}
	// Перечитываем с сайта, а не рисуем ожидаемое: сайт отвечает 200 и на
	// отказ, и «состояние не изменилось» — единственный надёжный признак.
	after, err := l.profile.ProfileControl(ctx, cookies)
	if errors.Is(err, love.ErrUnauthorized) {
		// Блокировка анкеты вполне может оборвать и саму сессию — тогда
		// проверить результат уже нечем. Молчать об этом хуже, чем сказать как
		// есть: человек пойдёт на сайт и увидит анкету своими глазами.
		_ = l.st.SetSessionValid(ctx, l.messenger, userID, false, time.Now())
		l.tr.Send(ctx, userID, "Отправил, но сайт закрыл сессию — проверьте анкету "+
			"на love.ngs.ru и сделайте /login ещё раз")
		return
	}
	if err != nil {
		l.reportProfileError(ctx, userID, err)
		return
	}
	if after.Blocked == ctrl.Blocked {
		l.log.Warn("сайт не принял управление анкетой", "user", userID, "blocked", ctrl.Blocked)
		l.showProfile(ctx, userID, &cb, "Сайт не принял это действие. ", after)
		return
	}
	l.showProfile(ctx, userID, &cb, "Готово. ", after)
}

// profileState — состояние анкеты с сайта под сессией пользователя; ok == false
// значит, что пользователю уже ответили, чем именно не задалось.
func (l *Logic) profileState(ctx context.Context, userID int64) (love.ProfileControl, bool) {
	cookies, ok := l.siteCookies(ctx, userID)
	if !ok {
		return love.ProfileControl{}, false
	}
	ctrl, err := l.profile.ProfileControl(ctx, cookies)
	if err != nil {
		l.reportProfileError(ctx, userID, err)
		return love.ProfileControl{}, false
	}
	return ctrl, true
}

// showProfile рисует состояние и кнопку действия. prefix — приставка вроде
// «Готово. », чтобы не плодить почти одинаковых текстов.
func (l *Logic) showProfile(ctx context.Context, userID int64, cb *kbd.Callback, prefix string, ctrl love.ProfileControl) {
	text := prefix + "Ваша анкета на сайте: " + profileStateText(ctrl.Blocked) + "."
	if !ctrl.Available {
		text += "\n\n" + msgProfileGone
	}
	l.show(ctx, userID, cb, text, profileKeyboard(ctrl))
}

func profileStateText(blocked bool) string {
	if blocked {
		return "заблокирована"
	}
	return "активна"
}

// profileKeyboard — одна кнопка с подписью самого сайта. Блокировка идёт через
// подтверждение, разблокировка — сразу.
func profileKeyboard(ctrl love.ProfileControl) *kbd.Keyboard {
	if !ctrl.Available {
		return kbd.New()
	}
	verb, arg, icon := verbProfileAsk, argProfileBlock, "🔒 "
	if ctrl.Blocked {
		verb, arg, icon = verbProfileSet, argProfileUnblock, "🔓 "
	}
	return kbd.New().Row(kbd.Button{Text: icon + ctrl.Label, Payload: kbd.Pack(verb, arg)})
}

// reportProfileError различает ровно то, что стоит различать: недействительную
// сессию (её гасим и зовём к /login) и всё остальное (сайт мог просто моргнуть
// 5xx или упереться в DDoS-Guard — сессия тут ни при чём).
func (l *Logic) reportProfileError(ctx context.Context, userID int64, err error) {
	if errors.Is(err, love.ErrUnauthorized) {
		_ = l.st.SetSessionValid(ctx, l.messenger, userID, false, time.Now())
		l.tr.Send(ctx, userID, "Сессия истекла. Сделайте /login ещё раз")
		return
	}
	l.log.Warn("управление анкетой не удалось", "user", userID, "err", err)
	l.tr.Send(ctx, userID, "Сайт сейчас не отвечает, попробуйте позже")
}
