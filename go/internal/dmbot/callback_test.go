package dmbot

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lovegw/internal/kbd"
	"lovegw/internal/store"
)

// press — нажатие кнопки с указанным payload.
func press(payload string) kbd.Callback {
	return kbd.Callback{AnswerID: "cb.1", MessageID: "mid.kb", Payload: payload}
}

// newsIDFromKB достаёт id черновика из кнопки «Опубликовать»: заодно проверка,
// что кнопка вообще показана.
func newsIDFromKB(t *testing.T, tr *fakeTransport) string {
	t.Helper()
	kb := tr.lastKB()
	if kb.Empty() {
		t.Fatal("превью новости должно приходить с кнопками")
	}
	verb, id, ok := kbd.Parse(kb.Rows[0][0].Payload)
	if !ok || verb != verbNews || id == "" {
		t.Fatalf("кнопка публикации: %q", kb.Rows[0][0].Payload)
	}
	return id
}

// На любое нажатие мессенджеру отвечают ровно один раз — иначе у нажавшего
// навсегда останется «спиннер». Мусорный payload состояния не трогает.
func TestCallbackAlwaysAnswers(t *testing.T) {
	ctx := context.Background()
	payloads := []string{
		kbd.Pack(verbLogin, ""), kbd.Pack(verbNote, ""), kbd.Pack(verbNote, argNoteOwn),
		kbd.Pack(verbNote, argNoteAnon), kbd.Pack(verbStatus, ""), kbd.Pack(verbSubs, ""),
		kbd.Pack(verbTalks, ""), kbd.Pack(verbCancel, ""), kbd.Pack(verbNews, "20260804-193012"),
		"1:неизвестный-глагол", "2:login", "мусор", "",
	}
	for _, p := range payloads {
		t.Run(p, func(t *testing.T) {
			l, tr, _ := newNewsLogic(t)
			l.HandleCallback(ctx, newsAdminID, press(p))
			if len(tr.answers) != 1 {
				t.Fatalf("ответов на нажатие: %d (%v)", len(tr.answers), tr.answers)
			}
		})
	}
}

// Мусорная кнопка не должна ни трогать состояние, ни писать в диалог лишнего.
func TestCallbackStalePayloadIsInert(t *testing.T) {
	ctx := context.Background()
	l, tr, _, st := newTestLogic(t, store.MessengerTelegram)
	const uid = 42

	l.HandleCallback(ctx, uid, press("2:login"))

	if len(tr.answers) != 1 || !strings.Contains(tr.answers[0], "устарела") {
		t.Errorf("ответ на кнопку прошлого релиза: %v", tr.answers)
	}
	if len(tr.sent) != 0 || len(tr.edits) != 0 {
		t.Errorf("устаревшая кнопка не должна ничего писать: %v %v", tr.sent, tr.edits)
	}
	if state, _ := st.DialogState(ctx, store.MessengerTelegram, uid); state != "" {
		t.Errorf("состояние после мусорного нажатия: %q", state)
	}
}

