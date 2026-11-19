//go:build windows

package osrunner

// Windows 真机层：给 conformance_test.go 的场景表提供受限令牌 + ACL 后端。

import "testing"

// conformancePlatform 返回 Windows 的环境与后端列表。
func conformancePlatform(t *testing.T) (conformanceEnv, []conformanceBackend) {
	t.Helper()
	env := conformanceNewEnv(t, "windows")
	env.Shell = []string{"cmd.exe", "/c"}
	if runner, err := newPlatformRunner(Config{}); err == nil {
		return env, []conformanceBackend{{Name: RunnerWindowsACL, Runner: runner, Enforcing: true}}
	} else {
		t.Logf("SKIP backend windows-acl: %v", err)
	}
	return env, nil
}
