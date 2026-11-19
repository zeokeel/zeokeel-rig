package patch_test

// 对抗审计（只读）：apply_patch 的 fsguard 迁移回归。
//
// 关注点：迁移是否引入行为变化（绝对链接目标）、沙箱策略在"符号链接目录"上是否
// 还能拦住、纯移动/去重键的边界、以及"不再有裸 os 写调用"的可守护性。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/patch"
)

// invokeInGoroutine 在 goroutine 里跑一次 Invoke，用于探测可能挂死的路径。
func invokeInGoroutine(t *testing.T, tool rig.Tool, patchText string) <-chan rig.ToolResult {
	t.Helper()
	payload := `{"patch":` + quoteJSONForAudit(patchText) + `}`
	done := make(chan rig.ToolResult, 1)
	go func() {
		result, err := tool.Invoke(context.Background(), rig.ToolCall{Name: patch.ToolName, Arguments: payload})
		if err != nil {
			done <- rig.ToolResult{Content: "go error: " + err.Error(), IsError: true}
			return
		}
		done <- result
	}()
	return done
}

// quoteJSONForAudit 把补丁文本编码成 JSON 字符串。
func quoteJSONForAudit(text string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, r := range text {
		switch r {
		case '\\':
			builder.WriteString(`\\`)
		case '"':
			builder.WriteString(`\"`)
		case '\n':
			builder.WriteString(`\n`)
		case '\t':
			builder.WriteString(`\t`)
		case '\r':
			builder.WriteString(`\r`)
		default:
			builder.WriteRune(r)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

// TestAuditAbsoluteSymlinkTargetIsUnusable 记录迁移后的行为收窄：链接目标写成
// **绝对路径**（哪怕指回工作区根内）时，读、写、删、Stat 全部失败——
// 只有 Lstat/Readlink 能看见它。
//
// doc.go 声称"这类文件可以被读取但不能被更新/删除"，实测**读取也失败**。
func TestAuditAbsoluteSymlinkTargetIsUnusable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	f := newFixture(t)
	realPath := f.write(t, "docs/real.md", "one\ntwo\n")
	linkPath := filepath.Join(f.root, "link.md")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Skipf("symlink: %v", err)
	}

	// 1) 更新链接 → 必须被拒（doc 已声明），但错误文案不能谎称"根外"。
	result := f.invoke(t, "*** Begin Patch\n*** Update File: link.md\n@@\n-one\n+ONE\n*** End Patch\n")
	message := requireError(t, result)
	if !strings.Contains(message, "link.md") {
		t.Fatalf("错误文案没提到路径: %q", message)
	}
	t.Logf("更新绝对链接目标的错误文案: %s", strings.TrimSpace(message))
	// 2) 删除链接 → 同样被拒。
	result = f.invoke(t, "*** Begin Patch\n*** Delete File: link.md\n*** End Patch\n")
	t.Logf("删除绝对链接目标的错误文案: %s", strings.TrimSpace(requireError(t, result)))

	// 3) 链接本身还在，目标没被改。
	if _, err := os.Lstat(linkPath); err != nil {
		t.Fatalf("链接被删掉了: %v", err)
	}
	if data, err := os.ReadFile(realPath); err != nil || string(data) != "one\ntwo\n" {
		t.Fatalf("链接目标被改动: %q err=%v", data, err)
	}

	// 4) 同一目录里**无关**的补丁必须照常工作（收窄不应污染其它文件）。
	f.write(t, "a.txt", "a\n")
	result = f.invoke(t, "*** Begin Patch\n*** Update File: a.txt\n@@\n-a\n+b\n*** End Patch\n")
	requireOK(t, result)
	if got := f.read(t, "a.txt"); got != "b\n" {
		t.Fatalf("无关文件内容 = %q", got)
	}
	// 5) 该工作区里的普通文件仍可被 patched（说明收窄只针对绝对链接目标）。
	if result := f.invokeDryRun(t, "*** Begin Patch\n*** Update File: docs/real.md\n@@\n-one\n+ONE\n*** End Patch\n"); result.IsError {
		t.Fatalf("直接 patched 链接目标本身应当可以: %s", result.Content)
	}
}

// TestAuditPolicyDenyGlobThroughSymlinkedDirectoryNoLongerBlocks 是旧用例
// TestAuditPolicyDenyGlobThroughSymlinkedDirectory 的反向版本（**DSH 对齐的有意
// 变更**）：拒绝 glob 只是记录，直接写与经符号链接目录写都必须成功。
func TestAuditPolicyDenyGlobThroughSymlinkedDirectoryNoLongerBlocks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	root := t.TempDir()
	f := &fixture{root: root}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	f.tool = patch.New(root, patch.WithPolicy(policy))

	f.write(t, "secrets/creds.md", "token\n")
	if err := os.Symlink("secrets", filepath.Join(root, "shared")); err != nil {
		t.Skipf("symlink: %v", err)
	}

	result := f.invoke(t, "*** Begin Patch\n*** Update File: secrets/creds.md\n@@\n-token\n+LEGIT\n*** End Patch\n")
	if result.IsError {
		t.Fatalf("直接写被拒绝（拒绝 glob 已停用）: %s", result.Content)
	}
	result = f.invoke(t, "*** Begin Patch\n*** Update File: shared/creds.md\n@@\n-LEGIT\n+VIA-LINK\n*** End Patch\n")
	if result.IsError {
		t.Fatalf("经符号链接目录写被拒绝（拒绝 glob 已停用）: %s", result.Content)
	}
	if got := f.read(t, "secrets/creds.md"); got != "VIA-LINK\n" {
		t.Fatalf("secrets/creds.md = %q, want 经链接写入生效", got)
	}
}

