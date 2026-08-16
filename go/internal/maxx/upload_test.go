package maxx

import (
	"testing"
	"time"
)

// Кэш токенов вложений отдаёт запись, пока она свежая, и забывает
// просроченную: протухший токен MAX останавливает весь тред заметки, а
// лишняя загрузка аватара стоит одного запроса.
func TestTokenCacheTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	u := &uploader{now: func() time.Time { return now }, tokens: map[string]cachedToken{}}

	u.store("https://cdn/a.jpg", "tok1")
	if got, ok := u.cached("https://cdn/a.jpg"); !ok || got != "tok1" {
		t.Fatalf("свежая запись: %q ok=%v", got, ok)
	}

	now = now.Add(tokenTTL + time.Minute)
	if got, ok := u.cached("https://cdn/a.jpg"); ok {
		t.Fatalf("просроченная запись не должна отдаваться: %q", got)
	}
	if len(u.tokens) != 0 {
		t.Errorf("просроченная запись должна удаляться, осталось %d", len(u.tokens))
	}
}

// Потолок числа записей: демон живёт месяцами, карта не должна расти вечно.
func TestTokenCacheEviction(t *testing.T) {
	now := time.Unix(1000, 0)
	u := &uploader{now: func() time.Time { return now }, tokens: map[string]cachedToken{}}
	for i := range tokenCacheLimit + 10 {
		u.store(string(rune('a'+i%26))+string(rune(i)), "tok")
	}
	if len(u.tokens) > tokenCacheLimit {
		t.Fatalf("записей %d, потолок %d", len(u.tokens), tokenCacheLimit)
	}
}
