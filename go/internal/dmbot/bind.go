package dmbot

// Привязка мессенджера к учётной записи площадки (/bind) и вход по ней.
//
// Здесь ВТОРАЯ половина дела: код рождается в живой сессии на площадке, а бот
// его гасит — потому что только бот знает, кто собеседник. Направление обратно
// входу (/site), и это не исключение из правила, а само правило: ключ рождается
// там, где сидит тот, кого он НАЗНАЧАЕТ. Ключ входа назначает, кого впустить, —
// значит рождается здесь. Ключ привязки назначает, к какой учётной записи
// прицепить мессенджер, — значит рождается там. Разбор обеих подмен — в шапке
// platform/binding.go.
//
// Отсюда же подтверждение НИКОМ: прежде чем привязать, бот показывает, ЧЬЯ это
// запись. Код, подсунутый злоумышленником, выдаёт себя чужим именем ещё до
// нажатия, — а без этого шага привязка прошла бы молча.

import (
	"context"
	"errors"
	"strconv"
	"time"

	"lovegw/internal/kbd"
)

// Отказы привязки, какими их видит диалоговое ядро. СВОИ, а не взятые у
// площадки: dmbot не импортирует platform вовсе — про НГС он знает через
// интерфейсы, и про площадку обязан знать так же. Переводит их адаптер в
// cmd/lovegw, там же, где собирается ссылка, — тем же приёмом, что web.ErrNoProfile.
var (
	// ErrBindCodeInvalid — код не найден, истёк или уже использован. Три случая
	// человеку значат одно и то же.
	ErrBindCodeInvalid = errors.New("код привязки недействителен")
	// ErrMessengerTaken — этот мессенджер уже привязан к ДРУГОЙ записи.
	ErrMessengerTaken = errors.New("мессенджер привязан к другой учётной записи")
	// ErrNoBinding — по этому мессенджеру входить некому.
	ErrNoBinding = errors.New("мессенджер ни к кому не привязан")
	// ErrBindNotAllowed — учётная запись перестала годиться между выдачей кода и
	// нажатием: обезличена, перестала быть участником, исчезла. Отдельно от
	// ErrBindCodeInvalid намеренно — код-то как раз верный, и совет «возьмите
	// новый» отправил бы человека по второму такому же кругу.
	ErrBindNotAllowed = errors.New("этой учётной записи привязка недоступна")
)

// SiteBinding (опц.) — площадка, умеющая привязать мессенджер и впустить по
// привязке. Способность отдельная от SiteLogin, хотя реализует их один и тот же
// объект: без площадки команды нет вовсе, а со старой площадкой (до миграции
// 0030) есть вход, но нет привязки.
type SiteBinding interface {
	// BindOffer — чья учётная запись стоит за кодом. Код при этом НЕ гасится:
	// человеку сперва показывают ник.
	BindOffer(ctx context.Context, code string) (nick string, err error)
	// Bind гасит код и заводит привязку.
	Bind(ctx context.Context, code, messenger string, messengerUserID int64) (nick string, err error)
	// BoundLoginLink — ссылка входа по УЖЕ существующей привязке. Пустой ник
	// вместе с ErrNoBinding означает «этот собеседник ни к кому не привязан».
	BoundLoginLink(ctx context.Context, messenger string, messengerUserID int64) (link string, expires time.Time, err error)
}

// SetSiteBinding подключает привязку. Как и все Set*-инжекции, зовётся строго
// до старта поллеров.
func (l *Logic) SetSiteBinding(b SiteBinding) {
	if b == nil {
		return
	}
	l.siteBind = b
}

const (
	msgBindNoCode = "Эта команда привязывает ваш мессенджер к учётной записи на «Зазеркалье» — " +
		"чтобы вернуть вход, когда анкеты на НГС не стало.\n\n" +
		"Код берётся на самой площадке: «Моя страница» → «Привязать мессенджер». " +
		"Оттуда пришлите сюда строку целиком, вместе с командой."
	msgBindBadCode = "Код не подошёл: он живёт минуты и годится один раз. " +
		"Возьмите новый на «Моей странице» площадки."
	msgBindTaken = "Этот мессенджер уже привязан к другой учётной записи площадки. " +
		"Сначала отвяжите его там — на «Моей странице»."
)

