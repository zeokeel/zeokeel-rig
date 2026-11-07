package skill_test

// Tests for the skill tool and the FileStore. The tool tests use a fake
// rig.SkillStore; the store tests build throwaway directories on disk.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/skill"
)

func toolMap(tools []rig.Tool) map[string]rig.Tool {
	byName := make(map[string]rig.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	return byName
}

func invoke(t *testing.T, tools map[string]rig.Tool, name, arguments string) rig.ToolResult {
	t.Helper()
	tool, ok := tools[name]
	if !ok {
		t.Fatalf("tool %q not found", name)
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "call-1", Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", name, err)
	}
	return result
}

func invokeError(t *testing.T, tools map[string]rig.Tool, name, arguments string) string {
	t.Helper()
	result := invoke(t, tools, name, arguments)
	if !result.IsError {
		t.Fatalf("%s: result = %q, want IsError", name, result.Content)
	}
	return result.Content
}

// fakeStore is a scripted rig.SkillStore.
type fakeStore struct {
	mu      sync.Mutex
	skills  []rig.Skill
	listErr error
	readErr error
}

func (s *fakeStore) List(context.Context) ([]rig.Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	return slices.Clone(s.skills), nil
}

func (s *fakeStore) Read(_ context.Context, name string) (rig.Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return rig.Skill{}, s.readErr
	}
	for _, entry := range s.skills {
		if entry.Name == name {
			return entry, nil
		}
	}
	return rig.Skill{}, errors.New("skill: unknown skill " + name)
}

func sampleSkills() []rig.Skill {
	return []rig.Skill{
		{Name: "release-checklist", Description: "How to cut a release", Path: "/skills/release/SKILL.md", Body: "# Release\nRun the tests, tag, push."},
		{Name: "debugging", Description: "Systematic debugging", Path: "/skills/debugging/SKILL.md", Body: "# Debug\nReproduce, bisect, fix. secretword"},
	}
}

func TestToolsShape(t *testing.T) {
	tools := skill.Tools(&fakeStore{})
	if len(tools) != 1 {
		t.Fatalf("skill.Tools returned %d tools, want 1", len(tools))
	}
	tool := tools[0]
	if tool.Name() != "skill" {
		t.Fatalf("tool name = %q, want skill", tool.Name())
	}
	if tool.Description() == "" {
		t.Fatal("skill tool has no description")
	}
	schema := tool.Schema()
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v", schema["type"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v", schema["properties"])
	}
	action, ok := properties["action"].(map[string]any)
	if !ok {
		t.Fatalf("missing action property: %#v", properties)
	}
	enum, ok := action["enum"].([]string)
	if !ok || len(enum) != 3 {
		t.Fatalf("action enum = %#v", action["enum"])
	}
}

