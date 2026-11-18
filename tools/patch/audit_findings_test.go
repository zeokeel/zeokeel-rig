package patch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件是对抗审计的回归用例：每条都对应一个真实缺陷（或其修复后的守门断言）。
// 命名统一以 TestAudit 开头，便于与作者自己的用例区分。

// TestAuditSymlinkTargetIsFollowedOnUpdate 断言"更新一个文件符号链接"时会写到链接
// 目标、保留链接本身。
//
// path.go 的设计注释明确写着"目标本身是符号链接时按链接目标校验（写入会跟随
// 链接）"，但落盘用的是 os.Rename(temp, 链接路径)：rename 不会跟随符号链接，
// 它会把链接本身替换成普通文件，导致链接目标保持旧内容、链接消失。
func TestAuditSymlinkTargetIsFollowedOnUpdate(t *testing.T) {
	root := t.TempDir()
	realPath := filepath.Join(root, "real.md")
	if err := os.WriteFile(realPath, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(root, "link.md")
	if err := os.Symlink("real.md", linkPath); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}

	out, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Update File: link.md\n@@\n-old\n+new\n*** End Patch\n")
	if isErr {
		t.Fatalf("补丁失败: %s", out)
	}
	if got := readFileString(t, realPath); got != "new\n" {
		t.Fatalf("链接目标 real.md = %q, want %q（写入必须跟随符号链接）", got, "new\n")
	}
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("link.md 不见了: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link.md 被替换成了普通文件（mode=%v），符号链接必须保留", info.Mode())
	}
}

// TestAuditBinaryAddRejected 断言新增文件时 NUL 字节会被拒绝：update/delete 都会
// 拒绝二进制文件，add 也必须一致，否则可以用补丁造出此后永远无法再编辑的文件。
func TestAuditBinaryAddRejected(t *testing.T) {
	root := t.TempDir()
	out, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Add File: bin.dat\n+\x00\x01raw\n*** End Patch\n")
	if !isErr {
		t.Fatalf("新增含 NUL 的文件应当被拒绝，实际成功: %q", out)
	}
	if !strings.Contains(out, "binary") {
		t.Fatalf("错误文案应当说明是二进制文件，实际: %q", out)
	}
	if _, err := os.Stat(filepath.Join(root, "bin.dat")); err == nil {
		t.Fatal("被拒绝的补丁仍然写出了文件")
	}
}

// TestAuditBOMPreservedOnFirstLineEdit 断言带 UTF-8 BOM 的文件在改写首行后仍保留
// BOM，且补丁里的上下文不需要包含 BOM（模型看不到它）。
func TestAuditBOMPreservedOnFirstLineEdit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bom.txt")
	if err := os.WriteFile(path, []byte("\ufefffirst\nsecond\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Update File: bom.txt\n@@\n-first\n+changed\n*** End Patch\n")
	if isErr {
		t.Fatalf("补丁失败（首行上下文不含 BOM 也应能匹配）: %s", out)
	}
	if got, want := readFileString(t, path), "\ufeffchanged\nsecond\n"; got != want {
		t.Fatalf("BOM 文件内容 = %q, want %q（BOM 必须保留）", got, want)
	}
}