// handleBind — команда /bind. Без кода объясняет, откуда он берётся: сюда
// попадают и те, кто просто увидел команду в меню.
func (l *Logic) handleBind(ctx context.Context, userID int64, messageID, arg string) {
	if l.siteBind == nil {
		return
	}
	// СООБЩЕНИЕ С КОДОМ УДАЛЯЕТСЯ, как удаляется сообщение с паролем у /login,
	// и по той же причине: MSG-XXXX-XXXX — живой ключ от учётной записи, а не
	// имя команды. Кто прочтёт его в чужой переписке и наберёт ту же команду СО
	// СВОЕГО аккаунта раньше, чем хозяин нажмёт «Привязать», тот и привяжет свой
	// мессенджер к чужой записи. Окно — до десяти минут.
	//
	// Удаляем ДО всякой проверки: негодный код тоже незачем оставлять на виду,
	// а ветвей выхода отсюда много, и одну из них однажды забыли бы.
	if messageID != "" {
		l.tr.DeleteMessage(ctx, userID, messageID)
	}
	if arg == "" {
		l.tr.Send(ctx, userID, msgBindNoCode)
		return
	}
	nick, err := l.siteBind.BindOffer(ctx, arg)
	if err != nil {
		if !errors.Is(err, ErrBindCodeInvalid) {
			l.log.Error("проверка кода привязки", "user", userID, "err", err)
			l.tr.Send(ctx, userID, msgInternalError)
			return
		}
		l.tr.Send(ctx, userID, msgBindBadCode)
		return
	}
	// Код едет в payload кнопки, а не в состоянии диалога: он короткий и
	// ASCII-шный (MSG-XXXX-XXXX), а состояние пришлось бы чистить руками после
	// отказа. Предел payload'а при этом проверяется, а не подразумевается.
	payload := kbd.Pack(verbBind, arg)
	if !kbd.Fits(payload) {
		l.tr.Send(ctx, userID, msgBindBadCode)
		return
	}
	l.tr.SendKeyboard(ctx, userID,
		"Привязать этот "+messengerName(l.messenger)+" к учётной записи «"+nick+"» на «Зазеркалье»?\n\n"+
			"После привязки с него можно будет войти под этим именем — командой /site, "+
			"даже когда анкеты на НГС нет. Если ник чужой, ничего не нажимайте: "+
			"код вам прислали, чтобы привязать ВАШ мессенджер к ЧУЖОЙ записи.",
		&kbd.Keyboard{Rows: [][]kbd.Button{{
			{Text: "Привязать", Payload: payload},
			{Text: btnCancel, Payload: kbd.Pack(verbCancel, "")},
		}}})
}

// cbBind — нажатие «Привязать».
func (l *Logic) cbBind(ctx context.Context, userID int64, cb kbd.Callback, arg string) {
	if l.siteBind == nil {
		return
	}
	nick, err := l.siteBind.Bind(ctx, arg, l.messenger, userID)
	switch {
	case err == nil:
		l.tr.EditMessage(ctx, userID, cb.MessageID,
			"Готово: этот "+messengerName(l.messenger)+" привязан к «"+nick+"».\n\n"+
				"Теперь /site выдаст вам ссылку входа даже без живой сессии НГС. "+
				"Отвязать — на «Моей странице» площадки.", nil)
	case errors.Is(err, ErrBindCodeInvalid):
		l.tr.EditMessage(ctx, userID, cb.MessageID, msgBindBadCode, nil)
	case errors.Is(err, ErrMessengerTaken):
		l.tr.EditMessage(ctx, userID, cb.MessageID, msgBindTaken, nil)
	default:
		l.log.Error("привязка мессенджера", "user", userID, "err", err)
		l.tr.EditMessage(ctx, userID, cb.MessageID, msgInternalError, nil)
	}
}

// boundLink — ссылка входа по привязке. Вторая дорога handleSite: первая
// требует живой сессии НГС, а у человека без анкеты её нет и не будет.
// Возвращает false, если привязки нет — тогда говорит handleSite, и говорит про
// сессию, потому что это и есть обычный случай.
func (l *Logic) boundLink(ctx context.Context, userID int64) bool {
	if l.siteBind == nil {
		return false
	}
	link, expires, err := l.siteBind.BoundLoginLink(ctx, l.messenger, userID)
	if err != nil {
		if !errors.Is(err, ErrNoBinding) {
			l.log.Error("ссылка входа по привязке", "user", userID, "err", err)
		}
		return false
	}
	mins := int(time.Until(expires).Round(time.Minute) / time.Minute)
	l.tr.Send(ctx, userID,
		"Вход на «Зазеркалье» — по этой ссылке:\n"+link+
			"\n\nОна одноразовая и живёт "+strconv.Itoa(mins)+" мин. "+
			"Открывать её нужно там, где вы хотите войти. Никому не пересылайте: "+
			"кто перешёл, тот и вошёл под вашим именем.")
	return true
}
