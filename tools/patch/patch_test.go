package patch_test

// apply_patch 的黑盒测试：每个用例都在临时工作区根上构造一个工具，只通过
// Invoke 观察行为，不碰任何私有实现细节。
//
// 用例组织成三组：
//   - 格式解析（统一 diff / 应用式补丁两种语法与它们的错误分支）；
//   - 匹配算法（三级空白容错、上下文偏移、歧义拒绝、找不到时的报错内容）；
//   - 落盘语义（原子性、dry_run、权限/换行保留、路径安全、二进制与大小上限）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/patch"
)

// fixture 是一个绑定到临时工作区根的 apply_patch 工具。
type fixture struct {
	root string
	tool rig.Tool
}

// workspacePolicy 返回以 root 为唯一可写根的 workspace-write 策略。
//
// apply_patch 现在强制要求策略（漏传时每次调用都报错），因此"测补丁逻辑"的
// 用例统一用这一份：它不限制根内的写入，也就不会遮盖被测行为。
func workspacePolicy(root string) *sandbox.Policy {
	return sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
}

// newFixture 在新建的临时目录上构造工具；opts 追加在默认选项之后（可用
// patch.WithPolicy 覆盖默认策略）。
func newFixture(t *testing.T, opts ...patch.Option) *fixture {
	t.Helper()
	root := t.TempDir()
	all := append([]patch.Option{patch.WithPolicy(workspacePolicy(root))}, opts...)
	return &fixture{root: root, tool: patch.New(root, all...)}
}

// invokeJSON 用原始 JSON 参数调用工具，并断言没有发生 Go 层错误。
func (f *fixture) invokeJSON(t *testing.T, arguments string) rig.ToolResult {
	t.Helper()
	result, err := f.tool.Invoke(context.Background(), rig.ToolCall{Name: patch.ToolName, Arguments: arguments})
	if err != nil {
		t.Fatalf("Invoke(%s): %v", arguments, err)
	}
	return result
}

// invoke 用补丁文本构造参数调用工具。
func (f *fixture) invoke(t *testing.T, patchText string) rig.ToolResult {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"patch": patchText})
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	return f.invokeJSON(t, string(payload))
}

// invokeDryRun 以单次调用的 dry_run=true 调用工具。
func (f *fixture) invokeDryRun(t *testing.T, patchText string) rig.ToolResult {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"patch": patchText, "dry_run": true})
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	return f.invokeJSON(t, string(payload))
}

// write 在根下写一个文件并返回绝对路径。
func (f *fixture) write(t *testing.T, relative, content string) string {
	t.Helper()
	absolute := filepath.Join(f.root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", relative, err)
	}
	if err := os.WriteFile(absolute, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", relative, err)
	}
	return absolute
}

// writeMode 在根下写一个指定权限的文件。
func (f *fixture) writeMode(t *testing.T, relative, content string, mode os.FileMode) string {
	t.Helper()
	absolute := f.write(t, relative, content)
	if err := os.Chmod(absolute, mode); err != nil {
		t.Fatalf("chmod %s: %v", relative, err)
	}
	return absolute
}

// read 读取根下文件的内容；不存在或读取失败直接失败。
func (f *fixture) read(t *testing.T, relative string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	return string(data)
}

// exists 报告根下文件是否存在。
func (f *fixture) exists(t *testing.T, relative string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(f.root, filepath.FromSlash(relative)))
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	t.Fatalf("stat %s: %v", relative, err)
	return false
}

