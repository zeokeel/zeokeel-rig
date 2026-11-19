package sandbox_test

// 命令环境：**默认原样继承**（DSH 对齐）。
//
// DSH 的 bash 不做环境过滤：模型能看到宿主环境里的 PYTHONPATH / VIRTUAL_ENV /
// GOPATH / 自定义键。Rig 以前加了一层 defaultEnvAllowlist，属于"DSH 没有、我们
// 自己加的"约束；按"防护由沙箱负责、放宽防护让模型充分发挥"的决定，默认改为原样
// 继承，只有宿主显式 WithEnvAllowlist(...) 时才过滤。
//
// 安全含义见 SECURITY.md「凭据」：命令能看到宿主环境变量（含凭据）；要隔离请在
// 容器/VM 里跑，或显式给一份白名单。

import (
	"slices"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func TestCommandEnvInheritsByDefault(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(testRoot(t)))
	base := []string{
		"PATH=/usr/bin:/bin",
		"RIG_AUDIT_SECRET=s3cr3t",
		"PYTHONPATH=/opt/py",
		"VIRTUAL_ENV=/venv",
		"GOPATH=/go",
		"RIG_CUSTOM_KEY=1",
	}
	env := policy.CommandEnv(base)
	if !slices.Equal(env, base) {
		t.Fatalf("CommandEnv 默认应当原样继承:\n got=%v\nwant=%v", env, base)
	}
	// 顺序与重复项也原样保留（不做归一化/去重）。
	dupes := []string{"PATH=/bin", "PATH=/other"}
	if got := policy.CommandEnv(dupes); !slices.Equal(got, dupes) {
		t.Fatalf("默认继承不得去重: %v", got)
	}
	// 必须是副本：调用方改返回值不能污染入参。
	env[0] = "PATH=/tampered"
	if base[0] != "PATH=/usr/bin:/bin" {
		t.Fatalf("CommandEnv 返回了与入参共享底层数组的切片: %v", base)
	}
}

func TestCommandEnvFiltersWhenAllowlistConfigured(t *testing.T) {
	policy := sandbox.New(
		sandbox.WithRoots(testRoot(t)),
		sandbox.WithEnvAllowlist("PATH", "RIG_AUDIT_SECRET"),
	)
	env := policy.CommandEnv([]string{
		"PATH=/bin", "RIG_AUDIT_SECRET=s3cr3t", "PYTHONPATH=/opt/py", "HOME=/h",
	})
	if !slices.Equal(env, []string{"PATH=/bin", "RIG_AUDIT_SECRET=s3cr3t"}) {
		t.Fatalf("显式白名单没有生效: %v", env)
	}
}

func TestCommandEnvExplicitEmptyAllowlistEmptiesEnv(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(testRoot(t)), sandbox.WithEnvAllowlist())
	if got := policy.CommandEnv([]string{"PATH=/bin", "HOME=/h"}); len(got) != 0 {
		t.Fatalf("显式空白名单应当给出空环境: %v", got)
	}
}

func TestCommandEnvNilPolicyStaysEmpty(t *testing.T) {
	var policy *sandbox.Policy
	if got := policy.CommandEnv([]string{"PATH=/bin"}); got != nil {
		t.Fatalf("nil Policy 的 CommandEnv = %v, want nil", got)
	}
}
