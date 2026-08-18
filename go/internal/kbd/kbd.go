// Пакет kbd — элементы интерфейса бота, общие для мессенджеров: кнопки под
// сообщением, нажатия на них и меню команд. Диалоговое ядро (dmbot) собирает
// клавиатуру и разбирает payload, а транспорты (телеграмная обёртка в dmbot и
// maxx.Mirror) переводят её в формат своего API. Пакет листовой намеренно:
// иначе транспорту пришлось бы импортировать диалоговое ядро. Ссылочные кнопки
// сюда не поднимаются — «💬 Обсудить» живёт в maxx и мессенджер-агностичной
// не бывает. А вот формат payload'а сюда поднимается весь, включая аргумент
// deep-link'а «/start» (StartSub): кладёт его постер, разбирает диалоговое
// ядро — ровно та же пара сторон, что у Pack/Parse.
package kbd

import "strings"

const (
	// Version — версия формата payload. Кнопка живёт в чужой истории сколько
	// угодно долго, и нажатие из прошлого релиза надо уметь отличить от своего.
	Version = "1"
	// PayloadLimit — предел Telegram на callback_data, в БАЙТАХ (у MAX 1024,
	// равняемся по строгому). Кириллица стоит два байта на знак, поэтому
	// payload только ASCII — см. Pack.
	PayloadLimit = 64
)

// Button — кнопка: подпись по-русски, payload — ASCII из Pack.
type Button struct {
	Text    string
	Payload string
}

// Keyboard — клавиатура под сообщением, строками сверху вниз.
type Keyboard struct {
	Rows [][]Button
}

// New создаёт пустую клавиатуру: kbd.New().Row(a, b).Row(c).
func New() *Keyboard { return &Keyboard{} }

// Row добавляет строку кнопок. Пустая строка игнорируется — так удобнее
// собирать меню, часть кнопок которого условна.
func (k *Keyboard) Row(btns ...Button) *Keyboard {
	if len(btns) > 0 {
		k.Rows = append(k.Rows, btns)
	}
	return k
}

// Empty — клавиатуры нет (nil-safe: снятие кнопок передаётся как nil).
func (k *Keyboard) Empty() bool { return k == nil || len(k.Rows) == 0 }

// Callback — нажатие, приведённое к общему виду.
type Callback struct {
	// AnswerID — чем отвечать мессенджеру, чтобы погасить «спиннер»:
	// в Telegram это callback_query.id, в MAX — callback_id.
	AnswerID string
	// MessageID — сообщение с клавиатурой, его правят после нажатия. Пусто —
	// сообщение недоступно (удалено в MAX, слишком старое в Telegram);
	// это рабочий случай, а не ошибка.
	MessageID string
	Payload   string
}

// Pack собирает payload «<версия>:<verb>[:<arg>]». Аргумент — только
// идентификатор или перечисление: свободный пользовательский текст в payload
// не кладут никогда, он не влезет в PayloadLimit (ключевое слово подписки
// длиной в твит уронит отправку всего сообщения, а не одну кнопку).
func Pack(verb, arg string) string {
	if arg == "" {
		return Version + ":" + verb
	}
	return Version + ":" + verb + ":" + arg
}

// Parse разбирает payload. ok == false — мусор или кнопка чужой версии;
// звать её обработчик нельзя, пользователю отвечают «кнопка устарела».
func Parse(payload string) (verb, arg string, ok bool) {
	parts := strings.SplitN(payload, ":", 3)
	if len(parts) < 2 || parts[0] != Version || parts[1] == "" {
		return "", "", false
	}
	if len(parts) == 3 {
		arg = parts[2] // аргумент может содержать «:» — режем ровно на три части
	}
	return parts[1], arg, true
}

// Fits — влезает ли payload в предел мессенджера.
func Fits(payload string) bool { return len(payload) <= PayloadLimit }

// VerbSubscribe — глагол кнопки «Подписаться» под постом канала. Он здесь, а не
// в dmbot, потому что кладёт его транспорт (maxx собирает клавиатуру поста), а
// разбирает диалоговое ядро — оба и так ходят сюда за Pack/Parse.
const VerbSubscribe = "sub"

// startSubPrefix — префикс payload'а deep-link'а «подписаться на заметку».
const startSubPrefix = "sub_"

// nativeIDPrefixLen — длина идентификатора, выданного собственной площадкой
// (platform.NativeIDBase — двенадцатизначное число). Продублирована здесь
// длиной, а не импортом: kbd — листовой пакет намеренно, и ради одной
// константы в него въехало бы всё ядро площадки вместе с pgx.
const nativeIDLen = 12

// Subscribable — предлагать ли подписку на эту заметку.
//
// Подписки живут в SQLite и знают только заметки НГС. У заметки, написанной НА
// ПЛОЩАДКЕ, в зеркальной базе строки нет вовсе — кнопка привела бы человека в
// «заметку не нашёл», а сработать подписка не смогла бы и потом: комментарии
// такой заметки приносит не зеркало. Различает их полоса идентификаторов: у
// НГС их шесть-восемь знаков, у нативных двенадцать.
func Subscribable(noteID string) bool {
	return numeric(noteID) && len(noteID) < nativeIDLen
}

// numeric — строка из одних цифр и непустая.
func numeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// StartSub — payload ссылки t.me/<бот>?start=<payload>: кнопка под постом
// канала ведёт в ЛС и приносит с собой id заметки. Так обходится главное
// ограничение Telegram — бот не пишет первым тому, кто его не запускал.
// Пусто — id не годится в payload: Telegram разрешает там только
// [A-Za-z0-9_-] и не больше 64 знаков.
func StartSub(noteID string) string {
	if !Subscribable(noteID) || len(startSubPrefix)+len(noteID) > PayloadLimit {
		return ""
	}
	return startSubPrefix + noteID
}

// ParseStartSub — id заметки из payload'а /start. ok == false — payload не наш
// (обычный /start, ссылка чужого релиза, мусор): такой игнорируют молча.
func ParseStartSub(payload string) (noteID string, ok bool) {
	rest, found := strings.CutPrefix(payload, startSubPrefix)
	if !found || rest == "" {
		return "", false
	}
	if StartSub(rest) == "" {
		return "", false
	}
	return rest, true
}

// Command — пункт меню команд бота: Telegram показывает его по «/», MAX — в
// своём списке команд. Name без ведущего слэша.
type Command struct {
	Name        string
	Description string
}
