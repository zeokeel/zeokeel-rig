//go:build darwin

package osrunner

// macOS 真机层：把 conformance_test.go 的同一张表跑在 Seatbelt（sandbox-exec）上。
//
// 只有在 macOS 机器上、且 RIG_CONFORMANCE=1 时才会真的执行；其它平台编译期就被
// 构建标签排除。CI 的 macOS job 里加：
//
//	RIG_CONFORMANCE=1 RIG_CONFORMANCE_SLOW=1 go test ./sandbox/osrunner/ -run Conformance -v
//
// 跑完必须清理：本文件里的用例只用 t.TempDir / 独立子目录，不写注册表、不装服务。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// conformancePlatform 返回 macOS 的环境与后端列表。
func conformancePlatform(t *testing.T) (conformanceEnv, []conformanceBackend) {
	t.Helper()
	env := conformanceNewEnv(t, "darwin")
	env.Shell = []string{"/bin/sh", "-c"}

	runner, err := New(Config{})
	if err != nil {
		t.Logf("SKIP backend seatbelt: %v", err)
		return env, nil
	}
	return env, []conformanceBackend{{Name: RunnerSeatbelt, Runner: runner, Enforcing: true}}
}

// TestConformanceProfileRenderingDarwin 是**纯逻辑层**的 macOS 部分：不执行任何
// 沙箱，只断言 SBPL 渲染把该拒的拒了、该放的放了。
//
// 断言的是**能力面**而不是字符串细节（SBPL 的具体行会变，但"工作区可写、.git
// 只读、断网、临时目录可写"这四条不能变）。
func TestConformanceProfileRenderingDarwin(t *testing.T) {
	env := conformanceNewEnv(t, "darwin")
	policy := sandbox.New(
		sandbox.WithRoots(env.Root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithNetwork(false),
	)
	// `.git` 必须真的存在：`Profile.Protected` 只渲染**存在**的保护项（不存在的
	// 进 MissingProtected）。以前这里只声明不创建，真机 CI 上 Protected 是空切片。
	gitDir := filepath.Join(env.Root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", gitDir, err)
	}
	policy.AddProtectedPaths(gitDir)

	profile, err := BuildProfile(policy)
	if err != nil || profile == nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if !conformanceContains(profile.Protected, gitDir) {
		t.Fatalf("Protected 缺少 .git（%q）: %v", gitDir, profile.Protected)
	}
	if !conformanceContains(profile.ReadWrite, env.Root) {
		t.Fatalf("ReadWrite 缺少工作区: %v", profile.ReadWrite)
	}
	if profile.Network {
		t.Fatal("AllowNetwork=false 时 profile.Network 应当为 false")
	}

	sbpl := SeatbeltProfile(policy)
	for _, fragment := range []string{
		"(deny file-write*)",
		"(allow file-write*",
		"(deny network*",
	} {
		if !conformanceContainsSubstring(sbpl, fragment) {
			t.Fatalf("SBPL 缺少 %q（工作区可写/.git 只读/断网 全靠它们）:\n%s", fragment, sbpl)
		}
	}
	if conformanceContainsSubstring(sbpl, "(allow default)") == false {
		t.Fatal("SBPL 应当保留 allow default（读与系统服务默认放行），否则连 dyld 都起不来")
	}
}

// conformanceContains 报告 paths 里是否有 want。
//
// 用 samePathForTest 而不是字符串相等：macOS 上 `/var/...` 与 `/private/var/...`
// 是同一个目录的两种写法，Windows 上 `t.TempDir()` 可能是 8.3 短名而产品返回长名。
func conformanceContains(paths []string, want string) bool {
	for _, path := range paths {
		if samePathForTest(path, want) {
			return true
		}
	}
	return false
}

// conformanceContainsSubstring 是 strings.Contains 的本地别名（避免与其它测试文件
// 里同名 helper 冲突——这个包里同时有多个 agent 在写用例）。
func conformanceContainsSubstring(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
