package search

// 审计修复的回归用例（v0.20）：
//   1) 超时治理到评分/排序循环；
//   2) mentions 模式感知 path；
//   3) auto 只在以 @ 开头时切 mentions，并在 text 模式附 @引用提示；
//   4) path / mentions 的分隔符态度统一。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countdownContext 的 Err() 只在被调用超过 budget 次之后才返回超时。
// 用来证明"循环里真的是周期性检查 ctx"，而不是只在开头查一次。
type countdownContext struct {
	context.Context
	remaining int32
}

func (c *countdownContext) Err() error {
	if atomic.AddInt32(&c.remaining, -1) < 0 {
		return context.DeadlineExceeded
	}
	return nil
}

// Done 永不关闭：本用例只依赖 Err()。
func (c *countdownContext) Done() <-chan struct{} { return nil }

// ---------------------------------------------------------------------------
// 1) 超时治理到评分/排序
// ---------------------------------------------------------------------------

func TestAuditScoringLoopHonoursContext(t *testing.T) {
	files := make([]sourceFile, 0, scoringCheckInterval*3)
	for index := 0; index < scoringCheckInterval*3; index++ {
		files = append(files, sourceFile{rel: fmt.Sprintf("pkg/file%05d.go", index)})
	}
	// 允许前 2 次检查通过（第 0 与第 256 个文件），第 3 次（第 512 个）超时。
	ctx := &countdownContext{Context: context.Background(), remaining: 2}
	content, err := scoreCandidateFiles(ctx, files, "file", DefaultMaxResults)
	if err == nil {
		t.Fatalf("scoring loop ignored the expired context and returned %d bytes", len(content))
	}
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "exceeded") &&
		!strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want a deadline/exceeded error", err)
	}
}

func TestAuditBM25SearchHonoursContext(t *testing.T) {
	index := newBM25Index()
	for docIndex := 0; docIndex < scoringCheckInterval*3; docIndex++ {
		index.add(fmt.Sprintf("doc%05d.txt", docIndex), []byte("alpha beta gamma"))
	}
	index.finalize()
	ctx := &countdownContext{Context: context.Background(), remaining: 2}
	if _, err := index.search(ctx, Tokenize("alpha beta")); err == nil {
		t.Fatal("bm25 scoring loop ignored the expired context")
	}
	// 未过期的 ctx 仍然正常工作。
	if matches, err := index.search(context.Background(), Tokenize("alpha beta")); err != nil || len(matches) == 0 {
		t.Fatalf("unexpired context: %d matches, err=%v", len(matches), err)
	}
}

