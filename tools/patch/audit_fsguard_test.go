package patch

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 本文件审计 tools/patch 迁移到 internal/fsguard（os.Root）之后的文件系统语义：
// 符号链接（相对/绝对/链式/目录/悬空）、ReadFileLimit 边界、TOCTOU 情形、回滚残留、
// 以及大小写不敏感平台上的去重键。

// auditPlanFor 用给定内容写一个最小的 update 计划并落盘（模拟"已通过校验"的阶段）。

// TestAuditFSSymlinkTargetInsideRoot 断言相对符号链接（根内）会被**写穿**：目标更新、
// 链接本身保留。
func TestAuditFSSymlinkTargetInsideRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.md"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.md", filepath.Join(root, "link.md")); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	out, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Update File: link.md\n@@\n-old\n+new\n*** End Patch\n")
	if isErr {
		t.Fatalf("相对链接的更新失败: %s", out)
	}
	if got := readFileString(t, filepath.Join(root, "real.md")); got != "new\n" {
		t.Fatalf("链接目标 = %q, want %q", got, "new\n")
	}
	info, err := os.Lstat(filepath.Join(root, "link.md"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("链接本身被替换: %v %v", info, err)
	}
}

// TestAuditFSAbsoluteSymlinkRejected 记录迁移带来的**语义收窄**：os.Root 在系统调用
// 层拒绝绝对链接目标，即使它指回根内。预检给出专门文案，且不产生任何改动。
func TestAuditFSAbsoluteSymlinkRejected(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.md"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real.md"), filepath.Join(root, "abs.md")); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	out, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Update File: abs.md\n@@\n-old\n+new\n*** End Patch\n")
	if !isErr {
		t.Fatalf("绝对链接目标应当被拒绝（os.Root 不支持），实际成功: %q", out)
	}
	if !strings.Contains(out, "absolute path") {
		t.Fatalf("错误文案应说明是绝对链接目标: %q", out)
	}
	if got := readFileString(t, filepath.Join(root, "real.md")); got != "old\n" {
		t.Fatalf("被拒绝的补丁改动了目标: %q", got)
	}
	if info, err := os.Lstat(filepath.Join(root, "abs.md")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("链接被破坏: %v %v", info, err)
	}
}

// TestAuditFSSymlinkEscapeRelative 断言相对符号链接逃出根时被拒绝，且根外无改动。
func TestAuditFSSymlinkEscapeRelative(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink("../"+filepath.Base(outside), filepath.Join(root, "up")); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	// 用目录链接逃逸：up/secret.md 指向根外文件。
	out, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Update File: up/secret.md\n@@\n-secret\n+pwned\n*** End Patch\n")
	if !isErr {
		t.Fatalf("相对链接逃逸被接受: %q", out)
	}
	if got := readFileString(t, filepath.Join(outside, "secret.md")); got != "secret\n" {
		t.Fatalf("根外文件被改写: %q", got)
	}
}

// TestAuditFSDirectorySymlinkInside 断言根内的目录符号链接可以正常穿过（新增 + 更新）。
func TestAuditFSDirectorySymlinkInside(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real", "a.md"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	out, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Update File: alias/a.md\n@@\n-old\n+new\n*** Add File: alias/b.md\n+fresh\n*** End Patch\n")
	if isErr {
		t.Fatalf("根内目录链接失败: %s", out)
	}
	if got := readFileString(t, filepath.Join(root, "real", "a.md")); got != "new\n" {
		t.Fatalf("real/a.md = %q", got)
	}
	if got := readFileString(t, filepath.Join(root, "real", "b.md")); got != "fresh\n" {
		t.Fatalf("real/b.md = %q", got)
	}
}