// mode 返回根下文件的权限位。
func (f *fixture) mode(t *testing.T, relative string) os.FileMode {
	t.Helper()
	info, err := os.Stat(filepath.Join(f.root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatalf("stat %s: %v", relative, err)
	}
	return info.Mode().Perm()
}

// requireError 断言结果是一次模型可见的失败，并返回其文本。
func requireError(t *testing.T, result rig.ToolResult) string {
	t.Helper()
	if !result.IsError {
		t.Fatalf("result = %q, want IsError=true", result.Content)
	}
	if strings.TrimSpace(result.Content) == "" {
		t.Fatal("error result has empty content")
	}
	return result.Content
}

// requireOK 断言结果成功，并返回其文本。
func requireOK(t *testing.T, result rig.ToolResult) string {
	t.Helper()
	if result.IsError {
		t.Fatalf("result error = %q, want success", result.Content)
	}
	return result.Content
}

// requireContains 断言文本包含全部子串。
func requireContains(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("text = %q, want it to contain %q", text, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 统一 diff
// ---------------------------------------------------------------------------

func TestUnifiedSingleFileUpdate(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.txt", "one\ntwo\nthree\n")
	out := requireOK(t, f.invoke(t, `--- a/a.txt
+++ b/a.txt
@@ -1,3 +1,3 @@
 one
-two
+TWO
 three
`))
	if got, want := f.read(t, "a.txt"), "one\nTWO\nthree\n"; got != want {
		t.Fatalf("a.txt = %q, want %q", got, want)
	}
	requireContains(t, out, "update a.txt", "(+1 -1)")
}

func TestUnifiedMultiFileUpdate(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.txt", "a\n")
	f.write(t, "dir/b.txt", "b\n")
	out := requireOK(t, f.invoke(t, `--- a/a.txt
+++ b/a.txt
@@ -1 +1 @@
-a
+A
--- a/dir/b.txt
+++ b/dir/b.txt
@@ -1 +1,2 @@
-b
+B
+c
`))
	if got, want := f.read(t, "a.txt"), "A\n"; got != want {
		t.Fatalf("a.txt = %q, want %q", got, want)
	}
	if got, want := f.read(t, "dir/b.txt"), "B\nc\n"; got != want {
		t.Fatalf("dir/b.txt = %q, want %q", got, want)
	}
	requireContains(t, out, "2 file(s)", "+3 -2")
}

func TestUnifiedAddFileFromDevNull(t *testing.T) {
	f := newFixture(t)
	out := requireOK(t, f.invoke(t, `--- /dev/null
+++ b/new/created.txt
@@ -0,0 +1,2 @@
+hello
+world
`))
	if got, want := f.read(t, "new/created.txt"), "hello\nworld\n"; got != want {
		t.Fatalf("new/created.txt = %q, want %q", got, want)
	}
	requireContains(t, out, "add new/created.txt", "(+2 -0)")
}

func TestUnifiedDeleteFileToDevNull(t *testing.T) {
	f := newFixture(t)
	f.write(t, "gone.txt", "x\ny\n")
	out := requireOK(t, f.invoke(t, `--- a/gone.txt
+++ /dev/null
@@ -1,2 +0,0 @@
-x
-y
`))
	if f.exists(t, "gone.txt") {
		t.Fatal("gone.txt still exists after a delete patch")
	}
	requireContains(t, out, "delete gone.txt", "(+0 -2)")
}

func TestUnifiedRenameAppliedAsMove(t *testing.T) {
	f := newFixture(t)
	f.write(t, "old.txt", "keep\n")
	out := requireOK(t, f.invoke(t, `--- a/old.txt
+++ b/new.txt
@@ -1 +1 @@
-keep
+kept
`))
	if f.exists(t, "old.txt") {
		t.Fatal("old.txt still exists after a rename patch")
	}
	if got, want := f.read(t, "new.txt"), "kept\n"; got != want {
		t.Fatalf("new.txt = %q, want %q", got, want)
	}
	requireContains(t, out, "move old.txt -> new.txt")
}

func TestUnifiedCRLFPreserved(t *testing.T) {
	f := newFixture(t)
	f.write(t, "win.txt", "alpha\r\nbeta\r\ngamma\r\n")
	requireOK(t, f.invoke(t, "--- a/win.txt\n+++ b/win.txt\n@@ -1,3 +1,3 @@\n alpha\n-beta\n+BETA\n gamma\n"))
	if got, want := f.read(t, "win.txt"), "alpha\r\nBETA\r\ngamma\r\n"; got != want {
		t.Fatalf("win.txt = %q, want CRLF-preserved %q", got, want)
	}
}

func TestUnifiedNoTrailingNewlinePreserved(t *testing.T) {
	f := newFixture(t)
	f.write(t, "eof.txt", "a\nb")
	requireOK(t, f.invoke(t, "--- a/eof.txt\n+++ b/eof.txt\n@@ -1,2 +1,2 @@\n a\n-b\n+B\n"))
	if got, want := f.read(t, "eof.txt"), "a\nB"; got != want {
		t.Fatalf("eof.txt = %q, want %q", got, want)
	}
}

func TestUnifiedAddsTrailingNewlineWhenAppendingAfterNone(t *testing.T) {
	f := newFixture(t)
	f.write(t, "eof.txt", "a")
	requireOK(t, f.invoke(t, "--- a/eof.txt\n+++ b/eof.txt\n@@ -1 +1,2 @@\n a\n+b\n"))
	if got, want := f.read(t, "eof.txt"), "a\nb\n"; got != want {
		t.Fatalf("eof.txt = %q, want %q", got, want)
	}
}

func TestUnifiedContextOffsetApplied(t *testing.T) {
	f := newFixture(t)
	f.write(t, "shifted.txt", "x\ny\ntarget\nz\n")
	// 声明的行号是 1，上下文实际在第 3 行：必须容错找到。
	out := requireOK(t, f.invoke(t, "--- a/shifted.txt\n+++ b/shifted.txt\n@@ -1,2 +1,2 @@\n y\n-target\n+TARGET\n"))
	if got, want := f.read(t, "shifted.txt"), "x\ny\nTARGET\nz\n"; got != want {
		t.Fatalf("shifted.txt = %q, want %q", got, want)
	}
	requireContains(t, out, "update shifted.txt")
}

func TestUnifiedInsertOnlyHunk(t *testing.T) {
	f := newFixture(t)
	f.write(t, "ins.txt", "a\nb\n")
	requireOK(t, f.invoke(t, `--- a/ins.txt
+++ b/ins.txt
@@ -1,0 +2,2 @@
+x
+y
`))
	if got, want := f.read(t, "ins.txt"), "a\nx\ny\nb\n"; got != want {
		t.Fatalf("ins.txt = %q, want %q", got, want)
	}
}

func TestUnifiedMalformedMissingPlusHeader(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.txt", "a\n")
	text := requireError(t, f.invoke(t, "--- a/a.txt\n@@ -1 +1 @@\n-a\n+A\n"))
	requireContains(t, text, "+++")
	if got, want := f.read(t, "a.txt"), "a\n"; got != want {
		t.Fatalf("a.txt changed on a malformed patch: %q", got)
	}
}

func TestUnifiedHunkCountMismatch(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.txt", "a\nb\n")
	// 声明 -1,5 但正文只有两行：必须在解析阶段拒绝。
	text := requireError(t, f.invoke(t, "--- a/a.txt\n+++ b/a.txt\n@@ -1,5 +1,5 @@\n a\n-b\n+B\n"))
	requireContains(t, text, "hunk")
}

func TestUnifiedGarbageTextRejected(t *testing.T) {
	f := newFixture(t)
	text := requireError(t, f.invoke(t, "this is not a patch\n"))
	requireContains(t, text, "unrecognized patch")
}

// ---------------------------------------------------------------------------
// 应用式补丁格式
// ---------------------------------------------------------------------------

func TestCodexAddFile(t *testing.T) {
	f := newFixture(t)
	out := requireOK(t, f.invoke(t, `*** Begin Patch
*** Add File: src/added.go
+package main
+
+func main() {}
*** End Patch`))
	if got, want := f.read(t, "src/added.go"), "package main\n\nfunc main() {}\n"; got != want {
		t.Fatalf("src/added.go = %q, want %q", got, want)
	}
	requireContains(t, out, "add src/added.go", "(+3 -0)")
}

func TestCodexUpdateWithContext(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.txt", "keep\nold\ntail\n")
	requireOK(t, f.invoke(t, `*** Begin Patch
*** Update File: a.txt
@@
 keep
-old
+new
*** End Patch`))
	if got, want := f.read(t, "a.txt"), "keep\nnew\ntail\n"; got != want {
		t.Fatalf("a.txt = %q, want %q", got, want)
	}
}

func TestCodexUpdateWithAnchor(t *testing.T) {
	f := newFixture(t)
	f.write(t, "code.go", "func a() {}\n\nfunc b() {\n\told()\n}\n")
	requireOK(t, f.invoke(t, `*** Begin Patch
*** Update File: code.go
@@ func b() {
-	old()
+	new()
*** End Patch`))
	if got, want := f.read(t, "code.go"), "func a() {}\n\nfunc b() {\n\tnew()\n}\n"; got != want {
		t.Fatalf("code.go = %q, want %q", got, want)
	}
}

func TestCodexUpdateWithMissingAnchorRejected(t *testing.T) {
	f := newFixture(t)
	f.write(t, "code.go", "func a() {}\n")
	text := requireError(t, f.invoke(t, `*** Begin Patch
*** Update File: code.go
@@ func nope() {
-func a() {}
+func c() {}
*** End Patch`))
	requireContains(t, text, "code.go", "anchor")
}

func TestCodexDeleteFile(t *testing.T) {
	f := newFixture(t)
	f.write(t, "gone.txt", "bye\n")
	out := requireOK(t, f.invoke(t, `*** Begin Patch
*** Delete File: gone.txt
*** End Patch`))
	if f.exists(t, "gone.txt") {
		t.Fatal("gone.txt still exists after Delete File")
	}
	requireContains(t, out, "delete gone.txt")
}

func TestCodexMoveFile(t *testing.T) {
	f := newFixture(t)
	f.write(t, "from.txt", "same content\n")
	out := requireOK(t, f.invoke(t, `*** Begin Patch
*** Update File: from.txt
*** Move to: to.txt
*** End Patch`))
	if f.exists(t, "from.txt") {
		t.Fatal("from.txt still exists after Move to")
	}
	if got, want := f.read(t, "to.txt"), "same content\n"; got != want {
		t.Fatalf("to.txt = %q, want %q", got, want)
	}
	requireContains(t, out, "move from.txt -> to.txt")
}

func TestCodexMoveFileWithEdits(t *testing.T) {
	f := newFixture(t)
	f.write(t, "from.txt", "one\ntwo\n")
	out := requireOK(t, f.invoke(t, `*** Begin Patch
*** Update File: from.txt
@@
-one
+ONE
*** Move to: dir/to.txt
*** End Patch`))
	if f.exists(t, "from.txt") {
		t.Fatal("from.txt still exists after Move to")
	}
	if got, want := f.read(t, "dir/to.txt"), "ONE\ntwo\n"; got != want {
		t.Fatalf("dir/to.txt = %q, want %q", got, want)
	}
	requireContains(t, out, "move from.txt -> dir/to.txt", "(+1 -1)")
}

func TestCodexMissingBeginRejected(t *testing.T) {
	f := newFixture(t)
	text := requireError(t, f.invoke(t, "*** Add File: a.txt\n+x\n*** End Patch"))
	requireContains(t, text, "*** Begin Patch")
}

func TestCodexMissingEndRejected(t *testing.T) {
	f := newFixture(t)
	text := requireError(t, f.invoke(t, "*** Begin Patch\n*** Add File: a.txt\n+x\n"))
	requireContains(t, text, "*** End Patch")
}

func TestCodexEmptyPatchRejected(t *testing.T) {
	f := newFixture(t)
	text := requireError(t, f.invoke(t, "*** Begin Patch\n*** End Patch"))
	requireContains(t, text, "no file operations")
}

func TestCodexUnknownDirectiveRejected(t *testing.T) {
	f := newFixture(t)
	text := requireError(t, f.invoke(t, "*** Begin Patch\n*** Frobnicate File: a.txt\n*** End Patch"))
	requireContains(t, text, "unknown")
}

func TestCodexAddFileRequiresPlusLines(t *testing.T) {
	f := newFixture(t)
	text := requireError(t, f.invoke(t, "*** Begin Patch\n*** Add File: a.txt\nplain line\n*** End Patch"))
	requireContains(t, text, "+")
	if f.exists(t, "a.txt") {
		t.Fatal("a.txt was created by an invalid Add File block")
	}
}

// ---------------------------------------------------------------------------
// 上下文匹配
// ---------------------------------------------------------------------------

func TestWhitespaceTrailingTolerated(t *testing.T) {
	f := newFixture(t)
	f.write(t, "ws.txt", "alpha   \nbeta\n")
	requireOK(t, f.invoke(t, "--- a/ws.txt\n+++ b/ws.txt\n@@ -1,2 +1,2 @@\n alpha\n-beta\n+BETA\n"))
	if got, want := f.read(t, "ws.txt"), "alpha   \nBETA\n"; got != want {
		t.Fatalf("ws.txt = %q, want untouched trailing whitespace preserved %q", got, want)
	}
}

// 第三级（忽略所有空白）匹配只影响"能不能找到上下文"，不会改写补丁给出的新行：
// 新增行按补丁原文写入，未命中的行保持原字节。
func TestWhitespaceInternalTolerated(t *testing.T) {
	f := newFixture(t)
	f.write(t, "ws.txt", "if  x  ==  y {\n\tbody()\n}\n")
	requireOK(t, f.invoke(t, "--- a/ws.txt\n+++ b/ws.txt\n@@ -1,3 +1,3 @@\n if x == y {\n-body()\n+body2()\n }\n"))
	if got, want := f.read(t, "ws.txt"), "if  x  ==  y {\nbody2()\n}\n"; got != want {
		t.Fatalf("ws.txt = %q, want %q", got, want)
	}
}

func TestContextNotFoundReportsExpectedAndActual(t *testing.T) {
	f := newFixture(t)
	f.write(t, "miss.txt", "line1\nline2\nline3\n")
	text := requireError(t, f.invoke(t, `--- a/miss.txt
+++ b/miss.txt
@@ -2,2 +2,2 @@
 line2
-nonexistent
+x
`))
	requireContains(t, text, "miss.txt:2", "expected", "line2", "actual")
}

func TestAmbiguousContextRejected(t *testing.T) {
	f := newFixture(t)
	f.write(t, "dup.txt", "same\nother\nsame\nother\n")
	text := requireError(t, f.invoke(t, "--- a/dup.txt\n+++ b/dup.txt\n@@ -3,2 +3,2 @@\n same\n-other\n+OTHER\n"))
	requireContains(t, text, "ambiguous", "dup.txt:1", "dup.txt:3")
	if got, want := f.read(t, "dup.txt"), "same\nother\nsame\nother\n"; got != want {
		t.Fatalf("dup.txt changed on an ambiguous patch: %q", got)
	}
}

func TestTwoHunksInOneFileApplySequentially(t *testing.T) {
	f := newFixture(t)
	f.write(t, "seq.txt", "alpha\nbeta\ngamma\ndelta\n")
	// 第二个 hunk 在第一个之后：hunk 之间按"从上一个 hunk 之后继续搜索"处理。
	out := requireOK(t, f.invoke(t, `--- a/seq.txt
+++ b/seq.txt
@@ -1,2 +1,2 @@
 alpha
-beta
+BETA
@@ -3,2 +3,2 @@
 gamma
-delta
+DELTA
`))
	if got, want := f.read(t, "seq.txt"), "alpha\nBETA\ngamma\nDELTA\n"; got != want {
		t.Fatalf("seq.txt = %q, want %q", got, want)
	}
	requireContains(t, out, "(+2 -2)")
}

// ---------------------------------------------------------------------------
// 路径安全
// ---------------------------------------------------------------------------

func TestAbsolutePathRejected(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	text := requireError(t, f.invoke(t, fmt.Sprintf(`*** Begin Patch
*** Add File: %s
+x
*** End Patch`, filepath.ToSlash(outside))))
	requireContains(t, text, "absolute")
}

func TestParentEscapeRejected(t *testing.T) {
	f := newFixture(t)
	text := requireError(t, f.invoke(t, `*** Begin Patch
*** Add File: ../escape.txt
+x
*** End Patch`))
	requireContains(t, text, "..")
}

func TestSymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("符号链接需要额外权限，Windows 上跳过")
	}
	f := newFixture(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(f.root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	text := requireError(t, f.invoke(t, `*** Begin Patch
*** Add File: link/escaped.txt
+x
*** End Patch`))
	requireContains(t, text, "link/escaped.txt", "outside")
	if _, err := os.Stat(filepath.Join(outside, "escaped.txt")); err == nil {
		t.Fatal("file was created outside the workspace root")
	}
}

func TestSymlinkInsideRootAllowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("符号链接需要额外权限，Windows 上跳过")
	}
	f := newFixture(t)
	f.write(t, "real/target.txt", "old\n")
	// 必须用**相对**目标：os.Root（fsguard 的底层）在系统调用层拒绝绝对链接目标，
	// 即使它指回根内。见 TestAuditFSAbsoluteSymlinkRejected 的说明。
	if err := os.Symlink("real", filepath.Join(f.root, "alias")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	requireOK(t, f.invoke(t, "--- a/alias/target.txt\n+++ b/alias/target.txt\n@@ -1 +1 @@\n-old\n+new\n"))
	if got, want := f.read(t, "real/target.txt"), "new\n"; got != want {
		t.Fatalf("real/target.txt = %q, want %q", got, want)
	}
}

func TestRootItselfRejected(t *testing.T) {
	f := newFixture(t)
	text := requireError(t, f.invoke(t, `*** Begin Patch
*** Delete File: .
*** End Patch`))
	requireContains(t, text, "root")
}

func TestDuplicatePathRejected(t *testing.T) {
	f := newFixture(t)
	f.write(t, "dup.txt", "a\n")
	text := requireError(t, f.invoke(t, `*** Begin Patch
*** Update File: dup.txt
@@
-a
+b
*** Update File: dup.txt
@@
-b
+c
*** End Patch`))
	requireContains(t, text, "dup.txt", "twice")
}

// ---------------------------------------------------------------------------
// 落盘语义
// ---------------------------------------------------------------------------

func TestDryRunDoesNotWrite(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.txt", "a\n")
	out := requireOK(t, f.invokeDryRun(t, "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-a\n+b\n"))
	if got, want := f.read(t, "a.txt"), "a\n"; got != want {
		t.Fatalf("a.txt = %q, dry_run must not write", got)
	}
	requireContains(t, out, "[dry-run]", "update a.txt", "(+1 -1)")
}

func TestDryRunOptionAppliesByDefault(t *testing.T) {
	f := newFixture(t, patch.WithDryRun(true))
	f.write(t, "a.txt", "a\n")
	out := requireOK(t, f.invoke(t, "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-a\n+b\n"))
	if got, want := f.read(t, "a.txt"), "a\n"; got != want {
		t.Fatalf("a.txt = %q, WithDryRun(true) must not write", got)
	}
	requireContains(t, out, "[dry-run]")
}

func TestDryRunArgumentFalseOverridesOption(t *testing.T) {
	f := newFixture(t, patch.WithDryRun(true))
	f.write(t, "a.txt", "a\n")
	out := requireOK(t, f.invokeJSON(t, `{"patch":"--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-a\n+b\n","dry_run":false}`))
	if got, want := f.read(t, "a.txt"), "b\n"; got != want {
		t.Fatalf("a.txt = %q, want %q (dry_run:false overrides the option)", got, want)
	}
	if strings.Contains(out, "[dry-run]") {
		t.Fatalf("result = %q, must not claim a dry run", out)
	}
}

func TestAtomicitySecondFileFailureLeavesFirstUnchanged(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.txt", "a\n")
	f.write(t, "b.txt", "totally different\n")
	text := requireError(t, f.invoke(t, `--- a/a.txt
+++ b/a.txt
@@ -1 +1 @@
-a
+A
--- a/b.txt
+++ b/b.txt
@@ -1 +1 @@
-missing
+X
`))
	requireContains(t, text, "b.txt:1")
	if got, want := f.read(t, "a.txt"), "a\n"; got != want {
		t.Fatalf("a.txt = %q, first file must stay unchanged when a later file fails", got)
	}
}

func TestAddedFileModeIs0644(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不报告 POSIX 权限位")
	}
	f := newFixture(t)
	requireOK(t, f.invoke(t, "*** Begin Patch\n*** Add File: perm.txt\n+x\n*** End Patch"))
	if got, want := f.mode(t, "perm.txt"), os.FileMode(0o644); got != want {
		t.Fatalf("perm.txt mode = %v, want %v", got, want)
	}
}

func TestExistingFileModePreserved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不报告 POSIX 权限位")
	}
	f := newFixture(t)
	f.writeMode(t, "script.sh", "#!/bin/sh\necho old\n", 0o755)
	requireOK(t, f.invoke(t, "--- a/script.sh\n+++ b/script.sh\n@@ -1,2 +1,2 @@\n #!/bin/sh\n-echo old\n+echo new\n"))
	if got, want := f.mode(t, "script.sh"), os.FileMode(0o755); got != want {
		t.Fatalf("script.sh mode = %v, want %v", got, want)
	}
	if got, want := f.read(t, "script.sh"), "#!/bin/sh\necho new\n"; got != want {
		t.Fatalf("script.sh = %q, want %q", got, want)
	}
}

