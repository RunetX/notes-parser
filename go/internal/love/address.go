package love

import (
	"strings"
	"unicode/utf8"
)

// Обращение в реплике. Дерево комментариев на сайте двухуровневое: parent_id
// указывает на КОРЕНЬ ветки, а не на реплику, которой отвечают. Настоящий
// адресат стоит префиксом в тексте — «Ник, текст», клиент подставляет его при
// ответе на комментарий. Замер по архиву: префикс есть у 93,6 % реплик, а
// автор корня ветки совпадает с адресатом лишь в 34,8 %.

// maxNickLen — потолок длины обращения в рунах. Ровно 20: столько же у самого
// длинного ника в архиве (сайт обрезает поле имени), поэтому порог не теряет ни
// одного настоящего ника, зато отсекает придаточные предложения, начинающиеся с
// запятой, — «Когда я разводилась в 35 лет, …».
const maxNickLen = 20

// AddressPrefix вырезает обращение «Ник, …» из начала реплики и приводит к
// нижнему регистру. Пустая строка — обращения нет.
func AddressPrefix(text string) string {
	nick, _, ok := splitAddress(text)
	if !ok {
		return ""
	}
	return strings.ToLower(nick)
}

// TrimAddressPrefix возвращает текст реплики без обращения. Нужен там, где
// адресат хранится ОТДЕЛЬНЫМ ребром (площадка, пакет platform): ник, размазанный
// по чужим телам, потом нечем ни переименовать, ни обезличить — а из ребра
// подпись дорисовывается по текущему нику.
//
// Обращения нет — текст возвращается как есть. Нет и остатка («Ник,» и больше
// ничего) — тоже: пустая реплика хуже реплики с обращением.
func TrimAddressPrefix(text string) string {
	_, rest, ok := splitAddress(text)
	if !ok || rest == "" {
		return text
	}
	return rest
}

// splitAddress делит реплику на обращение и остаток. Обращение всегда в первой
// строке: «Ник, текст».
func splitAddress(text string) (nick, rest string, ok bool) {
	line := text
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		line = text[:i]
	}
	i := strings.IndexByte(line, ',')
	if i < 0 {
		return "", "", false
	}
	nick = strings.TrimSpace(line[:i])
	if n := utf8.RuneCountInString(nick); n < 2 || n > maxNickLen {
		return "", "", false
	}
	return nick, strings.TrimLeft(text[i+1:], " \t\r\n"), true
}

// Addressees — память треда, отвечающая на вопрос «какой реплике отвечает
// обращение „Ник, …“». Сайт этого не говорит: parent_id указывает на КОРЕНЬ
// ветки (см. шапку файла), а мобильное дерево живому зеркалу недоступно — оно
// стоит отдельного запроса на заметку (platsink.ReplyScanner).
//
// Правило двухступенчатое, и вторая ступень оплачена жалобой владельца
// 23.08.2026. Было просто «последняя реплика этого человека в заметке», и на
// живом треде это развалилось так: Хатуль ответил Т 72Б в одной ветке (13:02),
// потом Лилит в другой (13:04), а ответ Т 72Б «Хатуль мадан, …» (13:07) уехал
// к последней — то есть в ветку Лилит, где Т 72Б не было вовсе.
//
// Поэтому сперва ищется последняя реплика адресата, обращённая К САМОМУ
// отвечающему: разговор в треде идёт пинг-понгом, и «Б, …» почти всегда
// отвечает на «А, …» того же Б. Не нашлось (человек вступает в чужой разговор
// или отвечает реплике без обращения) — берётся последняя реплика адресата,
// как раньше.
//
// Ссылка на реплику параметризована, потому что «реплика» у всех разная: у
// мессенджера это id сообщения (строка), у площадки — id её комментария.
type Addressees[T any] struct {
	last   map[string]T // ник → его последняя реплика
	lastTo map[said]T   // «кто кому» → последняя его реплика с этим обращением
	seen   map[string]bool
	seenTo map[said]bool
}

// said — пара «кто обращался к кому», ключ памяти обращений.
type said struct{ from, to string }

// NewAddressees заводит пустую память треда.
func NewAddressees[T any]() *Addressees[T] {
	return &Addressees[T]{
		last: map[string]T{}, lastTo: map[said]T{},
		seen: map[string]bool{}, seenTo: map[said]bool{},
	}
}

// Add запоминает реплику. Порядок вызовов — порядок треда: каждая следующая
// перебивает предыдущую того же автора, поэтому «последняя» получается сама.
func (a *Addressees[T]) Add(ref T, author, text string) {
	author = strings.ToLower(strings.TrimSpace(author))
	if author == "" {
		return
	}
	a.last[author] = ref
	a.seen[author] = true
	if to := AddressPrefix(text); to != "" {
		a.lastTo[said{author, to}] = ref
		a.seenTo[said{author, to}] = true
	}
}

// Resolve отвечает, какой реплике отвечает replier, написавший «nick, …».
// Второе значение — нашлась ли она вообще (ник мог не разойтись: адресуются и
// автору заметки, и тому, чья реплика ещё не доехала до приёмника).
func (a *Addressees[T]) Resolve(nick, replier string) (T, bool) {
	nick = strings.ToLower(strings.TrimSpace(nick))
	replier = strings.ToLower(strings.TrimSpace(replier))
	if nick != "" && replier != "" {
		if key := (said{nick, replier}); a.seenTo[key] {
			return a.lastTo[key], true
		}
	}
	if a.seen[nick] {
		return a.last[nick], true
	}
	var zero T
	return zero, false
}