// TestAuditFSReadFileLimitBoundary 断言大小上限的边界语义：恰好等于上限可读，
// 超一字节即拒绝，且超限时不在整读后才发现（稀疏大文件也能立刻给出文案）。
func TestAuditFSReadFileLimitBoundary(t *testing.T) {
	t.Run("恰好等于上限", func(t *testing.T) {
		root := t.TempDir()
		body := "0123456789"
		if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		out, isErr := invokeWith(t, New(root, WithMaxFileBytes(int64(len(body)))), "*** Begin Patch\n*** Update File: a.txt\n@@\n-0123456789\n+x\n*** End Patch\n")
		if isErr {
			t.Fatalf("恰好等于上限应当允许: %s", out)
		}
	})
	t.Run("超出一字节", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("0123456789"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, isErr := invokeWith(t, New(root, WithMaxFileBytes(9)), "*** Begin Patch\n*** Update File: a.txt\n@@\n-0123456789\n+x\n*** End Patch\n")
		if !isErr || !strings.Contains(out, "file too large to patch") {
			t.Fatalf("超限应当拒绝: err=%v out=%q", isErr, out)
		}
	})
	t.Run("稀疏大文件不整读", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "huge.bin")
		if err := os.Truncate(path, int64(DefaultMaxFileBytes)+4096); err != nil {
			t.Skipf("无法创建稀疏文件: %v", err)
		}
		out, isErr := invokeWith(t, New(root), "*** Begin Patch\n*** Update File: huge.bin\n@@\n-x\n+y\n*** End Patch\n")
		if !isErr || !strings.Contains(out, "file too large to patch") {
			t.Fatalf("超大文件应当在整读前被拒: err=%v out=%q", isErr, out)
		}
	})
}

// TestAuditFSRootSwappedForSymlink 断言"根在校验之后被换成符号链接"不会让写入落到
// 新目标：fsguard 的句柄绑定在打开时的目录 inode 上（这正是 TOCTOU 的收敛方式）。
func TestAuditFSRootSwappedForSymlink(t *testing.T) {
	base := t.TempDir()
	original := filepath.Join(base, "root")
	if err := os.MkdirAll(original, 0o755); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(base, "decoy")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatal(err)
	}

	guard, err := newRootFS(original)
	if err != nil {
		t.Fatalf("newRootFS: %v", err)
	}
	defer guard.Close()

	// 攻击者把根移走、用符号链接指向另一个目录顶替（模拟校验与落盘之间的竞态）。
	moved := filepath.Join(base, "root-moved")
	if err := os.Rename(original, moved); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.Symlink(decoy, original); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}

	err = commit(guard, []plan{{op: opAdd, rel: "x.md", writeRel: "x.md", content: []byte("payload\n"), mode: 0o644}}, false)
	if err != nil {
		t.Logf("写入被拒绝（同样安全）: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(decoy, "x.md")); statErr == nil {
		t.Fatal("写入落到了被顶替的目录（TOCTOU 逃逸）")
	}
	if err == nil {
		if _, statErr := os.Stat(filepath.Join(moved, "x.md")); statErr != nil {
			t.Fatalf("写入没有落到原来打开的目录: %v", statErr)
		}
	}
}