func TestAuditTimeoutIsExplicitOnBigCorpus(t *testing.T) {
	// 行为级断言：极短预算下不允许"成功返回"，必须是明确的超时错误。
	root := t.TempDir()
	for index := 0; index < 400; index++ {
		dir := filepath.Join(root, fmt.Sprintf("d%02d", index%20))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		name := filepath.Join(dir, fmt.Sprintf("f%04d.txt", index))
		if err := os.WriteFile(name, []byte("alpha beta gamma\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	tool := newTool(t, root, WithTimeout(time.Nanosecond))
	for _, mode := range []string{"files", "text"} {
		arguments := fmt.Sprintf(`{"query":"alpha beta","mode":%q}`, mode)
		result, err := invokeRaw(t, tool, arguments)
		if err != nil {
			continue // 父 ctx 取消也算明确失败
		}
		if !result.IsError {
			t.Fatalf("mode=%s with a 1ns budget returned success (%q), want an explicit timeout",
				mode, result.Content)
		}
		if !strings.Contains(result.Content, "timed out") {
			t.Fatalf("mode=%s timeout message = %q, want it to mention timed out", mode, result.Content)
		}
	}
}

// ---------------------------------------------------------------------------
// 2) mentions 模式感知 path
// ---------------------------------------------------------------------------

func TestAuditMentionsResolveRelativeToPath(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.txt":     "root a\n",
		"b.txt":     "root b\n",
		"src/a.txt": "src a\n",
		"docs/x.md": "docs\n",
	})
	tool := newTool(t, root)

	// 不带 path：相对根解析。
	if got := invoke(t, tool, `{"query":"@a.txt","mode":"mentions"}`); got.Content != "a.txt" {
		t.Fatalf("mentions without path = %q, want \"a.txt\"", got.Content)
	}
	// 带 path：相对 path 解析，且输出是根相对路径。
	if got := invoke(t, tool, `{"query":"@a.txt","mode":"mentions","path":"src"}`); got.Content != "src/a.txt" {
		t.Fatalf("mentions with path=src = %q, want \"src/a.txt\"", got.Content)
	}
	// path 下不存在 → missing（即使根下有同名文件）。
	got := invoke(t, tool, `{"query":"@b.txt","mode":"mentions","path":"src"}`)
	if got.Content != "b.txt (missing)" {
		t.Fatalf("mentions with path=src and root-only file = %q, want \"b.txt (missing)\"", got.Content)
	}
	// 行范围与 path 组合。
	if got := invoke(t, tool, `{"query":"@a.txt:3-5","mode":"mentions","path":"src"}`); got.Content != "src/a.txt:3-5" {
		t.Fatalf("mentions with path and range = %q, want \"src/a.txt:3-5\"", got.Content)
	}
	// 用 .. 从 path 回退但仍在根内：允许，并归一成根相对路径。
	if got := invoke(t, tool, `{"query":"@../a.txt","mode":"mentions","path":"src"}`); got.Content != "a.txt" {
		t.Fatalf("mentions with .. = %q, want \"a.txt\"", got.Content)
	}
	// 逃出根：missing（不是错误，也不是泄露）。
	escaped := invoke(t, tool, `{"query":"@../../etc/passwd","mode":"mentions","path":"src"}`)
	if !strings.Contains(escaped.Content, "(missing)") && escaped.Content != "" {
		t.Fatalf("escaping mention = %q, want missing", escaped.Content)
	}
}

// ---------------------------------------------------------------------------
// 3) auto 只在以 @ 开头时切 mentions；text 模式附提示
// ---------------------------------------------------------------------------

func TestAuditAutoOnlyMentionsWhenQueryStartsWithAt(t *testing.T) {
	root := writeTree(t, map[string]string{
		"src/a.go": "package src\n// needle function\n",
	})
	tool := newTool(t, root)

	// 以 @ 开头 → mentions，且**不**附提示（已经在解析引用）。
	leading := invoke(t, tool, `{"query":"@src/a.go:12"}`)
	if leading.Content != "src/a.go:12" {
		t.Fatalf("leading mention = %q, want \"src/a.go:12\"", leading.Content)
	}
	if strings.Contains(leading.Content, "# hints:") {
		t.Fatalf("mentions mode should not carry the hint: %q", leading.Content)
	}

	// 自然语言整句含 @ → text 模式 + 提示行（不再是只回一条 mention）。
	prose := invoke(t, tool, `{"query":"解释 @src/a.go:12 这个函数"}`)
	if prose.IsError {
		t.Fatalf("unexpected error: %s", prose.Content)
	}
	if !strings.Contains(prose.Content, "# hints: contains @references") {
		t.Fatalf("prose with @ = %q, want the mention hint", prose.Content)
	}
	if prose.Content == "src/a.go:12" {
		t.Fatal("prose with @ still collapsed into a bare mention")
	}

	// 显式 mode=text 也提示。
	explicit := invoke(t, tool, `{"query":"needle @src/a.go:12","mode":"text"}`)
	if !strings.Contains(explicit.Content, "# hints: contains @references") {
		t.Fatalf("mode=text with @ = %q, want the hint", explicit.Content)
	}
	// 没有 @ 的查询不提示。
	plain := invoke(t, tool, `{"query":"needle","mode":"text"}`)
	if strings.Contains(plain.Content, "# hints:") {
		t.Fatalf("plain query carry a hint: %q", plain.Content)
	}
	// files 模式不提示（提示只属于"内容检索没解析引用"的场景）。
	files := invoke(t, tool, `{"query":"a.go","mode":"files"}`)
	if strings.Contains(files.Content, "# hints:") {
		t.Fatalf("files mode carry a hint: %q", files.Content)
	}
}

// ---------------------------------------------------------------------------
// 4) 分隔符态度统一
// ---------------------------------------------------------------------------

func TestAuditPathAcceptsBothSeparators(t *testing.T) {
	root := writeTree(t, map[string]string{
		"src/deep/a.txt": "alpha beta\n",
	})
	tool := newTool(t, root)
	// 反斜杠写法与正斜杠等价（Linux 上也一样）。
	for _, path := range []string{"src/deep", `src\deep`, `src\deep\`} {
		arguments := fmt.Sprintf(`{"query":"alpha","mode":"text","path":%q}`, path)
		got := invoke(t, tool, arguments)
		if got.IsError {
			t.Fatalf("path=%q rejected: %s", path, got.Content)
		}
		if !strings.Contains(got.Content, "src/deep/a.txt") {
			t.Fatalf("path=%q result = %q", path, got.Content)
		}
	}
	// 反斜杠穿越必须被归一化后拒绝（不能当成一个合法文件名侥幸放过）。
	for _, path := range []string{`..\..`, `..\..\etc`, `src\..\..\..\etc`} {
		arguments := fmt.Sprintf(`{"query":"alpha","mode":"text","path":%q}`, path)
		got := invoke(t, tool, arguments)
		if !got.IsError {
			t.Fatalf("traversal path=%q was accepted: %q", path, got.Content)
		}
	}
	// mentions 同样接受反斜杠写法。
	if got := invoke(t, tool, `{"query":"@src\\deep\\a.txt","mode":"mentions"}`); got.Content != "src/deep/a.txt" {
		t.Fatalf("backslash mention = %q, want \"src/deep/a.txt\"", got.Content)
	}
}
