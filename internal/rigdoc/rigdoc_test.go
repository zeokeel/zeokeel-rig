package rigdoc

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/catalog"
	"github.com/zeokeel/zeokeel-rig/tools/full"
)

func TestCatalogShape(t *testing.T) {
	docs := BuildCatalog()
	// 目录覆盖两层名字：(1) 当前配置下真实构造的工具——full.All 在 SandboxGrants
	// 下会追加 sandbox_status 与 request_sandbox_access，再加上 kit 的 run_code；
	// (2) 登记在 catalog.Optional 里、当前配置**没构造**的条目（例如没有
	// SpillStore 时的 spill_read）：必须写成 requires <字段>，不能整条消失。
	constructed := make(map[string]bool)
	wantNames := make([]string, 0, len(docs))
	for _, tool := range full.All(full.AllOptions{Root: t.TempDir(), SandboxGrants: true}) {
		constructed[tool.Name()] = true
		wantNames = append(wantNames, tool.Name())
	}
	constructed["run_code"] = true
	wantNames = append(wantNames, "run_code")
	for _, entry := range catalog.Optional {
		if !constructed[entry.Name] {
			wantNames = append(wantNames, entry.Name)
		}
	}
	sort.Strings(wantNames)
	if len(docs) != len(wantNames) {
		t.Fatalf("catalog has %d tools, want constructed %d + declared-absent optional = %d",
			len(docs), len(constructed), len(wantNames))
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
		// 没被构造的名字只能是"声明过的缺席条件工具"：否则就是幻觉工具
		// （docs/ai 是模型读的东西，多一个不存在的工具就是教模型调用它）。
		if !constructed[doc.Name] && (!doc.Optional || !strings.HasPrefix(doc.Availability, "requires ")) {
			t.Fatalf("未构造的工具 %q 必须以 Optional + requires <字段> 出现: %+v", doc.Name, doc)
		}
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
		t.Fatalf("catalog 名字集与 full.All + run_code + 缺席条件工具不一致：少了 %v，多了 %v", missing, extra)
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
