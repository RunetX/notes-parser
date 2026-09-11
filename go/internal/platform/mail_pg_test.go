package platform

// Личная переписка — против НАСТОЯЩЕГО Postgres, и иначе нельзя: половина
// устройства эпика живёт в SQL. Нормализованная пара с CHECK и уникальным
// индексом, счётчики сторон, частичные индексы, перенормализация пары при
// обезличивании, сроки хранения — подделка подтвердила бы только саму себя.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// mailMember — участник, готовый переписываться: тень, ставшая участником, с
// обязательными согласиями И с четвёртым, необязательным.
func mailMember(t *testing.T, p *Platform, id int64, nick string) int64 {
	t.Helper()
	userID := bindMember(t, p, id, nick)
	grantTalks(t, p, userID)
	return userID
}

func grantTalks(t *testing.T, p *Platform, userID int64) {
	t.Helper()
	doc, err := ConsentDocOf(Operator{}, ConsentTalks)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.GrantConsent(context.Background(), userID, doc.Kind, doc.Version, "тест"); err != nil {
		t.Fatal(err)
	}
}

// rewindMail отматывает переписку назад во времени. Нужен потому, что потолки
// настоящие: одно письмо в десять секунд и один НОВЫЙ собеседник в пять минут, —
// и без отмотки почти всякий тест на второе письмо упирался бы в них.
func rewindMail(t *testing.T, p *Platform, d time.Duration) {
	t.Helper()
	ctx := context.Background()
	for _, sql := range []string{
		`UPDATE mail_messages SET sent_at = sent_at - $1::interval`,
		`UPDATE mail_dialogs SET created_at = created_at - $1::interval`,
	} {
		if _, err := p.pool.Exec(ctx, sql, d.String()); err != nil {
			t.Fatalf("отмотка переписки: %v", err)
		}
	}
}

func sendMail(t *testing.T, p *Platform, from, to int64, body string) MessageSent {
	t.Helper()
	out, err := p.SendMessage(context.Background(), from, to, body)
	if err != nil {
		t.Fatalf("письмо %d → %d: %v", from, to, err)
	}
	return out
}

// Пара НОРМАЛИЗОВАНА: кто бы кому ни написал первым, переписка у двоих одна.
// Держит это база (CHECK lo_id < hi_id плюс уникальный индекс), а не проверка в
// коде, — двух одновременно нажавших проверка не развела бы.
func TestПараПереписки(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	a := mailMember(t, p, 1493279, "Рио")
	b := mailMember(t, p, 175869, "Гадёныш")

	first := sendMail(t, p, a, b, "привет")
	rewindMail(t, p, time.Minute)
	if !first.First {
		t.Error("первое письмо не объявлено первым")
	}
	back := sendMail(t, p, b, a, "и тебе")
	if back.DialogID != first.DialogID {
		t.Fatalf("встречное письмо завело вторую переписку: %d против %d", back.DialogID, first.DialogID)
	}
	if back.First {
		t.Error("ответ объявлен первым письмом")
	}

	var n int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM mail_dialogs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("переписок %d, а пара одна", n)
	}
	// Себе написать нельзя, и проверка стоит ДО базы: отказ CHECK человеку не
	// покажешь.
	if _, err := p.SendMessage(ctx, a, a, "сам себе"); !errors.Is(err, ErrSelfMessage) {
		t.Fatalf("письмо самому себе: %v", err)
	}
}

