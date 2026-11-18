package osrunner

// F3：保护路径"构造时不存在"曾被静默丢弃——`BuildProfile` 用 existingPaths 过滤，
// 不存在的只进 MissingProtected（只报告不拦）。于是"在未被保护的子目录里 git init
// 之后写 .git/config"能成功，用户以为受保护、实际没有。
//
// 语义（本轮定稿）：
//   - 后端无法渲染不存在的路径（bwrap / Landlock / Windows ACL）→ **默认拒绝执行**；
//   - Seatbelt 的 SBPL 按访问时求值 → 直接渲染成 deny 规则，不需要拒绝；
//   - 宿主可显式选择"只报告"：`sandbox.WithProtectMissingPaths(false)`。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// missingProtectedPolicy 造一个"声明了保护、但该位置不存在"的策略。
func missingProtectedPolicy(t *testing.T, root string) (policy *sandbox.Policy, missing string) {
	t.Helper()
	missing = filepath.Join(root, ".git") // 不创建
	policy = sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithNetwork(false),
		sandbox.WithProtectedPaths(missing),
	)
	return policy, missing
}

// bwrap/Landlock（本机可用的那个）必须拒绝执行，且错误里含缺失路径。
func TestMissingProtectedFailsClosedByDefault(t *testing.T) {
	root := t.TempDir()
	policy, missing := missingProtectedPolicy(t, root)
	profile, err := BuildProfile(policy)
	if err != nil || profile == nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if len(profile.MissingProtected) == 0 {
		t.Fatalf("用例前提被破坏：%q 不存在的保护项没有进 MissingProtected", missing)
	}
	confineErr := checkMissingProtected(profile, policy, RunnerBwrap)
	if confineErr == nil {
		t.Fatalf("保护路径 %q 不存在，checkMissingProtected 却没有报错（fail-open）", missing)
	}
	if !strings.Contains(confineErr.Error(), missing) {
		t.Fatalf("错误信息里没有列出缺失路径 %q：%v", missing, confineErr)
	}
	if !strings.Contains(strings.ToLower(confineErr.Error()), "unprotected") &&
		!strings.Contains(strings.ToLower(confineErr.Error()), "does not exist") {
		t.Fatalf("错误信息没有解释「为什么不能继续」：%v", confineErr)
	}
}

// 显式选择"只报告"时不报错，但缺失项仍然可见。
func TestMissingProtectedCanBeOptedOut(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, ".git")
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithProtectedPaths(missing),
		sandbox.WithProtectMissingPaths(false),
	)
	profile, err := BuildProfile(policy)
	if err != nil || profile == nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if err := checkMissingProtected(profile, policy, RunnerBwrap); err != nil {
		t.Fatalf("WithProtectMissingPaths(false) 之后不该报错，得到：%v", err)
	}
	found := false
	for _, path := range profile.MissingProtected {
		if samePathForTest(path, missing) {
			found = true
		}
	}
	if !found {
		t.Fatalf("选择「只报告」后，%q 仍应出现在 MissingProtected：%v", missing, profile.MissingProtected)
	}
	// 诊断文案要能解释"本次不会被保护"。
	warning := missingProtectedWarning(profile)
	if !strings.Contains(warning, "will not protect") {
		t.Fatalf("诊断警告没有说明本次不会被保护：%q", warning)
	}
}

// Seatbelt：不存在的保护路径必须被渲染成 deny 规则（SBPL 按访问时求值）。
func TestSeatbeltRendersMissingProtectedPaths(t *testing.T) {
	root := t.TempDir()
	policy, missing := missingProtectedPolicy(t, root)
	profile, err := BuildProfile(policy)
	if err != nil || profile == nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if len(profile.MissingProtected) == 0 {
		t.Fatalf("用例前提被破坏：%q 没有进 MissingProtected", missing)
	}
	sbpl := SeatbeltProfile(policy)
	if sbpl == "" {
		t.Fatal("SeatbeltProfile 为空")
	}
	if !strings.Contains(sbpl, sbplString(missing)) {
		t.Fatalf("SBPL 里没有 %q 的 deny 规则（Seatbelt 本可以保护它）：\n%s", missing, sbpl)
	}
	if !strings.Contains(sbpl, "(deny file-write*") {
		t.Fatalf("SBPL 缺少 deny file-write* 规则：\n%s", sbpl)
	}
}

// git 控制面里"将来才出现"的子项（config.worktree / info）不能被当成硬失败，
// 否则正常仓库里每一条命令都会因为 fail-closed 而跑不起来。
func TestOptionalProtectedHintsDoNotFailClosed(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.ProtectGitMetadata(root), // 只登记只读子路径，不声明"必须存在"
	)
	profile, err := BuildProfile(policy)
	if err != nil || profile == nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	// config.worktree 等确实缺失，但它们只是提示。
	if err := checkMissingProtected(profile, policy, RunnerBwrap); err != nil {
		t.Fatalf("git 控制面的可选子项缺失不应导致 fail-closed：%v", err)
	}
}
