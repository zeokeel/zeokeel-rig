//go:build windows

package osrunner

// Windows 真机层：把 conformance_test.go 的同一张表跑在"受限令牌 + 目录 ACL"后端上。
//
// 只在 Windows 机器上、且 RIG_CONFORMANCE=1 时真的执行。CI 的 windows job 里加：
//
//	RIG_CONFORMANCE=1 go test ./sandbox/osrunner/ -run Conformance -v
//
// 注意：Windows 后端是 **partial**（读与网络不受限），因此表里"读"类场景与
// 网络类场景都标成 record；写类场景才是硬断言。
//
// 清理：用例全部落在 t.TempDir 下；本文件不碰注册表、不装服务、不建 junction
// （junction 用例在 conformance_test.go 里标了 perOS 跳过原因）。

import (
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// conformancePlatform 返回 Windows 的环境与后端列表。
func conformancePlatform(t *testing.T) (conformanceEnv, []conformanceBackend) {
	t.Helper()
	env := conformanceNewEnv(t, "windows")
	// cmd 的 /c 与 POSIX 的 -c 等价；脚本内容由表里的 Cmd 字段提供。
	env.Shell = []string{"cmd", "/c"}

	runner, err := New(Config{})
	if err != nil {
		t.Logf("SKIP backend windows-acl: %v", err)
		return env, nil
	}
	return env, []conformanceBackend{{Name: RunnerWindowsACL, Runner: runner, Enforcing: true}}
}

// TestConformanceProfileRenderingWindows 是**纯逻辑层**的 Windows 部分：只断言
// 受控语言（Constrained Language）命令的计划参数，不执行。
func TestConformanceProfileRenderingWindows(t *testing.T) {
	env := conformanceNewEnv(t, "windows")
	policy := sandbox.New(
		sandbox.WithRoots(env.Root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithNetwork(false),
	)
	policy.AddProtectedPaths(env.Root + `\.git`)

	profile, err := BuildProfile(policy)
	if err != nil || profile == nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if !conformanceHasPath(profile.Protected, env.Root+`\.git`) {
		t.Fatalf("Protected 缺少 .git: %v", profile.Protected)
	}
	if !conformanceHasPath(profile.ReadWrite, env.Root) {
		t.Fatalf("ReadWrite 缺少工作区: %v", profile.ReadWrite)
	}
	// danger-full-access 必须返回 (nil,nil)：Windows 后端此时应当是透传。
	full := sandbox.New(sandbox.WithRoots(env.Root), sandbox.WithMode(sandbox.ModeDangerFullAccess))
	if p, err := BuildProfile(full); err != nil || p != nil {
		t.Fatalf("danger-full-access 应当返回 (nil,nil)，得到 %v, %v", p, err)
	}
}

// conformanceHasPath 报告 paths 里是否有 want（Windows 上按大小写不敏感比较）。
func conformanceHasPath(paths []string, want string) bool {
	for _, path := range paths {
		if len(path) == len(want) {
			equal := true
			for index := 0; index < len(path); index++ {
				left, right := path[index], want[index]
				if left >= 'A' && left <= 'Z' {
					left += 'a' - 'A'
				}
				if right >= 'A' && right <= 'Z' {
					right += 'a' - 'A'
				}
				if left != right {
					equal = false
					break
				}
			}
			if equal {
				return true
			}
		}
	}
	return false
}
