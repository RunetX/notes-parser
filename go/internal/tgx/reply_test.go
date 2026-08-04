package tgx

import "testing"

// Комментарий-ответ уходит реплаем на сообщение адресата (Telegram сам рисует
// цитату исходного), а без адресата — на корень треда, как было раньше.
// Непонятный id адресата не должен валить пост.
func TestReplyTarget(t *testing.T) {
	m := &Mirror{log: testLogger()}
	const root = 777
	cases := []struct {
		name    string
		replyTo string
		want    int
	}{
		{name: "адресат известен", replyTo: "800", want: 800},
		{name: "адресата нет", replyTo: "", want: root},
		{name: "мусорный id адресата", replyTo: "mid.abc", want: root},
	}
	for _, c := range cases {
		if got := m.replyTarget(root, c.replyTo); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
