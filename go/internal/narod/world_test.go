package narod

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestWorld(t *testing.T) *World {
	t.Helper()
	w, err := OpenWorld(context.Background(), filepath.Join(t.TempDir(), "narod.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

func TestOpenWorldMigrates(t *testing.T) {
	ctx := context.Background()
	w := openTestWorld(t)
	v, err := w.WorldVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v != len(worldMigrations) {
		t.Fatalf("версия схемы %d, миграций %d", v, len(worldMigrations))
	}
}

// Повторное открытие ничего не ломает: демон переживает рестарт, а `narod
// enroll` зовут после каждой правки карточки.
func TestOpenWorldIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "narod.db")
	for i := 0; i < 2; i++ {
		w, err := OpenWorld(ctx, path)
		if err != nil {
			t.Fatalf("открытие %d: %v", i+1, err)
		}
		w.Close()
	}
}

func TestUpsertActorIsIdempotent(t *testing.T) {
	ctx := context.Background()
	w := openTestWorld(t)
	now := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)

	a := Actor{ID: "zavhoz", Kind: ActorPersona, PlatformUserID: 100000000042,
		Nick: "Завхоз", CardPath: "data/narod/cards/zavhoz.json"}
	if err := w.UpsertActor(ctx, a, now); err != nil {
		t.Fatal(err)
	}
	// Второй заход — правка ника, а не второй житель: у первого уже есть память
	// и отношения, и завести дубль значило бы их потерять.
	a.Nick = "Завхоз."
	if err := w.UpsertActor(ctx, a, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	actors, err := w.Actors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(actors) != 1 {
		t.Fatalf("акторов %d, ожидался 1", len(actors))
	}
	if actors[0].Nick != "Завхоз." {
		t.Errorf("ник не обновился: %s", actors[0].Nick)
	}
	if !actors[0].CreatedAt.Equal(now) {
		t.Errorf("дата заведения переписана: %s", actors[0].CreatedAt)
	}

	got, ok, err := w.ActorByPlatformUser(ctx, 100000000042)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.ID != "zavhoz" {
		t.Errorf("актор по анкете площадки не найден: %+v", got)
	}
	if _, ok, err := w.ActorByPlatformUser(ctx, 999); err != nil || ok {
		t.Errorf("нашёлся актор по чужой анкете: %v %v", ok, err)
	}
}

// Живой человек и ручной персонаж владельца — такие же узлы графа: без этого
// персонажи не смогли бы ни помнить их, ни отвечать им тем же механизмом.
func TestWorldHoldsHumansToo(t *testing.T) {
	ctx := context.Background()
	w := openTestWorld(t)
	now := time.Now()
	for _, a := range []Actor{
		{ID: "zavhoz", Kind: ActorPersona, Nick: "Завхоз"},
		{ID: "h:312811", Kind: ActorHuman, PlatformUserID: 312811, Nick: "Паноптикум"},
		{ID: "m:krasotka", Kind: ActorManual, Nick: "Красотка"},
	} {
		if err := w.UpsertActor(ctx, a, now); err != nil {
			t.Fatalf("%s: %v", a.ID, err)
		}
	}
	actors, err := w.Actors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(actors) != 3 {
		t.Fatalf("акторов %d, ожидалось 3", len(actors))
	}
}