func TestBinaryFileRejected(t *testing.T) {
	f := newFixture(t)
	f.write(t, "bin.dat", "head\x00tail\n")
	text := requireError(t, f.invoke(t, "--- a/bin.dat\n+++ b/bin.dat\n@@ -1 +1 @@\n-head\n+HEAD\n"))
	requireContains(t, text, "bin.dat", "binary")
}

func TestOversizedPatchRejected(t *testing.T) {
	f := newFixture(t, patch.WithMaxPatchBytes(64))
	text := requireError(t, f.invoke(t, "*** Begin Patch\n*** Add File: big.txt\n+"+strings.Repeat("x", 200)+"\n*** End Patch"))
	requireContains(t, text, "64", "bytes")
}

func TestMissingPatchArgument(t *testing.T) {
	f := newFixture(t)
	text := requireError(t, f.invokeJSON(t, `{}`))
	requireContains(t, text, "patch")
}

func TestEmptyPatchRejected(t *testing.T) {
	f := newFixture(t)
	text := requireError(t, f.invoke(t, "   \n"))
	requireContains(t, text, "patch")
}

func TestMixedOperationsKeepPatchOrder(t *testing.T) {
	f := newFixture(t)
	f.write(t, "update.txt", "u\n")
	f.write(t, "delete.txt", "d\n")
	out := requireOK(t, f.invoke(t, `*** Begin Patch
*** Add File: add.txt
+a
*** Update File: update.txt
@@
-u
+U
*** Delete File: delete.txt
*** End Patch`))
	if got, want := f.read(t, "add.txt"), "a\n"; got != want {
		t.Fatalf("add.txt = %q, want %q", got, want)
	}
	if got, want := f.read(t, "update.txt"), "U\n"; got != want {
		t.Fatalf("update.txt = %q, want %q", got, want)
	}
	if f.exists(t, "delete.txt") {
		t.Fatal("delete.txt still exists")
	}
	addAt := strings.Index(out, "add add.txt")
	updateAt := strings.Index(out, "update update.txt")
	deleteAt := strings.Index(out, "delete delete.txt")
	if addAt < 0 || updateAt < 0 || deleteAt < 0 || !(addAt < updateAt && updateAt < deleteAt) {
		t.Fatalf("result order = %q, want add < update < delete", out)
	}
}