// TestAuditFSRollbackLeavesNoResidue 断言落盘失败后：内容/权限恢复、无临时文件、
// 无残留目录。
func TestAuditFSRollbackLeavesNoResidue(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a.md")
	if err := os.WriteFile(target, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	// 让第二个计划失败：目标已经是目录。
	if err := os.Mkdir(filepath.Join(root, "boom"), 0o755); err != nil {
		t.Fatal(err)
	}
	guard, err := newRootFS(root)
	if err != nil {
		t.Fatalf("newRootFS: %v", err)
	}
	defer guard.Close()
	err = commit(guard, []plan{
		{op: opUpdate, rel: "a.md", writeRel: "a.md", existed: true, old: []byte("original\n"), mode: 0o600, content: []byte("changed\n")},
		{op: opAdd, rel: "deep/dir/boom", writeRel: "deep/dir/boom", content: []byte("y\n"), mode: 0o644},
		{op: opAdd, rel: "boom", writeRel: "boom", content: []byte("z\n"), mode: 0o644},
	}, false)
	if err == nil {
		t.Fatal("commit 应当失败")
	}
	if got := readFileString(t, target); got != "original\n" {
		t.Fatalf("回滚后 a.md = %q", got)
	}
	if info, statErr := os.Stat(target); statErr == nil && info.Mode().Perm() != 0o600 {
		t.Fatalf("回滚后权限 = %v", info.Mode().Perm())
	}
	for _, dir := range []string{"deep", filepath.Join("deep", "dir")} {
		if _, statErr := os.Stat(filepath.Join(root, dir)); statErr == nil {
			t.Fatalf("回滚后残留目录 %q", dir)
		}
	}
	assertNoTempFiles(t, root)
}

// TestAuditFSNoTempResidueOnSuccess 断言成功路径也不留临时文件。
func TestAuditFSNoTempResidueOnSuccess(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Update File: a.md\n@@\n-old\n+new\n*** Add File: sub/b.md\n+hi\n*** End Patch\n"); isErr {
		t.Fatalf("补丁失败: %s", out)
	}
	assertNoTempFiles(t, root)
}

// assertNoTempFiles 扫描整棵树，确保没有 .fsguard-*.tmp / .rigpatch-*.tmp 残留。
func assertNoTempFiles(t *testing.T, root string) {
	t.Helper()
	var found []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		name := info.Name()
		if strings.HasPrefix(name, ".fsguard-") || strings.HasPrefix(name, ".rigpatch-") {
			found = append(found, path)
		}
		return nil
	})
	if len(found) > 0 {
		t.Fatalf("残留临时文件: %v", found)
	}
}

// TestAuditFSCaseFoldDedupe 断言大小写不敏感平台会把 A.md 与 a.md 判为同一文件。
//
// 平台差异用 osNameForPathSemantics 注入（真实取值是 runtime.GOOS）：
//   - darwin/windows：同一补丁里 A.md 与 a.md → 报"同一文件写两次"；
//   - linux：两者是两个不同文件，都能创建。
func TestAuditFSCaseFoldDedupe(t *testing.T) {
	original := osNameForPathSemantics
	t.Cleanup(func() { osNameForPathSemantics = original })

	patchText := "*** Begin Patch\n*** Add File: A.md\n+upper\n*** Add File: a.md\n+lower\n*** End Patch\n"

	t.Run("darwin 视为同一文件", func(t *testing.T) {
		osNameForPathSemantics = func() string { return "darwin" }
		root := t.TempDir()
		out, isErr := probeInvoke(t, root, patchText)
		if !isErr {
			t.Fatalf("大小写不敏感平台上应当拒绝: %q", out)
		}
		if !strings.Contains(out, "twice") {
			t.Fatalf("错误文案应说明重复: %q", out)
		}
		entries, _ := os.ReadDir(root)
		if len(entries) != 0 {
			t.Fatalf("被拒绝的补丁留下了文件: %d", len(entries))
		}
	})

	t.Run("windows 视为同一文件", func(t *testing.T) {
		osNameForPathSemantics = func() string { return "windows" }
		root := t.TempDir()
		if out, isErr := probeInvoke(t, root, patchText); !isErr {
			t.Fatalf("大小写不敏感平台上应当拒绝: %q", out)
		}
	})

	t.Run("linux 视为两个文件", func(t *testing.T) {
		osNameForPathSemantics = func() string { return "linux" }
		root := t.TempDir()
		out, isErr := probeInvoke(t, root, patchText)
		if isErr {
			t.Fatalf("大小写敏感平台上应当允许: %s", out)
		}
		for _, name := range []string{"A.md", "a.md"} {
			if _, err := os.Stat(filepath.Join(root, name)); err != nil {
				t.Fatalf("%s 没有创建: %v", name, err)
			}
		}
	})
}