// Полный путь заметки кнопками: вопрос об авторстве → выбор → текст на сайт.
func TestCallbackNoteKindFlow(t *testing.T) {
	ctx := context.Background()
	l, tr, site, st := newTestLogic(t, store.MessengerTelegram)
	const uid = 42
	if err := st.UpsertSession(ctx, store.MessengerTelegram, uid,
		`[{"Name":"sid","Value":"x"}]`, time.Now()); err != nil {
		t.Fatal(err)
	}

	l.HandleCallback(ctx, uid, press(kbd.Pack(verbNote, "")))
	if state, _ := st.DialogState(ctx, store.MessengerTelegram, uid); state != stateAwaitNoteKind {
		t.Fatalf("состояние после вопроса об авторстве: %q", state)
	}
	if got := buttonTexts(tr.lastKB()); len(got) != 3 {
		t.Fatalf("кнопки выбора авторства: %v", got)
	}

	// Текст вместо нажатия — подсказка, а не «Не понимаю».
	l.HandleText(ctx, uid, "mid.1", "а можно я так")
	if strings.Contains(tr.lastSent(), "Не понимаю") {
		t.Errorf("в ожидании выбора текст не должен получать «Не понимаю»: %q", tr.lastSent())
	}

	l.HandleCallback(ctx, uid, press(kbd.Pack(verbNote, argNoteAnon)))
	if state, _ := st.DialogState(ctx, store.MessengerTelegram, uid); state != stateAwaitAnonNote {
		t.Fatalf("состояние после выбора «анонимно»: %q", state)
	}
	// Вопрос превратился в приглашение: второй ветки больше нет.
	edit := tr.lastEdit()
	if edit.messageID != "mid.kb" || edit.text != msgAskAnonNote {
		t.Fatalf("правка сообщения выбора: %+v", edit)
	}
	if got := buttonTexts(edit.kb); len(got) != 1 || !strings.Contains(got[0], "Отмена") {
		t.Errorf("после выбора остаётся только «Отмена»: %v", got)
	}

	l.HandleText(ctx, uid, "mid.2", "текст анонимной заметки")
	if len(site.noteTexts) != 1 || site.noteTexts[0] != "текст анонимной заметки" {
		t.Fatalf("заметка на сайт: %v", site.noteTexts)
	}
	if !site.lastAnonymous {
		t.Error("заметка должна уйти анонимной")
	}
}

// «Отмена» снимает любое состояние и убирает кнопки с приглашения.
func TestCallbackCancelFromEveryState(t *testing.T) {
	ctx := context.Background()
	states := []string{
		stateAwaitCredentials, stateAwaitNote, stateAwaitAnonNote,
		stateAwaitNoteKind, stateAwaitNews, "news:20260804-193012\n<b>x</b>", "pm:1",
	}
	for _, state := range states {
		t.Run(state, func(t *testing.T) {
			l, tr, _, st := newTestLogic(t, store.MessengerTelegram)
			const uid = 42
			if err := st.SetDialogState(ctx, store.MessengerTelegram, uid, state, time.Now()); err != nil {
				t.Fatal(err)
			}

			l.HandleCallback(ctx, uid, press(kbd.Pack(verbCancel, "")))

			if got, _ := st.DialogState(ctx, store.MessengerTelegram, uid); got != "" {
				t.Errorf("состояние после отмены: %q", got)
			}
			edit := tr.lastEdit()
			if edit.text != msgCancelled || !edit.kb.Empty() {
				t.Errorf("приглашение должно стать «%s» без кнопок: %+v", msgCancelled, edit)
			}
		})
	}
}

// Двойной тап по «Опубликовать» публикует новость один раз.
func TestCallbackDoubleTapPublishesOnce(t *testing.T) {
	ctx := context.Background()
	l, tr, ch := newNewsLogic(t)

	l.HandleText(ctx, newsAdminID, "1", "/news")
	l.HandleText(ctx, newsAdminID, "2", "<i>новость</i>")
	id := newsIDFromKB(t, tr)

	l.HandleCallback(ctx, newsAdminID, press(kbd.Pack(verbNews, id)))
	if ch.postCount() != 1 {
		t.Fatalf("постов после первого нажатия: %d", ch.postCount())
	}
	if edit := tr.lastEdit(); !strings.Contains(edit.text, "Готово") || !edit.kb.Empty() {
		t.Errorf("после публикации кнопки убираются: %+v", edit)
	}

	l.HandleCallback(ctx, newsAdminID, press(kbd.Pack(verbNews, id)))
	if ch.postCount() != 1 {
		t.Fatalf("повторное нажатие опубликовало ещё раз: %d", ch.postCount())
	}
	if edit := tr.lastEdit(); !strings.Contains(edit.text, "неактуален") {
		t.Errorf("ответ на повторное нажатие: %q", edit.text)
	}
}

// Два нажатия из разных горутин (в Telegram апдейты обрабатываются
// параллельно) не должны дать двух постов в канале.
func TestCallbackConcurrentPublishOnce(t *testing.T) {
	ctx := context.Background()
	l, tr, ch := newNewsLogic(t)

	l.HandleText(ctx, newsAdminID, "1", "/news")
	l.HandleText(ctx, newsAdminID, "2", "<i>новость</i>")
	payload := kbd.Pack(verbNews, newsIDFromKB(t, tr))

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.HandleCallback(ctx, newsAdminID, press(payload))
		}()
	}
	wg.Wait()

	if ch.postCount() != 1 {
		t.Fatalf("параллельные нажатия дали постов: %d", ch.postCount())
	}
}

