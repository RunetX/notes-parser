package chantext

import "testing"

// Каждый случай назван тем, что он утверждает про текст площадки в канале.
func TestFromSiteMarkup(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"жирный и курсив показываются", "[b]жир[/b] и [i]курсив[/i]",
			"<b>жир</b> и <i>курсив</i>"},
		{"незакрытый знак закрывается сам", "[b]хвост", "<b>хвост</b>"},
		{"закрывающий без открывающего пропадает", "текст[/b]", "текст"},
		{"перекрёстные не дают пустой пары", "[b][i]x[/b][/i]", "<b><i>x</i></b>"},
		{"перекрёстные возвращаются к тексту", "[b][i]x[/b]y[/i]", "<b><i>x</i></b><i>y</i>"},
		{"подчёркивание и зачёркивание снимаются", "[u]раз[/u] [s]два[/s]", "раз два"},
		{"цвет снимается, слова остаются", "[color=red]алое[/color]", "алое"},
		{"цвет не закрывает жирный", "[b]a[color=red]b[/color]c[/b]", "<b>abc</b>"},
		{"чужой HTML экранируется", "<b>чужое</b> & прочее",
			"&lt;b&gt;чужое&lt;/b&gt; &amp; прочее"},
		{"смайлы остаются кодами", "держи :::popcorn::: вот", "держи :::popcorn::: вот"},
		{"незнакомый знак остаётся текстом", "[q]цитата[/q]", "[q]цитата[/q]"},
		{"регистр знака не важен", "[B]жир[/B]", "<b>жир</b>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FromSiteMarkup(c.in)
			if got != c.want {
				t.Errorf("FromSiteMarkup(%q) = %q, ожидалось %q", c.in, got, c.want)
			}
		})
	}
}

// Непарный тег Telegram не принимает вовсе — сообщение отвергается, а очередь
// приёмника встаёт. Поэтому разбор обязан отдавать текст, который проходит ту
// же проверку, что и всё остальное, уходящее в канал.
func TestFromSiteMarkupAlwaysValid(t *testing.T) {
	texts := []string{
		"[b]", "[/b]", "[b][b][b]x", "[i][b]x[/i]y[/b]z",
		"[b][i][u][s][color=red][b][i][u][s]перебор глубины[/b]",
		"[color=нечто]x[/color]", "[b]<script>alert(1)</script>[/b]",
		"[b]a[/i]b[/b]", "текст без разметки",
	}
	for _, s := range texts {
		if err := ValidateHTML(FromSiteMarkup(s)); err != nil {
			t.Errorf("FromSiteMarkup(%q) = %q: %v", s, FromSiteMarkup(s), err)
		}
	}
}

// Глубже потолка теги не открываются, но текст не теряется.
func TestFromSiteMarkupDepthLimit(t *testing.T) {
	in := ""
	for i := 0; i < siteMaxDepth+3; i++ {
		in += "[b]"
	}
	in += "дно"
	got := FromSiteMarkup(in)
	if err := ValidateHTML(got); err != nil {
		t.Fatalf("невалидный HTML %q: %v", got, err)
	}
	if VisibleLen(got) != len([]rune("дно")) {
		t.Errorf("текст потерян: %q", got)
	}
}

// UTF16Len меряет то же, чем меряют сообщение сами мессенджеры.
func TestUTF16Len(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"abc", 3},
		{"ёж", 2},
		{"🙂", 2}, // вне BMP — суррогатная пара
		{"a🙂b", 4},
	}
	for _, c := range cases {
		if got := UTF16Len(c.in); got != c.want {
			t.Errorf("UTF16Len(%q) = %d, ожидалось %d", c.in, got, c.want)
		}
	}
}

// Выпуск дайджеста собран в подмножестве каналов, а публикуется заметкой — то
// есть плоским текстом со знаками НГС. Каждый случай называет, что при этом
// обязано уцелеть.
func TestToSiteMarkup(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"начертания становятся знаками", "<b>Спор</b> и <i>цитата</i>",
			"[b]Спор[/b] и [i]цитата[/i]"},
		{"у ссылки остаются и подпись, и адрес",
			`о <a href="https://t3h.ru/n/312811">заметке недели</a> все`,
			"о заметке недели — https://t3h.ru/n/312811 все"},
		{"адрес сам себе подпись не удваивается",
			`<a href="https://t3h.ru/n/1">https://t3h.ru/n/1</a>`,
			"https://t3h.ru/n/1"},
		{"сущности возвращаются знаками", "Маша &amp; Медведь &lt;3",
			"Маша & Медведь <3"},
		{"размеченная подпись ссылки не теряется",
			`<a href="https://t3h.ru/n/7"><b>жирная</b> подпись</a>`,
			"[b]жирная[/b] подпись — https://t3h.ru/n/7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ToSiteMarkup(c.in); got != c.want {
				t.Errorf("ToSiteMarkup(%q) = %q, ожидалось %q", c.in, got, c.want)
			}
		})
	}
}

// Круг замыкается: то, что площадка показывает знаками, канал показывает
// разметкой — и обратно. Проверяется на выпуске с обоими началами.
func TestSiteMarkupRoundTrip(t *testing.T) {
	const plain = "[b]Заметка недели[/b]\nвот [i]так[/i]"
	if got := ToSiteMarkup(FromSiteMarkup(plain)); got != plain {
		t.Errorf("круг не замкнулся: %q", got)
	}
}
