package platform

// Привязка мессенджера — против настоящего Postgres, потому что половина её
// устройства живёт в SQL: первичный ключ (kind, external_id), удаление ключа
// через DELETE ... RETURNING, снятие привязок одной транзакцией с отзывом
// согласия.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// bindMember — участник, готовый привязывать: тень, ставшая участником, с
// обязательными согласиями. Необязательное здесь НЕ даётся намеренно — его
// записывает сам StartBinding, и тест обязан видеть, что он это делает.
func bindMember(t *testing.T, p *Platform, id int64, nick string) int64 {
	t.Helper()
	ctx := context.Background()
	ingestNote(t, p, 312800+id%100, id, nick)
	userID, err := p.CompleteNGSLogin(ctx, MirroredAuthor{ID: id, Nick: nick}, GenderMale)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := RequiredConsentDocs(Operator{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if err := p.GrantConsent(ctx, userID, d.Kind, d.Version, "тест"); err != nil {
			t.Fatal(err)
		}
	}
	return userID
}

// Ради этого всё и затевалось: привязавший мессенджер входит БЕЗ анкеты НГС.
func TestBoundMessengerOpensTheDoor(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	id := bindMember(t, p, 1493279, "Рио")

	code, expires, err := p.StartBinding(ctx, id, "тест")
	if err != nil {
		t.Fatal(err)
	}
	if !expires.After(time.Now()) {
		t.Fatalf("срок кода в прошлом: %v", expires)
	}
	// Бот сперва СПРАШИВАЕТ, чья это запись, и код при этом не тратится:
	// подтверждение ником — единственное, что отличает подсунутый код от своего.
	who, nick, err := p.BindingOffer(ctx, code)
	if err != nil || who != id || nick != "Рио" {
		t.Fatalf("предложение привязки: %d %q %v", who, nick, err)
	}
	if _, _, err := p.BindMessenger(ctx, code, IdentityTelegram, 777); err != nil {
		t.Fatalf("привязка: %v", err)
	}
	got, gotNick, err := p.MessengerLogin(ctx, IdentityTelegram, 777)
	if err != nil || got != id || gotNick != "Рио" {
		t.Fatalf("вход по привязке: %d %q %v", got, gotNick, err)
	}
	// Код ОДНОРАЗОВЫЙ, и держит это отсутствие строки, а не отметка рядом.
	if _, _, err := p.BindMessenger(ctx, code, IdentityTelegram, 777); !errors.Is(err, ErrBindCodeInvalid) {
		t.Fatalf("код сгодился второй раз: %v", err)
	}
}

// Согласие на привязку записывается САМИМ StartBinding: подпись даёт человек,
// прочитав текст на экране, и появиться она обязана там же, где он нажал.
func TestBindingWritesItsConsent(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	id := bindMember(t, p, 1493279, "Рио")

	have, err := p.UserConsents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := have[ConsentBinding]; ok {
		t.Fatal("согласие на привязку взялось до того, как его спросили")
	}
	if _, _, err := p.StartBinding(ctx, id, "тест"); err != nil {
		t.Fatal(err)
	}
	have, err = p.UserConsents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !have.Has(ConsentBinding, 1) {
		t.Fatal("после выдачи кода согласия на привязку нет")
	}
}

// Отозванное согласие не даёт завести строку, даже если код на руках: между
// выдачей кода и привязкой человек мог нажать «Отозвать», и обработка без
// основания недопустима.
func TestRevokedConsentStopsTheBinding(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	id := bindMember(t, p, 1493279, "Рио")

	code, _, err := p.StartBinding(ctx, id, "тест")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RevokeConsent(ctx, id, ConsentBinding); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.BindMessenger(ctx, code, IdentityTelegram, 777); !errors.Is(err, ErrBindCodeInvalid) {
		t.Fatalf("привязка прошла после отзыва согласия: %v", err)
	}
}

// Чужую привязку не переписываем МОЛЧА: для того человека это способ входа.
func TestForeignBindingIsNotStolen(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	rio := bindMember(t, p, 1493279, "Рио")
	kisa := bindMember(t, p, 1488786, "Киса")

	code, _, err := p.StartBinding(ctx, rio, "тест")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.BindMessenger(ctx, code, IdentityTelegram, 777); err != nil {
		t.Fatal(err)
	}
	code2, _, err := p.StartBinding(ctx, kisa, "тест")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.BindMessenger(ctx, code2, IdentityTelegram, 777); !errors.Is(err, ErrMessengerTaken) {
		t.Fatalf("чужой мессенджер увели молча: %v", err)
	}
	if got, _, err := p.MessengerLogin(ctx, IdentityTelegram, 777); err != nil || got != rio {
		t.Fatalf("после отказа привязка уехала: %d %v", got, err)
	}
}

// Один мессенджер — одна привязка: прежний аккаунт той же породы отцепляется,
// иначе у человека копились бы забытые ключи, которых он не видит на «Моей
// странице» (номера мы не показываем) и потому не может снять.
func TestSecondAccountOfTheSameMessengerReplacesTheFirst(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	id := bindMember(t, p, 1493279, "Рио")

	for _, mid := range []int64{777, 888} {
		code, _, err := p.StartBinding(ctx, id, "тест")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.BindMessenger(ctx, code, IdentityTelegram, mid); err != nil {
			t.Fatalf("привязка %d: %v", mid, err)
		}
	}
	if _, _, err := p.MessengerLogin(ctx, IdentityTelegram, 777); !errors.Is(err, ErrNoBinding) {
		t.Fatalf("прежний телеграм остался ключом: %v", err)
	}
	if got, _, err := p.MessengerLogin(ctx, IdentityTelegram, 888); err != nil || got != id {
		t.Fatalf("новый телеграм не впускает: %d %v", got, err)
	}
	// А вот РАЗНЫЕ мессенджеры живут рядом: это два независимых ключа, и в этом
	// половина смысла — потеряв один, человек входит другим.
	code, _, err := p.StartBinding(ctx, id, "тест")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.BindMessenger(ctx, code, IdentityMAX, 999); err != nil {
		t.Fatal(err)
	}
	bound, err := p.UserBindings(ctx, id)
	if err != nil || len(bound) != 2 {
		t.Fatalf("привязок %d, ожидались две: %v", len(bound), err)
	}
}

// Отвязка закрывает дверь и гасит согласие: согласие, пережившее данные, — это
// бумага, которой нечего покрывать.
func TestUnbindClosesTheDoorAndRevokesConsent(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	id := bindMember(t, p, 1493279, "Рио")

	code, _, err := p.StartBinding(ctx, id, "тест")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.BindMessenger(ctx, code, IdentityTelegram, 777); err != nil {
		t.Fatal(err)
	}
	if err := p.Unbind(ctx, id, IdentityTelegram); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.MessengerLogin(ctx, IdentityTelegram, 777); !errors.Is(err, ErrNoBinding) {
		t.Fatalf("после отвязки дверь открыта: %v", err)
	}
	have, err := p.UserConsents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if have.Has(ConsentBinding, 1) {
		t.Fatal("согласие на привязку пережило саму привязку")
	}
	if err := p.Unbind(ctx, id, IdentityTelegram); !errors.Is(err, ErrNoBinding) {
		t.Fatalf("повторная отвязка отвечает не «уже нечего»: %v", err)
	}
}

// Отзыв ОБЩЕГО согласия отвязывает мессенджеры сам: пока строка identities жива,
// она остаётся действующим ключом от учётной записи — то есть отозванное
// согласие продолжало бы впускать.
func TestRevokingProcessingDropsBindings(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	id := bindMember(t, p, 1493279, "Рио")

	code, _, err := p.StartBinding(ctx, id, "тест")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.BindMessenger(ctx, code, IdentityTelegram, 777); err != nil {
		t.Fatal(err)
	}
	if err := p.RevokeConsent(ctx, id, ConsentProcessing); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.MessengerLogin(ctx, IdentityTelegram, 777); !errors.Is(err, ErrNoBinding) {
		t.Fatalf("после отзыва обработки вход по привязке жив: %v", err)
	}
}

// Ключи двух пород НЕ ходят по чужим дорогам (миграция 0030). Проверяется на
// пути данных, а не на формуле: обе породы лежат в одной таблице, и разойтись
// они могут молча — ключ привязки, годный как ключ входа, впустил бы того, кто
// ещё ничего не подтвердил.
func TestBindKeyIsNotALoginKey(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	id := bindMember(t, p, 1493279, "Рио")

	code, _, err := p.StartBinding(ctx, id, "тест")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.RedeemBotLogin(ctx, code); !errors.Is(err, ErrBotKeyInvalid) {
		t.Fatalf("код привязки сработал как ключ входа: %v", err)
	}
	key, _, err := p.StartBotLogin(ctx, id, IdentityTelegram, 777)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.BindMessenger(ctx, key, IdentityTelegram, 777); !errors.Is(err, ErrBindCodeInvalid) {
		t.Fatalf("ключ входа сработал как код привязки: %v", err)
	}
	// И друг друга они не гасят: просьба «дай ссылку входа» не должна убивать
	// начатую привязку — человек как раз держит её код на экране.
	if _, _, err := p.BindMessenger(ctx, code, IdentityTelegram, 777); err != nil {
		t.Fatalf("выдача ключа входа убила начатую привязку: %v", err)
	}
}

// Привязку заводит только УЧАСТНИК: у жителя и у служебной анкеты субъекта
// персональных данных нет вовсе, и согласия за них не даёт никто.
func TestOnlyMemberCanBind(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 312850, 1488786, "Киса") // тень: вход не завершён

	if _, _, err := p.StartBinding(ctx, 1488786, "тест"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("тень завела привязку: %v", err)
	}
	sysID, err := p.EnsureSystemUser(ctx, "Зазеркалье")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.StartBinding(ctx, sysID, "тест"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("служебная анкета завела привязку: %v", err)
	}
}