// Частичный сбой оставляет черновик и кнопку: это и есть механизм повтора —
// поэтому дедуп нажатий по payload был бы ошибкой.
func TestCallbackNewsRetryKeepsKeyboard(t *testing.T) {
	ctx := context.Background()
	l, tr, ch := newNewsLogic(t)
	ch.setFail(errors.New("bot api 502"))

	l.HandleText(ctx, newsAdminID, "1", "/news")
	l.HandleText(ctx, newsAdminID, "2", "<i>новость</i>")
	payload := kbd.Pack(verbNews, newsIDFromKB(t, tr))

	l.HandleCallback(ctx, newsAdminID, press(payload))
	if ch.postCount() != 0 {
		t.Fatalf("отказ не должен публиковать: %d", ch.postCount())
	}
	edit := tr.lastEdit()
	if !strings.Contains(edit.text, "ещё раз") {
		t.Errorf("предложение повторить: %q", edit.text)
	}
	if got := buttonTexts(edit.kb); len(got) != 2 {
		t.Fatalf("кнопка повтора должна остаться: %v", got)
	}

	ch.setFail(nil)
	l.HandleCallback(ctx, newsAdminID, press(payload))
	if ch.postCount() != 1 {
		t.Fatalf("повтор тем же нажатием должен дослать: %d", ch.postCount())
	}
}

// Кнопка от прошлого черновика новый не публикует.
func TestCallbackNewsStaleDraft(t *testing.T) {
	ctx := context.Background()
	l, tr, ch := newNewsLogic(t)

	l.HandleText(ctx, newsAdminID, "1", "/news")
	l.HandleText(ctx, newsAdminID, "2", "<i>новость</i>")
	_ = newsIDFromKB(t, tr)

	l.HandleCallback(ctx, newsAdminID, press(kbd.Pack(verbNews, "20200101-000000")))

	if ch.postCount() != 0 {
		t.Fatalf("кнопка чужого черновика опубликовала: %d", ch.postCount())
	}
	if edit := tr.lastEdit(); !strings.Contains(edit.text, "неактуален") {
		t.Errorf("ответ на протухшую кнопку: %q", edit.text)
	}
}

// Кнопка публикации у постороннего не работает — как и сама команда /news.
func TestCallbackNewsRejectsNonAdmin(t *testing.T) {
	ctx := context.Background()
	l, tr, ch := newNewsLogic(t)

	l.HandleCallback(ctx, newsAdminID+1, press(kbd.Pack(verbNews, "20260804-193012")))

	if ch.postCount() != 0 {
		t.Fatalf("посторонний опубликовал новость: %d", ch.postCount())
	}
	if edit := tr.lastEdit(); edit.text != msgNewsOff {
		t.Errorf("ответ постороннему: %q", edit.text)
	}
}

// Бот переписки исполняет только свои глаголы: чужая кнопка отправляет к боту
// команд и состояний не заводит.
func TestCallbackTalksOnlyRejectsForeignVerbs(t *testing.T) {
	ctx := context.Background()
	const uid = 42
	st := openTestStore(t)
	l, tr := newTestTalksLogic(t, st, store.MessengerTelegram)

	for _, p := range []string{kbd.Pack(verbLogin, ""), kbd.Pack(verbNote, ""),
		kbd.Pack(verbSubs, ""), kbd.Pack(verbStatus, "")} {
		l.HandleCallback(ctx, uid, press(p))
	}

	if len(tr.answers) != 4 {
		t.Fatalf("ответов на нажатия: %v", tr.answers)
	}
	for _, a := range tr.answers {
		if !strings.Contains(a, "переписка") {
			t.Errorf("ответ бота переписки на чужую кнопку: %q", a)
		}
	}
	if state, _ := st.DialogState(ctx, store.MessengerTelegram+":talks", uid); state != "" {
		t.Errorf("бот переписки не должен заводить состояний: %q", state)
	}
}

