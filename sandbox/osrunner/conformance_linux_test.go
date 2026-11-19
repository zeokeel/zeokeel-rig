//go:build linux

package osrunner

// Linux 真机层：给 conformance_test.go 的场景表提供两个真实后端（bwrap + Landlock）。
//
// 纯逻辑层（argv/规则渲染）的断言在 conformance_dsh_test.go 里，与 DSH 逐 token /
// 逐字符比对；这里只负责"把表跑起来"。

import (
	"os"
	"testing"
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