// Пять отказов адресата, поимённо. Ответ у них ОДИН на все причины: перебором
// номеров иначе выясняется, кто здесь завёл переписку, а кто её отключил.
func TestКомуПисатьНельзя(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	me := mailMember(t, p, 1493279, "Рио")

	// Тень: зеркало её видело, сама она не входила и согласия не давала.
	ingestNote(t, p, 312900, 606064, "Хатуль мадан")
	shadow := int64(606064)

	// Житель эпика «народ»: почтового ящика у персонажа нет.
	persona := mailMember(t, p, 1509145, "Artemisia")
	if _, err := p.pool.Exec(ctx, `UPDATE users SET persona = true WHERE id = $1`, persona); err != nil {
		t.Fatal(err)
	}
	// Служебная анкета площадки: писать оператору — это «Как связаться».
	service, err := p.EnsureSystemUser(ctx, "Зазеркалье")
	if err != nil {
		t.Fatal(err)
	}
	// Отозвавший согласие на переписку: решение владельца 11.09.2026 — новых
	// писем ему не принимаем.
	quit := mailMember(t, p, 1372959, "Полынь-Трава")
	if err := p.RevokeConsent(ctx, quit, ConsentTalks); err != nil {
		t.Fatal(err)
	}
	// Обезличенный: его больше нет.
	gone := mailMember(t, p, 1038894, "Пух")
	if _, err := p.AnonymizeUser(ctx, Viewer{}, gone); err != nil {
		t.Fatal(err)
	}

	for name, id := range map[string]int64{
		"тень": shadow, "житель": persona, "служебная анкета": service,
		"отозвавший согласие": quit, "обезличенный": gone,
		"несуществующий": 999999999,
	} {
		if _, err := p.SendMessage(ctx, me, id, "письмо"); !errors.Is(err, ErrNoRecipient) {
			t.Errorf("%s принял письмо: %v", name, err)
		}
		if _, err := p.CanWriteTo(ctx, me, id); !errors.Is(err, ErrNoRecipient) {
			t.Errorf("%s: кнопка «Написать» нарисовалась бы: %v", name, err)
		}
	}
}

// Согласие спрашивается ДВАЖДЫ: на своём экране до письма и ВТОРОЙ раз внутри
// транзакции отправки. Между экраном и нажатием человек мог нажать «Отозвать», и
// письмо, записанное после отзыва, было бы обработкой без основания.
func TestСогласиеНаПереписку(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	a := bindMember(t, p, 1493279, "Рио") // БЕЗ четвёртого согласия
	b := mailMember(t, p, 175869, "Гадёныш")

	if _, err := p.SendMessage(ctx, a, b, "привет"); !errors.Is(err, ErrNoTalkConsent) {
		t.Fatalf("письмо без подписи: %v", err)
	}
	// Ошибка ТИПИЗИРОВАНА: по ней морда ведёт на экран документа, а не прячет
	// кнопку. Спрятанная кнопка ничего не объясняет.
	if _, err := p.CanWriteTo(ctx, a, b); !errors.Is(err, ErrNoTalkConsent) {
		t.Fatalf("CanWriteTo без подписи: %v", err)
	}
	grantTalks(t, p, a)
	sent := sendMail(t, p, a, b, "привет")

	// Читать — тоже по подписи: согласие здесь не разрешение писать, а
	// основание обрабатывать переписку вообще.
	if _, _, err := p.Dialog(ctx, a, sent.DialogID, 0, 50); err != nil {
		t.Fatalf("чтение своей переписки: %v", err)
	}
	if err := p.RevokeConsent(ctx, a, ConsentTalks); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Dialog(ctx, a, sent.DialogID, 0, 50); !errors.Is(err, ErrNoTalkConsent) {
		t.Fatalf("чтение после отзыва: %v", err)
	}
	rewindMail(t, p, time.Minute)
	if _, err := p.SendMessage(ctx, a, b, "ещё"); !errors.Is(err, ErrNoTalkConsent) {
		t.Fatalf("письмо после отзыва: %v", err)
	}
	// Текст при этом остался лежать: стереть его раньше срока хранения площадка
	// не вправе, и сказано об этом в самом документе.
	var body string
	if err := p.pool.QueryRow(ctx,
		`SELECT body FROM mail_messages WHERE id = $1`, sent.MessageID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if body != "привет" {
		t.Fatalf("после отзыва текст письма стал %q", body)
	}
}

// Отзыв ОБЩЕГО согласия тянет переписку за собой: без основания обрабатывать
// нечего. Механика уже была (allConsentKinds), тест стережёт, что новый вид из
// неё не выпал.
func TestОтзывОбработкиГаситПереписку(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	a := mailMember(t, p, 1493279, "Рио")

	if err := p.RevokeConsent(ctx, a, ConsentProcessing); err != nil {
		t.Fatal(err)
	}
	ok, err := hasLiveConsent(ctx, p.pool, a, ConsentTalks)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("отзыв обработки оставил согласие на переписку действующим")
	}
}