func TestSkillNameOnlyLoads(t *testing.T) {
	store := &fakeStore{skills: []rig.Skill{{Name: "deploy", Description: "deploy it", Body: "run deploy"}}}
	byName := toolMap(skill.Tools(store))
	result := invoke(t, byName, "skill", `{"name":"deploy"}`)
	if result.IsError || !strings.Contains(result.Content, "run deploy") {
		t.Fatalf("name-only skill call = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestNilStoreUsesDefaultFileStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZEOKEEL_SKILLS", filepath.Join(home, "empty"))
	byName := toolMap(skill.Tools(nil))
	result := invoke(t, byName, "skill", `{"action":"list"}`)
	if result.IsError {
		t.Fatalf("list with default store = %q", result.Content)
	}
	if !strings.Contains(result.Content, "no skills found") {
		t.Fatalf("empty default store = %q", result.Content)
	}
	// A typed nil store must also fall back instead of panicking.
	var typedNil *fakeStore
	byName = toolMap(skill.Tools(typedNil))
	if result := invoke(t, byName, "skill", `{"action":"list"}`); result.IsError {
		t.Fatalf("typed nil store list = %q", result.Content)
	}
}

func TestList(t *testing.T) {
	byName := toolMap(skill.Tools(&fakeStore{skills: sampleSkills()}))
	result := invoke(t, byName, "skill", `{"action":"list"}`)
	if result.IsError {
		t.Fatalf("list = %q", result.Content)
	}
	for _, fragment := range []string{"release-checklist", "How to cut a release", "debugging", "Systematic debugging"} {
		if !strings.Contains(result.Content, fragment) {
			t.Fatalf("list missing %q:\n%s", fragment, result.Content)
		}
	}
	// Empty store has a friendly message, not an error.
	empty := toolMap(skill.Tools(&fakeStore{}))
	if result := invoke(t, empty, "skill", `{"action":"list"}`); result.IsError || !strings.Contains(result.Content, "no skills found") {
		t.Fatalf("empty list = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestSearch(t *testing.T) {
	byName := toolMap(skill.Tools(&fakeStore{skills: sampleSkills()}))
	// Match by name.
	result := invoke(t, byName, "skill", `{"action":"search","query":"release"}`)
	if result.IsError || !strings.Contains(result.Content, "release-checklist") || strings.Contains(result.Content, "debugging:") {
		t.Fatalf("search by name = %q", result.Content)
	}
	// Match by body only (case-insensitive).
	result = invoke(t, byName, "skill", `{"action":"search","query":"SECRETWORD"}`)
	if result.IsError || !strings.Contains(result.Content, "debugging") || strings.Contains(result.Content, "release-checklist") {
		t.Fatalf("search by body = %q", result.Content)
	}
	// Multi-word queries require all terms.
	result = invoke(t, byName, "skill", `{"action":"search","query":"systematic fix"}`)
	if result.IsError || !strings.Contains(result.Content, "debugging") {
		t.Fatalf("multi-term search = %q", result.Content)
	}
	// No match is a text result, not an error.
	result = invoke(t, byName, "skill", `{"action":"search","query":"nonexistent-term"}`)
	if result.IsError || !strings.Contains(result.Content, "no skills found matching") {
		t.Fatalf("empty search = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestLoad(t *testing.T) {
	byName := toolMap(skill.Tools(&fakeStore{skills: sampleSkills()}))
	result := invoke(t, byName, "skill", `{"action":"load","name":"release-checklist"}`)
	if result.IsError {
		t.Fatalf("load = %q", result.Content)
	}
	for _, fragment := range []string{"name: release-checklist", "path: /skills/release/SKILL.md", "How to cut a release", "Run the tests, tag, push."} {
		if !strings.Contains(result.Content, fragment) {
			t.Fatalf("load missing %q:\n%s", fragment, result.Content)
		}
	}
	text := invokeError(t, byName, "skill", `{"action":"load","name":"nope"}`)
	if !strings.Contains(text, "unknown skill") {
		t.Fatalf("unknown skill error = %q", text)
	}
}

func TestArgumentsValidation(t *testing.T) {
	byName := toolMap(skill.Tools(&fakeStore{skills: sampleSkills()}))
	cases := map[string]string{
		`{"action":"explode"}`:             "unknown action",
		`{"action":"search"}`:              "query is required",
		`{"action":"search","query":"  "}`: "query is required",
		`{"action":"load"}`:                "name is required",
		`{"action":"load","name":" "}`:     "name is required",
		`{"action":"list","bogus":1}`:      "", // unknown fields are ignored by encoding/json
	}
	for arguments, want := range cases {
		result := invoke(t, byName, "skill", arguments)
		if arguments == `{"action":"list","bogus":1}` {
			if result.IsError {
				t.Fatalf("%s unexpectedly failed: %q", arguments, result.Content)
			}
			continue
		}
		if !result.IsError || !strings.Contains(result.Content, want) {
			t.Fatalf("%s: result = %q (IsError=%v), want mention of %q", arguments, result.Content, result.IsError, want)
		}
	}
}

func TestStoreErrors(t *testing.T) {
	byName := toolMap(skill.Tools(&fakeStore{listErr: errors.New("disk unreadable")}))
	text := invokeError(t, byName, "skill", `{"action":"list"}`)
	if !strings.Contains(text, "disk unreadable") {
		t.Fatalf("list store error = %q", text)
	}
	byName = toolMap(skill.Tools(&fakeStore{readErr: errors.New("read failed")}))
	text = invokeError(t, byName, "skill", `{"action":"load","name":"x"}`)
	if !strings.Contains(text, "read failed") {
		t.Fatalf("read store error = %q", text)
	}
}

func TestInvalidArgumentsJSONAndCancellation(t *testing.T) {
	byName := toolMap(skill.Tools(&fakeStore{}))
	if _, err := byName["skill"].Invoke(context.Background(), rig.ToolCall{Arguments: `{"bad":`}); err == nil {
		t.Fatal("invalid JSON did not return a Go error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := byName["skill"].Invoke(ctx, rig.ToolCall{Arguments: `{"action":"list"}`}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled invoke error = %v", err)
	}
}

func TestConcurrentToolCalls(t *testing.T) {
	fake := &fakeStore{skills: sampleSkills()}
	byName := toolMap(skill.Tools(fake))
	var wg sync.WaitGroup
	for index := 0; index < 16; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			arguments := []string{
				`{"action":"list"}`,
				`{"action":"search","query":"release"}`,
				`{"action":"load","name":"debugging"}`,
			}[index%3]
			result, err := byName["skill"].Invoke(context.Background(), rig.ToolCall{Arguments: arguments})
			if err != nil || result.IsError {
				t.Errorf("concurrent call failed: %v / %q", err, result.Content)
			}
		}(index)
	}
	wg.Wait()
}

// --- FileStore tests ------------------------------------------------------

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func newSkillTree(t *testing.T) (root, first, second string) {
	t.Helper()
	root = t.TempDir()
	first = filepath.Join(root, "first")
	second = filepath.Join(root, "second")
	writeFile(t, filepath.Join(first, "alpha", "SKILL.md"),
		"---\nname: alpha\ndescription: Alpha does things\n---\n# Alpha\nBody with secretword.\n")
	writeFile(t, filepath.Join(first, "beta", "SKILL.md"),
		"# Beta heading\nBeta body line.\n")
	writeFile(t, filepath.Join(first, "gamma.md"),
		"# Gamma\nGamma top-level body.\n")
	writeFile(t, filepath.Join(first, "malformed", "SKILL.md"),
		"---\nthis is not front matter\n---\n# Malformed heading\nMalformed body.\n")
	if err := os.MkdirAll(filepath.Join(first, "empty"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeFile(t, filepath.Join(first, "dup", "SKILL.md"), "# Dup first\nfirst copy\n")
	writeFile(t, filepath.Join(second, "dup", "SKILL.md"), "# Dup second\nsecond copy\n")
	writeFile(t, filepath.Join(second, "zeta", "SKILL.md"), "# Zeta\ndescription: Zeta skill\n")
	return root, first, second
}

func TestFileStoreListAndRead(t *testing.T) {
	_, first, second := newSkillTree(t)
	store := skill.NewFileStore(first, second)
	skills, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byName := make(map[string]rig.Skill, len(skills))
	for _, entry := range skills {
		byName[entry.Name] = entry
	}
	for _, name := range []string{"alpha", "beta", "gamma", "malformed", "dup", "zeta"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("missing skill %q in %v", name, skills)
		}
	}
	if _, ok := byName["empty"]; ok {
		t.Fatal("directory without SKILL.md was listed")
	}
	if got := byName["alpha"].Description; got != "Alpha does things" {
		t.Fatalf("alpha description = %q", got)
	}
	if got := byName["beta"].Description; got != "Beta heading" {
		t.Fatalf("beta description = %q", got)
	}
	if got := byName["gamma"].Description; got != "Gamma" {
		t.Fatalf("gamma description = %q", got)
	}
	if got := byName["malformed"].Description; got != "Malformed heading" {
		t.Fatalf("malformed description = %q", got)
	}
	if got := byName["dup"].Description; got != "Dup first" {
		t.Fatalf("duplicate name should keep the first directory: %q", got)
	}
	if !strings.HasSuffix(byName["alpha"].Path, filepath.Join("alpha", "SKILL.md")) {
		t.Fatalf("alpha path = %q", byName["alpha"].Path)
	}

	loaded, err := store.Read(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(loaded.Body, "secretword") {
		t.Fatalf("alpha body = %q", loaded.Body)
	}
	if _, err := store.Read(context.Background(), "nope"); err == nil {
		t.Fatal("Read(unknown) should fail")
	}
}

func TestFileStoreMissingAndEmptyDirs(t *testing.T) {
	root := t.TempDir()
	store := skill.NewFileStore(filepath.Join(root, "missing"), filepath.Join(root, "empty"))
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	skills, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(skills) != 0 {
		t.Fatalf("skills = %v, want none", skills)
	}
	// An empty path list is valid too.
	if skills, err := skill.NewFileStore().List(context.Background()); err != nil || len(skills) != 0 {
		t.Fatalf("empty store = %v, %v", skills, err)
	}
}

func TestFileStoreSymlinkEscape(t *testing.T) {
	root, first, _ := newSkillTree(t)
	outside := filepath.Join(root, "outside.md")
	writeFile(t, outside, "# Outside\nshould not be listed\n")
	if err := os.Symlink(outside, filepath.Join(first, "evil.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	store := skill.NewFileStore(first)
	skills, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, entry := range skills {
		if entry.Name == "evil" {
			t.Fatalf("symlink escaping the skill directory was listed: %+v", entry)
		}
	}
}

func TestFileStoreToolIntegration(t *testing.T) {
	_, first, second := newSkillTree(t)
	byName := toolMap(skill.Tools(skill.NewFileStore(first, second)))
	result := invoke(t, byName, "skill", `{"action":"list"}`)
	if result.IsError || !strings.Contains(result.Content, "alpha") {
		t.Fatalf("list = %q (IsError=%v)", result.Content, result.IsError)
	}
	result = invoke(t, byName, "skill", `{"action":"search","query":"secretword"}`)
	if result.IsError || !strings.Contains(result.Content, "alpha") || strings.Contains(result.Content, "- zeta") {
		t.Fatalf("search body-only = %q", result.Content)
	}
	result = invoke(t, byName, "skill", `{"action":"load","name":"alpha"}`)
	if result.IsError || !strings.Contains(result.Content, "secretword") || !strings.Contains(result.Content, "name: alpha") {
		t.Fatalf("load = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestDefaultDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	customOne := t.TempDir()
	customTwo := t.TempDir()
	t.Setenv("ZEOKEEL_SKILLS", customOne+string(os.PathListSeparator)+customTwo)

	dirs := skill.DefaultDirs("/workspace")
	want := []string{customOne, customTwo, filepath.Join("/workspace", ".zeokeel", "skills"), filepath.Join(home, ".zeokeel", "skills")}
	if len(dirs) != len(want) {
		t.Fatalf("DefaultDirs = %v, want %v", dirs, want)
	}
	for index := range want {
		if dirs[index] != want[index] {
			t.Fatalf("DefaultDirs[%d] = %q, want %q", index, dirs[index], want[index])
		}
	}

	// NewFileStore on the default dirs finds a skill in $ZEOKEEL_SKILLS.
	writeFile(t, filepath.Join(customOne, "env-skill", "SKILL.md"), "# Env Skill\ndescription: from env\n")
	store := skill.NewDefaultFileStore("/workspace")
	skills, err := store.List(context.Background())
	if err != nil || len(skills) == 0 || skills[0].Name != "env-skill" {
		t.Fatalf("default store list = %+v, %v", skills, err)
	}
	// The tool with a nil store uses the same defaults.
	byName := toolMap(skill.Tools(nil))
	if result := invoke(t, byName, "skill", `{"action":"list"}`); result.IsError || !strings.Contains(result.Content, "env-skill") {
		t.Fatalf("nil store default list = %q", result.Content)
	}
}

func TestFileStoreConcurrentAccess(t *testing.T) {
	_, first, second := newSkillTree(t)
	store := skill.NewFileStore(first, second)
	var wg sync.WaitGroup
	for index := 0; index < 16; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if _, err := store.List(context.Background()); err != nil {
				t.Errorf("concurrent List: %v", err)
			}
			if index%2 == 0 {
				if _, err := store.Read(context.Background(), "alpha"); err != nil {
					t.Errorf("concurrent Read: %v", err)
				}
			}
		}(index)
	}
	wg.Wait()
}

func TestFileStoreTestdataSample(t *testing.T) {
	store := skill.NewFileStore("testdata")
	skills, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List(testdata): %v", err)
	}
	if len(skills) != 1 || skills[0].Name != "release-checklist" {
		t.Fatalf("testdata skills = %+v", skills)
	}
	if !strings.Contains(skills[0].Description, "release") || !strings.Contains(skills[0].Body, "Checklist") {
		t.Fatalf("testdata skill = %+v", skills[0])
	}
}
