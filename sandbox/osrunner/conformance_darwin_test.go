//go:build darwin

package osrunner

// macOS 真机层：给 conformance_test.go 的场景表提供 sandbox-exec 后端。

import "testing"

// conformancePlatform 返回 macOS 的环境与后端列表。
func conformancePlatform(t *testing.T) (conformanceEnv, []conformanceBackend) {
	t.Helper()
	env := conformanceNewEnv(t, "darwin")
	env.Shell = []string{"/bin/sh", "-c"}
	if runner, err := newPlatformRunner(Config{}); err == nil {
		return env, []conformanceBackend{{Name: RunnerSeatbelt, Runner: runner, Enforcing: true}}
	} else {
		t.Logf("SKIP backend seatbelt: %v", err)
	}
	return env, nil
}