// TestAuditGitQuotedPath 断言 git 给非 ASCII 路径加的 C 风格引号会被还原。
//
// git diff 对非 ASCII 文件名输出 "a/\344\270\255\346\226\207.md"；不还原就会
// 生成一个名字里带引号与反斜杠的垃圾目录树（或对已存在的文件报"不存在"）。
func TestAuditGitQuotedPath(t *testing.T) {
	root := t.TempDir()
	name := "中文.md"
	if err := os.WriteFile(filepath.Join(root, name), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 真实 git diff 的字节形态（\344\270\255\346\226\207 = "中文"）。
	patchText := "--- \"a/\\344\\270\\255\\346\\226\\207.md\"\n+++ \"b/\\344\\270\\255\\346\\226\\207.md\"\n@@ -1 +1 @@\n-old\n+new\n"
	out, isErr := probeInvoke(t, root, patchText)
	if isErr {
		t.Fatalf("带 git 引号的合法补丁被拒绝: %s", out)
	}
	if got := readFileString(t, filepath.Join(root, name)); got != "new\n" {
		t.Fatalf("%s = %q, want %q", name, got, "new\n")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("工作区里出现了多余文件: %q", names)
	}
}

// TestAuditPathConflictRejected 断言"同一补丁里既把某个路径当文件又当目录"在
// buildPlans 阶段就被拒绝，而不是在落盘阶段以难懂的 rename 错误失败。
func TestAuditPathConflictRejected(t *testing.T) {
	root := t.TempDir()
	patchText := "*** Begin Patch\n*** Add File: x\n+file\n*** Add File: x/y.md\n+nested\n*** End Patch\n"
	out, isErr := probeInvoke(t, root, patchText)
	if !isErr {
		t.Fatalf("文件/目录冲突应当被拒绝，实际成功: %q", out)
	}
	if !strings.Contains(out, "conflict") {
		t.Fatalf("错误文案应当说明是路径冲突，实际: %q", out)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("被拒绝的补丁留下了 %d 个文件系统条目: %q", len(entries), out)
	}
}

// TestAuditCommitCleansCreatedDirectories 断言落盘阶段失败时不会留下新建目录
// （"零改动"承诺的一部分）：失败的补丁不能在workspace 里留下空目录。
func TestAuditCommitCleansCreatedDirectories(t *testing.T) {
	root := t.TempDir()
	// 让第二个计划的 rename 失败：目标已经是目录。
	if err := os.Mkdir(filepath.Join(root, "boom"), 0o755); err != nil {
		t.Fatal(err)
	}
	plans := []plan{
		{op: opAdd, rel: "new/deep/a.md", writeRel: "new/deep/a.md", content: []byte("x\n"), mode: 0o644},
		{op: opAdd, rel: "boom", writeRel: "boom", content: []byte("y\n"), mode: 0o644},
	}
	guard, err := newRootFS(root)
	if err != nil {
		t.Fatalf("newRootFS: %v", err)
	}
	defer guard.Close()
	if err := commit(guard, plans, false); err == nil {
		t.Fatal("commit 应当失败")
	}
	if _, err := os.Stat(filepath.Join(root, "new")); err == nil {
		t.Fatal("落盘失败后 new/ 目录被留下了（违反零改动）")
	}
	entries, _ := os.ReadDir(root)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 1 || names[0] != "boom" {
		t.Fatalf("工作区内容 = %q, want 只剩 boom", names)
	}
}

// TestAuditRollbackRestoresBytesAndMode 断言落盘阶段失败时逆序回滚会恢复第一个
// 文件的原字节与原权限位，且不残留临时文件。
func TestAuditRollbackRestoresBytesAndMode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Log("以 root 运行：只验证回滚结果，不依赖权限位制造失败")
	}
	root := t.TempDir()
	target := filepath.Join(root, "a.md")
	if err := os.WriteFile(target, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "boom"), 0o755); err != nil {
		t.Fatal(err)
	}
	plans := []plan{
		{op: opUpdate, rel: "a.md", writeRel: "a.md", existed: true, old: []byte("original\n"), mode: 0o600, content: []byte("changed\n")},
		{op: opAdd, rel: "boom", writeRel: "boom", content: []byte("y\n"), mode: 0o644},
	}
	guard, err := newRootFS(root)
	if err != nil {
		t.Fatalf("newRootFS: %v", err)
	}
	defer guard.Close()
	if err := commit(guard, plans, false); err == nil {
		t.Fatal("commit 应当失败")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("a.md 不见了: %v", err)
	}
	if got := readFileString(t, target); got != "original\n" {
		t.Fatalf("回滚后 a.md = %q, want %q", got, "original\n")
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("回滚后 a.md 权限 = %v, want -rw-------", info.Mode().Perm())
	}
	temps, _ := filepath.Glob(filepath.Join(root, ".rigpatch-*.tmp"))
	moreTemps, _ := filepath.Glob(filepath.Join(root, "*.tmp"))
	temps = append(temps, moreTemps...)
	if len(temps) != 0 {
		t.Fatalf("残留临时文件: %q", temps)
	}
}

