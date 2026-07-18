package love

import (
	"strconv"
	"testing"
	"time"
)

func TestCookiesFromJSONFiltersExpired(t *testing.T) {
	now := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	data := []byte(`[
		{"name":"live","value":"1","expires":` + itoa(now.Add(time.Hour).Unix()) + `},
		{"name":"dead","value":"2","expires":` + itoa(now.Add(-time.Hour).Unix()) + `},
		{"name":"session","value":"3","expires":0}
	]`)
	cookies, err := CookiesFromJSON(data, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(cookies) != 2 {
		t.Fatalf("ожидалось 2 живых куки, получено %d", len(cookies))
	}
	names := map[string]bool{}
	for _, c := range cookies {
		names[c.Name] = true
	}
	if !names["live"] || !names["session"] || names["dead"] {
		t.Errorf("отфильтровано неверно: %v", names)
	}
}

func TestCookiesFromJSONAllExpiredIsEmpty(t *testing.T) {
	now := time.Now()
	data := []byte(`[{"name":"x","value":"1","expires":` + itoa(now.Add(-time.Hour).Unix()) + `}]`)
	cookies, err := CookiesFromJSON(data, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(cookies) != 0 {
		t.Errorf("все куки протухли, ожидался пустой список, получено %d", len(cookies))
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
