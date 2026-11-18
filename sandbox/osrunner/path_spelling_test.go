package osrunner

// 防回归：同一目录的多种写法（macOS 的 /var vs /private/var、Windows 的 8.3 短名、
// 符号链接别名）在**所有**下发面里必须收敛成同一种写法。
//
// 为什么需要这条：三轮 CI 红都是同一类问题——某个列表里混着两种写法，于是
// 字符串比较、`winPathWithinFold`、日志去重全部判不出来。断言层面改用
// samePathForTest 只是治标；产品侧"每个列表内部写法统一"才是治本。
//
// 这里用 symlink 造出"同一目录的两种写法"（在 Linux/macOS/Windows 上语义相同），
// 然后检查 Profile.Protected / Profile.ReadWrite / Profile.MissingProtected 与
// Windows 计划层的 --workspace / --protect 都不出现第二种写法。

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
	// 父目录存在、自身不存在：会进 MissingProtected（这条路径的写法同理要统一）。
	missingConfig := filepath.Join(gitDir, "config")

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
	check("Profile.Protected", profile.Protected, gitDir)
	check("Profile.MissingProtected", profile.MissingProtected, missingConfig)
	// 工具链缓存根本来就不在工作区里（它们在 HOME 下），这里不参与"同一目录两种
	// 写法"的检查——但要保证它们自己也是规范化后的写法（不含别名）。
	for _, root := range rootPaths(profile.ToolchainCaches) {
		if strings.Contains(root, "-alias") {
			t.Fatalf("ToolchainCaches 出现别名写法：%q", root)
		}
	}

	// Windows 计划层：--workspace 与 --protect 的值同样只能有一种写法。
	confined, planErr := buildWindowsConfinedCommand("rig.exe", []string{"cmd.exe"}, policy)
	if planErr != nil {
		t.Fatalf("buildWindowsConfinedCommand: %v", planErr)
	}
	check("--workspace", windowsFlagValues(confined.Argv, winFlagWorkspace), realDir)
	check("--protect", windowsFlagValues(confined.Argv, winFlagProtect), gitDir)
}