// TestAuditPolicyProtectedPathThroughSymlinkedDirectoryNoLongerBlocks 是旧用例
// TestAuditPolicyProtectedPathThroughSymlinkedDirectory 的反向版本（**DSH 对齐的
// 有意变更**）：保护区已停用，直接写与经符号链接目录写都必须成功。
func TestAuditPolicyProtectedPathThroughSymlinkedDirectoryNoLongerBlocks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	root := t.TempDir()
	f := &fixture{root: root}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	f.tool = patch.New(root, patch.WithPolicy(policy))

	f.write(t, "vault/key.md", "k\n")
	if err := os.Symlink("vault", filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if result := f.invoke(t, "*** Begin Patch\n*** Update File: vault/key.md\n@@\n-k\n+K\n*** End Patch\n"); result.IsError {
		t.Fatalf("曾受保护路径被拒绝（保护已停用）: %s", result.Content)
	}
	if result := f.invoke(t, "*** Begin Patch\n*** Update File: alias/key.md\n@@\n-K\n+K2\n*** End Patch\n"); result.IsError {
		t.Fatalf("经符号链接目录写曾受保护路径被拒绝（保护已停用）: %s", result.Content)
	}
	if got := f.read(t, "vault/key.md"); got != "K2\n" {
		t.Fatalf("vault/key.md = %q, want 经链接写入生效", got)
	}
}

// TestAuditFIFOInWorkspaceHangsPatch 记录一个可用性缺口：补丁更新 FIFO 时，
// fsguard 的 OpenFile 会一直阻塞，apply_patch **没有超时**，调用方会永久挂住。
func TestAuditFIFOInWorkspaceHangsPatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 FIFO")
	}
	if _, err := exec.LookPath("mkfifo"); err != nil {
		t.Skip("没有 mkfifo")
	}
	f := newFixture(t)
	fifo := filepath.Join(f.root, "pipe")
	if out, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Skipf("mkfifo: %v %s", err, out)
	}
	done := invokeInGoroutine(t, f.tool, "*** Begin Patch\n*** Update File: pipe\n@@\n-x\n+y\n*** End Patch\n")
	select {
	case result := <-done:
		// 若实现改成"先 Stat 拒掉非普通文件"，这里会拿到可读错误——那才是期望行为。
		if !result.IsError {
			t.Fatalf("更新 FIFO 竟然成功了: %s", result.Content)
		}
		t.Logf("FIFO 被拒绝（良好）: %s", result.Content)
	case <-time.After(500 * time.Millisecond):
		// 现状：挂住。用写端解开，避免测试进程泄漏。
		if writer, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
			_, _ = writer.Write([]byte("x\n"))
			_ = writer.Close()
		}
		select {
		case result := <-done:
			t.Fatalf("更新 FIFO 阻塞了 500ms 才返回（现状：无超时，调用方会永久挂住）: %s", result.Content)
		case <-time.After(2 * time.Second):
			t.Fatalf("更新 FIFO 永久阻塞：apply_patch 没有超时保护（现状记录）")
		}
	}
}

