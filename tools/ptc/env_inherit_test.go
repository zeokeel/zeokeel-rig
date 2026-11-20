package ptc

// run_code 解释器进程的环境：与 sandbox 同一口径——**默认原样继承**，
// 只有宿主显式 WithEnvAllowlist(...) 时才过滤。
//
// 以前这里在"没有策略"时也会套一份最小白名单（手抄 sandbox 的名单），属于
// "DSH 没有、我们自己加的"约束；现在白名单只在显式配置时生效。

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

const envInheritSecretKey = "RIG_AUDIT_SECRET"

// osEnviron 是 os.Environ 的薄封装，便于在断言里重复取当前环境。
func osEnviron() []string { return os.Environ() }

func envHasKey(entries []string, key string) bool {
	for _, entry := range entries {
		if name, _, ok := strings.Cut(entry, "="); ok && name == key {
			return true
		}
	}
	return false
}

// TestInterpreterEnvInheritsByDefault：无策略时必须原样继承宿主环境。
func TestInterpreterEnvInheritsByDefault(t *testing.T) {
	t.Setenv(envInheritSecretKey, "s3cr3t-value")
	t.Setenv("PYTHONPATH", "/opt/py")

	got := interpreterEnv(nil)
	if !slices.Equal(got, osEnviron()) {
		t.Fatalf("无策略时解释器环境应当原样继承 os.Environ()（len got=%d want=%d）",
			len(got), len(osEnviron()))
	}
	for _, key := range []string{envInheritSecretKey, "PYTHONPATH", "PATH"} {
		if !envHasKey(got, key) {
			t.Fatalf("继承的环境里缺少 %s", key)
		}
	}
}

// TestInterpreterEnvDefaultPolicyInherits：挂了策略但没配白名单，同样原样继承。
func TestInterpreterEnvDefaultPolicyInherits(t *testing.T) {
	t.Setenv(envInheritSecretKey, "s3cr3t-value")
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()))
	got := interpreterEnv(policy)
	if !envHasKey(got, envInheritSecretKey) {
		t.Fatalf("有策略（未配白名单）时仍应原样继承，实际丢了 %s", envInheritSecretKey)
	}
}

// TestInterpreterEnvFiltersWithExplicitAllowlist：显式白名单仍然生效。
func TestInterpreterEnvFiltersWithExplicitAllowlist(t *testing.T) {
	t.Setenv(envInheritSecretKey, "s3cr3t-value")
	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithEnvAllowlist("PATH"),
	)
	got := interpreterEnv(policy)
	if envHasKey(got, envInheritSecretKey) {
		t.Fatalf("显式白名单只放行 PATH，却仍带着 %s", envInheritSecretKey)
	}
	if !envHasKey(got, "PATH") {
		t.Fatalf("显式白名单丢了 PATH: %v", got)
	}
	for _, entry := range got {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(name, "PATH") {
			t.Fatalf("显式白名单只放行 PATH，却出现 %q", entry)
		}
	}
}
