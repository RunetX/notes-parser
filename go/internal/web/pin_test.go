package web

// Закрепление заметки наверху ленты и то, что рядом с ним стоит: надпись у
// закрытого обсуждения и вход «Добавить комментарий» прямо из ленты.
//
// Проверяется здесь не вёрстка, а три решения. Закреплённое живёт в ОБЩЕМ
// списке и опознаётся меткой (иначе старая запись наверху читается как
// поломка сортировки). Закреплённых спрашивают только на первой странице
// (шапка, едущая за читателем, — это не закрепление, а помеха). И закрытое
// обсуждение выглядит в ленте ровно как чужая отметка НГС «не актуальна»:
// читателю оба состояния значат одно.

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

func pinnedNote() platform.NoteView {
	n := sampleNote()
	n.ID = 312900
	n.Body = "Площадка переехала, правила здесь."
	n.Pinned = true
	return n
}

// Закреплённое идёт первым, в общем списке и с меткой.
func TestPinnedNoteLeadsTheFeed(t *testing.T) {
	st := &fakeStore{total: 1, notes: []platform.NoteView{sampleNote()}, pinned: []platform.NoteView{pinnedNote()}}
	body := do(openServer(t, st), guest(t, "GET", "/")).Body.String()

	i, j := strings.Index(body, "/n/312900"), strings.Index(body, "/n/312811")
	if i < 0 || j < 0 {
		t.Fatal("в ленте нет обеих заметок")
	}
	if i > j {
		t.Error("закреплённая стоит ниже обычной")
	}
	if !strings.Contains(body, `class="pin"`) || !strings.Contains(body, "Закреплено") {
		t.Error("у закреплённой нет метки — наверху ленты она читается как сбой порядка")
	}
	if n := strings.Count(body, `<li class="note`); n != 2 {
		t.Errorf("заметок на странице %d, ожидалось 2 (закреплённая + обычная)", n)
	}
}

// На второй странице закреплённых не спрашивают вовсе: человек листает ленту
// как раз затем, чтобы уйти от начала.
func TestPinnedOnlyOnFirstPage(t *testing.T) {
	st := &fakeStore{total: 100, notes: []platform.NoteView{sampleNote()}, pinned: []platform.NoteView{pinnedNote()}}
	h := openServer(t, st)

	do(h, guest(t, "GET", "/?page=2"))
	if st.pinnedCalls != 0 {
		t.Errorf("закреплённые спрошены на второй странице (%d раз)", st.pinnedCalls)
	}
	do(h, guest(t, "GET", "/"))
	if st.pinnedCalls != 1 {
		t.Errorf("на первой странице закреплённые спрошены %d раз, ожидался 1", st.pinnedCalls)
	}
}

// Кнопка закрепления — у модератора и только у него.
func TestPinButtonBelongsToModerator(t *testing.T) {
	for _, c := range []struct {
		name string
		role platform.Role
		want bool
	}{
		{"модератор", platform.RoleModerator, true},
		{"участник", platform.RoleUser, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, token := modServer(t, c.role)
			body := do(h, as(guest(t, "GET", "/n/312811"), token)).Body.String()
			if got := strings.Contains(body, `value="pin"`); got != c.want {
				t.Errorf("кнопка «Закрепить» показана: %v, ожидалось %v", got, c.want)
			}
		})
	}
}

// Нажатие доходит до ядра, а «уже закреплено» и обратное действие различаются.
func TestPinActionsReachTheCore(t *testing.T) {
	h, mod, token := modServer(t, platform.RoleModerator)

	for _, do_ := range []string{"pin", "unpin"} {
		form := url.Values{"do": {do_}, "note": {"312811"}, "back": {"/n/312811"}}
		w := do(h, postAs(t, "/mod/act", form, token))
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%s: код %d, ожидался 303", do_, w.Code)
		}
	}
	if len(mod.acts) != 2 || mod.acts[0] != "pin" || mod.acts[1] != "unpin" {
		t.Errorf("до ядра дошло %v, ожидались pin и unpin", mod.acts)
	}
}

// Потолок закреплённых — правило ленты, и говорится о нём человеком: модератор
// должен понять, что снять лишнее, а не увидеть пятисотку.
func TestPinLimitIsExplained(t *testing.T) {
	h, mod, token := modServer(t, platform.RoleModerator)
	mod.pinnedFull = true

	w := do(h, postAs(t, "/mod/act", url.Values{"do": {"pin"}, "note": {"312811"}}, token))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Сначала открепите") {
		t.Error("модератору не сказано, что делать с потолком")
	}
}