// TestAuditEscapeAttemptsRejected 断言各种越界路径都失败且不产生任何文件。
func TestAuditEscapeAttemptsRejected(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Log("以 root 运行，符号链接以外仍应被拒绝")
	}
	outside := t.TempDir()
	cases := []struct{ name, path string }{
		{"POSIX 绝对路径", "/tmp/escape.md"},
		{"Windows 盘符", `C:\escape.md`},
		{"UNC 路径", `\\server\share\x.md`},
		{"双斜杠", "//tmp/x.md"},
		{"上跳一层", "../escape.md"},
		{"上跳后回绕", "a/../../escape.md"},
		{"根目录本身", "."},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			patchText := "*** Begin Patch\n*** Add File: " + testCase.path + "\n+x\n*** End Patch\n"
			out, isErr := probeInvoke(t, root, patchText)
			if !isErr {
				t.Fatalf("越界路径 %q 被接受: %q", testCase.path, out)
			}
			if _, err := os.Stat(filepath.Join(outside, "escape.md")); err == nil {
				t.Fatal("在 root 之外创建了文件")
			}
		})
	}
	// 目录符号链接逃逸
	t.Run("目录符号链接逃逸", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
			t.Skipf("符号链接不可用: %v", err)
		}
		out, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Add File: link/escaped.md\n+x\n*** End Patch\n")
		if !isErr {
			t.Fatalf("符号链接逃逸被接受: %q", out)
		}
		if _, err := os.Stat(filepath.Join(outside, "escaped.md")); err == nil {
			t.Fatal("文件被写到了 root 之外")
		}
	})
}

