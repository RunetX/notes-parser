package web

// Песочница (эпик «народ») глазами читателя: значок, отсутствие формы и
// объяснение вместо неё.

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// stageStore — та же заметка, но помеченная песочницей.
func stageStore() *fakeStore {
	n := sampleNote()
	n.ID = platform.NativeIDBase + 5
	n.Stage = true
	return &fakeStore{total: 1, notes: []platform.NoteView{n}, note: n, thread: sampleThread()}
}

func stagePaths(st *fakeStore) []string {
	return []string{"/", "/n/" + itoa64(st.note.ID)}
}

// Читать песочницу может кто угодно: она в ленте, у неё своя страница и свой
// значок. Закрыта в ней только запись.
func TestStageIsReadableByEveryone(t *testing.T) {
	st := stageStore()
	h := newTestServer(t, st, Config{})
	for _, path := range stagePaths(st) {
		w := do(h, guest(t, "GET", path))
		if w.Code != 200 {
			t.Fatalf("%s: код %d", path, w.Code)
		}
		if !strings.Contains(w.Body.String(), `class="orig"`) {
			t.Errorf("%s: у песочницы нет метки происхождения", path)
		}
	}
}

// Формы ответа в песочнице нет ни у гостя, ни у участника — и на её месте стоит
// ОБЪЯСНЕНИЕ. Молчащее место под тредом читается поломкой, а гостю «войдите,
// чтобы ответить» было бы неправдой: войдя, он всё равно не сможет.
func TestStageExplainsWhyThereIsNoForm(t *testing.T) {
	st := stageStore()
	path := "/n/" + itoa64(st.note.ID)

	h := newTestServer(t, st, Config{})
	body := do(h, guest(t, "GET", path)).Body.String()
	if strings.Contains(body, `class="wform"`) {
		t.Error("гостю в песочнице показали форму ответа")
	}
	if !strings.Contains(body, "жители") || !strings.Contains(body, "/help#narod") {
		t.Errorf("гостю не объяснили, почему формы нет:\n%s", tailOf(body))
	}

	hm, _, token := writeServer(t, st)
	member := do(hm, as(guest(t, "GET", path), token)).Body.String()
	if strings.Contains(member, `class="wform"`) {
		t.Error("участнику в песочнице показали форму ответа")
	}
	if !strings.Contains(member, "/help#narod") {
		t.Error("участнику не объяснили, почему формы нет")
	}
}

// В ленте у песочницы нет и ссылки «Добавить комментарий»: она вела бы к
// странице, на которой ответить нельзя.
func TestStageHasNoAddCommentInFeed(t *testing.T) {
	h, _, token := writeServer(t, stageStore())
	feed := do(h, as(guest(t, "GET", "/"), token)).Body.String()
	if strings.Contains(feed, "Добавить комментарий") {
		t.Error("в ленте у песочницы стоит «Добавить комментарий»")
	}
}

// А обычная заметка от этого не пострадала: у участника форма на месте.
func TestOrdinaryNoteStillHasTheForm(t *testing.T) {
	h, _, token := writeServer(t, noteStore())
	body := do(h, as(guest(t, "GET", "/n/312811"), token)).Body.String()
	if !strings.Contains(body, `action="/n/312811/reply"`) {
		t.Error("у обычной заметки пропала форма ответа")
	}
	if strings.Contains(body, "/help#narod") {
		t.Error("обычная заметка объявила себя песочницей")
	}
}

// ------------------------------------------------- отдать жителям готовую заметку

// silentMirrorNote — ЗЕРКАЛЬНАЯ заметка без единой реплики: старая запись с
// НГС, под которой разговора не случилось. Ради таких песочница и правится на
// форме (владелец 30.08.2026: «хочу со временем заполнить старые заметки и те,
// что удаляют без дискуссии»).
func silentMirrorNote() *fakeStore {
	st := noteStore()
	st.note.ID = 312811 // полоса НГС: id строки равен номеру заметки на сайте
	st.note.Own = false
	st.note.CommentCount = 0
	st.note.PublishedAt = time.Now().Add(-72 * time.Hour)
	return st
}

// Пустой заметке песочницу предлагают, заметке с разговором — нет: правило
// держит ядро, а форма не показывает кнопку, которая ответит отказом.
func TestПесочницуПредлагаютТолькоМолчащейЗаметке(t *testing.T) {
	for _, c := range []struct {
		name string
		st   *fakeStore
		want bool
	}{
		{"молчащая зеркальная", silentMirrorNote(), true},
		{"с разговором", foreignNativeNote(), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, token := modServerOn(t, c.st, platform.RoleAdmin)
			path := "/n/" + itoa64(c.st.note.ID) + "/edit"
			body := do(h, as(guest(t, "GET", path), token)).Body.String()
			if got := strings.Contains(body, `name="stage"`); got != c.want {
				t.Errorf("галочка песочницы: %v, ожидалось %v", got, c.want)
			}
		})
	}
}

