package web

// Обращение показывается один раз — и решает это свидетельство треда.
//
// Поломка, ради которой всё заведено: у переименовавшегося человека приём не
// узнал обращение в теле (сверял с нынешним ником), и страница выдала
// «Паноптикум, Рантье, привычное…» — один человек старым ником и новым.
//
// Первая попытка чинить это формой строки («до первой запятой — значит
// обращение») была откачена в тот же час: на живой странице она приняла за ники
// 62 фразы из 227 — «Вы не представляете, как я люблю холодок», «пока писала,
// вспоминала подробности». Поэтому здесь ровно два рода проверок: ник
// узнаётся по ПОВТОРУ, а фразы из того случая записаны поимённо и обязаны
// оставаться текстом.

import (
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

const (
	oldNick = "Рантье"     // как человека звали в 2026-м
	newNick = "Паноптикум" // как его зовут сейчас
)

// thread — тред, где к одному и тому же человеку обращаются СТАРЫМ ником. Его
// собственная реплика (id 1) есть на странице, поэтому адресат разрешается.
func thread(bodies ...string) (platform.NoteView, []platform.CommentView) {
	at := time.Date(2026, 7, 18, 6, 0, 0, 0, time.UTC)
	note := platform.NoteView{ID: 312853, Author: platform.Author{ID: 999, Nick: "Анюта"}}
	out := []platform.CommentView{{
		ID: 1, Author: platform.Author{ID: 1472546, Nick: newNick},
		Body: "исходная реплика", PublishedAt: at,
	}}
	for i, b := range bodies {
		out = append(out, platform.CommentView{
			ID: int64(100 + i),
			// Авторы РАЗНЫЕ: ник узнаётся по тому, что им зовут разные люди, а
			// повтор от одного и того же — это его привычка говорить «да, ...».
			Author:      platform.Author{ID: int64(500 + i), Nick: "Betty Boop"},
			Body:        b,
			ReplyTo:     &platform.ReplyRef{CommentID: 1, Nick: newNick},
			PublishedAt: at,
		})
	}
	return note, out
}

// render — тело реплики так, как его увидит читатель страницы.
func render(t *testing.T, note platform.NoteView, cs []platform.CommentView, i int) string {
	t.Helper()
	return string(commentBodyHTML(newAddressBook(note, cs), cs[i]))
}

// Старый ник узнаётся по повтору: им обращаются к одному человеку дважды, и
// второе обращение поверх этого уже не рисуется.
func TestRepeatedOldNickIsRecognised(t *testing.T) {
	note, cs := thread(oldNick+", привычное, далеко не всегда хорошее.", oldNick+", оне не понимают")

	for i := 1; i <= 2; i++ {
		got := render(t, note, cs, i)
		if strings.Contains(got, newNick) {
			t.Errorf("реплика %d: обращение дорисовано поверх написанного: %s", i, got)
		}
		if !strings.Contains(got, `<b class="to">`+oldNick+`</b>, `) {
			t.Errorf("реплика %d: обращение автора не выделено жирным: %s", i, got)
		}
	}
	if got := render(t, note, cs, 1); !strings.Contains(got, "привычное, далеко не всегда хорошее.") {
		t.Errorf("текст потерялся: %s", got)
	}
}

// Фразы из того самого случая. Каждая начинается «словами до запятой», и ни
// одна не должна стать ником: настоящий адресат обязан остаться на месте.
func TestPhrasesAreNotAddresses(t *testing.T) {
	phrases := []string{
		"Вы не представляете, как я люблю холодок",
		"пока писала, вспоминала подробности - посмеялась.",
		"а почему ты считаешь, что это игра?",
		"Любишь медок, люби и холодок (с)",
		"размечталась, ага",
		"такие вещи надо знать и понимать, а не гадать",
	}
	note, cs := thread(phrases...)

	for i := range phrases {
		got := render(t, note, cs, i+1)
		if !strings.Contains(got, `<a class="to" href="#c1">`+newNick+`</a>, `) {
			t.Errorf("%q: пропал настоящий адресат: %s", phrases[i], got)
		}
		if strings.Contains(got, `<b class="to">`) {
			t.Errorf("%q: фраза принята за ник: %s", phrases[i], got)
		}
	}
}

// Ник участника этой же заметки — заведомо ник, а не фраза: повтора для него не
// требуется.
func TestParticipantNickIsAnAddress(t *testing.T) {
	note, cs := thread("Анюта, адекватный чему?")
	cs[1].ReplyTo = &platform.ReplyRef{CommentID: 1, Nick: newNick}

	got := render(t, note, cs, 1)
	if !strings.Contains(got, `<b class="to">Анюта</b>, `) {
		t.Errorf("ник автора заметки не узнан: %s", got)
	}
}

// Известное ограничение, записанное честно: одиночное обращение старым ником в
// коротком треде свидетельства не даёт, и обращение выйдет дважды. Это дешевле
// ошибки в другую сторону — порезанного чужого текста.
func TestSingleOldNickStaysDoubled(t *testing.T) {
	note, cs := thread(oldNick + ", привычное")

	got := render(t, note, cs, 1)
	if !strings.Contains(got, newNick) || !strings.Contains(got, oldNick) {
		t.Errorf("ожидалось, что одиночный случай останется как был: %s", got)
	}
}

// Ник с угловой скобкой приезжает в страницу экранированным, как и любой чужой
// текст.
func TestAddressFromBodyIsEscaped(t *testing.T) {
	note, cs := thread(`<b>Ник</b>, текст`, `<b>Ник</b>, ещё текст`)

	got := render(t, note, cs, 1)
	if strings.Contains(got, "<b>Ник</b>,") {
		t.Errorf("разметка из тела уехала в страницу как есть: %s", got)
	}
	if !strings.Contains(got, "&lt;b&gt;Ник&lt;/b&gt;") {
		t.Errorf("ник не экранирован: %s", got)
	}
}

// Разбор формы. Он ещё не отвечает «ник ли это» — только «похоже ли на
// обращение по форме», — и здесь проверяются его границы.
func TestLeadingAddressBoundaries(t *testing.T) {
	cases := []struct {
		name string
		body string
		nick string
		rest string
		ok   bool
	}{
		{"обычное обращение", "Анюта, адекватный чему?", "Анюта", "адекватный чему?", true},
		{"ник из двух слов", "Инженер Шурик 54, оне не понимают", "Инженер Шурик 54", "оне не понимают", true},
		{"ник без пробела после запятой", "Пух,согласна", "Пух", "согласна", true},
		{"фраза с точкой", "Ну что ж. Ладно, поехали", "", "", false},
		{"перенос строки до запятой", "Ник\nещё строка, текст", "", "", false},
		{"слишком длинно", strings.Repeat("а", 41) + ", текст", "", "", false},
		// Реплика из одного обращения — рабочий случай, а не брак формы
		// (правило изменено 23.08.2026, см. leadingAddress): пустым остаётся
		// тело, а не показ, потому что само обращение рисуется из ребра.
		{"после запятой пусто", "Ник,", "Ник", "", true},
		{"запятая первой", ", текст", "", "", false},
		{"запятой нет вовсе", "просто текст", "", "", false},
		{"пробел перед запятой", "Ник , текст", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nick, rest, ok := leadingAddress(c.body)
			if ok != c.ok {
				t.Fatalf("разобрано как обращение: %v, ожидалось %v", ok, c.ok)
			}
			if ok && nick != c.nick {
				t.Errorf("ник %q, ожидался %q", nick, c.nick)
			}
			if ok && rest != c.rest {
				t.Errorf("остаток %q, ожидался %q", rest, c.rest)
			}
		})
	}
}

