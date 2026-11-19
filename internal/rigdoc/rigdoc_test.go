package rigdoc

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/catalog"
	"github.com/zeokeel/zeokeel-rig/tools/full"
)

func TestCatalogShape(t *testing.T) {
	docs := BuildCatalog()
	// 目录覆盖"可构造"的工具面：full.All 在 SandboxGrants 下会追加
	// sandbox_status 与 request_sandbox_access，再加上 kit 的 run_code。
	wantNames := make([]string, 0, len(docs))
	for _, tool := range full.All(full.AllOptions{Root: t.TempDir(), SandboxGrants: true}) {
		wantNames = append(wantNames, tool.Name())
	}
	wantNames = append(wantNames, "run_code")
	sort.Strings(wantNames)
	if want := len(full.All(full.AllOptions{Root: t.TempDir(), SandboxGrants: true})) + 1; len(docs) != want {
		t.Fatalf("catalog has %d tools, want full.All(SandboxGrants) + run_code = %d", len(docs), want)
	}
	seen := make(map[string]bool, len(docs))
	gotNames := make([]string, 0, len(docs))
	for _, doc := range docs {
		if doc.Name == "" || doc.Description == "" || doc.Category == "" {
			t.Fatalf("incomplete tool doc: %+v", doc)
		}
		if seen[doc.Name] {
			t.Fatalf("duplicate tool %q", doc.Name)
		}
		seen[doc.Name] = true
		gotNames = append(gotNames, doc.Name)
	}
	if !seen["run_code"] {
		t.Fatal("run_code missing from catalog")
	}
	// 名字级比对：只数个数发现不了
	// "目录里少了一个真工具、多了一个幻觉工具"——两者一抵消计数不变，而 docs/ai
	// 是模型读的东西，多一个不存在的工具就是教模型调用不存在的东西。
	sort.Strings(gotNames)
	if !slices.Equal(gotNames, wantNames) {
		missing, extra := diffNames(wantNames, gotNames)
		t.Fatalf("catalog 名字集与 full.All + run_code 不一致：少了 %v，多了 %v", missing, extra)
	}
}

// diffNames 返回 want 有 got 没有、got 有 want 没有的名字。
func diffNames(want, got []string) (missing, extra []string) {
	for _, name := range want {
		if !slices.Contains(got, name) {
			missing = append(missing, name)
		}
	}
	for _, name := range got {
		if !slices.Contains(want, name) {
			extra = append(extra, name)
		}
	}
	return missing, extra
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
	staticCount := len(catalog.Static)
	modernCount := len(catalog.Modern)

	readme := read("README.md")
	if !strings.Contains(readme, "v"+rig.Version) {
		t.Errorf("README does not mention current version v%s", rig.Version)
	}
	// docs/COVERAGE.md 必须保留一行权威口径（与 `go test ./tools/full` 打印的
	// catalog complete 行同形）；文档改版时这行是机器可读的锚点。
	coverage := read("docs/COVERAGE.md")
	line := regexp.MustCompile(`catalog complete: (\d+) static tools, (\d+) legacy names, (\d+) modern names, (\d+) documented gaps, (\d+) server-bound`).FindStringSubmatch(coverage)
	if line == nil {
		t.Fatalf("docs/COVERAGE.md has no `catalog complete:` line")
	}
	wantLine := []int{staticCount, len(catalog.Legacy), modernCount, len(catalog.DocumentedGaps), len(catalog.ServerBound)}
	for i, want := range wantLine {
		got, _ := strconv.Atoi(line[i+1])
		if got != want {
			t.Errorf("docs/COVERAGE.md catalog complete column %d = %d, want %d", i+1, got, want)
		}
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