// Чёрный список в ОБЕ стороны, и ответы разные: своё закрытие снимает кнопка,
// чужое не снимает ничто.
func TestЧёрныйСписок(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	a := mailMember(t, p, 1493279, "Рио")
	b := mailMember(t, p, 175869, "Гадёныш")
	sent := sendMail(t, p, a, b, "привет")
	rewindMail(t, p, time.Minute)

	if err := p.BlockUser(ctx, b, a); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SendMessage(ctx, a, b, "ещё"); !errors.Is(err, ErrBlockedByPeer) {
		t.Fatalf("закрытый принял письмо: %v", err)
	}
	if _, err := p.SendMessage(ctx, b, a, "ответ"); !errors.Is(err, ErrBlockedByYou) {
		t.Fatalf("закрывший получил не свою ошибку: %v", err)
	}
	// Закрыв человека, переписку с ним убирают со своей страницы той же
	// транзакцией: видеть его письма в списке после нажатия — не то, чего ждут.
	list, err := p.Dialogs(ctx, b, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("у закрывшего осталось %d переписок", len(list))
	}
	blocked, err := p.BlockedList(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 1 || blocked[0].ID != a {
		t.Fatalf("чёрный список: %+v", blocked)
	}
	if err := p.UnblockUser(ctx, b, a); err != nil {
		t.Fatal(err)
	}
	// Повтор снятия молчит: кнопка, отвечающая «состояние уже такое» на второе
	// нажатие, объясняет не то, о чём спрашивали.
	if err := p.UnblockUser(ctx, b, a); err != nil {
		t.Fatalf("повторное снятие: %v", err)
	}
	if _, err := p.SendMessage(ctx, b, a, "ответ"); err != nil {
		t.Fatalf("после снятия запрета: %v", err)
	}
	// Скрытая переписка ВСПЛЫВАЕТ от нового письма — иначе «убрать у себя»
	// молча стало бы блокировкой, о которой отправитель не знает.
	list, err = p.Dialogs(ctx, a, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].DialogID != sent.DialogID {
		t.Fatalf("переписка не всплыла: %+v", list)
	}
}

// ЗАКРЫТИЕ ГАСИТ И КОЛОКОЛЬЧИК, а не только счётчик писем.
//
// Дверей у непрочитанного ДВЕ: своя у переписки (mail_sides.unread, пункт меню)
// и общая у шины (notifications, колокольчик), — и закрывались они порознь.
// BlockUser обнулял первую и не трогал вторую, то есть письма уходили из списка,
// а колокольчик горел дальше и вёл в переписку, которую человек только что
// закрыл. Найдено на Ш3, когда у кнопки появилась рука: до этого закрыть
// переписку можно было только тестом.
//
// Тест НА ПУТИ ДАННЫХ: повод раздаётся настоящим FanOut и спрашивается тем же
// Unread, каким его считает шапка, — формула тут ни при чём, дефект жил в том,
// что один шаг забыли позвать.
func TestЗакрытиеГаситПовод(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	a := mailMember(t, p, 1493279, "Рио")
	b := mailMember(t, p, 175869, "Гадёныш")
	sendMail(t, p, a, b, "привет")
	if _, err := p.FanOut(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if n, err := p.Unread(ctx, b); err != nil || n != 1 {
		t.Fatalf("колокольчик до закрытия: %d (%v)", n, err)
	}
	if err := p.BlockUser(ctx, b, a); err != nil {
		t.Fatal(err)
	}
	if n, err := p.Unread(ctx, b); err != nil || n != 0 {
		t.Fatalf("колокольчик после закрытия: %d (%v)", n, err)
	}
	if n, err := p.UnreadMail(ctx, b); err != nil || n != 0 {
		t.Fatalf("счётчик писем после закрытия: %d (%v)", n, err)
	}
	// Закрыть можно и того, кто ещё не написал: переписки нет, гасить нечего, и
	// это не отказ.
	c := mailMember(t, p, 606064, "Хатуль мадан")
	if err := p.BlockUser(ctx, b, c); err != nil {
		t.Fatalf("закрытие без переписки: %v", err)
	}
}

// Бан отправителя письма не пускает: иначе забаненный флудер переезжает в
// личку. Бан АДРЕСАТА писать ему не мешает — запрет про публикации.
func TestБанИПисьма(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	a := mailMember(t, p, 1493279, "Рио")
	b := mailMember(t, p, 175869, "Гадёныш")

	if _, err := p.pool.Exec(ctx,
		`UPDATE users SET banned_until = now() + interval '1 day' WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SendMessage(ctx, a, b, "письмо"); !errors.Is(err, ErrBanned) {
		t.Fatalf("забаненный написал письмо: %v", err)
	}
	if _, err := p.SendMessage(ctx, b, a, "письмо забаненному"); err != nil {
		t.Fatalf("письмо забаненному: %v", err)
	}
}

// ПОТОЛОК ЧАСТОТЫ РАБОТАЕТ — и это тест про найденную мину, а не про
// арифметику. До 11.09.2026 enforceRate подставляла NativeIDBase вторым
// аргументом сама; у mail_messages своя последовательность с единицы, значит
// `id >= 1e11` ложно для КАЖДОЙ строки, счёт всегда ноль — и потолок писем молча
// не работал бы вовсе. Тест обязан падать на прежнем коде.
func TestПотолокПисемРаботает(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	a := mailMember(t, p, 1493279, "Рио")
	b := mailMember(t, p, 175869, "Гадёныш")
	sent := sendMail(t, p, a, b, "первое")

	// Добиваем окно до потолка прямо в таблице: письма разложены по часу с
	// запасом от правила «одно в десять секунд», чтобы сработало именно часовое.
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO mail_messages (dialog_id, sender_id, body, sent_at)
		SELECT $1, $2, 'добивка', now() - interval '50 minutes' + (i * interval '30 seconds')
		  FROM generate_series(1, $3) AS i`,
		sent.DialogID, a, MessagesPerHour-1); err != nil {
		t.Fatal(err)
	}
	if _, err := p.pool.Exec(ctx,
		`UPDATE mail_messages SET sent_at = now() - interval '55 minutes' WHERE id = $1`,
		sent.MessageID); err != nil {
		t.Fatal(err)
	}
	_, err := p.SendMessage(ctx, a, b, "лишнее")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("девяносто первое письмо прошло: %v", err)
	}
	// Отказ обязан НАЗЫВАТЬ СРОК: «подождите немного», где немного бывает часом,
	// читается как пропажа написанного (замер 06.09.2026 у реплик).
	var limited *RateLimited
	if !errors.As(err, &limited) {
		t.Fatalf("отказ без срока: %T", err)
	}
	if limited.RetryAt.IsZero() || !limited.RetryAt.After(time.Now()) {
		t.Fatalf("срок повтора %v — в прошлом или не посчитан", limited.RetryAt)
	}
	if limited.Max != MessagesPerHour {
		t.Fatalf("сработало правило с потолком %d, а ожидался часовой", limited.Max)
	}
}

// Потолок ПЕРВЫХ писем считает ПЕРЕПИСКИ, а не письма: рассылка отличается от
// разговорчивости не числом сказанного, а числом новых собеседников.
func TestПотолокПервыхПисемСчитаетПереписки(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	a := mailMember(t, p, 1493279, "Рио")
	b := mailMember(t, p, 175869, "Гадёныш")
	c := mailMember(t, p, 606064, "Хатуль мадан")

	sendMail(t, p, a, b, "первому")
	rewindMail(t, p, time.Minute)
	// Второй НОВЫЙ собеседник в те же пять минут — отказ по своему правилу.
	if _, err := p.SendMessage(ctx, a, c, "второму"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("второй новый собеседник подряд: %v", err)
	}
	// А второе письмо ТОМУ ЖЕ собеседнику в это же окно проходит: потолок
	// первых писем к нему не относится вовсе.
	if _, err := p.SendMessage(ctx, a, b, "ещё первому"); err != nil {
		t.Fatalf("второе письмо прежнему собеседнику: %v", err)
	}
}

// Молчание в ответ — не отказ, но и не приглашение: четвёртое письмо тому, кто
// не ответил ни разу, это уже не разговор.
func TestТриПисьмаВМолчание(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	a := mailMember(t, p, 1493279, "Рио")
	b := mailMember(t, p, 175869, "Гадёныш")

	for i := 0; i < UnansweredMax; i++ {
		sendMail(t, p, a, b, "письмо")
		rewindMail(t, p, time.Hour)
	}
	if _, err := p.SendMessage(ctx, a, b, "лишнее"); !errors.Is(err, ErrUnanswered) {
		t.Fatalf("четвёртое письмо в молчание: %v", err)
	}
	// Один ответ снимает счёт: собеседник перестал быть незнакомым.
	sendMail(t, p, b, a, "ну ладно")
	rewindMail(t, p, time.Hour)
	if _, err := p.SendMessage(ctx, a, b, "продолжаем"); err != nil {
		t.Fatalf("после ответа: %v", err)
	}
}

// Непрочитанное: счётчик, отметка прочтения и пересчёт по самим письмам.
func TestНепрочитанное(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	a := mailMember(t, p, 1493279, "Рио")
	b := mailMember(t, p, 175869, "Гадёныш")

	sent := sendMail(t, p, a, b, "первое")
	rewindMail(t, p, time.Hour)
	sendMail(t, p, a, b, "второе")

	n, err := p.UnreadMail(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("непрочитанных у адресата %d, ожидалось 2", n)
	}
	// У ОТПРАВИТЕЛЯ своих писем непрочитанных нет: счётчик про сторону, а не
	// про переписку.
	if n, err = p.UnreadMail(ctx, a); err != nil || n != 0 {
		t.Fatalf("непрочитанных у отправителя %d (%v)", n, err)
	}
	// Отметка по границе: прочитано только первое.
	if err := p.MarkDialogRead(ctx, b, sent.DialogID, sent.MessageID); err != nil {
		t.Fatal(err)
	}
	if n, err = p.UnreadMail(ctx, b); err != nil || n != 1 {
		t.Fatalf("после отметки первого: %d (%v)", n, err)
	}
	// Повтор безобиден: граница двигается только вперёд.
	if err := p.MarkDialogRead(ctx, b, sent.DialogID, sent.MessageID); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkDialogRead(ctx, b, sent.DialogID, 0); err != nil {
		t.Fatal(err)
	}
	if n, err = p.UnreadMail(ctx, b); err != nil || n != 0 {
		t.Fatalf("после отметки всего: %d (%v)", n, err)
	}
	// Пересчёт обязан сойтись с правдой: денормализованное число, которому
	// нечем сойтись, однажды разойдётся навсегда.
	if _, err := p.pool.Exec(ctx, `UPDATE mail_sides SET unread = 77`); err != nil {
		t.Fatal(err)
	}
	if n, err = p.RecountUnread(ctx, b); err != nil || n != 0 {
		t.Fatalf("пересчёт: %d (%v)", n, err)
	}
	// Чужую переписку не отдаём и не отмечаем: строки в mail_sides нет, ответ —
	// «не найдено», а не «это не ваша переписка».
	c := mailMember(t, p, 606064, "Хатуль мадан")
	if err := p.MarkDialogRead(ctx, c, sent.DialogID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("отметка чужой переписки: %v", err)
	}
	if _, _, err := p.Dialog(ctx, c, sent.DialogID, 0, 50); !errors.Is(err, ErrNotFound) {
		t.Fatalf("чтение чужой переписки: %v", err)
	}
}

// ШИНА ЧИСТА, и это главный тест эпика. Событие письма несёт ССЫЛКУ на
// переписку; ни в events, ни в notifications не должно оказаться ни слова из
// письма — ни колонкой, ни выдержкой, ни служебным полем.
//
// Проверяется ПОИСКОМ ПО ВСЕЙ СТРОКЕ (e::text), а не по названным колонкам:
// колонку, добавленную через год, список колонок не поймает, а строка целиком
// поймает.
func TestВШинеНетНиСловаПисьма(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	a := mailMember(t, p, 1493279, "Рио")
	b := mailMember(t, p, 175869, "Гадёныш")

	const secret = "пароль-от-сейфа-42"
	sent := sendMail(t, p, a, b, "держи: "+secret)
	if _, err := p.FanOut(ctx, 100); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"events", "notifications"} {
		var n int
		if err := p.pool.QueryRow(ctx,
			`SELECT count(*) FROM `+table+` t WHERE t::text LIKE '%' || $1 || '%'`, secret).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("в %s нашлось %d строк с текстом письма", table, n)
		}
	}
	// Повод при этом РОЗДАН и ведёт в переписку: граница переносится, а не
	// ломает колокольчик.
	notices, err := p.Notifications(ctx, b, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 {
		t.Fatalf("поводов у адресата %d, ожидался один", len(notices))
	}
	got := notices[0]
	switch {
	case got.Kind != EventMessage || got.Reason != ReasonMessage:
		t.Fatalf("вид %d, повод %d", got.Kind, got.Reason)
	case got.DialogID != sent.DialogID:
		t.Fatalf("повод ведёт в переписку %d вместо %d", got.DialogID, sent.DialogID)
	case got.Excerpt != "":
		t.Fatalf("в поводе выдержка из письма: %q", got.Excerpt)
	case got.NoteID != 0 || got.CommentID != 0:
		t.Fatalf("повод о письме ссылается на публикацию: %d/%d", got.NoteID, got.CommentID)
	}
	// САМОМУ СЕБЕ повода не бывает: отправитель и так знает, что написал.
	if mine, err := p.Notifications(ctx, a, 0, 50); err != nil || len(mine) != 0 {
		t.Fatalf("поводов у отправителя %d (%v)", len(mine), err)
	}
	// Прочитал переписку — колокольчик погас. Путь данных, а не формула:
	// счётчик считается по mail_sides, а повод живёт в notifications, и связать
	// их можно было только через dialog_id.
	if err := p.MarkDialogRead(ctx, b, sent.DialogID, 0); err != nil {
		t.Fatal(err)
	}
	if n, err := p.Unread(ctx, b); err != nil || n != 0 {
		t.Fatalf("колокольчик после прочтения письма: %d (%v)", n, err)
	}
}

