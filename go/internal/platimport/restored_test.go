package platimport

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

// restoredThread прогоняет readRestoredThread по заметке фикстуры.
func restoredThread(t *testing.T, db *sql.DB, noteID, noteAuthor int64) ([][]any, RestoredStats) {
	t.Helper()
	ctx := context.Background()
	nicks, err := readNicks(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := db.PrepareContext(ctx, `
		SELECT id, author_id, text, published_at FROM comments
		 WHERE note_id = ? AND id < 0 ORDER BY id DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	var st RestoredStats
	rows, _, err := readRestoredThread(ctx, stmt, noteID, noteAuthor, nicks, &st)
	if err != nil {
		t.Fatal(err)
	}
	return rows, st
}

// Ключ третьей полосы обязан быть выведен из ключа архива и никуда не уехать:
// на нём держится идемпотентность добора (повтор считает то же самое) и порядок
// реплик в треде (|id| = note_id*1000 + номер, то есть растёт по времени).
func TestRestoredIDIsDeterministicAndInBand(t *testing.T) {
	first, second := restoredID(-150871000), restoredID(-150871001)
	if !platform.IsRestored(first) || !platform.IsRestored(second) {
		t.Fatalf("ключи %d и %d вышли из полосы восстановленного", first, second)
	}
	if platform.IsNative(first) || platform.IsNGS(first) {
		t.Fatalf("ключ %d опознан как нативный или как ключ НГС", first)
	}
	if first >= second {
		t.Fatalf("порядок реплик потерян: %d >= %d", first, second)
	}
	if again := restoredID(-150871000); again != first {
		t.Fatalf("повтор дал другой ключ: %d против %d", again, first)
	}
	// Положительный ключ архива — это ключ сайта, и трогать его нельзя.
	if got := restoredID(312811); got != 312811 {
		t.Fatalf("ключ НГС переразмечен: %d", got)
	}
}

func TestCutAddress2010(t *testing.T) {
	// Участники треда. «Димон» и «Димон_Таибычев» стоят рядом не для красоты:
	// ники 2010 года сплошь приставки друг друга, и на этой паре ломается
	// разбор «первое слово после Для».
	seen := map[string]int64{
		"мари":           11,
		"димон":          12,
		"димон_таибычев": 13,
		"чертенок №13":   14,
		"chus++":         15,
		"нюш@":           16,
	}
	cases := []struct {
		name, body, wantRest, wantNick string
		wantOK                         bool
	}{
		{"с именем в скобках", "Для Мари (Иллюзия) сматри вниательнее",
			"сматри вниательнее", "мари", true},
		{"без имени в скобках", "Для Chus++ А чо слушает сосед?",
			"А чо слушает сосед?", "chus++", true},
		{"самый длинный ник", "Для Димон_Таибычев не...на практике",
			"не...на практике", "димон_таибычев", true},
		{"ник со скобкой и цифрой", "Для ЧеРтёнОк №13 (Солнышко) по гуглу!?",
			"по гуглу!?", "чертенок №13", true},
		{"ник со знаком", "Для НюШ@ ходят...", "ходят...", "нюш@", true},
		{"регистр и ё", "для чертёнок №13 (Солнышко) ага", "ага", "чертенок №13", true},
		{"чужой ник", "Для Некто (Имя) привет", "", "", false},
		{"обращение и больше ничего", "Для Мари", "", "", false},
		{"обращение и пустое имя", "Для Мари (Иллюзия)", "", "", false},
		{"нет обращения", "Мари, сматри вниательнее", "", "", false},
		{"слово «для» в тексте", "Для меня это новость", "", "", false},
		{"длинная скобка остаётся тексту", "Для Мари (" + strings.Repeat("я", 60) + ") текст",
			"(" + strings.Repeat("я", 60) + ") текст", "мари", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rest, nick, ok := cutAddress2010(c.body, seen)
			if ok != c.wantOK {
				t.Fatalf("разбор %q: ok = %v, ожидалось %v (остаток %q)", c.body, ok, c.wantOK, rest)
			}
			if !ok {
				// Неразобранное обращение обязано вернуть тело нетронутым:
				// срезать его нечем, а потерять — значит потерять смысл реплики.
				if rest != c.body {
					t.Fatalf("тело изменено при неудачном разборе: %q", rest)
				}
				return
			}
			if rest != c.wantRest || nick != c.wantNick {
				t.Fatalf("разбор %q: (%q, %q), ожидалось (%q, %q)", c.body, rest, nick, c.wantRest, c.wantNick)
			}
		})
	}
}

// Тред 2010 года целиком: ключи третьей полосы, ребро из обращения, снятое
// обращение и — главное — тело, оставленное в покое там, где адресат не нашёлся.
func TestReadRestoredThread(t *testing.T) {
	db := fixture(t)
	exec(t, db, `INSERT INTO users (id, name) VALUES
		(341431, 'Позитив'), (-31, 'Мари'), (-32, 'Увалень'), (-33, 'Ромашка')`)
	exec(t, db, `INSERT INTO notes (id, author_id, published_at) VALUES (150871, 341431, '2010-08-05T04:00:00Z')`)
	// Увалень и сам автор заметки в треде не пишут: обращения к ним разбираются,
	// но привязать их не к чему — ровно случай дампа, покрывающего шесть суток
	// треда, который начался раньше.
	exec(t, db, `INSERT INTO comments (id, note_id, author_id, text, published_at) VALUES
		(-150871000, 150871, -31, 'первая реплика', '2010-08-05T04:58:23Z'),
		(-150871001, 150871, -33, 'Для Мари (Иллюзия) сматри вниательнее', '2010-08-05T05:01:00Z'),
		(-150871002, 150871, -31, 'Для Увалень (Сергей) мне тоже везет', '2010-08-05T05:02:00Z'),
		(-150871003, 150871, -33, 'Для Позитив (Автор) а ты кто', '2010-08-05T05:03:00Z')`)

	rows, st := restoredThread(t, db, 150871, 341431)
	if len(rows) != 4 {
		t.Fatalf("реплик %d, ожидалось 4", len(rows))
	}
	for i, r := range rows {
		if id := r[colID].(int64); !platform.IsRestored(id) {
			t.Fatalf("реплика %d: ключ %d вне полосы восстановленного", i, id)
		}
	}

	// Вторая реплика адресована первой: ребро найдено, обращение ушло в него.
	second := rows[1]
	if got := second[colReplyTo]; got != rows[0][colID] {
		t.Fatalf("ребро второй реплики: %v, ожидалось %v", got, rows[0][colID])
	}
	if got := second[colBody].(string); got != "сматри вниательнее" {
		t.Fatalf("тело второй реплики: %q", got)
	}
	if got := second[colSource].(int16); got != int16(platform.ReplyPrefix) {
		t.Fatalf("источник ребра: %v, ожидался «обращение»", got)
	}

	// Третья адресована тому, кто в дампе ещё не писал (его реплики просто нет:
	// дамп покрывает шесть суток, а тред старше). Ребра нет — и обращение
	// обязано остаться в теле, иначе реплика потеряет адресата совсем.
	third := rows[2]
	if third[colReplyTo] != nil {
		t.Fatalf("у третьей реплики появилось ребро: %v", third[colReplyTo])
	}
	if got := third[colBody].(string); got != "Для Увалень (Сергей) мне тоже везет" {
		t.Fatalf("тело третьей реплики срезано: %q", got)
	}

	// Четвёртая адресована автору заметки. Ник опознан, но ребра нет: ответ в
	// корень треда — это и есть ответ автору.
	fourth := rows[3]
	if fourth[colReplyTo] != nil {
		t.Fatalf("обращение к автору заметки дало ребро: %v", fourth[colReplyTo])
	}
	if got := fourth[colBody].(string); got != "Для Позитив (Автор) а ты кто" {
		t.Fatalf("тело четвёртой реплики срезано: %q", got)
	}

	if st.EdgeAddr != 1 || st.Trimmed != 1 || st.EdgeNone != 3 {
		t.Fatalf("счётчики: рёбер %d, снято %d, без ребра %d", st.EdgeAddr, st.Trimmed, st.EdgeNone)
	}
}

// Автор реплики обязан приехать анкетой третьей полосы, а не пустым автором:
// именно к этой тени привязывают ветерана приглашением.
func TestRestoredThreadAuthors(t *testing.T) {
	db := fixture(t)
	exec(t, db, `INSERT INTO users (id, name) VALUES (341431, 'Позитив'), (-31, 'Мари')`)
	exec(t, db, `INSERT INTO notes (id, author_id, published_at) VALUES (150871, -31, '2010-08-05T04:00:00Z')`)
	exec(t, db, `INSERT INTO comments (id, note_id, author_id, text, published_at) VALUES
		(-150871000, 150871, -31, 'без анкеты', '2010-08-05T04:58:23Z'),
		(-150871001, 150871, 341431, 'с анкетой', '2010-08-05T04:59:23Z')`)

	rows, _ := restoredThread(t, db, 150871, -31)
	if got := rows[0][colAuthor]; got != restoredID(-31) {
		t.Fatalf("автор без анкеты: %v, ожидался %d", got, restoredID(-31))
	}
	if got := rows[1][colAuthor]; got != int64(341431) {
		t.Fatalf("автор с анкетой переразмечен: %v", got)
	}
	// Снимок ника в author_display не пишется: ник живёт в users и меняется
	// переименованием, как у всех остальных.
	if got := rows[0][colDisplay].(string); got != "" {
		t.Fatalf("author_display заполнен: %q", got)
	}
}