// Свидетельство находится и тогда, когда реплики адресата на странице нет:
// её мог скрыть модератор или она осталась на другой странице линейного вида.
// Ключ — нынешний ник адресата, а он у ребра есть всегда.
func TestEvidenceWorksWithoutTargetOnPage(t *testing.T) {
	note, cs := thread(oldNick+", привычное", oldNick+", ещё раз")
	cs = cs[1:] // реплики адресата на странице нет

	got := string(commentBodyHTML(newAddressBook(note, cs), cs[0]))
	if !strings.Contains(got, `<b class="to">`+oldNick+`</b>, `) {
		t.Errorf("свидетельство не найдено без реплики адресата: %s", got)
	}
}

// Зачин, повторённый разными людьми, ником не становится: «да, ...» и «ну, ...»
// в разговоре повторяются сами по себе. Список — вето поверх свидетельства.
func TestOpenersNeverBecomeNicks(t *testing.T) {
	note, cs := thread("да, согласна", "да, и не говори", "ну, бывает", "ну, что тут скажешь")

	for i := 1; i <= 4; i++ {
		got := render(t, note, cs, i)
		if strings.Contains(got, `<b class="to">`) {
			t.Errorf("реплика %d: зачин принят за ник: %s", i, got)
		}
		if !strings.Contains(got, `<a class="to"`) {
			t.Errorf("реплика %d: пропал настоящий адресат: %s", i, got)
		}
	}
}

