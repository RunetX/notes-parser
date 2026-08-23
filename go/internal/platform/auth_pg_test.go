package platform

// Интеграционные тесты входа — против настоящего Postgres (гейт и предохранитель
// в pg_test.go). Проверять здесь есть что: половина устройства входа живёт в
// SQL — частичный уникальный индекс «живой челлендж на анкету ровно один»,
// FOR UPDATE вокруг счётчика попыток, откат входа одной транзакцией.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Ради этого весь путь и затевался: человек входит — и его прошлые реплики
// становятся своими БЕЗ переноса данных, потому что id строки равен id анкеты, а
// тень завело зеркало годами раньше.
func TestLoginPromotesShadowInPlace(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 312811, 1493279, "Рио")

	before, err := p.UserByID(ctx, 1493279)
	if err != nil || before.Kind != KindShadow {
		t.Fatalf("до входа: kind %d, err %v — ожидалась тень", before.Kind, err)
	}

	ch, err := p.StartProfileChallenge(ctx, 1493279)
	if err != nil {
		t.Fatal(err)
	}
	about := "о себе: люблю кино\n" + ch.Code + "\nвот так"
	if err := p.VerifyProfileChallenge(ctx, 1493279, ch.Code, about); err != nil {
		t.Fatalf("проверка кода: %v", err)
	}
	id, err := p.CompleteNGSLogin(ctx,
		MirroredAuthor{ID: 1493279, Nick: "Рио"}, GenderMale)
	if err != nil {
		t.Fatal(err)
	}
	if id != 1493279 {
		t.Fatalf("вход завёл нового пользователя %d вместо анкеты", id)
	}
	after, err := p.UserByID(ctx, id)
	if err != nil || after.Kind != KindMember {
		t.Fatalf("после входа: kind %d, err %v", after.Kind, err)
	}
	// Заметка, зеркалированная до входа, теперь его собственная.
	n, err := p.NoteViewByID(ctx, Viewer{UserID: id}, 312811)
	if err != nil || !n.Own {
		t.Fatalf("прежняя заметка не стала своей: own=%v, err=%v", n.Own, err)
	}
	// Использованный код убран: строка в чужом «о себе» и так просится вон.
	var live int
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM auth_challenges WHERE subject = '1493279'`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Errorf("после входа осталось челленджей: %d", live)
	}
}

// Код в чужом «о себе» виден всем, поэтому кода в анкете мало: нужна ещё кука
// того, кто эту проверку начал. Здесь проверяется вторая половина — с чужим
// кодом в куке проверка не проходит, даже если анкета «правильная».
func TestVerifyNeedsTheCodeFromTheStarter(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	ch, err := p.StartProfileChallenge(ctx, 1493279)
	if err != nil {
		t.Fatal(err)
	}
	// Посторонний подсмотрел код в анкете, но в его куке лежит другой.
	if err := p.VerifyProfileChallenge(ctx, 1493279, "T3H-XXXX-YYYY", ch.Code); !errors.Is(err, ErrNoChallenge) {
		t.Fatalf("проверка с чужой кукой: %v, ожидалось ErrNoChallenge", err)
	}
}

// Новый код заменяет прежний: plaintext мы не храним, показать старый нечем.
func TestChallengeIsReplacedNotDuplicated(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	first, err := p.StartProfileChallenge(ctx, 1493279)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.StartProfileChallenge(ctx, 1493279)
	if err != nil {
		t.Fatal(err)
	}
	if first.Code == second.Code {
		t.Fatal("повторная выдача вернула тот же код")
	}
	var live int
	if err := p.pool.QueryRow(ctx, `
		SELECT count(*) FROM auth_challenges
		 WHERE subject = '1493279' AND verified_at IS NULL`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Fatalf("живых челленджей %d, ожидался 1", live)
	}
	if err := p.VerifyProfileChallenge(ctx, 1493279, first.Code, first.Code); !errors.Is(err, ErrNoChallenge) {
		t.Errorf("старый код всё ещё работает: %v", err)
	}
}

// Счётчик попыток обязан расти и на НЕУДАЧНОЙ проверке, иначе он не считает
// ничего: каждая проверка — это наш запрос к НГС, и темп бережём именно так.
func TestFailedAttemptsAreCounted(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	ch, err := p.StartProfileChallenge(ctx, 1493279)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < challengeMaxAttempts; i++ {
		if err := p.VerifyProfileChallenge(ctx, 1493279, ch.Code, "тут кода нет"); !errors.Is(err, ErrCodeNotFound) {
			t.Fatalf("попытка %d: %v", i, err)
		}
	}
	if err := p.VerifyProfileChallenge(ctx, 1493279, ch.Code, ch.Code); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("после потолка: %v, ожидалось ErrTooManyAttempts", err)
	}
}

// Отзыв согласия на распространение обязан ИСПОЛНЯТЬСЯ, а не отмечаться:
// публикации исчезают со страниц в тот же момент, и счётчик под заметкой тоже.
func TestRevokingDistributionHidesEverythingAtOnce(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	if err := p.EnsureConsentDocs(ctx, Operator{}); err != nil {
		t.Fatal(err)
	}
	ingestNote(t, p, 312811, 1493279, "Рио")
	ingestNote(t, p, 312812, 175869, "Гадёныш")
	ingestComment(t, p, 63207290, 312812, 1493279, 0)

	id, err := p.CompleteNGSLogin(ctx, MirroredAuthor{ID: 1493279, Nick: "Рио"}, GenderMale)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := CurrentConsentDocs(Operator{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if err := p.GrantConsent(ctx, id, d.Kind, d.Version, "тест"); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.RevokeConsent(ctx, id, ConsentDistribution); err != nil {
		t.Fatal(err)
	}

	feed, err := p.Feed(ctx, Viewer{}, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range feed {
		if n.ID == 312811 {
			t.Error("заметка отозвавшего согласие всё ещё в ленте")
		}
	}
	thread, err := p.Thread(ctx, Viewer{}, 312812)
	if err != nil {
		t.Fatal(err)
	}
	if len(thread) != 0 {
		t.Errorf("в треде осталось %d комментариев отозвавшего", len(thread))
	}
	// Счётчик денормализован, и без пересчёта под заметкой висело бы
	// «Комментарии 1» при пустом треде.
	host, err := p.NoteViewByID(ctx, Viewer{}, 312812)
	if err != nil {
		t.Fatal(err)
	}
	if host.CommentCount != 0 {
		t.Errorf("счётчик комментариев %d, ожидался 0", host.CommentCount)
	}

	// Возврат согласия возвращает всё обратно: тексты не удалялись.
	if err := p.GrantConsent(ctx, id, ConsentDistribution, 1, "тест"); err != nil {
		t.Fatal(err)
	}
	back, err := p.Thread(ctx, Viewer{}, 312812)
	if err != nil || len(back) != 1 {
		t.Errorf("после возврата согласия в треде %d комментариев, err %v", len(back), err)
	}
	// И счётчик возвращается тем же числом, каким уходил: он правится разницей,
	// а знак у разницы один на оба направления.
	again, err := p.NoteViewByID(ctx, Viewer{}, 312812)
	if err != nil {
		t.Fatal(err)
	}
	if again.CommentCount != 1 {
		t.Errorf("после возврата согласия счётчик %d, ожидалась единица", again.CommentCount)
	}
}

// Первое согласие на распространение публикаций НЕ трогает: у входящего впервые
// ничего не спрятано, и обходить его реплики незачем. Стоил такой обход дорого —
// 18.08.2026 трое не смогли пройти этот экран вовсе, тяжелее всех участнику со
// 138 тыс. реплик в 14 тыс. тредов.
//
// Проверяется тем, что заведомо РАЗОШЕДШИЙСЯ счётчик остаётся разошедшимся:
// чинит его RecountComments, а не чей-то вход.
func TestFirstDistributionConsentTouchesNothing(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	if err := p.EnsureConsentDocs(ctx, Operator{}); err != nil {
		t.Fatal(err)
	}
	ingestNote(t, p, 312812, 175869, "Гадёныш")
	ingestComment(t, p, 63207290, 312812, 1493279, 0)
	if _, err := p.pool.Exec(ctx,
		`UPDATE notes SET comment_count = 42 WHERE id = 312812`); err != nil {
		t.Fatal(err)
	}

	id, err := p.CompleteNGSLogin(ctx, MirroredAuthor{ID: 1493279, Nick: "Рио"}, GenderMale)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.GrantConsent(ctx, id, ConsentDistribution, 1, "тест"); err != nil {
		t.Fatal(err)
	}

	host, err := p.NoteViewByID(ctx, Viewer{}, 312812)
	if err != nil {
		t.Fatal(err)
	}
	if host.CommentCount != 42 {
		t.Errorf("счётчик стал %d: вход пересчитал чужие треды — ровно та работа, "+
			"которая не укладывалась в срок веб-морды", host.CommentCount)
	}
	thread, err := p.Thread(ctx, Viewer{}, 312812)
	if err != nil || len(thread) != 1 {
		t.Errorf("в треде %d комментариев, err %v — вход не должен ни прятать, ни открывать", len(thread), err)
	}
}

// Отказ на экране согласия откатывает вход целиком, а не оставляет в базе
// участника, который ни на что не соглашался.
func TestAbortLoginRollsBack(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 312811, 1493279, "Рио")

	id, err := p.CompleteNGSLogin(ctx, MirroredAuthor{ID: 1493279, Nick: "Рио"}, GenderMale)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := p.CreateSession(ctx, id, "тест")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AbortLogin(ctx, id); err != nil {
		t.Fatal(err)
	}
	u, err := p.UserByID(ctx, id)
	if err != nil || u.Kind != KindShadow {
		t.Fatalf("после отката: kind %d, err %v — ожидалась тень", u.Kind, err)
	}
	if _, err := p.SessionUser(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Errorf("сессия пережила откат: %v", err)
	}
	var ids int
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM identities WHERE user_id = $1`, id).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	if ids != 0 {
		t.Errorf("связь с анкетой пережила откат")
	}

	// А вот у того, кто уже подписал согласие, откат ничего не сносит: это
	// действующий участник, а не брошенный на полпути вход.
	if err := p.EnsureConsentDocs(ctx, Operator{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CompleteNGSLogin(ctx, MirroredAuthor{ID: 1493279, Nick: "Рио"}, GenderMale); err != nil {
		t.Fatal(err)
	}
	if err := p.GrantConsent(ctx, id, ConsentProcessing, 1, "тест"); err != nil {
		t.Fatal(err)
	}
	if err := p.AbortLogin(ctx, id); err != nil {
		t.Fatal(err)
	}
	u, err = p.UserByID(ctx, id)
	if err != nil || u.Kind != KindMember {
		t.Fatalf("откат снёс участника с согласием: kind %d", u.Kind)
	}
}

// Сессия — непрозрачный токен; в базе только его sha256, и отзыв действует
// сразу (ради этого она и не JWT).
func TestSessionLifecycle(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 312811, 1493279, "Рио")
	id, err := p.CompleteNGSLogin(ctx, MirroredAuthor{ID: 1493279, Nick: "Рио"}, GenderMale)
	if err != nil {
		t.Fatal(err)
	}
	token, expires, err := p.CreateSession(ctx, id, "тест")
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(expires) < 24*time.Hour {
		t.Errorf("срок сессии %v — подозрительно короткий", time.Until(expires))
	}
	var stored string
	if err := p.pool.QueryRow(ctx,
		`SELECT encode(token_sha, 'hex') FROM web_sessions WHERE user_id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, token) {
		t.Error("в базе лежит сам токен, а не его хеш")
	}
	if u, err := p.SessionUser(ctx, token); err != nil || u.ID != id {
		t.Fatalf("чтение сессии: %v", err)
	}
	if err := p.RevokeSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SessionUser(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Errorf("отозванная сессия всё ещё жива: %v", err)
	}
}

// Приглашение переживает смерть НГС и умеет привязывать пришедшего к уже
// существующей тени — тогда его прежний след становится своим.
func TestInviteBindsToExistingShadow(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 312811, 1493279, "Рио")
	ingestNote(t, p, 312812, 175869, "Гадёныш")

	admin, err := p.CompleteNGSLogin(ctx, MirroredAuthor{ID: 175869, Nick: "Гадёныш"}, GenderMale)
	if err != nil {
		t.Fatal(err)
	}
	code, err := p.CreateInvite(ctx, admin, 1493279, "Рио потерял анкету", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	id, err := p.RedeemInvite(ctx, code, "неважно")
	if err != nil {
		t.Fatal(err)
	}
	if id != 1493279 {
		t.Fatalf("приглашение завело нового пользователя %d вместо привязки", id)
	}
	n, err := p.NoteViewByID(ctx, Viewer{UserID: id}, 312811)
	if err != nil || !n.Own {
		t.Errorf("прежняя заметка не стала своей: own=%v", n.Own)
	}
	// Одноразовость.
	if _, err := p.RedeemInvite(ctx, code, "ещё раз"); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("приглашение сработало дважды: %v", err)
	}
}

// Приглашение без привязки заводит участника в НАТИВНОЙ полосе: анкеты НГС у
// него нет, и занимать её номер нельзя.
func TestInviteWithoutBindCreatesNativeUser(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 312812, 175869, "Гадёныш")
	admin, err := p.CompleteNGSLogin(ctx, MirroredAuthor{ID: 175869, Nick: "Гадёныш"}, GenderMale)
	if err != nil {
		t.Fatal(err)
	}
	code, err := p.CreateInvite(ctx, admin, 0, "", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	id, err := p.RedeemInvite(ctx, code, "Новенький")
	if err != nil {
		t.Fatal(err)
	}
	if !IsNative(id) {
		t.Fatalf("участник без анкеты получил id %d вне нативной полосы", id)
	}
}

// Со СТРАНИЦЫ приглашение выдаёт администратор, и только он: модератор решает
// про слова, а инвайт — про людей. Проверяется здесь вся дорога целиком, потому
// что каждая её часть — это отдельное правило: право, журнал, список и отзыв.
func TestIssueInviteIsAdminOnly(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 312811, 1493279, "Рио")
	admin := Viewer{UserID: mustUser(t, p, "Гадёныш"), Role: RoleAdmin}

	// Модератору отказывает ЯДРО, а не форма: список того, что позволено,
	// обязан читаться в одном месте.
	moder := Viewer{UserID: mustUser(t, p, "Хатуль мадан"), Role: RoleModerator}
	if _, err := p.IssueInvite(ctx, moder, 1493279, "", InviteTTL); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("модератор выдал приглашение: %v", err)
	}
	// Опечатка в номере обязана прозвучать при ВЫДАЧЕ, а не при попытке войти
	// по уже разосланному коду.
	if _, err := p.IssueInvite(ctx, admin, 999999999, "", InviteTTL); !errors.Is(err, ErrNotFound) {
		t.Fatalf("приглашение выдано несуществующему участнику: %v", err)
	}

	code, err := p.IssueInvite(ctx, admin, 1493279, "Рио потерял анкету", InviteTTL)
	if err != nil {
		t.Fatal(err)
	}
	list, err := p.Invites(ctx, InviteListLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("в списке %d приглашений, ожидалось одно", len(list))
	}
	in := list[0]
	switch {
	case in.BindUser != 1493279 || in.BindNick != "Рио":
		t.Errorf("привязка потеряна: %d %q", in.BindUser, in.BindNick)
	case in.IssuedBy != admin.UserID:
		t.Errorf("выдавшим записан %d, а не администратор", in.IssuedBy)
	case in.Note != "Рио потерял анкету":
		t.Errorf("пометка потеряна: %q", in.Note)
	case !in.Live(time.Now()):
		t.Error("только что выданное приглашение уже не живо")
	}

	// Журнал: «кого впустили» — такая же часть модерации, как «кого скрыли». А
	// вот КОДА в журнале быть не должно ни в каком виде.
	entries, err := p.AuditTail(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var logged bool
	for _, e := range entries {
		if e.Action == ActionInvite && e.Subject.Kind == SubjectInvite && e.Subject.ID == 1493279 {
			logged = true
		}
		if fmt.Sprint(e.Details) != "" && strings.Contains(fmt.Sprint(e.Details), code) {
			t.Fatal("код приглашения попал в журнал")
		}
	}
	if !logged {
		t.Error("выдача приглашения не попала в журнал")
	}

	// Отзыв гасит код, ещё не использованный, и повтор отзыва — не ошибка.
	if err := p.RevokeInvite(ctx, moder, in.CreatedAt); !errors.Is(err, ErrNotAdmin) {
		t.Errorf("модератор отозвал приглашение: %v", err)
	}
	if err := p.RevokeInvite(ctx, admin, in.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RedeemInvite(ctx, code, "неважно"); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("отозванный код всё ещё работает: %v", err)
	}
	if err := p.RevokeInvite(ctx, admin, in.CreatedAt); !errors.Is(err, ErrNothingToDo) {
		t.Errorf("повторный отзыв ответил %v", err)
	}
}

// Опубликованная редакция согласия неизменяема: молча переписанный текст
// превращает все прежние согласия в бумажку без содержания.
func TestPublishedConsentIsImmutable(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	// Этот тест — про саму ПУБЛИКАЦИЮ, поэтому начинает с чистого листа: общий
	// помощник выпускает редакции безличным оператором (так их накатывает
	// администратор вместе со схемой), а здесь проверяется, что выпущенное
	// нельзя переписать молча.
	if _, err := p.pool.Exec(ctx, `DELETE FROM consent_docs`); err != nil {
		t.Fatalf("очистка редакций: %v", err)
	}
	if err := p.EnsureConsentDocs(ctx, Operator{Name: "Первый"}); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureConsentDocs(ctx, Operator{Name: "Первый"}); err != nil {
		t.Fatalf("повторная публикация того же текста должна быть пустой операцией: %v", err)
	}
	err := p.EnsureConsentDocs(ctx, Operator{Name: "Второй"})
	if err == nil || !strings.Contains(err.Error(), "без смены номера версии") {
		t.Fatalf("подмена выпущенной редакции прошла молча: %v", err)
	}
}

// Канал лички: код проверяется ВВОДОМ, второй половины нет и не нужно —
// сообщение лежит в ящике, читать который может только владелец анкеты.
func TestTalksCodeVerifiesByInput(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	const profile = 1493279

	ch, err := p.StartTalksChallenge(ctx, profile)
	if err != nil {
		t.Fatalf("выдача кода: %v", err)
	}
	if err := p.VerifyTalksCode(ctx, profile, "T3H-ZZZZ-ZZZZ"); !errors.Is(err, ErrCodeMismatch) {
		t.Fatalf("чужой код: %v, ожидался ErrCodeMismatch", err)
	}
	// Регистр и пробелы неважны: код диктуют и переписывают руками.
	if err := p.VerifyTalksCode(ctx, profile, "  "+strings.ToLower(ch.Code)+" "); err != nil {
		t.Fatalf("свой код: %v", err)
	}
	// Второй раз тот же код не работает: он уже отмечен проверенным.
	if err := p.VerifyTalksCode(ctx, profile, ch.Code); !errors.Is(err, ErrNoChallenge) {
		t.Errorf("повтор проверенного кода: %v, ожидался ErrNoChallenge", err)
	}
}

// Вход начинает кто угодно, а сообщение приходит НАСТОЯЩЕМУ владельцу анкеты, то
// есть служебный аккаунт можно сделать рассыльщиком чужими руками. Потолок
// отправок — единственное, что этому мешает.
func TestTalksChallengeLimitsSends(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	const profile = 1038894

	first, err := p.StartTalksChallenge(ctx, profile)
	if err != nil {
		t.Fatalf("первая отправка: %v", err)
	}
	if _, err := p.StartTalksChallenge(ctx, profile); !errors.Is(err, ErrCodeJustSent) {
		t.Fatalf("вторая отправка сразу: %v, ожидался ErrCodeJustSent", err)
	}
	// Спустя паузу код меняется, а срок остаётся прежним — иначе его можно было
	// бы продлевать вечно.
	for i := 2; i <= 3; i++ {
		agePendingChallenge(t, p, profile)
		next, err := p.StartTalksChallenge(ctx, profile)
		if err != nil {
			t.Fatalf("отправка %d: %v", i, err)
		}
		if next.Code == first.Code {
			t.Fatal("повторная отправка выдала прежний код, а plaintext мы не храним")
		}
		if next.ExpiresAt.After(first.ExpiresAt.Add(time.Second)) {
			t.Errorf("срок кода продлился: было %v, стало %v", first.ExpiresAt, next.ExpiresAt)
		}
	}
	agePendingChallenge(t, p, profile)
	if _, err := p.StartTalksChallenge(ctx, profile); !errors.Is(err, ErrTooManyAttempts) {
		t.Errorf("четвёртая отправка: %v, ожидался ErrTooManyAttempts", err)
	}
}

// agePendingChallenge состаривает живой челлендж, чтобы не ждать паузы между
// отправками настоящие минуты.
func agePendingChallenge(t *testing.T, p *Platform, profile int64) {
	t.Helper()
	if _, err := p.pool.Exec(context.Background(), `
		UPDATE auth_challenges SET created_at = created_at - $3::interval
		 WHERE kind = $1 AND subject = $2 AND verified_at IS NULL`,
		ChallengeTalks, subjectOf(profile), (TalksResendAfter + time.Minute).String()); err != nil {
		t.Fatalf("сдвиг времени челленджа: %v", err)
	}
}
