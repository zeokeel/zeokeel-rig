package skills_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/skills"
)

func newEditor(t *testing.T) (*skills.Store, string) {
	t.Helper()
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, skills.DataDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	store := skills.New(skills.Options{
		DataDir: dataDir,
		Roots:   func() []string { return []string{filepath.Join(dataDir, skills.DataDirName)} },
	})
	return store, dataDir
}

func TestCreateUpdateDeleteSkill(t *testing.T) {
	store, dataDir := newEditor(t)
	ctx := context.Background()
	created, err := store.CreateSkill(ctx, skills.Draft{Name: "reviewer", Description: "Reviews code", Body: "# Reviewer\nBe strict."})
	if err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	if created.Name != "reviewer" || !strings.Contains(created.Body, "Be strict") {
		t.Fatalf("created = %+v", created)
	}
	if _, err := store.CreateSkill(ctx, skills.Draft{Name: "reviewer"}); err == nil {
		t.Fatal("duplicate create accepted")
	}
	updated, err := store.UpdateSkill(ctx, "reviewer", skills.Draft{Name: "reviewer", Description: "Updated", Body: "New body"})
	if err != nil {
		t.Fatalf("UpdateSkill: %v", err)
	}
	if updated.Description != "Updated" || !strings.Contains(updated.Body, "New body") {
		t.Fatalf("updated = %+v", updated)
	}
	if _, err := store.UpdateSkill(ctx, "missing", skills.Draft{Name: "missing"}); err == nil {
		t.Fatal("update of an unknown skill accepted")
	}
	if err := store.DeleteSkill(ctx, "reviewer"); err != nil {
		t.Fatalf("DeleteSkill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, skills.DataDirName, "reviewer")); !os.IsNotExist(err) {
		t.Fatalf("skill dir still exists: %v", err)
	}
	if err := store.DeleteSkill(ctx, "reviewer"); err == nil {
		t.Fatal("delete of an unknown skill accepted")
	}
}

func TestSkillNameAndDataDirValidation(t *testing.T) {
	store, _ := newEditor(t)
	ctx := context.Background()
	for _, name := range []string{"", "bad/name", "..", "with space", strings.Repeat("a", 65)} {
		if _, err := store.CreateSkill(ctx, skills.Draft{Name: name}); err == nil {
			t.Fatalf("name %q accepted", name)
		}
	}
	noData := skills.New(skills.Options{})
	if _, err := noData.CreateSkill(ctx, skills.Draft{Name: "x"}); err == nil {
		t.Fatal("create without a data dir accepted")
	}
}

func TestDeleteRejectsOtherRoots(t *testing.T) {
	dataDir := t.TempDir()
	other := t.TempDir()
	writeSkill(t, other, "outside", "---\ndescription: outside\n---\nbody")
	store := skills.New(skills.Options{
		DataDir: dataDir,
		Roots:   func() []string { return []string{other} },
	})
	if err := store.DeleteSkill(context.Background(), "outside"); err == nil {
		t.Fatal("delete from a read-only root accepted")
	}
}
