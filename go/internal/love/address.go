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