// TestAuditExactMatchPreferred 断言三级匹配确实是"精确优先"。
func TestAuditExactMatchPreferred(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.md")
	// 第 1 行只能忽略空白才能匹配，第 3 行是精确匹配。
	if err := os.WriteFile(path, []byte("value  \nother\nvalue\ntail\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Update File: a.md\n@@\n-value\n+changed\n*** End Patch\n")
	if isErr {
		t.Fatalf("补丁失败: %s", out)
	}
	if got, want := readFileString(t, path), "value  \nother\nchanged\ntail\n"; got != want {
		t.Fatalf("内容 = %q, want %q（应选精确匹配的第 3 行）", got, want)
	}
}

// TestAuditDeleteOfSymlinkKeepsTarget 断言删除符号链接删的是链接本身，目标文件
// 必须保留（与 update 的"跟随链接"形成对称语义）。
func TestAuditDeleteOfSymlinkKeepsTarget(t *testing.T) {
	root := t.TempDir()
	realPath := filepath.Join(root, "real.md")
	if err := os.WriteFile(realPath, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(root, "link.md")
	if err := os.Symlink("real.md", linkPath); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	out, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Delete File: link.md\n*** End Patch\n")
	if isErr {
		t.Fatalf("补丁失败: %s", out)
	}
	if _, err := os.Lstat(linkPath); err == nil {
		t.Fatal("符号链接没有被删除")
	}
	if got := readFileString(t, realPath); got != "old\n" {
		t.Fatalf("链接目标 real.md = %q, want %q（删除链接不能删掉目标）", got, "old\n")
	}
}

// TestAuditPureInsertAppliedTwiceDuplicates 记录"纯插入 hunk 重复应用"的现状：
// 第二次应用同样成功并再插一遍。这里不断言"应该失败"（这是行号寻址插入的固有
// 语义），只把行为钉住，避免悄悄变化。
func TestAuditPureInsertAppliedTwiceDuplicates(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.md")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patchText := "--- a/a.md\n+++ b/a.md\n@@ -1,0 +2,1 @@\n+inserted\n"
	if _, isErr := probeInvoke(t, root, patchText); isErr {
		t.Fatal("第一次应用应当成功")
	}
	if _, isErr := probeInvoke(t, root, patchText); isErr {
		t.Fatal("第二次应用同样成功（已知语义）")
	}
	if got, want := readFileString(t, path), "one\ninserted\ninserted\ntwo\n"; got != want {
		t.Fatalf("内容 = %q, want %q", got, want)
	}
}

// TestAuditDuplicatePathsStillRejected 守门：同一路径在补丁里出现两次必须报错。
func TestAuditDuplicatePathsStillRejected(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Update File: a.md\n@@\n-x\n+y\n*** Delete File: a.md\n*** End Patch\n")
	if !isErr {
		t.Fatalf("重复路径应当被拒绝: %q", out)
	}
	if !strings.Contains(out, "twice") {
		t.Fatalf("错误文案应当说明重复，实际: %q", out)
	}
	if got := readFileString(t, filepath.Join(root, "a.md")); got != "x\n" {
		t.Fatalf("被拒绝的补丁改动了文件: %q", got)
	}
}

// TestAuditErrorWrappingIsStable 断言工具错误都是 IsError 结果而不是 Go error，
// 只有参数解码与 ctx 取消返回 Go error。
func TestAuditErrorWrappingIsStable(t *testing.T) {
	root := t.TempDir()
	if _, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Delete File: missing.md\n*** End Patch\n"); !isErr {
		t.Fatal("删除不存在的文件应当是 IsError 结果")
	}
	tool := New(root)
	if _, err := tool.Invoke(t.Context(), rigCall(`{"patch":"*** Begin Patch\n*** End Patch\n"}`)); err != nil {
		t.Fatalf("空操作补丁不应返回 Go error: %v", err)
	}
	if _, err := tool.Invoke(t.Context(), rigCall(`{"patch":`)); err == nil {
		t.Fatal("坏参数应当是 Go error")
	} else if errors.Is(err, t.Context().Err()) {
		t.Fatalf("unexpected context error: %v", err)
	}
}

// TestAuditFileSymlinkOutsideRootRejected 守门：目标是**指向根外的文件符号链接**时
// 必须拒绝（"跟随链接写入"的修复不能让写入逃出根）。
func TestAuditFileSymlinkOutsideRootRejected(t *testing.T) {
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "secret.md")
	if err := os.WriteFile(outsidePath, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(outsidePath, filepath.Join(root, "evil.md")); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	out, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Update File: evil.md\n@@\n-secret\n+pwned\n*** End Patch\n")
	if !isErr {
		t.Fatalf("指向根外的文件符号链接被接受: %q", out)
	}
	if got := readFileString(t, outsidePath); got != "secret\n" {
		t.Fatalf("根外文件被改写: %q", got)
	}
	// 删除路径同样不能删根外目标。
	out, isErr = probeInvoke(t, root, "*** Begin Patch\n*** Delete File: evil.md\n*** End Patch\n")
	if isErr {
		t.Logf("删除指向根外的链接也被拒绝（可接受）: %s", out)
	} else if _, err := os.Stat(outsidePath); err != nil {
		t.Fatalf("根外文件被删除: %v", err)
	}
}

// FuzzParsePatch 对解析入口做模糊测试：任何输入都必须以"返回错误"或"返回操作
// 列表"结束，不能 panic、不能卡死。
func FuzzParsePatch(f *testing.F) {
	seeds := []string{
		"*** Begin Patch\n*** Add File: a.md\n+x\n*** End Patch\n",
		"--- a/a.md\n+++ b/a.md\n@@ -1 +1 @@\n-x\n+y\n",
		"--- /dev/null\n+++ b/x\n@@ -0,0 +1 @@\n+hi\n",
		"@@ -1,1 +1,1 @@\n-a\n+b\n",
		"*** Begin Patch\n*** Update File: a\n@@ anchor\n-a\n*** Move to: b\n*** End Patch\n",
		"--- \"a/\\344\\270\\255.md\"\n+++ \"b/\\344\\270\\255.md\"\n@@ -1 +1 @@\n-a\n+b\n",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		ops, err := parsePatch(text)
		if err != nil {
			return
		}
		for _, op := range ops {
			if strings.ContainsRune(op.path, 0) {
				continue
			}
		}
	})
}