// Модератор решает про СЛОВА: кто в заметке вправе говорить — не его вопрос, и
// формы правки ему не существует вовсе.
func TestПесочницаНаПравкеЗакрытаОтМодератора(t *testing.T) {
	st := silentMirrorNote()
	h, _, token := modServerOn(t, st, platform.RoleModerator)
	w := do(h, as(guest(t, "GET", "/n/"+itoa64(st.note.ID)+"/edit"), token))
	if w.Code == http.StatusOK && strings.Contains(w.Body.String(), `name="stage"`) {
		t.Error("модератору предложили перевести заметку в песочницу")
	}
}

// Выбор доходит до ядра — и доходит вместе с причиной, которая уйдёт в журнал.
// Снятие тоже: правило симметрично, пока в заметке не говорили.
func TestПесочницаСФормыПравкиДоходитДоЯдра(t *testing.T) {
	st := silentMirrorNote()
	h, mod, token := modServerOn(t, st, platform.RoleAdmin)
	path := "/n/" + itoa64(st.note.ID) + "/edit"

	w := do(h, postAs(t, path, url.Values{
		"stage": {"1"}, "reason": {"сцена для жителей"},
	}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d", w.Code)
	}
	if !mod.stageSet || mod.stageNote != st.note.ID || !mod.stageOn {
		t.Fatalf("до ядра дошло %d/%v (звали=%v)", mod.stageNote, mod.stageOn, mod.stageSet)
	}
	if mod.stageWhy != "сцена для жителей" {
		t.Errorf("причина не дошла: %q", mod.stageWhy)
	}

	// Уже песочница — и галочку сняли: ядро зовут снять признак.
	st2 := silentMirrorNote()
	st2.note.Stage = true
	h2, mod2, token2 := modServerOn(t, st2, platform.RoleAdmin)
	if w := do(h2, postAs(t, path, url.Values{}, token2)); w.Code != http.StatusSeeOther {
		t.Fatalf("код %d", w.Code)
	}
	if !mod2.stageSet || mod2.stageOn {
		t.Errorf("снятие не дошло: звали=%v, значение=%v", mod2.stageSet, mod2.stageOn)
	}
}

// Ничего не менялось — ядро не зовут вовсе: администратор мог открыть форму
// ради картинки и не трогать признак.
func TestНеТронутуюПесочницуЯдруНеШлют(t *testing.T) {
	st := silentMirrorNote()
	h, mod, token := modServerOn(t, st, platform.RoleAdmin)

	if w := do(h, postAs(t, "/n/"+itoa64(st.note.ID)+"/edit", url.Values{}, token)); w.Code != http.StatusSeeOther {
		t.Fatalf("код %d", w.Code)
	}
	if mod.stageSet {
		t.Error("ядро позвали, хотя признак не менялся")
	}
}

// Отказ «в заметке уже говорили» объясняется словами и правилом, а не
// пятисоткой: администратор должен понять, что дело не в правах, а во времени.
func TestОтказПесочницыОбъясняется(t *testing.T) {
	st := silentMirrorNote()
	h, mod, token := modServerOn(t, st, platform.RoleAdmin)
	mod.stageErr = platform.ErrStageHasThread

	w := do(h, postAs(t, "/n/"+itoa64(st.note.ID)+"/edit", url.Values{"stage": {"1"}}, token))
	if w.Code != http.StatusForbidden {
		t.Fatalf("код %d, ожидался честный отказ", w.Code)
	}
	if !strings.Contains(w.Body.String(), "уже есть реплики") {
		t.Errorf("отказ не объяснён:\n%s", tailOf(w.Body.String()))
	}
}

// ------------------------------------------------------- завести песочницу

// stageWriteServer — вошедший с заданной ролью и живой Writer. Модератора здесь
// нет вовсе, и это часть проверки: песочница — вопрос о том, кто здесь ГОВОРИТ,
// а не о словах, поэтому показ галочки от подключённой очереди не зависит.
func stageWriteServer(t *testing.T, role platform.Role) (http.Handler, *fakeWriter, string) {
	t.Helper()
	auth, token := signedInAs(t, platform.User{
		ID: testProfileID, Nick: testNick, Kind: platform.KindMember, Role: role,
	})
	wr := &fakeWriter{}
	return newFullServer(t, noteStore(), auth, wr, nil, nil, Config{}), wr, token
}

// Галочку видит только администратор. Заводить песочницу вправе он и сам житель
// (platform.stageGuard), но житель формой не пользуется вовсе — значит на
// экране это дверь ровно для одного.
func TestПесочницуПредлагаютТолькоАдминистратору(t *testing.T) {
	for _, c := range []struct {
		name string
		role platform.Role
		want bool
	}{
		{"участник", platform.RoleUser, false},
		{"модератор", platform.RoleModerator, false},
		{"администратор", platform.RoleAdmin, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, token := stageWriteServer(t, c.role)
			body := do(h, as(guest(t, "GET", "/new"), token)).Body.String()
			if got := strings.Contains(body, `name="stage"`); got != c.want {
				t.Errorf("галочка песочницы: %v, ожидалось %v", got, c.want)
			}
		})
	}
}

