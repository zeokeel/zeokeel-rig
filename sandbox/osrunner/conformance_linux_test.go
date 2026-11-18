//go:build linux

package osrunner

// Linux 真机层：把 conformance_test.go 的表跑在两个真实后端上（bwrap + Landlock）。
//
// 之所以两个后端都跑：审计已经证明它们在**网络隔离**、**元数据治理**、**私有 /tmp**
// 上有实质差异——同一张表跑两遍才能把差异暴露成数据，而不是文档里的一句话。

import (
	"os"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// conformancePlatform 返回 Linux 的环境与后端列表。
func conformancePlatform(t *testing.T) (conformanceEnv, []conformanceBackend) {
	t.Helper()
	env := conformanceNewEnv(t, "linux")
	env.Shell = []string{"/bin/sh", "-c"}
	var backends []conformanceBackend
	if path, reason := probeBwrapDetailed(DefaultProbeTimeout); reason == "" {
		backends = append(backends, conformanceBackend{Name: RunnerBwrap, Runner: &bwrapRunner{path: path}, Enforcing: true})
	} else {
		t.Logf("SKIP backend bwrap: %s", reason)
	}
	if abi, ok := probeLandlock(DefaultProbeTimeout); ok {
		if exe, err := os.Executable(); err == nil && exe != "" {
			backends = append(backends, conformanceBackend{Name: RunnerLandlock, Runner: &landlockRunner{exe: exe, abi: abi}, Enforcing: true})
		}
	} else {
		t.Log("SKIP backend landlock: 本机 Landlock 不可用或 helper 探测失败")
	}
	return env, backends
}

// TestConformanceProfileRenderingLinux 是**纯逻辑层**的 Linux 部分：
// 同一份策略渲染出来的 profile 必须满足"该拒的拒、该放的放"，且两个后端看到的
// 可写面/保护面要同源（BwrapArgs 与 LandlockGrants 都从 BuildProfile 派生）。
func TestConformanceProfileRenderingLinux(t *testing.T) {
	env := conformanceNewEnv(t, "linux")
	root := env.Root
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithNetwork(false),
	)
	policy.AddProtectedPaths(root + "/.git")
	if err := os.MkdirAll(root+"/.git", 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	profile, err := BuildProfile(policy)
	if err != nil || profile == nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	// 1) 保护路径必须出现在 Protected 里（否则 .git 实际可写）。
	if !conformanceContainsPath(profile.Protected, root+"/.git") {
		t.Fatalf("Protected 缺少 .git: %v", profile.Protected)
	}
	// 2) 工作区必须在可写面里。
	if !conformanceContainsPath(profile.ReadWrite, root) {
		t.Fatalf("ReadWrite 缺少工作区: %v", profile.ReadWrite)
	}
	// 3) 网络关闭时必须渲染成"断网"。
	if profile.Network {
		t.Fatal("AllowNetwork=false 时 profile.Network 应当为 false")
	}
	// 4) 两个后端的渲染必须包含同一份可写/保护面。
	bwrapArgs := BwrapArgs(policy)
	if !conformanceArgsContain(bwrapArgs, "--unshare-net") {
		t.Fatalf("Network=false 时 bwrap 缺 --unshare-net: %v", bwrapArgs)
	}
	if !conformanceArgsContainPair(bwrapArgs, "--ro-bind", root+"/.git") {
		t.Fatalf("bwrap 缺少 .git 的只读覆盖: %v", bwrapArgs)
	}
	readOnly, readWrite := LandlockGrants(policy)
	if !conformanceContainsPath(readOnly, "/") {
		t.Fatalf("landlock 缺少 --ro /: %v", readOnly)
	}
	// 5) Landlock 的保护语义是"写授权下移"：.git 必须**不在**可写集合里。
	if conformanceContainsPath(readWrite, root+"/.git") {
		t.Fatalf("landlock 把受保护的 .git 放进了 --rw 集合: %v", readWrite)
	}
	// 6) 下移的代价（**已知边界，锁住现状**）：一旦某个根里含保护路径，根自身会进
	//    只读集合，写授权只剩下它**已存在的子项**。后果是"工作区根级的文件"在
	//    Landlock 下不可写（新建与覆写都不行），而进程内策略 AllowsWrite 仍说可以
	//    ——两者不一致，见 conformance 表的 workspace/*-in-workspace-root 两行。
	if !conformanceContainsPath(readOnly, root) {
		t.Fatalf("landlock 应当把含保护路径的根放进只读集合（下移写授权），实际 RO=%v", readOnly)
	}
	if !conformanceContainsPath(readWrite, root+"/.git/objects") && conformanceContainsPath(readWrite, root+"/.git") {
		t.Fatalf("landlock 把 .git 的子树放进了可写集合: %v", readWrite)
	}
	// 6) danger-full-access 不渲染任何约束（profile 为 nil）。
	full := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeDangerFullAccess))
	if p, err := BuildProfile(full); err != nil || p != nil {
		t.Fatalf("danger-full-access 应当返回 (nil,nil)，得到 %v, %v", p, err)
	}
}

// containsPath 报告 paths 里是否有 want（按 Clean 比较）。
func conformanceContainsPath(paths []string, want string) bool {
	for _, path := range paths {
		if samePathForTest(path, want) {
			return true
		}
	}
	return false
}

// argsContain 报告参数里是否有 want。
func conformanceArgsContain(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// argsContainPair 报告是否存在相邻的 flag+value。
func conformanceArgsContainPair(args []string, flag, value string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag && args[index+1] == value {
			return true
		}
	}
	return false
}

var _ = sandbox.ModeWorkspaceWrite