// ---------------------------------------------------------------- закрытое обсуждение

// Закрытый модератором тред в ЛЕНТЕ подписан ровно так же, как отметка НГС «не
// актуальна»: заглянуть можно, вступить нельзя — для читателя это одно и то же.
func TestLockedNoteReadsAsNotActual(t *testing.T) {
	n := sampleNote()
	n.Locked = true
	body := do(openServer(t, &fakeStore{total: 1, notes: []platform.NoteView{n}}), guest(t, "GET", "/")).Body.String()

	if !strings.Contains(body, "Заметка не актуальна, но вы можете ознакомиться с её обсуждением") {
		t.Error("у закрытого обсуждения в ленте нет надписи, которая была на НГС")
	}
	if strings.Contains(body, `>Комментарии <span`) {
		t.Error("рядом осталась обычная ссылка «Комментарии»")
	}
}

// А на СТРАНИЦЕ заметки замок называется своим именем: здесь человек собирался
// писать, и ему важно, кто закрыл дверь — чужая отметка или наш модератор.
func TestLockedNotePageNamesTheModerator(t *testing.T) {
	st := noteStore()
	st.note.Locked = true
	body := do(openServer(t, st), guest(t, "GET", "/n/312811")).Body.String()

	if !strings.Contains(body, "Обсуждение закрыто модератором") {
		t.Error("на странице заметки не сказано, что тред закрыл модератор")
	}
}

// ---------------------------------------------------------------- «Добавить комментарий»

// Вход в разговор прямо из ленты — у вошедшего участника, как на НГС. Гостю его
// там нет вовсе: ссылка, ведущая к отказу, хуже её отсутствия.
func TestAddCommentLinkForSignedInOnly(t *testing.T) {
	st := &fakeStore{total: 1, notes: []platform.NoteView{sampleNote()}}
	auth, token := signedInAs(t, platform.User{ID: testProfileID, Nick: testNick, Kind: platform.KindMember})
	h := newFullServer(t, st, auth, &fakeWriter{}, nil, nil, Config{})

	member := do(h, as(guest(t, "GET", "/"), token)).Body.String()
	if !strings.Contains(member, "Добавить комментарий") {
		t.Error("вошедшему не предложено добавить комментарий из ленты")
	}
	if !strings.Contains(member, `href="/n/312811#reply"`) {
		t.Error("ссылка ведёт не к форме ответа")
	}
	if guestBody := do(h, guest(t, "GET", "/")).Body.String(); strings.Contains(guestBody, "Добавить комментарий") {
		t.Error("гостю показана ссылка, которая ответит ему отказом")
	}
}

// Под НЕАКТУАЛЬНОЙ заметкой ссылки нет при любой причине — и у замка
// модератора, и у чужой отметки НГС. Строкой выше уже сказано «ознакомиться с
// обсуждением», то есть разговор окончен; звать в него следующей же ссылкой
// значит спорить с самим собой.
//
// Право писать при этом прежнее: отметка НГС его не отнимает (Ш5, она стоит у
// 62 % зеркальных заметок, и 75 % всех комментариев пришло ПОСЛЕ неё) — форма
// на странице такой заметки работает, и это проверяется здесь же.
func TestAddCommentLinkGoneWhenDiscussionIsOver(t *testing.T) {
	locked, marked := sampleNote(), sampleNote()
	locked.ID, locked.Locked = 312901, true
	marked.ID, marked.CommentsClosed = 312902, true

	st := &fakeStore{total: 2, notes: []platform.NoteView{locked, marked}}
	auth, token := signedInAs(t, platform.User{ID: testProfileID, Nick: testNick, Kind: platform.KindMember})
	h := newFullServer(t, st, auth, &fakeWriter{}, nil, nil, Config{})

	body := do(h, as(guest(t, "GET", "/"), token)).Body.String()
	for _, id := range []string{"312901", "312902"} {
		if strings.Contains(body, `href="/n/`+id+`#reply"`) {
			t.Errorf("под неактуальной заметкой %s предложено добавить комментарий", id)
		}
	}

	// Но форма ответа у отмеченной НГС заметки остаётся: ссылку сняло РЕШЕНИЕ О
	// ПОКАЗЕ, а не запрет писать, и путать это нельзя.
	st.note = marked
	page := do(h, as(guest(t, "GET", "/n/312902"), token)).Body.String()
	if !strings.Contains(page, `action="/n/312902/reply"`) {
		t.Error("отметка НГС отняла и форму ответа, хотя писать она не запрещает")
	}
}
