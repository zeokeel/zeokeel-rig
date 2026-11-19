package osrunner

// 防回归：同一目录的多种写法（macOS 的 /var vs /private/var、Windows 的 8.3 短名、
// 符号链接别名）在**所有**下发面里必须收敛成同一种写法。
//
// 为什么需要这条：三轮 CI 红都是同一类问题——某个列表里混着两种写法，于是
// 字符串比较、`winPathWithinFold`、日志去重全部判不出来。断言层面改用
// samePathForTest 只是治标；产品侧"每个列表内部写法统一"才是治本。
//
// 这里用 symlink 造出"同一目录的两种写法"（在 Linux/macOS/Windows 上语义相同），
// 然后检查 Profile.ReadWrite 与 Windows 计划层的 --workspace 都不出现第二种写法。
//
// 对齐 DSH 后，Protected / MissingProtected / 工具链缓存根本不再参与渲染
// （--protect 也不再下发），所以它们不再属于"下发面"，本用例只覆盖仍会下发的
// 两个面。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// sameDirectorySpellings 返回一段路径里所有"指向同一个对象"的字面量写法（去重）。
func sameDirectorySpellings(entries []string, want string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, entry := range entries {
		if !samePathForTest(entry, want) {
			continue
		}
		if _, ok := seen[entry]; ok {
			continue
		}
		seen[entry] = struct{}{}
		out = append(out, entry)
	}
	return out
}

func TestSinglePathSpellingAcrossRendering(t *testing.T) {
	realDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	alias := realDir + "-alias"
	if linkErr := os.Symlink(realDir, alias); linkErr != nil {
		t.Skipf("本平台/文件系统不支持创建符号链接（Windows 需要开发者模式）：%v", linkErr)
	}
	t.Cleanup(func() { _ = os.Remove(alias) })

	gitDir := filepath.Join(realDir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", gitDir, err)
	}

	policy := sandbox.New(
		sandbox.WithRoots(alias), // 工作区用**别名**写法，逼出规范化逻辑
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithNetwork(false),
		sandbox.WithReadOnlySubpaths(alias, filepath.Join(".git", "config")),
	)
	policy.AddProtectedPaths(filepath.Join(alias, ".git"))

	profile, err := BuildProfile(policy)
	if err != nil || profile == nil {
		t.Fatalf("BuildProfile: %v", err)
	}

	check := func(label string, entries []string, want string) {
		spellings := sameDirectorySpellings(entries, want)
		if len(spellings) == 0 {
			t.Fatalf("%s 里没有任何条目指向 %q（用例前提被破坏）：%v", label, want, entries)
		}
		if len(spellings) > 1 {
			t.Fatalf("%s 里同一个目录出现了 %d 种写法 %v（必须收敛成一种）", label, len(spellings), spellings)
		}
		if strings.Contains(spellings[0], "-alias") {
			t.Fatalf("%s 用的是别名写法 %q，应当统一到真实路径", label, spellings[0])
		}
	}

	check("Profile.ReadWrite", profile.ReadWrite, realDir)

	// Windows 计划层：--workspace 的值同样只能有一种写法（不再有 --protect）。
	confined, planErr := buildWindowsConfinedCommand("rig.exe", []string{"cmd.exe"}, policy)
	if planErr != nil {
		t.Fatalf("buildWindowsConfinedCommand: %v", planErr)
	}
	check("--workspace", windowsFlagValues(confined.Argv, winFlagWorkspace), realDir)
	if protects := windowsFlagValues(confined.Argv, winFlagProtect); len(protects) != 0 {
		t.Fatalf("对齐 DSH 后不应再下发 --protect：%v", protects)
	}
}