// СЕССИЯ СКОЛЬЗИТ: срок отсчитывается от последнего визита, а не от входа.
// Тест на пути данных — читается настоящий expires_at, потому что величина эта
// живёт в SQL и «посчитано верно» здесь ничего не значит.
func TestSessionSlidesWithVisits(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	id := bindMember(t, p, 1493279, "Рио")

	token, first, err := p.CreateSession(ctx, id, "тест")
	if err != nil {
		t.Fatal(err)
	}
	// Отматываем визит назад: продление огрублено до часа, иначе строка сессии
	// переписывалась бы на каждый запрос страницы.
	if _, err := p.pool.Exec(ctx, `
		UPDATE web_sessions
		   SET last_seen_at = now() - interval '2 hours',
		       expires_at = expires_at - interval '2 hours'
		 WHERE user_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	_, until, err := p.SessionUser(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := p.pool.QueryRow(ctx,
		`SELECT expires_at FROM web_sessions WHERE user_id = $1`, id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.After(first.Add(-time.Minute)) {
		t.Fatalf("срок сессии не продлился: было %v, стало %v", first, after)
	}
	// НОВЫЙ СРОК ОБЯЗАН УЕХАТЬ НАРУЖУ: по нему морда переставляет куку, а без
	// этого скользила бы только строка в базе — браузер выбросил бы куку в тот
	// же день, что и до правки, и вся дверь осталась бы запертой по расписанию.
	if until.IsZero() {
		t.Fatal("продление не названо наружу: куку переставить будет нечем")
	}
	if until.Sub(after).Abs() > time.Second {
		t.Errorf("наружу уехал не тот срок, что лёг в базу: %v против %v", until, after)
	}
	// Второй визит подряд продления НЕ даёт — порог в час стоит затем, чтобы
	// строка не переписывалась на каждый запрос страницы.
	if _, again, err := p.SessionUser(ctx, token); err != nil || !again.IsZero() {
		t.Errorf("продлили второй раз подряд: %v, %v", again, err)
	}
}

// ОТОЗВАННУЮ СЕССИЮ ПРОДЛЕНИЕ НЕ ВОСКРЕШАЕТ.
//
// Чтение сессии и её продление — два разных запроса, и между ними помещается
// «выйти на всех устройствах», нажатое в соседней вкладке. Без повторной
// проверки revoked_at продление вернуло бы к жизни ровно ту сессию, которую
// человек только что погасил, — то есть кнопка, ради которой скользящий срок и
// оплачен, отменялась бы случайным запросом.
func TestRenewalDoesNotResurrectARevokedSession(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	id := bindMember(t, p, 1493279, "Рио")

	token, _, err := p.CreateSession(ctx, id, "тест")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.pool.Exec(ctx, `
		UPDATE web_sessions SET last_seen_at = now() - interval '2 hours' WHERE user_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := p.RevokeUserSessions(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.SessionUser(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("отозванная сессия читается: %v", err)
	}
	var revoked *time.Time
	if err := p.pool.QueryRow(ctx,
		`SELECT revoked_at FROM web_sessions WHERE user_id = $1`, id).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked == nil {
		t.Error("продление сняло отзыв сессии")
	}
}

// НЕОБЯЗАТЕЛЬНЫЙ ДОКУМЕНТ НЕ СТОИТ НА ДОРОГЕ — ни на входе, ни у публикации.
//
// Это и есть весь рычаг, ради которого третий документ заведён отдельным видом,
// а не абзацем в общем согласии: абзац означал бы новую редакцию processing, то
// есть переподписку ВСЕМИ до единого, — а владелец назвал прямо, что так больше
// нельзя. Проверяется поэтому не «функция вернула список», а обе двери, где
// подпись спрашивают на самом деле.
func TestOptionalConsentIsNotAskedAtTheDoor(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	id := bindMember(t, p, 1493279, "Рио") // подписаны РОВНО обязательные

	missing, err := p.MissingConsent(ctx, id, Operator{})
	if err != nil {
		t.Fatal(err)
	}
	if missing.Kind != "" {
		t.Fatalf("на входе спросили ещё один документ: %s", missing.Kind)
	}
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: id, Body: "первая заметка"}); err != nil {
		t.Fatalf("публиковать без согласия на привязку нельзя: %v", err)
	}
	// А в СПИСКЕ документов площадки он стоит: прочесть его надо уметь до того,
	// как нажмёшь «Привязать», иначе это бумага, которую видит один нажавший.
	all, err := CurrentConsentDocs(Operator{})
	if err != nil {
		t.Fatal(err)
	}
	req, err := RequiredConsentDocs(Operator{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) <= len(req) {
		t.Fatalf("документов %d, обязательных %d — необязательные потерялись", len(all), len(req))
	}
	// Проверяется не ЧИСЛО, а состав: с каждым новым необязательным документом
	// разница росла бы, и тест пришлось бы править вместо того, чтобы он ловил.
	// Ловить он обязан одно — что в обязательный список не просочился лишний
	// вид: именно этим деление списков и стоит.
	for _, d := range req {
		if d.Kind != ConsentProcessing && d.Kind != ConsentDistribution {
			t.Fatalf("в обязательных оказался %s: у входа выросла стена", d.Kind)
		}
	}
	for _, kind := range []string{ConsentBinding, ConsentTalks} {
		var listed bool
		for _, d := range all {
			listed = listed || d.Kind == kind
		}
		if !listed {
			t.Fatalf("%s нет в списке документов площадки: прочесть его будет негде", kind)
		}
	}
}

// ОТЗЫВ ПРИВЯЗКИ НЕ ТРОГАЕТ ЗАМЕТКИ, и это не мелочь: обезличивание необратимо
// (соответствие «кто → какая могила» не хранится нигде), а человек, снимающий
// способ ВХОДА, ничего подобного не просил. Проход по заметкам был безусловным,
// пока видов было два и оба были про распространение; третий, необязательный,
// это правило сломал бы молча — заметил бы только тот, у кого с них исчезло имя.
func TestRevokingBindingKeepsTheName(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	id := bindMember(t, p, 1493279, "Рио")

	noteID, err := p.CreateNote(ctx, NewNote{AuthorID: id, Body: "моя заметка"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.StartBinding(ctx, id, "тест"); err != nil {
		t.Fatal(err)
	}
	if err := p.RevokeConsent(ctx, id, ConsentBinding); err != nil {
		t.Fatal(err)
	}
	note, err := p.NoteViewByID(ctx, Viewer{}, noteID)
	if err != nil {
		t.Fatal(err)
	}
	if note.Author.ID != id || note.Author.Nick != "Рио" {
		t.Fatalf("после отзыва привязки заметка обезличена: автор %d/%q", note.Author.ID, note.Author.Nick)
	}
}

// КЛЮЧИ НЕ ГАСЯТ ДРУГ ДРУГА — проверка обеих подметаний сразу.
//
// Ради этого в 0030 и заведена колонка purpose. Обе выдачи начинают с того, что
// сметают прежние ключи человека, и без фильтра по породе они били бы по чужой:
// попросил ссылку входа — умерла начатая привязка, код которой у тебя на экране;
// начал привязку — умерла только что выданная ссылка.
//
// Тест написан ПОСЛЕ мутационной проверки, показавшей, что прежний
// TestBindKeyIsNotALoginKey этого не ловит: убери фильтр из подметания
// StartBinding — и весь набор оставался зелёным. Обе половины здесь обязаны
// падать поодиночке, поэтому и подметаний два, и проверки после каждого.
func TestTwoKindsOfKeyDoNotSweepEachOther(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	id := bindMember(t, p, 1493279, "Рио")

	code, _, err := p.StartBinding(ctx, id, "тест")
	if err != nil {
		t.Fatal(err)
	}
	// Подметание ВХОДА не должно трогать ключ привязки.
	key, _, err := p.StartBotLogin(ctx, id, IdentityTelegram, 777)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.BindingOffer(ctx, code); err != nil {
		t.Fatalf("выдача ссылки входа убила начатую привязку: %v", err)
	}
	// Подметание ПРИВЯЗКИ не должно трогать ключ входа.
	if _, _, err := p.StartBinding(ctx, id, "тест"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.RedeemBotLogin(ctx, key); err != nil {
		t.Fatalf("начатая привязка убила выданную ссылку входа: %v", err)
	}
}

// ПРЕФИКС КОДА ПРИВЯЗКИ — НЕ КОСМЕТИКА, и до этого теста его можно было вернуть
// к T3H, не уронив ни одной проверки (мутационный прогон 11.09.2026).
//
// Код входа человек по инструкции ВСТАВЛЯЕТ В ПУБЛИЧНОЕ поле «о себе» на НГС.
// Код привязки — живой ключ от учётной записи: кто отправит его боту, тот и
// привяжет свой мессенджер. Перепутав их, человек опубликовал бы второй на виду
// у всех. Разводит их префикс, и стеречь его обязан тест, а не внимание.
func TestBindCodeWearsItsOwnPrefix(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	id := bindMember(t, p, 1493279, "Рио")

	code, _, err := p.StartBinding(ctx, id, "тест")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, bindPrefix+"-") {
		t.Errorf("код привязки %q не носит своего префикса %q", code, bindPrefix)
	}
	if strings.HasPrefix(code, codePrefix+"-") {
		t.Errorf("код привязки выглядит как код входа: %q", code)
	}
	// И невод, которым код входа ищут в чужом «о себе», не должен его узнавать:
	// иначе опубликованный по ошибке ключ привязки ещё и засчитался бы за код.
	if CodeRe.MatchString(code) {
		t.Errorf("код привязки опознаётся как код входа в анкете: %q", code)
	}
}

// РАДИ ЭТОГО ЧЕЛОВЕКА ЧЕТВЁРТАЯ ДВЕРЬ И ЗАВЕДЕНА: у пришедшего по приглашению
// анкеты НГС нет вовсе, и номер ему выдан из НАТИВНОЙ полосы.
//
// Круг проверяется ЦЕЛИКОМ — привязка, ссылка, погашение ключа, — потому что
// дефект жил ровно в его последнем шаге: BoundLoginLink ссылку выдавал, а
// CompleteBotLogin отвергал такой номер («вне полосы НГС»), уже потратив
// одноразовый ключ. То есть дверь была мертва ровно для тех, кому предназначена,
// а стенд этого не видел: binding_pg_test доходил до MessengerLogin и
// останавливался.
//
// Заодно проверяется, чего делать НЕ надо: обряда анкеты у этой двери нет.
// Записав такому человеку ngs_profile, площадка засвидетельствовала бы владение
// анкетой, которой он не показывал и которой у него может не быть никогда.
func TestInvitedMemberLogsInByBinding(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	if err := p.EnsureConsentDocs(ctx, Operator{}); err != nil {
		t.Fatal(err)
	}
	admin, err := p.CompleteNGSLogin(ctx, MirroredAuthor{ID: 175869, Nick: "Гадёныш"}, GenderMale)
	if err != nil {
		t.Fatal(err)
	}
	code, err := p.CreateInvite(ctx, admin, 0, "тест", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	id, err := p.RedeemInvite(ctx, code, "Новенький")
	if err != nil {
		t.Fatal(err)
	}
	if IsNGS(id) {
		t.Fatalf("приглашённый получил номер из полосы НГС: %d", id)
	}
	docs, err := RequiredConsentDocs(Operator{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if err := p.GrantConsent(ctx, id, d.Kind, d.Version, "тест"); err != nil {
			t.Fatal(err)
		}
	}

	bindCode, _, err := p.StartBinding(ctx, id, "тест")
	if err != nil {
		t.Fatalf("приглашённому отказано в привязке: %v", err)
	}
	if _, _, err := p.BindMessenger(ctx, bindCode, IdentityTelegram, 4242); err != nil {
		t.Fatalf("привязка не завелась: %v", err)
	}

	// Дальше — ровно то, что делает бот по команде /site.
	userID, _, err := p.MessengerLogin(ctx, IdentityTelegram, 4242)
	if err != nil || userID != id {
		t.Fatalf("вход по привязке: %d, %v", userID, err)
	}
	key, _, err := p.StartBoundLogin(ctx, userID, IdentityTelegram, 4242)
	if err != nil {
		t.Fatal(err)
	}
	got, method, err := p.RedeemBotLogin(ctx, key)
	if err != nil {
		t.Fatalf("ключ не погасился: %v", err)
	}
	if method != MethodBind {
		t.Errorf("ключ не помечен привязкой: method=%q", method)
	}
	if _, err := p.CompleteBotLogin(ctx, got, method); err != nil {
		t.Fatalf("вход не завершился — четвёртая дверь мертва для приглашённого: %v", err)
	}

	// Обряда анкеты не было: ngs_profile ему не записан.
	var ngs int
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM identities WHERE user_id = $1 AND kind = $2`, id, IdentityNGS).Scan(&ngs); err != nil {
		t.Fatal(err)
	}
	if ngs != 0 {
		t.Errorf("вход по привязке записал владение анкетой НГС: строк %d", ngs)
	}
}
