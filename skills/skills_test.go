package skills_test

// Filesystem skill discovery: roots are host-supplied, same-name skills shadow,
// frontmatter parsing follows the exact "---" rule, and reads stay bounded.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/skills"
)

func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListReadAndShadowing(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeSkill(t, first, "review", "---\nname: review\ndescription: First review skill\n---\nBody one")
	writeSkill(t, second, "review", "---\nname: review\ndescription: Shadowed\n---\nBody two")
	writeSkill(t, second, "deploy", "No frontmatter here.\n# Deploy\nDo the deploy.")
	store := skills.New(skills.Options{Roots: func() []string { return []string{first, second} }})

	list, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List = %+v, want 2 skills (shadowed duplicate hidden)", list)
	}
	byName := map[string]string{}
	for _, skill := range list {
		byName[skill.Name] = skill.Description
	}
	if byName["review"] != "First review skill" {
		t.Fatalf("review description = %q, want the higher-precedence root", byName["review"])
	}
	if byName["deploy"] != "Deploy" {
		t.Fatalf("deploy description = %q", byName["deploy"])
	}
	if warnings := store.Warnings(); len(warnings) == 0 || !strings.Contains(warnings[0], "review") {
		t.Fatalf("warnings = %v, want the shadowed skill reported", warnings)
	}
	skill, err := store.Read(context.Background(), "review")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(skill.Body, "Body one") {
		t.Fatalf("Read body = %q", skill.Body)
	}
}

func TestReadRejectsTraversalAndUnknown(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "ok", "---\ndescription: ok skill\n---\nbody")
	store := skills.New(skills.Options{Roots: func() []string { return []string{root} }})
	if _, err := store.Read(context.Background(), "../etc/passwd"); err == nil {
		t.Fatal("path traversal accepted")
	}
	if _, err := store.Read(context.Background(), "missing"); err == nil {
		t.Fatal("unknown skill accepted")
	}
}

func TestUnclosedFrontmatterIsText(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "unclosed", "---\ndescription: not frontmatter\nNo closing rule.\n# Heading\nBody")
	store := skills.New(skills.Options{Roots: func() []string { return []string{root} }})
	list, err := store.List(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %+v/%v", list, err)
	}
	if !strings.Contains(list[0].Body, "description: not frontmatter") {
		t.Fatalf("unclosed frontmatter was stripped: %q", list[0].Body)
	}
}
