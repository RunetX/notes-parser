package love

import (
	"strings"
	"testing"
	"time"
)

// Кусок настоящей страницы /guests/ (13.08.2026), сокращённый до трёх записей.
const guestsHTML = `<ul class="lv-people__list">
<li class="lv-people__item"><a href="/profile/1450213/" class="lv-user-info__decorated-wrap_newmain _real-user"><span class="lv-people__link lv-people__link_male" data-userid="1450213"><img src="/static/i/new/profile/male300px.png" class="lv-people__photo">
<div class="lv-people__info">
<span class="lv-user-info__nick-wrap_main "><span class="lv-user-info__nick_main" title="ЗАЯ в трениках">ЗАЯ в трениках&nbsp;</span></span><span class="lv-people__info-age">47&nbsp;лет</span><div class="lv-people__time">Был: вчера&nbsp;в 23:53</div></div></span></a></li>
<li class="lv-people__item"><a href="/profile/175869/" class="lv-user-info__decorated-wrap_newmain _real-user"><span class="lv-people__link lv-people__link_male" data-userid="175869"><img src="https://n1s1.hsmedia.ru/preview/love/avatars/x.jpg" class="lv-people__photo">
<div class="lv-people__info">
<span class="lv-user-info__nick-wrap_main "><span class="lv-user-info__nick_main" title="Гадёныш">Гадёныш&nbsp;</span></span><span class="lv-people__info-age">42&nbsp;года</span><div class="lv-people__time">Был: вчера&nbsp;в 19:38</div></div></span></a></li>
<li class="lv-people__item"><a href="/profile/1439395/" class="lv-user-info__decorated-wrap_newmain _real-user"><span class="lv-people__link lv-people__link_female" data-userid="1439395"><img src="/static/i/f.png" class="lv-people__photo">
<div class="lv-people__info">
<span class="lv-user-info__nick-wrap_main "><span class="lv-user-info__nick_main" title="Axeinos">Axeinos&nbsp;</span></span><span class="lv-people__info-age">39&nbsp;лет</span><div class="lv-people__time">Была: 7 августа&nbsp;в 20:38</div></div></span></a></li>
</ul>`

func TestParseGuests(t *testing.T) {
	now := time.Date(2026, 8, 13, 3, 20, 0, 0, nsk)
	guests, err := ParseGuests(strings.NewReader(guestsHTML), now)
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if len(guests) != 3 {
		t.Fatalf("гостей %d, ожидалось 3", len(guests))
	}
	want := []struct {
		id   int64
		nick string
		at   time.Time
	}{
		{1450213, "ЗАЯ в трениках", time.Date(2026, 8, 12, 23, 53, 0, 0, nsk)},
		{175869, "Гадёныш", time.Date(2026, 8, 12, 19, 38, 0, 0, nsk)},
		{1439395, "Axeinos", time.Date(2026, 8, 7, 20, 38, 0, 0, nsk)},
	}
	for i, w := range want {
		g := guests[i]
		if g.ID != w.id || g.Nick != w.nick {
			t.Errorf("#%d: получено u%d %q, ожидалось u%d %q", i, g.ID, g.Nick, w.id, w.nick)
		}
		if !g.VisitedAt.Equal(w.at) {
			t.Errorf("#%d (%s): время визита %v, ожидалось %v", i, w.nick, g.VisitedAt, w.at)
		}
	}
}

// Пустой список — законная ситуация: в анкету могли не заходить.
func TestParseGuestsEmpty(t *testing.T) {
	guests, err := ParseGuests(strings.NewReader(`<ul class="lv-people__list"></ul>`), time.Now())
	if err != nil || len(guests) != 0 {
		t.Fatalf("пустой список должен разбираться без ошибки: %d, %v", len(guests), err)
	}
}

// Незнакомый формат времени не должен ронять разбор: запись нужна и без даты,
// а исходная строка сохраняется, чтобы дрейф было видно в логе.
func TestParseGuestsUnknownTimeKeepsRow(t *testing.T) {
	html := strings.Replace(guestsHTML, "Был: вчера&nbsp;в 19:38", "только что", 1)
	guests, err := ParseGuests(strings.NewReader(html), time.Now())
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if len(guests) != 3 {
		t.Fatalf("гостей %d, ожидалось 3", len(guests))
	}
	if !guests[1].VisitedAt.IsZero() || guests[1].Raw != "только что" {
		t.Fatalf("нераспознанное время потеряно: %+v", guests[1])
	}
}

// Переход через Новый год: декабрьская запись в январе — это прошлый год.
func TestParseVisitTimeAcrossNewYear(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, nsk)
	got := parseVisitTime("Была: 28 декабря в 21:15", now)
	want := time.Date(2025, 12, 28, 21, 15, 0, 0, nsk)
	if !got.Equal(want) {
		t.Fatalf("получено %v, ожидалось %v", got, want)
	}
}
