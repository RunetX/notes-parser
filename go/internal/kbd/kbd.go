// Пакет kbd — кнопки под сообщением и нажатия на них, общие для мессенджеров.
// Диалоговое ядро (dmbot) собирает клавиатуру и разбирает payload, а транспорты
// (телеграмная обёртка в dmbot и maxx.Mirror) переводят её в формат своего API.
// Пакет листовой намеренно: иначе транспорту пришлось бы импортировать
// диалоговое ядро. Ссылочные кнопки сюда не поднимаются — «💬 Обсудить» живёт
// в maxx и мессенджер-агностичной не бывает.
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