// Выбор доходит до ядра — и доходит ОТДЕЛЬНО от анонимности: вопросы разные, и
// заметка бывает и той, и другой сразу. Не отмеченная галочка — обычная
// заметка, а не «песочница по умолчанию».
func TestПризнакПесочницыДоходитДоЯдра(t *testing.T) {
	h, wr, token := stageWriteServer(t, platform.RoleAdmin)
	w := do(h, postAs(t, "/new", url.Values{
		"body": {"третье свидание"}, "stage": {"1"},
	}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d", w.Code)
	}
	if !wr.note.Stage {
		t.Error("до ядра не дошло, что это песочница")
	}

	h2, wr2, token2 := stageWriteServer(t, platform.RoleAdmin)
	do(h2, postAs(t, "/new", url.Values{"body": {"обычная заметка"}}, token2))
	if wr2.note.Stage {
		t.Error("обычная заметка ушла в ядро песочницей")
	}
}

// Отказ ядра объясняется словами, а не пятисоткой, и не теряет ни текста, ни
// самой галочки: человек, у которого пропала заметка, второй раз её не напишет.
func TestОтказПесочницыНеТеряетФорму(t *testing.T) {
	h, wr, token := stageWriteServer(t, platform.RoleAdmin)
	wr.fail = platform.ErrStageClosed

	w := do(h, postAs(t, "/new", url.Values{
		"body": {"третье свидание"}, "stage": {"1"},
	}, token))
	if w.Code != http.StatusForbidden {
		t.Fatalf("код %d, ожидался честный отказ", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "третье свидание") {
		t.Error("текст заметки потерян при отказе")
	}
	if !strings.Contains(body, `name="stage" value="1" checked`) {
		t.Errorf("галочка не пережила отказ:\n%s", tailOf(body))
	}
}

// tailOf — хвост страницы для сообщения об ошибке: целиком она слишком велика.
func tailOf(s string) string {
	if len(s) > 1200 {
		return s[len(s)-1200:]
	}
	return s
}

// ПОДПИСЬ ДВЕРИ называет то, зачем в неё идут, а не поля формы за ней.
//
// Поймано боем 30.08.2026: владелец смотрел на страницу зеркальной заметки
// 313128, искал, как запустить жителей, и не находил — ссылка на форму правки
// называлась «Картинка». Галочка песочницы за этой дверью уже стояла, а вывеска
// осталась от прежнего содержимого. Дефект не в форме и не в ядре, а ровно в
// одном слове, и потому проверяется именно слово.
func TestДверьНазываетПесочницу(t *testing.T) {
	for _, c := range []struct {
		name       string
		st         *fakeStore
		want, gone string
	}{
		// Молчащая зеркальная — та самая, ради которой всё и делалось.
		{"молчащая зеркальная", silentMirrorNote(), "Песочница", "Картинка"},
		// В заметке уже говорили: песочницы не будет, и обещать её нельзя —
		// остаётся прежняя подпись про картинку копии.
		{"зеркальная с разговором", talkedMirrorNote(), "Картинка", "Песочница"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, token := modServerOn(t, c.st, platform.RoleAdmin)
			body := do(h, as(guest(t, "GET", "/n/"+itoa64(c.st.note.ID)), token)).Body.String()
			if !strings.Contains(body, `<span class="lbl">`+c.want+`</span>`) {
				t.Errorf("двери не хватает подписи %q — по ней её и ищут", c.want)
			}
			if strings.Contains(body, `<span class="lbl">`+c.gone+`</span>`) {
				t.Errorf("дверь подписана %q, а ведёт не туда", c.gone)
			}
		})
	}
}

// talkedMirrorNote — зеркальная заметка, под которой уже говорили: песочницей
// ей не стать, и правило это держит ядро.
func talkedMirrorNote() *fakeStore {
	st := silentMirrorNote()
	st.note.CommentCount = 7
	return st
}
