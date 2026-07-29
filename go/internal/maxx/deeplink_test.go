package maxx

import "testing"

// Формат снят с живой ссылки клиента MAX:
// https://max.ru/c/-77288422645019/AZ-usNkuF4A — chat_id десятичным, хвост mid
// в base64url. Первые 8 байт mid совпадают с chat_id.
func TestMessageLink(t *testing.T) {
	const chat = -77288422645019
	got := MessageLink(chat, "mid.ffffb9b4e305e2e5019fadf85ce51328")
	want := "https://max.ru/c/-77288422645019/AZ-t-FzlEyg"
	if got != want {
		t.Errorf("ссылка на сообщение: got %q, want %q", got, want)
	}

	for _, bad := range []string{"", "mid.", "mid.короткий", "12345", "mid.zzzzb9b4e305e2e5019fadf85ce51328"} {
		if link := MessageLink(chat, bad); link != "" {
			t.Errorf("непонятный mid %q должен давать пустую ссылку, получено %q", bad, link)
		}
	}
}
