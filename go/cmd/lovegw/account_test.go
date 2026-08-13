package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lovegw/internal/acct"
	"lovegw/internal/config"
	"lovegw/internal/love"
	"lovegw/internal/store"
)

// testConfig кладёт минимальный конфиг во временный каталог: боевая БД и
// accounts.db окажутся там же, рядом друг с другом.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"db_path": ` + quote(filepath.ToSlash(filepath.Join(dir, "lovegw.db"))) + `,
		"admin_tg_user_id": 42, "log_level": "error"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func quote(s string) string { return `"` + s + `"` }

func testCookiesJSON(t *testing.T, name string) string {
	t.Helper()
	js, err := love.CookiesToJSON([]*http.Cookie{{Name: name, Value: "v"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return js
}

func putAccount(t *testing.T, cfg *config.Config, name string, valid bool) {
	t.Helper()
	ctx := context.Background()
	db, err := openAccounts(ctx, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Put(ctx, acct.Account{Name: name, Nick: "Ник" + name}, testCookiesJSON(t, name), time.Now()); err != nil {
		t.Fatal(err)
	}
	if !valid {
		if err := db.SetValid(ctx, name, false, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

// Резерв: первый живой аккаунт из списка и есть тот, под кем идём.
func TestSiteCookiesPicksFirstValid(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	putAccount(t, cfg, "dead", false)
	putAccount(t, cfg, "reserve", true)

	cookies, who, err := siteCookies(ctx, cfg, "dead,reserve", store.MessengerTelegram, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(cookies) != 1 || cookies[0].Name != "reserve" {
		t.Fatalf("взяты куки %+v", cookies)
	}
	if !strings.Contains(who, "reserve") {
		t.Fatalf("кто = %q, ожидался reserve", who)
	}
}

func TestSiteCookiesAllUnusable(t *testing.T) {
	cfg := testConfig(t)
	putAccount(t, cfg, "dead", false)

	_, _, err := siteCookies(context.Background(), cfg, "dead,нетакого", store.MessengerTelegram, 0, false)
	if err == nil {
		t.Fatal("ожидалась ошибка: годных аккаунтов нет")
	}
	if !strings.Contains(err.Error(), "невалидной") || !strings.Contains(err.Error(), "нет такого аккаунта") {
		t.Fatalf("ошибка не объясняет причины: %v", err)
	}
}

// Без -account поведение прежнее: сессия админа из боевой БД.
func TestSiteCookiesFallsBackToAdminSession(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)

	st, err := openStore(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSession(ctx, store.MessengerTelegram, 42, testCookiesJSON(t, "admin"), time.Now()); err != nil {
		t.Fatal(err)
	}
	st.Close()

	cookies, who, err := siteCookies(ctx, cfg, "", store.MessengerTelegram, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(cookies) != 1 || cookies[0].Name != "admin" {
		t.Fatalf("взяты куки %+v", cookies)
	}
	if !strings.Contains(who, "боевой") {
		t.Fatalf("кто = %q", who)
	}
}

// Обращение ставится по адресату, но не задваивается, если человек написал его
// сам, — то же правило, что у моста.
func TestWithAddressPrefix(t *testing.T) {
	page := love.CommentsPage{Comments: []love.Comment{{ID: 7, AuthorName: "Ягода"}}}
	if got := withAddressPrefix(page, 7, "привет"); got != "Ягода, привет" {
		t.Fatalf("получено %q", got)
	}
	if got := withAddressPrefix(page, 7, "Ягода, привет"); got != "Ягода, привет" {
		t.Fatalf("обращение задвоилось: %q", got)
	}
	if got := withAddressPrefix(page, 0, "привет"); got != "привет" {
		t.Fatalf("в корень заметки обращение не нужно: %q", got)
	}
	if got := withAddressPrefix(page, 999, "привет"); got != "привет" {
		t.Fatalf("неизвестный адресат: %q", got)
	}
}