// TestAuditFSSymlinkChainInside 断言链式符号链接（link → link2 → file）写到最终文件。
func TestAuditFSSymlinkChainInside(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.md"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.md", filepath.Join(root, "link2.md")); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	if err := os.Symlink("link2.md", filepath.Join(root, "link1.md")); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	out, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Update File: link1.md\n@@\n-old\n+new\n*** End Patch\n")
	if isErr {
		t.Fatalf("链式链接更新失败: %s", out)
	}
	if got := readFileString(t, filepath.Join(root, "real.md")); got != "new\n" {
		t.Fatalf("最终目标 = %q, want %q", got, "new\n")
	}
}

// TestAuditFSDanglingSymlink 断言指向不存在目标的链接会得到"不存在"的可读错误。
func TestAuditFSDanglingSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("missing.md", filepath.Join(root, "dangling.md")); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	out, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Update File: dangling.md\n@@\n-old\n+new\n*** End Patch\n")
	if !isErr {
		t.Fatalf("悬空链接应当报错: %q", out)
	}
	if !strings.Contains(out, "does not exist") {
		t.Fatalf("错误文案应说明文件不存在: %q", out)
	}
}

// TestAuditFSMoveSymlinkMovesLinkOnly 断言移动符号链接时移动的是链接本身，目标不动。
func TestAuditFSMoveSymlinkMovesLinkOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.md"), []byte("body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.md", filepath.Join(root, "link.md")); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	out, isErr := probeInvoke(t, root, "*** Begin Patch\n*** Update File: link.md\n*** Move to: moved.md\n*** End Patch\n")
	if isErr {
		t.Fatalf("移动链接失败: %s", out)
	}
	if got := readFileString(t, filepath.Join(root, "real.md")); got != "body\n" {
		t.Fatalf("目标被改动: %q", got)
	}
	info, err := os.Lstat(filepath.Join(root, "moved.md"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("moved.md 不是符号链接: %v %v", info, err)
	}
	if _, statErr := os.Lstat(filepath.Join(root, "link.md")); statErr == nil {
		t.Fatal("源链接没有被移除")
	}
}

// ---------------------------------------------------------------------------
// 策略与符号链接目录的对齐（B1 回归）
// ---------------------------------------------------------------------------

// TestAuditFSDenyGlobThroughDirLink 断言 deny glob 不会因为"目录符号链接"被绕过。
//
// 攻击：策略 deny `secrets/**`，补丁写 `shared/creds.md`（`shared -> secrets`）。
// 词法路径 `root/shared/creds.md` 匹配不上 glob，若不把链接解析结果回填到 rel/abs，
// 策略校验就会放行，而实际写入落在 `secrets/creds.md`。
func TestAuditFSDenyGlobThroughDirLink(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secrets", "creds.md"), []byte("token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("secrets", filepath.Join(root, "shared")); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithDeniedGlobs("secrets/**"),
	)
	tool := New(root, WithPolicy(policy))

	// 对照：直接写被拒。
	if out, isErr := invokeWith(t, tool, "*** Begin Patch\n*** Update File: secrets/creds.md\n@@\n-token\n+LEAKED\n*** End Patch\n"); !isErr {
		t.Fatalf("直接写被 deny 的路径竟然成功: %q", out)
	}
	// 攻击路径：经目录链接写。
	out, isErr := invokeWith(t, tool, "*** Begin Patch\n*** Update File: shared/creds.md\n@@\n-token\n+LEAKED\n*** End Patch\n")
	if !isErr {
		t.Fatalf("经目录链接绕过了 deny glob: %q", out)
	}
	if got := readFileString(t, filepath.Join(root, "secrets", "creds.md")); got != "token\n" {
		t.Fatalf("被保护文件被改写: %q", got)
	}
}

