package maxx

import (
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
)

// midPrefix — префикс id сообщения MAX: «mid.<32 hex>».
const midPrefix = "mid."

// MessageLink собирает ссылку на конкретное сообщение чата — в клиенте она
// открывает именно его, а не начало чата. Формат снят с живой ссылки:
// https://max.ru/c/<chat_id>/<base64url последних 8 байт mid>. Первые 8 байт
// mid — это сам chat_id (проверено: mid.ffffb9b4e305e2e5… ↔ -77288422645019),
// поэтому chatID здесь только для читаемости ссылки.
//
// Пустая строка — mid непонятного вида: вызывающий откатывается на ссылку-
// приглашение чата (кнопка тогда ведёт в начало обсуждения, как раньше).
func MessageLink(chatID int64, mid string) string {
	tail, ok := midTail(mid)
	if !ok {
		return ""
	}
	return "https://max.ru/c/" + strconv.FormatInt(chatID, 10) + "/" + tail
}

// midTail — вторая половина mid (собственно id сообщения) в base64url без
// набивки: именно её MAX ставит в ссылку.
func midTail(mid string) (string, bool) {
	raw := strings.TrimPrefix(mid, midPrefix)
	if len(raw) != 32 {
		return "", false
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return "", false
	}
	return base64.RawURLEncoding.EncodeToString(b[8:]), true
}