// TestAuditDirtyButLegitimatePatches 覆盖真实世界里"脏但合法"的补丁形态：
// 允许成功或给出可读错误，但绝不能 panic、绝不能误改文件。
func TestAuditDirtyButLegitimatePatches(t *testing.T) {
	tests := []struct {
		name    string
		content string // 目标文件初始内容，空表示文件不存在
		path    string
		patch   string
		want    string // 期望的最终内容；空表示应当报错
	}{
		{
			name:    "@@ 头带函数上下文（git 默认输出）",
			path:    "a.go",
			content: "func main() {\n\told\n}\n",
			patch:   "--- a/a.go\n+++ b/a.go\n@@ -1,3 +1,3 @@ func main() {\n func main() {\n-\told\n+\tnew\n }\n",
			want:    "func main() {\n\tnew\n}\n",
		},
		{
			name:    "\\ No newline at end of file 标记",
			path:    "a.txt",
			content: "old",
			patch:   "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-old\n\\ No newline at end of file\n+new\n\\ No newline at end of file\n",
			want:    "new",
		},
		{
			name:    "整份补丁用 CRLF 传输",
			path:    "a.txt",
			content: "one\ntwo\n",
			patch:   "*** Begin Patch\r\n*** Update File: a.txt\r\n@@\r\n-one\r\n+ONE\r\n*** End Patch\r\n",
			want:    "ONE\ntwo\n",
		},
		{
			name:    "hunk 前有缩进（模型常干）",
			path:    "a.txt",
			content: "one\n",
			patch:   "  *** Begin Patch\n*** Update File: a.txt\n@@\n-one\n+ONE\n*** End Patch\n",
			want:    "ONE\n",
		},
		{
			name:  "Add File 空正文",
			path:  "empty.txt",
			patch: "*** Begin Patch\n*** Add File: empty.txt\n*** End Patch\n",
			want:  "",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, testCase.path)
			if testCase.content != "" {
				if err := os.WriteFile(target, []byte(testCase.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, isErr := probeInvoke(t, root, testCase.patch)
			if testCase.want == "" && isErr {
				return // 期望报错
			}
			if isErr {
				t.Fatalf("合法补丁被拒绝: %s", out)
			}
			if got := readFileString(t, target); got != testCase.want {
				t.Fatalf("内容 = %q, want %q（结果: %s）", got, testCase.want, out)
			}
		})
	}
}

// TestAuditCodexUpdateIsNotIdempotent 断言应用式补丁（带上下文）第二次应用
// 会失败而不是重复插入：这是模型重试场景最重要的安全属性。
func TestAuditCodexUpdateIsNotIdempotent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.md")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patchText := "*** Begin Patch\n*** Update File: a.md\n@@\n one\n+inserted\n two\n*** End Patch\n"
	if out, isErr := probeInvoke(t, root, patchText); isErr {
		t.Fatalf("第一次应用失败: %s", out)
	}
	out, isErr := probeInvoke(t, root, patchText)
	if !isErr {
		t.Fatalf("第二次应用应当失败（上下文已变），实际成功: %q", out)
	}
	if got, want := readFileString(t, path), "one\ninserted\ntwo\n"; got != want {
		t.Fatalf("内容 = %q, want %q（不得重复插入）", got, want)
	}
}

// TestAuditSameFileViaSymlinkAndPathRejected 断言"符号链接 + 它的目标"在同一个
// 补丁里被同时修改会被拒绝（否则两个计划写同一物理文件，后写者静默胜出）。
func TestAuditSameFileViaSymlinkAndPathRejected(t *testing.T) {
	root := t.TempDir()
	realPath := filepath.Join(root, "real.md")
	if err := os.WriteFile(realPath, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.md", filepath.Join(root, "link.md")); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	patchText := "*** Begin Patch\n*** Update File: link.md\n@@\n-old\n+first\n*** Update File: real.md\n@@\n-old\n+second\n*** End Patch\n"
	out, isErr := probeInvoke(t, root, patchText)
	if !isErr {
		t.Fatalf("同一文件经两条路径被写两次应当被拒绝: %q", out)
	}
	if got := readFileString(t, realPath); got != "old\n" {
		t.Fatalf("被拒绝的补丁改动了文件: %q", got)
	}
}