// Подписки кнопками: список с «✖» на каждую, снятие перерисовывает список на
// месте, «Добавить» заводит слово диалогом.
func TestCallbackSubscriptionsFlow(t *testing.T) {
	ctx := context.Background()
	l, tr, _, st := newTestLogic(t, store.MessengerTelegram)
	const uid = 42
	for _, kw := range []string{"Граф", "Барон"} {
		if _, err := st.AddSubscription(ctx, store.MessengerTelegram, kw, uid); err != nil {
			t.Fatal(err)
		}
	}

	l.HandleCallback(ctx, uid, press(kbd.Pack(verbSubs, "")))
	got := buttonTexts(tr.lastKB())
	if len(got) != 3 { // две подписки + «Добавить»
		t.Fatalf("кнопки списка подписок: %v", got)
	}

	// Снимаем первую подписку её же кнопкой.
	subs, _ := st.SubscriptionsByUser(ctx, store.MessengerTelegram, uid)
	l.HandleCallback(ctx, uid, press(kbd.Pack(verbUnsub, strconv.FormatInt(subs[0].ID, 10))))
	left, _ := st.SubscriptionsByUser(ctx, store.MessengerTelegram, uid)
	if len(left) != 1 || left[0].Keyword != "Граф" {
		t.Fatalf("после отписки осталось: %+v", left)
	}
	edit := tr.lastEdit()
	if !strings.Contains(edit.text, "Граф") || strings.Contains(edit.text, "Барон") {
		t.Errorf("список перерисован на месте: %q", edit.text)
	}
	// Повторный тап по той же кнопке безвреден.
	l.HandleCallback(ctx, uid, press(kbd.Pack(verbUnsub, strconv.FormatInt(subs[0].ID, 10))))
	if again, _ := st.SubscriptionsByUser(ctx, store.MessengerTelegram, uid); len(again) != 1 {
		t.Errorf("повторная отписка изменила список: %+v", again)
	}

	// «Добавить» — слово диалогом, а не аргументом команды.
	l.HandleCallback(ctx, uid, press(kbd.Pack(verbSubAdd, "")))
	if state, _ := st.DialogState(ctx, store.MessengerTelegram, uid); state != stateAwaitSubscription {
		t.Fatalf("состояние после «Добавить»: %q", state)
	}
	l.HandleText(ctx, uid, "mid.1", "Мавр")
	after, _ := st.SubscriptionsByUser(ctx, store.MessengerTelegram, uid)
	if len(after) != 2 {
		t.Fatalf("подписка словом из диалога: %+v", after)
	}
	if state, _ := st.DialogState(ctx, store.MessengerTelegram, uid); state != "" {
		t.Errorf("состояние после ввода слова: %q", state)
	}
}

// Список диалогов: кнопка на диалог, перелистывание правит то же сообщение,
// нажатие открывает диалог (дальше текст уходит собеседнику).
func TestCallbackTalksPaging(t *testing.T) {
	ctx := context.Background()
	const uid = 42
	st := openTestStore(t)
	l, tr := newTestTalksLogic(t, st, store.MessengerTelegram)
	router := &fakeRouter{ret: true}
	l.SetTalkRouter(router)

	var firstPeer int64
	for i := range talksPageSize + 3 {
		id, err := st.UpsertTalkPeer(ctx, store.TalkPeer{
			Messenger: store.MessengerTelegram, OwnerUserID: uid,
			PassportID: strconv.Itoa(i), Nick: "Собеседник " + strconv.Itoa(i)})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstPeer = id
		}
	}

	// Из меню список приходит новым сообщением: меню — пульт, его не затираем.
	l.HandleCallback(ctx, uid, press(kbd.Pack(verbTalks, "")))
	first := buttonTexts(tr.lastKB())
	if len(first) != talksPageSize+1 { // страница + «Вперёд»
		t.Fatalf("кнопки первой страницы: %v", first)
	}
	if len(tr.edits) != 0 {
		t.Errorf("открытие списка из меню не должно править сообщение: %+v", tr.edits)
	}

	// Перелистывание — правка уже показанного списка.
	l.HandleCallback(ctx, uid, press(kbd.Pack(verbTalks, "1")))
	edit := tr.lastEdit()
	if len(buttonTexts(edit.kb)) != 3+1 { // остаток + «Назад»
		t.Fatalf("кнопки второй страницы: %v", buttonTexts(edit.kb))
	}
	if !strings.Contains(edit.text, "Страница 2") {
		t.Errorf("номер страницы: %q", edit.text)
	}

	// Страница за краем не должна ронять — показываем первую.
	l.HandleCallback(ctx, uid, press(kbd.Pack(verbTalks, "99")))
	if got := buttonTexts(tr.lastEdit().kb); len(got) != talksPageSize+1 {
		t.Errorf("страница за краем: %v", got)
	}

	l.HandleCallback(ctx, uid, press(kbd.Pack(verbTalk, strconv.FormatInt(firstPeer, 10))))
	if state, _ := st.DialogState(ctx, store.MessengerTelegram+":talks", uid); state != statePMPrefix+strconv.FormatInt(firstPeer, 10) {
		t.Fatalf("состояние после открытия диалога: %q", state)
	}
	l.HandleText(ctx, uid, "mid.1", "привет")
	if len(router.calls) != 1 || router.calls[0].peerID != firstPeer {
		t.Errorf("текст ушёл в открытый диалог: %+v", router.calls)
	}
}