// Повтор ОДНИМ автором свидетельством не считается — иначе привычка одного
// человека начинать с «блин,» превратилась бы в чужой ник.
func TestRepeatByOneAuthorIsNotEvidence(t *testing.T) {
	note, cs := thread("Мурзик, привет", "Мурзик, ну как ты")
	cs[1].Author = platform.Author{ID: 777, Nick: "Betty Boop"}
	cs[2].Author = platform.Author{ID: 777, Nick: "Betty Boop"}

	if got := render(t, note, cs, 1); strings.Contains(got, `<b class="to">`) {
		t.Errorf("повтор одним автором принят за свидетельство: %s", got)
	}
}

// Реплика без ребра не меняется вовсе: дорисовывать там нечего, и трогать текст
// показ не вправе.
func TestBodyWithoutEdgeIsUntouched(t *testing.T) {
	note, cs := thread(oldNick+", привычное", oldNick+", ещё")
	cs[1].ReplyTo = nil

	got := render(t, note, cs, 1)
	if strings.Contains(got, `class="to"`) {
		t.Errorf("у реплики без ребра появилось обращение: %s", got)
	}
	if !strings.Contains(got, oldNick+", привычное") {
		t.Errorf("текст изменён: %s", got)
	}
}

// И то же самое на живой странице: книга обращений строится в обработчике, а не
// в тесте, — иначе проверялось бы не то, что видит читатель.
func TestNotePageShowsOneAddress(t *testing.T) {
	note, cs := thread(oldNick+", привычное", oldNick+", оне не понимают")
	st := &fakeStore{note: note, thread: cs}
	body := do(openServer(t, st), guest(t, "GET", "/n/312853")).Body.String()

	if n := strings.Count(body, `<b class="to">`+oldNick+`</b>`); n != 2 {
		t.Errorf("обращений старым ником на странице %d, ожидалось 2", n)
	}
	if strings.Contains(body, `<a class="to" href="#c1">`+newNick+`</a>, `+oldNick) {
		t.Error("на странице осталось двойное обращение")
	}
}

// Жалоба владельца 23.08.2026: ответ показал ник ДВАЖДЫ — «Анна, Анна,».
//
// Так выглядит реплика, от которой в теле осталось одно обращение: показ рисовал
// своё обращение из ребра, а уцелевшее в теле «Ник,» печатал следом, потому что
// разбор формы считал пустой остаток браком. Причина, по которой тело оказалось
// таким, — потерянные эмодзи (love.bodyText), и починены оба конца: этот отвечает
// за то, чтобы ник не двоился НИКОГДА, включая строки, которые уже лежат в базе.
func TestAddressOnlyBodyShowsNickOnce(t *testing.T) {
	note, cs := thread(newNick + ",")

	got := render(t, note, cs, 1)
	if n := strings.Count(got, newNick); n != 1 {
		t.Fatalf("ник показан %d раз(а): %s", n, got)
	}
	if !strings.Contains(got, `<b class="to">`+newNick+`</b>, `) {
		t.Errorf("обращение автора не выделено: %s", got)
	}
}

// Та же реплика, но обращение — НЕ ник: «Кстати,» и больше ничего. Слово обязано
// остаться текстом, а адресат — дорисоваться из ребра, иначе пустой остаток стал
// бы способом съесть первое слово чужой реплики.
func TestAddressOnlyBodyKeepsOpenerAsText(t *testing.T) {
	note, cs := thread("Кстати,")

	got := render(t, note, cs, 1)
	if !strings.Contains(got, "Кстати,") {
		t.Errorf("слово съедено: %s", got)
	}
	if !strings.Contains(got, `<a class="to" href="#c1">`+newNick+`</a>, `) {
		t.Errorf("настоящий адресат пропал: %s", got)
	}
}
