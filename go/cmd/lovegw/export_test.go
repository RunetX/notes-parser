package main

import (
	"testing"

	"lovegw/internal/archive"
)

func TestBuildTree(t *testing.T) {
	// 1(корень) ← 2, 3;  2 ← 4.  Плюс сирота 99 (родитель 500 вне выборки).
	flat := []archive.StoredComment{
		{ID: 1, ParentID: 0, Author: archive.User{ID: 10, Name: "A"}},
		{ID: 2, ParentID: 1, Author: archive.User{ID: 20, Name: "B"}},
		{ID: 3, ParentID: 1, Author: archive.User{ID: 30, Name: "C"}},
		{ID: 4, ParentID: 2, Author: archive.User{ID: 10, Name: "A"}},
		{ID: 99, ParentID: 500, Author: archive.User{ID: 40, Name: "D"}},
	}
	roots := buildTree(flat)

	// Корни: комментарий 1 и сирота 99.
	if len(roots) != 2 {
		t.Fatalf("корней: got %d, want 2", len(roots))
	}
	var root1 *exportComment
	for _, r := range roots {
		if r.ID == 1 {
			root1 = r
		}
	}
	if root1 == nil {
		t.Fatal("корень 1 не найден")
	}
	if len(root1.Replies) != 2 {
		t.Fatalf("у 1 должно быть 2 ответа, got %d", len(root1.Replies))
	}
	// Порядок детей — по id: 2, затем 3.
	if root1.Replies[0].ID != 2 || root1.Replies[1].ID != 3 {
		t.Errorf("порядок детей 1: got %d,%d want 2,3", root1.Replies[0].ID, root1.Replies[1].ID)
	}
	// У 2 — один вложенный ответ 4.
	if len(root1.Replies[0].Replies) != 1 || root1.Replies[0].Replies[0].ID != 4 {
		t.Errorf("вложенность 1→2→4 не построена")
	}
	// Автор пробрасывается.
	if root1.Author.Name != "A" {
		t.Errorf("автор корня: got %q, want A", root1.Author.Name)
	}
}