// TestAuditFSProtectedPathThroughDirLink 同上，攻击面换成 ProtectedPaths。
func TestAuditFSProtectedPathThroughDirLink(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "vault"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vault", "key.md"), []byte("k\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("vault", filepath.Join(root, "alias")); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithProtectedPaths(filepath.Join(root, "vault")),
	)
	tool := New(root, WithPolicy(policy))

	if out, isErr := invokeWith(t, tool, "*** Begin Patch\n*** Update File: vault/key.md\n@@\n-k\n+K\n*** End Patch\n"); !isErr {
		t.Fatalf("直接写保护区竟然成功: %q", out)
	}
	out, isErr := invokeWith(t, tool, "*** Begin Patch\n*** Update File: alias/key.md\n@@\n-k\n+K\n*** End Patch\n")
	if !isErr {
		t.Fatalf("经目录链接绕过了保护区: %q", out)
	}
	if got := readFileString(t, filepath.Join(root, "vault", "key.md")); got != "k\n" {
		t.Fatalf("保护区文件被改写: %q", got)
	}
}

// TestAuditFSDirLinkToUnprotectedStillWritable 是正向对照：目录链接指向**未被保护**
// 的目录时必须照常可写（确认修复不是"一刀切拒绝所有目录链接"）。
func TestAuditFSDirLinkToUnprotectedStillWritable(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "guide.md"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("docs", filepath.Join(root, "manual")); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithDeniedGlobs("secrets/**"),
	)
	tool := New(root, WithPolicy(policy))

	out, isErr := invokeWith(t, tool, "*** Begin Patch\n*** Update File: manual/guide.md\n@@\n-old\n+new\n*** End Patch\n")
	if isErr {
		t.Fatalf("指向未保护目录的链接被误拒: %s", out)
	}
	if got := readFileString(t, filepath.Join(root, "docs", "guide.md")); got != "new\n" {
		t.Fatalf("docs/guide.md = %q, want %q", got, "new\n")
	}
	// 同一补丁里"直写 + 经链接写"同一个物理文件：现在会按解析后的落点判定为同一
	// 文件（写入两次），而不是静默后写者胜。
	duplicate := "*** Begin Patch\n*** Update File: docs/guide.md\n@@\n-new\n+one\n*** Update File: manual/guide.md\n@@\n-new\n+two\n*** End Patch\n"
	dupOut, dupErr := invokeWith(t, tool, duplicate)
	if !dupErr {
		t.Fatalf("经链接重复写同一文件应当被拒绝: %q", dupOut)
	}
	if !strings.Contains(dupOut, "twice") && !strings.Contains(dupOut, "same file") {
		t.Fatalf("错误文案应说明重复: %q", dupOut)
	}
	if got := readFileString(t, filepath.Join(root, "docs", "guide.md")); got != "new\n" {
		t.Fatalf("被拒绝的重复写入改动了文件: %q", got)
	}
}

// TestAuditFIFORejectedImmediately 断言 FIFO 在三秒内（实测应当毫秒级）被拒绝，
// 而不是阻塞在 open 上把回合挂死。
func TestAuditFIFORejectedImmediately(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 FIFO")
	}
	mkfifo, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("没有 mkfifo")
	}
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe")
	if out, err := exec.Command(mkfifo, fifo).CombinedOutput(); err != nil {
		t.Skipf("mkfifo: %v %s", err, out)
	}
	tool := New(root)

	done := make(chan struct{})
	var result string
	var isErr bool
	start := time.Now()
	go func() {
		result, isErr = invokeWith(t, tool, "*** Begin Patch\n*** Update File: pipe\n@@\n-x\n+y\n*** End Patch\n")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("更新 FIFO 阻塞超过 3s（应当立即拒绝非普通文件）")
	}
	elapsed := time.Since(start)
	if !isErr {
		t.Fatalf("更新 FIFO 竟然成功: %q", result)
	}
	if !strings.Contains(result, "not a regular file") {
		t.Fatalf("错误文案应说明不是普通文件: %q", result)
	}
	if elapsed > time.Second {
		t.Fatalf("FIFO 拒绝耗时 %v，超过 1s 门槛", elapsed)
	}
	// FIFO 本身必须还在（被拒绝的补丁不能改工作区）。
	if info, err := os.Lstat(fifo); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("FIFO 被改动: %v %v", info, err)
	}
}
