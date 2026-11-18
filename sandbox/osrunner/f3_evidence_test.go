//go:build linux

package osrunner

// F3 的真机证据：`git init` 出来的 `.git` 在"构造时还不存在"的前提下不再被静默放行。
//
//	RIG_SANDBOX_EVIDENCE=1 go test ./sandbox/osrunner/ -run EvidenceF3 -v

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func TestEvidenceF3GitProtectMissing(t *testing.T) {
	if os.Getenv("RIG_SANDBOX_EVIDENCE") != "1" {
		t.Skip("证据用例默认跳过；设 RIG_SANDBOX_EVIDENCE=1 启用")
	}
	runner := localBwrap(t)
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// 关键：保护的是 <ws>/.git —— 此刻它**还不存在**（就是审计场景）
	policy := sandbox.New(
		sandbox.WithRoots(ws),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithNetwork(false),
		sandbox.WithProtectedPaths(filepath.Join(ws, ".git")),
	)
	profile, _ := BuildProfile(policy)
	t.Logf("MissingProtected = %v", profile.MissingProtected)

	script := "cd " + filepath.Join(ws, "nested") + " && git init -q . && echo evil >> .git/config && echo WROTE"
	confined, err := runner.Confine([]string{"/bin/sh", "-c", script}, policy)
	if err != nil {
		t.Logf("bwrap Confine 拒绝执行：%v", err)
	} else {
		cmd := exec.Command(confined.Argv[0], confined.Argv[1:]...)
		out, runErr := cmd.CombinedOutput()
		t.Logf("bwrap 实际执行：err=%v out=%s", runErr, strings.TrimSpace(string(out)))
	}

	// 对照：宿主显式选择"只报告"时会怎样（记录旧行为，便于报告）
	lenient := sandbox.New(
		sandbox.WithRoots(ws),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithNetwork(false),
		sandbox.WithProtectedPaths(filepath.Join(ws, ".git")),
		sandbox.WithProtectMissingPaths(false),
	)
	confined2, err2 := runner.Confine([]string{"/bin/sh", "-c", script}, lenient)
	if err2 != nil {
		t.Logf("只报告模式下 Confine 仍然失败：%v", err2)
		return
	}
	cmd2 := exec.Command(confined2.Argv[0], confined2.Argv[1:]...)
	out2, runErr2 := cmd2.CombinedOutput()
	t.Logf("只报告模式实际执行：err=%v out=%s", runErr2, strings.TrimSpace(string(out2)))

	// Seatbelt 的等价证据（纯渲染，可在 Linux 上验）
	sbpl := SeatbeltProfile(policy)
	t.Logf("Seatbelt 渲染该缺失路径=%v", strings.Contains(sbpl, sbplString(filepath.Join(ws, ".git"))))
}
