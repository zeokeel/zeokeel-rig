package sandbox_test

// read-only + OS runner 的命令校验（A4 收尾）。
//
// read-only 档以前在策略层就整体拒绝 shell，于是"runner 已经能挡住写工作区"的
// 组合也跑不了任何命令。CheckConfinedCommand 是给那种组合用的：它保留命令语法与
// **宿主显式配置的允许/拒绝名单**，只跳过"只读档不许有写操作"这一条——因为写围栏
// 由 OS runner 承担。CheckCommand 的旧语义（read-only 一律拒绝）逐字不变。

import (
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// TestCheckConfinedCommandAllowsWritesInReadOnly 钉住 read-only+confined 下
// 带写重定向的命令能通过策略层校验（口径与工具层一致：先 ParseShellCommand，
// 再校验 argv[0]）。同一个 argv 用旧入口 CheckCommand 仍然被拒。
func TestCheckConfinedCommandAllowsWritesInReadOnly(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeReadOnly),
		sandbox.WithAllowShellOperators(),
		sandbox.WithAnyCommandAllowed(true),
	)
	argv, err := policy.ParseShellCommand(`echo x > "$TMPDIR/f"`)
	if err != nil {
		t.Fatalf("ParseShellCommand = %v, want nil", err)
	}
	if err := policy.CheckConfinedCommand(argv, ""); err != nil {
		t.Fatalf("CheckConfinedCommand(read-only) = %v, want nil（写围栏由 runner 负责）", err)
	}
	if err := policy.CheckCommand(argv, ""); err == nil {
		t.Fatal("CheckCommand(read-only) = nil, want rejection（旧入口语义不变）")
	}
}

// TestCheckCommandStillRejectsReadOnly 钉住兼容性：旧签名/旧方法在 read-only 下
// 仍然拒绝（不因为新增了 confined 变体就放宽）。
func TestCheckCommandStillRejectsReadOnly(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithMode(sandbox.ModeReadOnly))
	err := policy.CheckCommand([]string{"echo", "hi"}, "")
	if err == nil {
		t.Fatal("CheckCommand(read-only) = nil, want rejection（旧语义必须不变）")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("CheckCommand(read-only) = %v, want a read-only reason", err)
	}
}

// TestCheckConfinedCommandKeepsDenyList 钉住 confined 变体**不**放宽宿主显式
// 配置的命令名单。
func TestCheckConfinedCommandKeepsDenyList(t *testing.T) {
	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeReadOnly),
		sandbox.WithAnyCommandAllowed(true),
		sandbox.WithCommandDenylist("rm"),
	)
	err := policy.CheckConfinedCommand([]string{"rm", "-rf", "x"}, "")
	if err == nil {
		t.Fatal("CheckConfinedCommand(denied command) = nil, want rejection（deny 名单必须继续生效）")
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Fatalf("CheckConfinedCommand = %v, want a deny reason", err)
	}
	if err := policy.CheckConfinedCommand([]string{"echo", "hi"}, ""); err != nil {
		t.Fatalf("CheckConfinedCommand(echo) = %v, want nil", err)
	}
}

// TestCheckConfinedCommandKeepsAllowList 钉住 confined 变体仍按 allow 名单过滤。
func TestCheckConfinedCommandKeepsAllowList(t *testing.T) {
	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeReadOnly),
		sandbox.WithCommandAllowlist("echo"),
	)
	if err := policy.CheckConfinedCommand([]string{"echo", "hi"}, ""); err != nil {
		t.Fatalf("CheckConfinedCommand(echo) = %v, want nil", err)
	}
	err := policy.CheckConfinedCommand([]string{"curl", "http://example.invalid"}, "")
	if err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("CheckConfinedCommand(curl) = %v, want an allowlist rejection", err)
	}
}

// TestCheckConfinedCommandKeepsWorkdirFence 钉住 workdir 必须仍在根内。
func TestCheckConfinedCommandKeepsWorkdirFence(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly))
	if err := policy.CheckConfinedCommand([]string{"echo", "hi"}, ".."); err == nil {
		t.Fatal("CheckConfinedCommand(workdir=..) = nil, want rejection")
	}
}
