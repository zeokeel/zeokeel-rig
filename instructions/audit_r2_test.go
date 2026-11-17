package instructions_test

// 第二轮：lead 拍板的三项加固（import 根限制 / 总量预算 / dirty 三态）。
// 先写用例（红）再实现（绿）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/instructions"
)

// repoWithFile 建一个带 .git 的仓库根，并返回外部目录（仓库之外）。
func repoWithFile(t *testing.T) (root string, outside string) {
	t.Helper()
	root = t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	outside = t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "TOKEN=outside\n")
	return root, outside
}

// --- WithImportRoot -------------------------------------------------------

// 默认（不传选项）就用发现到的仓库根：@../ 逃逸必须被拦住。
func TestImportRootDefaultBlocksParentEscape(t *testing.T) {
	root, outside := repoWithFile(t)
	writeFile(t, filepath.Join(root, "AGENTS.md"), "@../"+filepath.Base(outside)+"/secret.txt\nafter\n")

	set := mustLoad(t, root, instructions.WithUserPath(""))
	doc := localDoc(t, set, root)
	if strings.Contains(doc.Content, "TOKEN=outside") {
		t.Fatalf("repo 外文件被内联: %q", doc.Content)
	}
	if !strings.Contains(doc.Content, "<!-- import not found:") {
		t.Fatalf("content = %q, want a not-found comment in place", doc.Content)
	}
	if !strings.Contains(doc.Content, "after") {
		t.Fatalf("content = %q, want the following line preserved", doc.Content)
	}
}

func TestImportRootBlocksAbsoluteEscape(t *testing.T) {
	root, outside := repoWithFile(t)
	absolute := filepath.Join(outside, "secret.txt")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "@"+absolute+"\n")

	set := mustLoad(t, root, instructions.WithUserPath(""))
	if got := localDoc(t, set, root).Content; strings.Contains(got, "TOKEN=outside") {
		t.Fatalf("绝对路径逃逸未被拦住: %q", got)
	}
}

func TestImportRootBlocksSymlinkEscape(t *testing.T) {
	root, outside := repoWithFile(t)
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.md")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	writeFile(t, filepath.Join(root, "AGENTS.md"), "@./link.md\n")

	set := mustLoad(t, root, instructions.WithUserPath(""))
	got := localDoc(t, set, root).Content
	if strings.Contains(got, "TOKEN=outside") {
		t.Fatalf("symlink 逃逸未被拦住: %q", got)
	}
	if !strings.Contains(got, "<!-- import not found: ./link.md -->") {
		t.Fatalf("content = %q, want a not-found comment", got)
	}
}

func TestImportRootAllowsSymlinkInsideRoot(t *testing.T) {
	root, _ := repoWithFile(t)
	writeFile(t, filepath.Join(root, "docs", "real.md"), "INSIDE\n")
	if err := os.Symlink(filepath.Join(root, "docs", "real.md"), filepath.Join(root, "alias.md")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	writeFile(t, filepath.Join(root, "AGENTS.md"), "@./alias.md\n")

	set := mustLoad(t, root, instructions.WithUserPath(""))
	if got := localDoc(t, set, root).Content; got != "INSIDE\n" {
		t.Fatalf("content = %q, want the in-root symlink expanded", got)
	}
}

func TestImportRootAllowsNormalImport(t *testing.T) {
	root, _ := repoWithFile(t)
	writeFile(t, filepath.Join(root, "docs", "guide.md"), "GUIDE\n")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "before\n@./docs/guide.md\nafter\n")

	set := mustLoad(t, root, instructions.WithUserPath(""))
	if got := localDoc(t, set, root).Content; got != "before\nGUIDE\nafter\n" {
		t.Fatalf("content = %q, want the normal import expanded", got)
	}
}

// 空串 = 不限制（保旧行为）。
func TestImportRootEmptyDisablesRestriction(t *testing.T) {
	root, outside := repoWithFile(t)
	writeFile(t, filepath.Join(root, "AGENTS.md"), "@../"+filepath.Base(outside)+"/secret.txt\n")

	set := mustLoad(t, root, instructions.WithUserPath(""), instructions.WithImportRoot(""))
	if got := localDoc(t, set, root).Content; !strings.Contains(got, "TOKEN=outside") {
		t.Fatalf("content = %q, want the unrestricted (legacy) behaviour", got)
	}
}