// Письмо не идёт НИ В ОЧЕРЕДЬ АВТОМАТА, ни на НГС. Первое — решение L4 (личную
// переписку не отдают сторонней модели), второе — потому что на сайте она легла
// бы публичной репликой.
func TestПисьмоНеУходитНаПроверкуИНаСайт(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	a := mailMember(t, p, 1493279, "Рио")
	b := mailMember(t, p, 175869, "Гадёныш")
	sendMail(t, p, a, b, "письмо")

	for _, table := range []string{"moderation_queue", "ngs_outbox"} {
		var n int
		if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("после письма в %s появилось %d строк", table, n)
		}
	}
}

// Скрытие переписки — не удаление и не блокировка: письма лежат, счётчик
// гаснет, а новое письмо возвращает её в список.
func TestСкрытиеПереписки(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	a := mailMember(t, p, 1493279, "Рио")
	b := mailMember(t, p, 175869, "Гадёныш")
	sent := sendMail(t, p, a, b, "привет")
	rewindMail(t, p, time.Hour)

	if err := p.HideDialog(ctx, b, sent.DialogID); err != nil {
		t.Fatal(err)
	}
	list, err := p.Dialogs(ctx, b, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("скрытая переписка осталась в списке: %+v", list)
	}
	if n, err := p.UnreadMail(ctx, b); err != nil || n != 0 {
		t.Fatalf("счётчик после скрытия: %d (%v)", n, err)
	}
	// Открыть её по прямому адресу по-прежнему можно: скрыт ПОКАЗ в списке, а
	// не сама переписка.
	if _, msgs, err := p.Dialog(ctx, b, sent.DialogID, 0, 50); err != nil || len(msgs) != 1 {
		t.Fatalf("чтение скрытой переписки: %d писем (%v)", len(msgs), err)
	}
	sendMail(t, p, a, b, "ещё раз")
	if list, err = p.Dialogs(ctx, b, 0, 50); err != nil || len(list) != 1 {
		t.Fatalf("переписка не всплыла от нового письма: %d (%v)", len(list), err)
	}
	if list[0].Excerpt != "ещё раз" || list[0].LastFromMe {
		t.Fatalf("выдержка списка: %q, своё письмо: %v", list[0].Excerpt, list[0].LastFromMe)
	}
	if list[0].Peer.ID != a || list[0].Peer.Nick != "Рио" {
		t.Fatalf("собеседник в списке: %+v", list[0].Peer)
	}
}