func TestUpdateMissingFileRejected(t *testing.T) {
	f := newFixture(t)
	text := requireError(t, f.invoke(t, `*** Begin Patch
*** Update File: nope.txt
@@
-a
+b
*** End Patch`))
	requireContains(t, text, "nope.txt", "does not exist")
}

func TestDeleteMissingFileRejected(t *testing.T) {
	f := newFixture(t)
	text := requireError(t, f.invoke(t, `*** Begin Patch
*** Delete File: nope.txt
*** End Patch`))
	requireContains(t, text, "nope.txt", "does not exist")
}

func TestAddExistingFileRejected(t *testing.T) {
	f := newFixture(t)
	f.write(t, "exists.txt", "x\n")
	text := requireError(t, f.invoke(t, `*** Begin Patch
*** Add File: exists.txt
+x
*** End Patch`))
	requireContains(t, text, "exists.txt", "already exists")
	if got, want := f.read(t, "exists.txt"), "x\n"; got != want {
		t.Fatalf("exists.txt = %q, want untouched %q", got, want)
	}
}

func TestMoveToExistingPathRejected(t *testing.T) {
	f := newFixture(t)
	f.write(t, "from.txt", "a\n")
	f.write(t, "to.txt", "b\n")
	text := requireError(t, f.invoke(t, `*** Begin Patch
*** Update File: from.txt
*** Move to: to.txt
*** End Patch`))
	requireContains(t, text, "to.txt", "already exists")
	if got, want := f.read(t, "to.txt"), "b\n"; got != want {
		t.Fatalf("to.txt = %q, want untouched %q", got, want)
	}
}