// 宿主显式传入的 user/extra 文档不受限：它们不是仓库内容。
func TestImportRootExtraDocumentIsUnrestricted(t *testing.T) {
	root, outside := repoWithFile(t)
	extra := filepath.Join(t.TempDir(), "user.md")
	writeFile(t, extra, "@../"+filepath.Base(outside)+"/secret.txt\n")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root\n")

	set := mustLoad(t, root, instructions.WithUserPath(""), instructions.WithExtraPaths(extra))
	found := false
	for _, doc := range set.Documents {
		if doc.Path == extra {
			found = true
			if !strings.Contains(doc.Content, "TOKEN=outside") {
				t.Fatalf("extra doc content = %q, want unrestricted imports", doc.Content)
			}
		}
	}
	if !found {
		t.Fatalf("extra document not loaded: %v", docPaths(set.Documents))
	}
}

// 显式根可以比仓库根更窄。
func TestImportRootCustomSubdir(t *testing.T) {
	root, _ := repoWithFile(t)
	writeFile(t, filepath.Join(root, "docs", "outside-sub.md"), "OUTSIDE-SUB\n")
	writeFile(t, filepath.Join(root, "sub", "inside.md"), "INSIDE-SUB\n")
	writeFile(t, filepath.Join(root, "sub", "AGENTS.md"), "@../docs/outside-sub.md\n@./inside.md\n")

	set := mustLoad(t, filepath.Join(root, "sub"), instructions.WithUserPath(""), instructions.WithImportRoot(filepath.Join(root, "sub")))
	doc := findDoc(t, set, filepath.Join(root, "sub", "AGENTS.md"))
	if strings.Contains(doc.Content, "OUTSIDE-SUB") {
		t.Fatalf("content = %q, sub 根之外的 import 必须被拦住", doc.Content)
	}
	if !strings.Contains(doc.Content, "INSIDE-SUB") {
		t.Fatalf("content = %q, 根内的 import 应当展开", doc.Content)
	}
}

// --- WithTotalBytes -------------------------------------------------------

// 超预算时按优先级从高到低保留：local 先于 root。
func TestTotalBytesKeepsMostSpecificDocuments(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	blob := strings.Repeat("r", 60<<10)
	writeFile(t, filepath.Join(root, "AGENTS.md"), blob)
	writeFile(t, filepath.Join(root, "sub", "AGENTS.md"), blob)

	set := mustLoad(t, filepath.Join(root, "sub"), instructions.WithUserPath(""), instructions.WithTotalBytes(100<<10))
	rootDoc := findDoc(t, set, filepath.Join(root, "AGENTS.md"))
	localDoc := findDoc(t, set, filepath.Join(root, "sub", "AGENTS.md"))
	if localDoc.Content != blob {
		t.Fatalf("local doc was dropped; content len=%d", len(localDoc.Content))
	}
	if rootDoc.Content != "<!-- document skipped: total budget exceeded -->" {
		t.Fatalf("root doc = %q, want the skip placeholder", rootDoc.Content)
	}
	if set.SkippedDocuments() != 1 {
		t.Fatalf("SkippedDocuments() = %d, want 1", set.SkippedDocuments())
	}
	rendered := set.Render(root)
	if !strings.Contains(rendered, "<!-- document skipped: total budget exceeded -->") {
		t.Fatalf("rendered = %q, want the placeholder at the skipped document's position", rendered)
	}
	if !strings.Contains(rendered, "[instructions truncated: 1 document(s) skipped]") {
		t.Fatalf("rendered = %q, want the truncation footer", rendered)
	}
}

