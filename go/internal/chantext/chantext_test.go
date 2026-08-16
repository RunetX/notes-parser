package chantext

import (
	"strings"
	"testing"
)

func TestVisibleLen(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"просто текст", 12},
		{"<b>жирный</b>", 6},
		{`<a href="https://очень/длинный/url">яд</a>`, 2},
		{"&lt;тег&gt;", 5},
	}
	for _, tc := range cases {
		if got := VisibleLen(tc.text); got != tc.want {
			t.Errorf("VisibleLen(%q) = %d, ожидалось %d", tc.text, got, tc.want)
		}
	}
}

func TestValidateHTML(t *testing.T) {
	ok := []string{
		"просто текст",
		"<b>жирный</b> и <i>наклонный</i>",
		`ссылка <a href="https://love.ngs.ru/notes/1/">заметка</a>`,
		"экранированное &lt;не тег&gt;",
		"",
	}
	for _, s := range ok {
		if err := ValidateHTML(s); err != nil {
			t.Errorf("ValidateHTML(%q) = %v, ожидалось без ошибки", s, err)
		}
	}

	bad := []struct {
		text string
		want string
	}{
		{"<div>чужой тег</div>", "не поддерживается"},
		{"<b>незакрытый", "незакрытый тег"},
		{"перекрытые <b><i>теги</b></i>", "непарный тег"},
		{"закрыт без открытия</b>", "непарный тег"},
		{"голая скобка 5 < 7", "экранировать"},
		{`<a href="javascript:alert(1)">яд</a>`, "только http"},
		{`<a href="data:text/html;base64,PHNjcmlwdD4=">яд</a>`, "только http"},
		{`<a href="/notes/1/">относительная</a>`, "только http"},
		{`<a href="https://x/" onclick="steal()">лишний атрибут</a>`, "ровно один атрибут"},
		{`<a>без href</a>`, "ровно один атрибут"},
		{`<b class="x">атрибут у b</b>`, "не должно быть атрибутов"},
	}
	for _, tc := range bad {
		err := ValidateHTML(tc.text)
		if err == nil {
			t.Errorf("ValidateHTML(%q) прошёл, ожидалась ошибка", tc.text)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidateHTML(%q) = %v, ожидалось про %q", tc.text, err, tc.want)
		}
	}
}

func TestTruncateClosesOpenTags(t *testing.T) {
	got := Truncate("<b>"+strings.Repeat("а", 50)+"</b>", 10)
	if got != "<b>"+strings.Repeat("а", 10)+"…</b>" {
		t.Errorf("обрезка внутри тега: %q", got)
	}
	got = Truncate("даже &lt;без&gt; тегов", 6)
	if got != "даже &lt;…" {
		t.Errorf("сущность — одна руна: %q", got)
	}
}
