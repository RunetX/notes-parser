package platnarod

// Сцена против настоящего Postgres.
//
// Проверяется здесь ОДНО, но главное: что жителям на смежном обсуждении подаётся
// текст ОРИГИНАЛА, а не служебное тело двойника. Подделкой это не проверить —
// весь предмет и есть запрос с двумя соединениями к notes.
//
// Запуск: LOVEGW_TEST_PG_DSN=postgres://... go test ./internal/platnarod/
// Без переменной тесты пропускаются, и `go test ./...` на машине без Postgres
// остаётся зелёным.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

var shared *platform.Platform

func TestMain(m *testing.M) {
	dsn := os.Getenv("LOVEGW_TEST_PG_DSN")
	if dsn == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	p, err := platform.Open(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Postgres: %v\n", err)
		os.Exit(1)
	}
	// Тот же предохранитель, что у соседей: тесты сносят схему целиком, и боевая
	// база не должна оказаться на другом конце DSN даже при опечатке.
	var db string
	if err := p.Pool().QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		fmt.Fprintf(os.Stderr, "имя базы: %v\n", err)
		os.Exit(1)
	}
	if !strings.Contains(db, "test") {
		fmt.Fprintf(os.Stderr, "отказ работать с базой %q: тесты сносят схему целиком, "+
			"имя тестовой базы обязано содержать «test»\n", db)
		os.Exit(1)
	}
	shared = p
	code := m.Run()
	p.Close()
	os.Exit(code)
}

func stagePlatform(t *testing.T) *platform.Platform {
	t.Helper()
	if shared == nil {
		t.Skip("нет LOVEGW_TEST_PG_DSN — тест против настоящего Postgres пропущен")
	}
	ctx := context.Background()
	if _, err := shared.Pool().Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("очистка схемы: %v", err)
	}
	if err := shared.Migrate(ctx); err != nil {
		t.Fatalf("миграции: %v", err)
	}
	// Тексты согласий публикуются вместе со схемой — без них согласие участника
	// падает на внешнем ключе consents → consent_docs.
	if err := shared.EnsureConsentDocs(ctx, platform.Operator{}); err != nil {
		t.Fatalf("тексты согласий: %v", err)
	}
	return shared
}

func mustMember(t *testing.T, p *platform.Platform, nick string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := p.CreateNativeUser(ctx, nick)
	if err != nil {
		t.Fatalf("участник %q: %v", nick, err)
	}
	docs, err := platform.CurrentConsentDocs(platform.Operator{})
	if err != nil {
		t.Fatalf("тексты согласий: %v", err)
	}
	for _, d := range docs {
		if err := p.GrantConsent(ctx, id, d.Kind, d.Version, "тест"); err != nil {
			t.Fatalf("согласие %s: %v", d.Kind, err)
		}
	}
	return id
}

// ДВОЙНИК ПОДАЁТ ЖИТЕЛЯМ ТЕКСТ ОРИГИНАЛА.
//
// Это и есть весь смысл смежного обсуждения: копии чужого текста нигде нет —
// поставить чужие слова под другим именем нельзя, — а разговор жителей идёт про
// ту самую заметку. Собирается текст запросом в момент, когда он нужен, и потому
// не может разойтись с оригиналом ни правкой, ни обезличиванием.
func TestДвойникПодаётЖителямТекстОригинала(t *testing.T) {
	p := stagePlatform(t)
	ctx := context.Background()

	admin := mustMember(t, p, "Садовник")
	if err := p.SetRole(ctx, platform.Viewer{UserID: admin, Role: platform.RoleAdmin},
		admin, platform.RoleAdmin); err != nil {
		t.Fatalf("права: %v", err)
	}
	author := mustMember(t, p, "Ирма")
	note, err := p.CreateNote(ctx, platform.NewNote{AuthorID: author, Body: "мужчина обещал перезвонить"})
	if err != nil {
		t.Fatal(err)
	}
	twin, err := p.CreateSynthThreadAsAdmin(ctx,
		platform.Viewer{UserID: admin, Role: platform.RoleAdmin}, note)
	if err != nil {
		t.Fatalf("двойник: %v", err)
	}

	notes, err := New(p).StageNotesSince(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got bool
	for _, n := range notes {
		if n.ID != twin {
			continue
		}
		got = true
		if n.Body != "мужчина обещал перезвонить" {
			t.Errorf("жителям подан текст %q, а не текст оригинала", n.Body)
		}
		if n.AuthorNick != "Ирма" {
			t.Errorf("автор для жителей %q, ожидалась Ирма", n.AuthorNick)
		}
		if n.AuthorID != author {
			t.Errorf("автор для жителей %d, ожидался %d", n.AuthorID, author)
		}
	}
	if !got {
		t.Fatalf("двойник %d не попал на сцену вовсе", twin)
	}
	// Скрытый оригинал уводит со сцены и двойника: читать текст, которого на
	// площадке больше нет, жителям незачем — служба закроет такой тред сама.
	if err := p.HideSubject(ctx, platform.Viewer{UserID: admin, Role: platform.RoleAdmin},
		platform.NoteSubject(note), platform.CatOther, "проверка"); err != nil {
		t.Fatalf("скрытие оригинала: %v", err)
	}
	notes, err = New(p).StageNotesSince(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		if n.ID == twin {
			t.Error("двойник остался на сцене со скрытым оригиналом")
		}
	}
}