// ОБЕЗЛИЧИВАНИЕ: имя уходит, тексты остаются, пара пересобирается заново.
//
// Перенормализация — самый хитрый шаг эпика: могила получает свежий номер,
// заведомо больший обоих, и простая подстановка развалила бы CHECK lo_id < hi_id
// у каждой переписки, где человек был старшим номером.
func TestОбезличиваниеПереписки(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	// Номера подобраны так, чтобы человек был и МЛАДШИМ в одной паре, и
	// СТАРШИМ в другой: иначе перенормализация проверялась бы наполовину.
	me := mailMember(t, p, 1038894, "Пух")
	low := mailMember(t, p, 175869, "Гадёныш")
	high := mailMember(t, p, 1493279, "Рио")

	first := sendMail(t, p, me, low, "письмо первому")
	rewindMail(t, p, time.Hour)
	second := sendMail(t, p, high, me, "письмо от второго")
	rewindMail(t, p, time.Hour)
	if err := p.BlockUser(ctx, me, low); err != nil {
		t.Fatal(err)
	}

	res, err := p.AnonymizeUser(ctx, Viewer{}, me)
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages != 1 {
		t.Fatalf("переехало писем %d, ожидалось 1", res.Messages)
	}
	// Тексты на месте — обе стороны разговора целы.
	for _, id := range []int64{first.MessageID, second.MessageID} {
		var body string
		var sender int64
		if err := p.pool.QueryRow(ctx,
			`SELECT body, sender_id FROM mail_messages WHERE id = $1`, id).Scan(&body, &sender); err != nil {
			t.Fatal(err)
		}
		if body == "" {
			t.Fatalf("письмо %d потеряло текст", id)
		}
		if sender == me {
			t.Fatalf("письмо %d осталось подписано прежним автором", id)
		}
	}
	// Пара цела: ни одной строки с нарушенным порядком и ни одной ссылки на
	// прежнего человека — включая started_by, по которому его иначе нашли бы
	// одним запросом.
	var bad int
	if err := p.pool.QueryRow(ctx, `
		SELECT count(*) FROM mail_dialogs
		 WHERE lo_id >= hi_id OR lo_id = $1 OR hi_id = $1 OR started_by = $1`, me).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("после обезличивания %d переписок указывают на человека или нарушают порядок", bad)
	}
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM mail_sides WHERE user_id = $1 OR peer_id = $1`, me).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("стороны переписки не переехали: %d", bad)
	}
	// Чёрный список УДАЛЯЕТСЯ, а не переезжает: «этот закрыл вот того» — связь
	// между двумя людьми, и перенеся её, мы сохранили бы то самое соответствие.
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM mail_blocks`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("чёрный список пережил обезличивание: %d строк", bad)
	}
	// И уникальность пары не сломана: ключ остался ключом.
	if err := p.pool.QueryRow(ctx, `
		SELECT count(*) FROM (SELECT lo_id, hi_id FROM mail_dialogs
		                       GROUP BY lo_id, hi_id HAVING count(*) > 1) d`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("после перенормализации завелись одинаковые пары: %d", bad)
	}
}