// 默认 256 KiB：7 份 64 KiB 文档只保留 4 份（最具体的 4 份）。
func TestTotalBytesDefaultBudget(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	blob := strings.Repeat("x", instructions.DefaultMaxBytes)
	dir := root
	for depth := 0; depth < 6; depth++ {
		writeFile(t, filepath.Join(dir, "AGENTS.md"), blob)
		dir = filepath.Join(dir, "d")
		mkdir(t, dir)
	}
	writeFile(t, filepath.Join(dir, "AGENTS.md"), blob)

	set := mustLoad(t, dir, instructions.WithUserPath(""))
	if got, want := len(set.Documents), 7; got != want {
		t.Fatalf("documents = %d, want %d", got, want)
	}
	if got, want := set.SkippedDocuments(), 3; got != want {
		t.Fatalf("SkippedDocuments() = %d, want %d (256 KiB / 64 KiB)", got, want)
	}
	// 被保留的一定是最靠近 cwd 的那几份：最后一份（local）必须是原文。
	last := set.Documents[len(set.Documents)-1]
	if len(last.Content) != len(blob) {
		t.Fatalf("local document was truncated or skipped: %d bytes", len(last.Content))
	}
	// 最远的 root 必须被跳过。
	if set.Documents[0].Content != "<!-- document skipped: total budget exceeded -->" {
		t.Fatalf("root document = %q, want the skip placeholder", set.Documents[0].Content)
	}
	rendered := set.Render(root)
	if !strings.Contains(rendered, "[instructions truncated: 3 document(s) skipped]") {
		t.Fatalf("rendered footer missing")
	}
	if got := len(rendered); got > instructions.DefaultTotalBytes+3*200+4096 {
		t.Fatalf("rendered = %d bytes, budget not enforced", got)
	}
}

// 未超预算时不得出现占位或尾注。
func TestTotalBytesNoFooterWhenUnderBudget(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "small\n")
	set := mustLoad(t, root, instructions.WithUserPath(""))
	if set.SkippedDocuments() != 0 {
		t.Fatalf("SkippedDocuments() = %d, want 0", set.SkippedDocuments())
	}
	rendered := set.Render(root)
	if strings.Contains(rendered, "document skipped") || strings.Contains(rendered, "[instructions truncated") {
		t.Fatalf("rendered = %q, want no skip markers", rendered)
	}
}

// <=0 用默认：这份补丁远超默认预算。
func TestTotalBytesNonPositiveUsesDefault(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	blob := strings.Repeat("y", instructions.DefaultMaxBytes)
	dir := root
	for depth := 0; depth < 5; depth++ {
		writeFile(t, filepath.Join(dir, "AGENTS.md"), blob)
		dir = filepath.Join(dir, "n")
		mkdir(t, dir)
	}
	writeFile(t, filepath.Join(dir, "AGENTS.md"), blob)
	set := mustLoad(t, dir, instructions.WithUserPath(""), instructions.WithTotalBytes(0))
	if set.SkippedDocuments() == 0 {
		t.Fatal("WithTotalBytes(0) did not fall back to the default budget")
	}
}

// --- dirty 三态 -----------------------------------------------------------

// 零值必须是 unknown：本包从不检测脏状态，不能声称工作区干净。
func TestDirtyDefaultsToUnknown(t *testing.T) {
	env := instructions.Environment{WorkspaceRoot: "/ws", Cwd: "/ws", OS: "linux", Arch: "amd64", Time: time.Now(), GitBranch: "main", TempDir: "/tmp"}
	got := env.Render()
	if !strings.Contains(got, "git: branch=main dirty=unknown") {
		t.Fatalf("Render = %q, want dirty=unknown by default", got)
	}
	if strings.Contains(got, "dirty=no") {
		t.Fatalf("Render = %q, zero value must not claim a clean tree", got)
	}
}

func TestDirtyExplicitStates(t *testing.T) {
	cases := []struct {
		state instructions.GitDirtyState
		want  string
	}{
		{instructions.GitDirtyUnknown, "dirty=unknown"},
		{instructions.GitDirtyNo, "dirty=no"},
		{instructions.GitDirtyYes, "dirty=yes"},
	}
	for _, tc := range cases {
		env := instructions.Environment{GitBranch: "main", GitDirty: tc.state, OS: "linux", Arch: "amd64", Time: time.Now()}
		if got := env.Render(); !strings.Contains(got, tc.want) {
			t.Fatalf("state %v: Render = %q, want %q", tc.state, got, tc.want)
		}
	}
}
