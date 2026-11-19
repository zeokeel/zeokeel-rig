package sqlite_test

// Spilled tool results, kept where the rest of the session lives: a large output
// becomes a row, and the model gets a handle instead of a file path.

import (
	"context"
	"errors"
	"strings"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// TestSpillSaveAndLoad covers the round trip the model relies on.
func TestSpillSaveAndLoad(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	spill := db.Spill()
	content := strings.Repeat("输出行\n", 2000)
	ref, err := spill.Save(ctx, rig.ToolCall{Name: "bash", ID: "c1"}, content)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if ref.ID == "" || ref.Bytes != len(content) {
		t.Fatalf("ref = %+v", ref)
	}
	got, err := spill.Load(ctx, ref.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != content {
		t.Fatalf("loaded %d bytes, want %d", len(got), len(content))
	}
	if _, err := spill.Load(ctx, "no-such-ref"); !errors.Is(err, rig.ErrSpillNotFound) {
		t.Fatalf("Load unknown = %v, want ErrSpillNotFound", err)
	}
}

// TestSpillGCKeepsTheNewest covers the bound on stored results: the host says how
// many to keep, the store drops the oldest.
func TestSpillGCKeepsTheNewest(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	spill := db.Spill()
	ids := make([]string, 0, 3)
	for _, name := range []string{"a", "b", "c"} {
		ref, err := spill.Save(ctx, rig.ToolCall{Name: name, ID: name}, "内容-"+name)
		if err != nil {
			t.Fatalf("Save %s: %v", name, err)
		}
		ids = append(ids, ref.ID)
	}
	dropped, err := spill.GC(ctx, 2)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if _, err := spill.Load(ctx, ids[0]); !errors.Is(err, rig.ErrSpillNotFound) {
		t.Fatalf("oldest survived GC: %v", err)
	}
	for _, id := range ids[1:] {
		if _, err := spill.Load(ctx, id); err != nil {
			t.Fatalf("kept ref %s missing: %v", id, err)
		}
	}
	// keep <= 0 drops nothing: "no limit" is expressed by not calling GC.
	if n, err := spill.GC(ctx, 0); err != nil || n != 0 {
		t.Fatalf("GC(0) = %d, %v", n, err)
	}
}