func TestEmptyUpdateBodyIsNoOpMoveOrUpdate(t *testing.T) {
	f := newFixture(t)
	f.write(t, "same.txt", "same\n")
	out := requireOK(t, f.invoke(t, `*** Begin Patch
*** Update File: same.txt
*** End Patch`))
	if got, want := f.read(t, "same.txt"), "same\n"; got != want {
		t.Fatalf("same.txt = %q, want %q", got, want)
	}
	requireContains(t, out, "update same.txt", "(+0 -0)")
}

func TestResultSummaryFormat(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.txt", "a\n")
	out := requireOK(t, f.invoke(t, "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1,2 @@\n-a\n+A\n+b\n"))
	requireContains(t, out, "1 file(s)", "+2 -1", "update a.txt")
	if !strings.HasPrefix(out, "applied patch: ") {
		t.Fatalf("result = %q, want the stable \"applied patch: \" prefix", out)
	}
}

func TestNonExistentRootFailsClosed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-root")
	tool := patch.New(missing, patch.WithPolicy(workspacePolicy(missing)))
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"patch":"*** Begin Patch\n*** Add File: a.txt\n+x\n*** End Patch"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	text := requireError(t, result)
	requireContains(t, text, "root")
}

func TestContextCancelledReturnsGoError(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.tool.Invoke(ctx, rig.ToolCall{Arguments: `{"patch":"*** Begin Patch\n*** End Patch"}`}); err == nil {
		t.Fatal("Invoke on a cancelled context returned nil error")
	}
}