// TestAuditPureMoveEdges 覆盖纯移动改用 Rename 之后的边界。
func TestAuditPureMoveEdges(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.md", "a\n")
	f.write(t, "exists.md", "e\n")

	// 目标已存在：拒绝，且源不动。
	result := f.invoke(t, "*** Begin Patch\n*** Update File: a.md\n*** Move to: exists.md\n*** End Patch\n")
	requireError(t, result)
	if got := f.read(t, "a.md"); got != "a\n" {
		t.Fatalf("移动失败后源文件被改动: %q", got)
	}
	if got := f.read(t, "exists.md"); got != "e\n" {
		t.Fatalf("已存在的目标被覆盖: %q", got)
	}

	// 目标父目录不存在：自动创建后成功。
	result = f.invoke(t, "*** Begin Patch\n*** Update File: a.md\n*** Move to: deep/nested/b.md\n*** End Patch\n")
	requireOK(t, result)
	if got := f.read(t, "deep/nested/b.md"); got != "a\n" {
		t.Fatalf("移动后内容 = %q", got)
	}
	if f.exists(t, "a.md") {
		t.Fatal("移动后源文件仍然存在")
	}

	// 移动目标与另一个操作冲突：必须整体拒绝（一个文件都不改）。
	f.write(t, "c.md", "c\n")
	result = f.invoke(t, "*** Begin Patch\n*** Update File: deep/nested/b.md\n*** Move to: d.md\n*** Add File: d.md\n+x\n*** End Patch\n")
	requireError(t, result)
	if !f.exists(t, "deep/nested/b.md") {
		t.Fatal("冲突时源文件被移动了（应当零改动）")
	}
	if f.exists(t, "d.md") {
		t.Fatal("冲突时目标文件被创建了（应当零改动）")
	}
}

// TestAuditCaseVariantsOnLinux 复核对 dedupeKey 平台近似的实际后果：Linux（大小写
// 敏感）上同名的两个大小写变体是**两个文件**，两个都写是正确行为；硬链接则是
// 两个路径、一个 inode，原子写会把它拆成两个 inode（记录语义，不是越权）。
func TestAuditCaseVariantsOnLinux(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("只在大小写敏感平台上验证")
	}
	f := newFixture(t)
	f.write(t, "Foo.md", "foo\n")
	f.write(t, "foo.md", "foo2\n")
	result := f.invoke(t, "*** Begin Patch\n*** Update File: Foo.md\n@@\n-foo\n+FOO\n*** Update File: foo.md\n@@\n-foo2\n+FOO2\n*** End Patch\n")
	requireOK(t, result)
	if got := f.read(t, "Foo.md"); got != "FOO\n" {
		t.Fatalf("Foo.md = %q", got)
	}
	if got := f.read(t, "foo.md"); got != "FOO2\n" {
		t.Fatalf("foo.md = %q", got)
	}

	// 硬链接：两个路径同一 inode，原子写会各自换成新 inode（内容各自正确）。
	f.write(t, "hard-a.md", "h\n")
	if err := os.Link(filepath.Join(f.root, "hard-a.md"), filepath.Join(f.root, "hard-b.md")); err != nil {
		t.Skipf("hardlink: %v", err)
	}
	result = f.invoke(t, "*** Begin Patch\n*** Update File: hard-a.md\n@@\n-h\n+HA\n*** Update File: hard-b.md\n@@\n-h\n+HB\n*** End Patch\n")
	requireOK(t, result)
	if got := f.read(t, "hard-a.md"); got != "HA\n" {
		t.Fatalf("hard-a.md = %q", got)
	}
	if got := f.read(t, "hard-b.md"); got != "HB\n" {
		t.Fatalf("hard-b.md = %q", got)
	}
}

// TestAuditNoBareOSWriteCalls 是"不再有裸 os 写调用"的守护测试：tools/patch 的
// 非测试代码里不允许再出现 os.Rename/WriteFile/Remove/MkdirAll/CreateTemp/
// OpenFile/Symlink，只允许构造期对 root 自身做一次 os.Stat。
func TestAuditNoBareOSWriteCalls(t *testing.T) {
	forbidden := []string{
		"os.Rename(", "os.WriteFile(", "os.Remove(", "os.MkdirAll(",
		"os.CreateTemp(", "os.OpenFile(", "os.Symlink(", "os.Mkdir(",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for lineIndex, line := range strings.Split(string(source), "\n") {
			for _, pattern := range forbidden {
				if strings.Contains(line, pattern) {
					t.Errorf("%s:%d 出现了裸 os 写调用 %s: %s", name, lineIndex+1, pattern, strings.TrimSpace(line))
				}
			}
		}
	}
}