// Меню команд публикуется под роль бота; админская /news в него не попадает.
func TestPublishCommandsByRole(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)
	l.PublishCommands(ctx)
	names := commandNames(tr.commands)
	for _, want := range []string{"start", "login", "add_note", "mysubs", "cancel"} {
		if !strings.Contains(names, want) {
			t.Errorf("в меню бота команд нет /%s: %s", want, names)
		}
	}
	if strings.Contains(names, "news") {
		t.Errorf("админская /news в меню не значится: %s", names)
	}
	if strings.Contains(names, "talks") {
		t.Errorf("без роутера переписки /talks в меню нет: %s", names)
	}
	l.SetTalkRouter(&fakeRouter{ret: true})
	l.PublishCommands(ctx)
	if !strings.Contains(commandNames(tr.commands), "talks") {
		t.Errorf("с роутером /talks должна появиться: %s", commandNames(tr.commands))
	}

	talks, talksTr := newTestTalksLogic(t, st, store.MessengerTelegram)
	talks.PublishCommands(ctx)
	if names := commandNames(talksTr.commands); strings.Contains(names, "login") {
		t.Errorf("меню бота переписки: %s", names)
	}
}

func commandNames(cmds []kbd.Command) string {
	names := make([]string, 0, len(cmds))
	for _, c := range cmds {
		names = append(names, c.Name)
	}
	return strings.Join(names, " ")
}

// Главное меню под роль бота: у переписки своё, у бота команд «Переписка»
// появляется только когда диалоги ведёт он сам.
func TestMainMenuByRole(t *testing.T) {
	ctx := context.Background()
	const uid = 42
	st := openTestStore(t)

	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)
	l.Greet(ctx, uid)
	menu := strings.Join(buttonTexts(tr.lastKB()), " ")
	for _, want := range []string{"Войти", "Статус", "Написать заметку", "Подписки"} {
		if !strings.Contains(menu, want) {
			t.Errorf("в меню бота команд нет «%s»: %q", want, menu)
		}
	}
	if strings.Contains(menu, "Переписка") {
		t.Errorf("без роутера переписки кнопки быть не должно: %q", menu)
	}

	l.SetTalkRouter(&fakeRouter{ret: true})
	l.Greet(ctx, uid)
	if menu := strings.Join(buttonTexts(tr.lastKB()), " "); !strings.Contains(menu, "Переписка") {
		t.Errorf("с роутером переписки кнопка должна появиться: %q", menu)
	}

	talks, talksTr := newTestTalksLogic(t, st, store.MessengerTelegram)
	talks.Greet(ctx, uid)
	menu = strings.Join(buttonTexts(talksTr.lastKB()), " ")
	if !strings.Contains(menu, "Мои диалоги") || strings.Contains(menu, "Войти") {
		t.Errorf("меню бота переписки: %q", menu)
	}
}