// ---------------------------------------------------------------------------
// Schema
// ---------------------------------------------------------------------------

func TestSchemaContract(t *testing.T) {
	tool := patch.New(t.TempDir())
	schema := tool.Schema()
	if got, want := schema["type"], "object"; got != want {
		t.Fatalf("schema type = %v, want %v", got, want)
	}
	if got, want := schema["additionalProperties"], false; got != want {
		t.Fatalf("schema additionalProperties = %v, want false", got)
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "patch" {
		t.Fatalf("schema required = %v, want [patch]", schema["required"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %T, want map", schema["properties"])
	}
	patchProp, ok := properties["patch"].(map[string]any)
	if !ok || patchProp["type"] != "string" {
		t.Fatalf("properties.patch = %v, want a string property", properties["patch"])
	}
	if description, _ := patchProp["description"].(string); strings.TrimSpace(description) == "" {
		t.Fatal("properties.patch.description is empty")
	}
	dryRunProp, ok := properties["dry_run"].(map[string]any)
	if !ok || dryRunProp["type"] != "boolean" {
		t.Fatalf("properties.dry_run = %v, want a boolean property", properties["dry_run"])
	}
	if _, ok := properties["dry_run"]; ok {
		if _, required := dryRunProp["required"]; required {
			t.Fatal("dry_run must stay optional")
		}
	}
	if got, want := tool.Name(), "apply_patch"; got != want {
		t.Fatalf("Name() = %q, want %q", got, want)
	}
	if strings.TrimSpace(tool.Description()) == "" {
		t.Fatal("Description() is empty")
	}
}

func TestSchemaJSONEncodable(t *testing.T) {
	tool := patch.New(t.TempDir())
	if _, err := json.Marshal(tool.Schema()); err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
}

func TestCodexEndOfFileMarkerIgnored(t *testing.T) {
	f := newFixture(t)
	f.write(t, "eof.txt", "a\nb\n")
	requireOK(t, f.invoke(t, `*** Begin Patch
*** Update File: eof.txt
@@
-b
+B
*** End of File
*** End Patch`))
	if got, want := f.read(t, "eof.txt"), "a\nB\n"; got != want {
		t.Fatalf("eof.txt = %q, want %q", got, want)
	}
}

func TestMixedLineEndingsPreservedByteForByte(t *testing.T) {
	f := newFixture(t)
	// 第一行 CRLF、第二行 LF、第三行无行尾：未被触碰的行必须逐字节保持不变。
	f.write(t, "mixed.txt", "one\r\ntwo\nthree")
	requireOK(t, f.invoke(t, "--- a/mixed.txt\n+++ b/mixed.txt\n@@ -1,3 +1,3 @@\n one\n-two\n+TWO\n three\n"))
	if got, want := f.read(t, "mixed.txt"), "one\r\nTWO\nthree"; got != want {
		t.Fatalf("mixed.txt = %q, want %q", got, want)
	}
}

// 新增文件的 diff 只能有 '+' 行：夹带上下文行时行数声明必然对不上，解析阶段
// 就要拒绝（应用式补丁的同类限制见 TestCodexAddFileRequiresPlusLines）。
func TestUnifiedAddFileWithContextLineRejected(t *testing.T) {
	f := newFixture(t)
	text := requireError(t, f.invoke(t, `--- /dev/null
+++ b/ctx.txt
@@ -0,0 +1,2 @@
+line
 line
`))
	requireContains(t, text, "declares")
	if f.exists(t, "ctx.txt") {
		t.Fatal("ctx.txt was created by an invalid add-file diff")
	}
}

func TestDryRunStillValidatesFailures(t *testing.T) {
	f := newFixture(t)
	text := requireError(t, f.invokeDryRun(t, `*** Begin Patch
*** Update File: missing.txt
@@
-a
+b
*** End Patch`))
	requireContains(t, text, "missing.txt", "does not exist")
}

func TestDeleteFileContentMismatchRejected(t *testing.T) {
	f := newFixture(t)
	f.write(t, "keep.txt", "actual content\n")
	text := requireError(t, f.invoke(t, `--- a/keep.txt
+++ /dev/null
@@ -1 +0,0 @@
-different content
`))
	requireContains(t, text, "keep.txt")
	if got, want := f.read(t, "keep.txt"), "actual content\n"; got != want {
		t.Fatalf("keep.txt = %q, want it untouched %q", got, want)
	}
}

// 补丁文本本身可能是 CRLF 编码（模型从 Windows 环境生成时很常见）：行尾的 "\r"
// 必须在解析时剥掉，否则每一行都会因尾部多一个字节而匹配失败。
func TestCRLFEncodedPatchText(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.txt", "one\ntwo\n")
	patchText := strings.ReplaceAll("--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+TWO\n", "\n", "\r\n")
	requireOK(t, f.invoke(t, patchText))
	if got, want := f.read(t, "a.txt"), "one\nTWO\n"; got != want {
		t.Fatalf("a.txt = %q, want %q", got, want)
	}
}

// Windows 风格绝对路径即使当前平台是 Linux 也必须拒绝：补丁可能来自另一台机器。
func TestWindowsStyleAbsolutePathRejected(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{`C:\Windows\evil.txt`, `C:/Windows/evil.txt`, `\\server\share\evil.txt`} {
		text := requireError(t, f.invoke(t, "*** Begin Patch\n*** Add File: "+path+"\n+x\n*** End Patch"))
		requireContains(t, text, "absolute")
	}
}

func TestAppendAtEndOfFile(t *testing.T) {
	f := newFixture(t)
	f.write(t, "tail.txt", "a\nb\n")
	requireOK(t, f.invoke(t, "--- a/tail.txt\n+++ b/tail.txt\n@@ -2,0 +3,1 @@\n+c\n"))
	if got, want := f.read(t, "tail.txt"), "a\nb\nc\n"; got != want {
		t.Fatalf("tail.txt = %q, want %q", got, want)
	}
}

// 真实 `git diff` 输出带 diff --git / index / mode 等元数据行：它们不影响语义，
// 必须被跳过；路径与内容由 ---/+++ 头与 hunk 决定。
func TestGitDiffMetadataLinesSkipped(t *testing.T) {
	f := newFixture(t)
	f.write(t, "src/a.go", "package a\n")
	out := requireOK(t, f.invoke(t, `diff --git a/src/a.go b/src/a.go
index 1234567..89abcde 100644
--- a/src/a.go
+++ b/src/a.go
@@ -1 +1,2 @@
 package a
+// touched
`))
	if got, want := f.read(t, "src/a.go"), "package a\n// touched\n"; got != want {
		t.Fatalf("src/a.go = %q, want %q", got, want)
	}
	requireContains(t, out, "update src/a.go", "(+1 -0)")
}