// TestAuditAbsoluteDirectorySymlinkBlocksNormalLayouts 量化"绝对链接目标一律拒绝"
// 对正常仓库布局的冲击：补丁目标是**经绝对目录链接**到达的文件时，补丁整体失败。
// （相对目录链接不受影响，见 TestAuditPolicyDenyGlobThroughSymlinkedDirectory 的反面。）
func TestAuditAbsoluteDirectorySymlinkBlocksNormalLayouts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	f := newFixture(t)
	f.write(t, "real/x.md", "one\n")
	if err := os.Symlink(filepath.Join(f.root, "real"), filepath.Join(f.root, "abs-dir")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	result := f.invoke(t, "*** Begin Patch\n*** Update File: abs-dir/x.md\n@@\n-one\n+ONE\n*** End Patch\n")
	if !result.IsError {
		t.Fatalf("经绝对目录链接的补丁竟然成功了（与 doc 的收窄描述不一致）")
	}
	// 错误文案应当可操作：说清楚是"链接目标必须是相对路径"，而不是"在根外"。
	if strings.Contains(result.Content, "outside the workspace root") {
		t.Logf("错误文案把\"根内的绝对链接\"说成了根外（会误导用户）: %s", strings.TrimSpace(result.Content))
	}
	// 直接写真实路径仍然可以。
	result = f.invoke(t, "*** Begin Patch\n*** Update File: real/x.md\n@@\n-one\n+ONE\n*** End Patch\n")
	requireOK(t, result)
}

// TestAuditDirectorySymlinkFsguardSemantics 记录 fsguard 层面的事实：**相对**的目录
// 链接会被内核安全地跟随（仍在根内），这是 patch 的 deny-glob 校验必须自己解决的
// 原因——写入最终落在链接目标上。
func TestAuditDirectorySymlinkFsguardSemantics(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	f := newFixture(t)
	f.write(t, "secrets/creds.md", "token\n")
	if err := os.Symlink("secrets", filepath.Join(f.root, "shared")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	// 不带策略：经符号链接目录的更新应当成功，并且真的改到 secrets/creds.md。
	result := f.invoke(t, "*** Begin Patch\n*** Update File: shared/creds.md\n@@\n-token\n+changed\n*** End Patch\n")
	requireOK(t, result)
	if got := f.read(t, "secrets/creds.md"); got != "changed\n" {
		t.Fatalf("secrets/creds.md = %q，期望写入穿过目录链接", got)
	}
}

// TestAuditWrittenTargetKeyConsistency 回答"writtenTargets 的键（跟随后的目标）与
// claim 的键（用户写的路径）是否一致"：不一致但**有效**——两个不同路径落到同一
// 物理文件时，writtenTargets 能拦住"后写者胜 + 结果文案谎称改了两个文件"。
func TestAuditWrittenTargetKeyConsistency(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	f := newFixture(t)
	f.write(t, "real.md", "one\n")
	if err := os.Symlink("real.md", filepath.Join(f.root, "link1.md")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := os.Symlink("real.md", filepath.Join(f.root, "link2.md")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	// 两个不同路径 → 同一物理文件：必须拒绝（否则会写两次、报两个文件）。
	result := f.invoke(t, "*** Begin Patch\n*** Update File: link1.md\n@@\n-one\n+A\n*** Update File: link2.md\n@@\n-one\n+B\n*** End Patch\n")
	requireError(t, result)
	if got := f.read(t, "real.md"); got != "one\n" {
		t.Fatalf("冲突时目标被改动: %q", got)
	}

	// 链接与它的目标同时出现在补丁里：同样拒绝。
	result = f.invoke(t, "*** Begin Patch\n*** Update File: link1.md\n@@\n-one\n+A\n*** Update File: real.md\n@@\n-one\n+B\n*** End Patch\n")
	requireError(t, result)

	// 删除目标 + 更新链接：也属于"同一物理文件被两个操作碰到"。
	result = f.invoke(t, "*** Begin Patch\n*** Update File: link1.md\n@@\n-one\n+A\n*** Delete File: real.md\n*** End Patch\n")
	if !result.IsError {
		t.Fatalf("同一物理文件的更新+删除没有被拒绝")
	}
}