// Выгрузка СУБЪЕКТУ и выдача ОРГАНАМ — разные вещи, и разница видна в текстах.
// Субъекту чужие слова не выгружаются (их выгрузит их автор), органам отдаются
// обе стороны: запрос всегда о переписке, а половина её бессмысленна.
func TestВыгрузкаИВыдачаПереписки(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	a := mailMember(t, p, 1493279, "Рио")
	b := mailMember(t, p, 175869, "Гадёныш")
	sendMail(t, p, a, b, "моё-слово")
	rewindMail(t, p, time.Hour)
	sendMail(t, p, b, a, "чужое-слово")

	var mine bytes.Buffer
	if err := p.ExportUser(ctx, a, &mine); err != nil {
		t.Fatal(err)
	}
	switch s := mine.String(); {
	case !strings.Contains(s, "моё-слово"):
		t.Error("в выгрузке субъекта нет его собственного письма")
	case strings.Contains(s, "чужое-слово"):
		t.Error("в выгрузку субъекта попал текст ЧУЖОГО письма")
	case !strings.Contains(s, "входящие_письма"):
		t.Error("в выгрузке нет раздела входящих")
	}

	var official bytes.Buffer
	if err := p.ExportMail(ctx, a, time.Time{}, time.Time{}, &official); err != nil {
		t.Fatal(err)
	}
	s := official.String()
	if !strings.Contains(s, "моё-слово") || !strings.Contains(s, "чужое-слово") {
		t.Error("выдача органам обязана содержать ОБЕ стороны разговора")
	}
	// Период сужает выдачу, и границы у него дневные.
	var narrow bytes.Buffer
	tomorrow := time.Now().Add(48 * time.Hour)
	if err := p.ExportMail(ctx, a, tomorrow, time.Time{}, &narrow); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(narrow.String(), "моё-слово") {
		t.Error("период не сузил выдачу")
	}
	// Выдача обязана оставлять след в журнале, и след этот СВОЙ, не общий с
	// выгрузкой субъекту: журнал отвечает на вопрос «кому отдали чужие письма».
	if err := p.LogMailExport(ctx, Viewer{}, a, time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = $1`, ActionMailExport).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("записей о выдаче в журнале %d", n)
	}
}

// СРОКИ ХРАНЕНИЯ. Полгода — содержание, год — сведения о приёме-передаче.
// Строка письма переживает свой текст, и это не небрежность, а закон.
func TestУборкаПереписки(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	a := mailMember(t, p, 1493279, "Рио")
	b := mailMember(t, p, 175869, "Гадёныш")
	sent := sendMail(t, p, a, b, "старое письмо")

	// Свежая переписка уборку переживает целиком.
	if got, err := p.PruneMail(ctx, 100); err != nil || got.Any() {
		t.Fatalf("уборка тронула свежее: %+v (%v)", got, err)
	}
	rewindMail(t, p, KeepMessageBody+24*time.Hour)
	got, err := p.PruneMail(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bodies != 1 || got.Messages != 0 {
		t.Fatalf("первый срок: %+v", got)
	}
	var (
		body   string
		purged *time.Time
	)
	if err := p.pool.QueryRow(ctx,
		`SELECT body, purged_at FROM mail_messages WHERE id = $1`, sent.MessageID).
		Scan(&body, &purged); err != nil {
		t.Fatalf("строка письма не пережила стирания содержания: %v", err)
	}
	if body != "" || purged == nil {
		t.Fatalf("содержание %q, отметка %v", body, purged)
	}
	// Читателю на месте письма — объяснение, а не пустота: молча исчезнувшее
	// письмо читается как потеря.
	_, msgs, err := p.Dialog(ctx, a, sent.DialogID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || !msgs[0].Purged {
		t.Fatalf("письмо без отметки о сроке: %+v", msgs)
	}
	// Год — и уходит сама строка, а следом опустевшая переписка со сторонами.
	old := (KeepMessageMeta + 24*time.Hour).String()
	for _, sql := range []string{
		`UPDATE mail_messages SET sent_at = now() - $1::interval`,
		`UPDATE mail_dialogs SET last_message_at = now() - $1::interval`,
	} {
		if _, err := p.pool.Exec(ctx, sql, old); err != nil {
			t.Fatal(err)
		}
	}
	total := MailPruned{}
	for range 5 {
		got, err = p.PruneMail(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		total.Messages += got.Messages
		total.Dialogs += got.Dialogs
		if !got.Any() {
			break
		}
	}
	if total.Messages != 1 || total.Dialogs != 1 {
		t.Fatalf("второй срок: %+v", total)
	}
	var left int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM mail_sides`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("стороны пережили свою переписку: %d", left)
	}
}
