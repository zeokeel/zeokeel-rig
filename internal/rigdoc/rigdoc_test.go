package rigdoc

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/audit"
	"github.com/zeokeel/zeokeel-rig/tools/catalog"
	"github.com/zeokeel/zeokeel-rig/tools/full"
)

func TestCatalogShape(t *testing.T) {
	docs := BuildCatalog()
	if len(docs) != len(full.All(full.AllOptions{Root: t.TempDir()}))+1 {
		t.Fatalf("catalog has %d tools, want full.All + run_code", len(docs))
	}
	seen := make(map[string]bool, len(docs))
	for _, doc := range docs {
		if doc.Name == "" || doc.Description == "" || doc.Category == "" {
			t.Fatalf("incomplete tool doc: %+v", doc)
		}
		if seen[doc.Name] {
			t.Fatalf("duplicate tool %q", doc.Name)
		}
		seen[doc.Name] = true
	}
	if !seen["run_code"] {
		t.Fatal("run_code missing from catalog")
	}
}

func TestGeneratedFilesAreCurrent(t *testing.T) {
	root := filepath.Join("..", "..")
	docs := BuildCatalog()
	wantJSON := JSON(docs)
	wantMarkdown := []byte(Markdown(docs))
	for path, want := range map[string][]byte{
		filepath.Join(root, "docs", "ai", "tools.json"): wantJSON,
		filepath.Join(root, "docs", "ai", "tools.md"):   wantMarkdown,
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s is stale; run: go run ./cmd/rigdoc", path)
		}
	}
}

func TestDocumentationCounts(t *testing.T) {
	root := filepath.Join("..", "..")
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}
	staticCount := len(full.All(full.AllOptions{Root: t.TempDir()}))
	modernCount := len(catalog.Modern)
	auditModernCount := len(audit.Modern)

	readme := read("README.md")
	if !strings.Contains(readme, "v"+rig.Version) {
		t.Errorf("README does not mention current version v%s", rig.Version)
	}
	toolsDoc := read("docs/TOOLS.md")
	match := regexp.MustCompile(`Static tools built by[^|]*\|\s*\*\*(\d+)\*\*`).FindStringSubmatch(toolsDoc)
	if match == nil {
		t.Fatalf("docs/TOOLS.md has no static-tool count row")
	}
	if got, _ := strconv.Atoi(match[1]); got != staticCount {
		t.Errorf("docs/TOOLS.md static count = %d, want %d", got, staticCount)
	}
	coverage := read("docs/COVERAGE.md")
	for _, want := range []string{
		strconv.Itoa(staticCount) + " static tools",
		strconv.Itoa(modernCount) + " modern names",
		"modern=" + strconv.Itoa(auditModernCount),
	} {
		if !strings.Contains(coverage, want) {
			t.Errorf("docs/COVERAGE.md is missing %q", want)
		}
	}
	zhCoverage := read("docs/zh/COVERAGE.md")
	if !strings.Contains(zhCoverage, strconv.Itoa(staticCount)+" 个无需外部进程") {
		t.Errorf("docs/zh/COVERAGE.md static count is stale (want %d)", staticCount)
	}
	changelog := read("CHANGELOG.md")
	if !strings.Contains(changelog, "## v"+rig.Version) {
		t.Errorf("CHANGELOG.md has no v%s section", rig.Version)
	}
}

func TestWriteAndCheck(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "tools.json")
	mdPath := filepath.Join(dir, "tools.md")
	if err := Write(jsonPath, mdPath); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := Check(jsonPath, mdPath); err != nil {
		t.Fatalf("Check after Write: %v", err)
	}
	if err := Check(filepath.Join(dir, "missing.json"), mdPath); err == nil {
		t.Fatal("Check accepted a missing JSON file")
	}
	if err := os.WriteFile(mdPath, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	if err := Check(jsonPath, mdPath); err == nil {
		t.Fatal("Check accepted a stale Markdown file")
	}
}
