//go:build linux

package osrunner

// bwrap 的两个加固点：宿主守护进程 socket 屏蔽、私有 /tmp 与"工作区在 /tmp 下"
// 的冲突。都是审计在真机上踩出来的。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// localBwrap 返回本机 bwrap 路径；不可用时跳过。
func localBwrap(t *testing.T) *bwrapRunner {
	t.Helper()
	path, reason := probeBwrapDetailed(DefaultProbeTimeout)
	if reason != "" {
		t.Skipf("bwrap 不可用：%s", reason)
	}
	return &bwrapRunner{path: path}
}

// runBwrap 在 bwrap 下执行命令，返回合并输出。
func runBwrap(t *testing.T, runner *bwrapRunner, policy *sandbox.Policy, argv ...string) (string, error) {
	t.Helper()
	confined, err := runner.Confine(argv, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	cmd := exec.Command(confined.Argv[0], confined.Argv[1:]...)
	cmd.Env = os.Environ()
	out, runErr := cmd.CombinedOutput()
	return string(out), runErr
}

// ---- 1) 守护进程 socket 屏蔽 -------------------------------------------------

// bwrapSocketMaskingArgs 必须把每个存在的 socket 渲染成 `--ro-bind /dev/null <sock>`。
func TestBwrapSocketMaskingArgsShape(t *testing.T) {
	got := bwrapSocketMaskingArgs([]string{"/var/run/docker.sock"})
	want := []string{"--ro-bind", "/dev/null", "/var/run/docker.sock"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q（整体 %v）", i, got[i], want[i], got)
		}
	}
}

// 守护进程 socket 候选清单必须覆盖 docker/podman/containerd 的两种落点。
func TestDaemonSocketPathsCoverage(t *testing.T) {
	want := []string{
		"/var/run/docker.sock", "/run/docker.sock",
		"/run/podman/podman.sock", "/var/run/podman/podman.sock",
		"/run/containerd/containerd.sock", "/var/run/containerd/containerd.sock",
	}
	have := map[string]bool{}
	for _, path := range daemonSocketPaths() {
		have[path] = true
	}
	for _, path := range want {
		if !have[path] {
			t.Errorf("候选清单缺少 %q", path)
		}
	}
}

// 只屏蔽**存在**的 socket：不存在的路径不能被渲染成挂载（bwrap 会直接失败）。
func TestExistingDaemonSocketsOnlyReturnsExisting(t *testing.T) {
	for _, path := range existingDaemonSockets() {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("existingDaemonSockets 返回了不存在的 %q", path)
		}
	}
}

// 真机：被屏蔽的 socket 在沙箱里应当表现为 /dev/null（或直接不可用）。
func TestBwrapMasksDockerSocketInSandbox(t *testing.T) {
	runner := localBwrap(t)
	sockets := existingDaemonSockets()
	if len(sockets) == 0 {
		t.Skip("宿主没有 docker/podman/containerd socket，无法验证屏蔽效果")
	}
	target := sockets[0]
	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	out, err := runBwrap(t, runner, policy, "/bin/sh", "-c", "ls -l "+target+" 2>&1 || true")
	if err != nil {
		t.Fatalf("bwrap 运行失败：%v\n%s", err, out)
	}
	// 屏蔽生效的判据：它不再是 socket（ls 首列不是 s），而是 /dev/null 的字符
	// 设备（`crw... 1, 3`），或者干脆不可访问。
	trimmed := strings.TrimSpace(out)
	fields := strings.Fields(trimmed)
	masked := strings.Contains(trimmed, "No such file") ||
		(len(fields) > 0 && strings.HasPrefix(fields[0], "c")) ||
		strings.Contains(trimmed, "1, 3")
	if !masked {
		t.Fatalf("%s 在沙箱内没有被屏蔽：%s", target, trimmed)
	}
}

// ---- 2) 私有 /tmp 与工作区在 /tmp 下 ----------------------------------------

// 工作区在 /tmp **之下**时：tmpfs 仍然启用，但工作区必须被重新挂回来并且
// 诊断里说明这件事（实测 bwrap 0.9：--tmpfs /tmp 之后的 --bind 会创建挂载点）。
func TestBwrapKeepsPrivateTmpAndRebindsWorkspaceUnderTmp(t *testing.T) {
	ws := t.TempDir() // 默认就在 /tmp 下
	if !strings.HasPrefix(ws, "/tmp/") && !strings.HasPrefix(ws, "/private/tmp/") {
		t.Skipf("t.TempDir() 不在 /tmp 下（%s），无法验证", ws)
	}
	policy := sandbox.New(
		sandbox.WithRoots(ws),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if !profile.PrivateTmp {
		t.Fatalf("工作区在 /tmp **之下**时不应禁用私有 /tmp：%s", profile.TmpDisabledReason)
	}
	if !strings.Contains(profile.TmpDisabledReason, "re-bound") {
		t.Fatalf("诊断没有说明工作区被重新挂回：%q", profile.TmpDisabledReason)
	}
	// 挂载顺序：--tmpfs /tmp 必须排在 --bind <ws> 之前，否则工作区被 tmpfs 盖住。
	args := BwrapArgs(policy)
	tmpfsAt, bindAt := -1, -1
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--tmpfs" && args[i+1] == "/tmp" {
			tmpfsAt = i
		}
		if args[i] == "--bind" && sandbox.PathEqual(args[i+1], ws) {
			bindAt = i
		}
	}
	if tmpfsAt < 0 {
		t.Fatalf("缺少 --tmpfs /tmp：%v", args)
	}
	if bindAt < 0 {
		t.Fatalf("缺少 --bind %s：%v", ws, args)
	}
	if bindAt < tmpfsAt {
		t.Fatalf("--bind <ws> 必须排在 --tmpfs /tmp 之后（否则工作区被遮住）：%v", args)
	}
}

// 策略根**就是** /tmp 时两者不可兼得：禁用私有 tmpfs，保证工作区可见。
func TestBwrapDisablesPrivateTmpWhenWorkspaceIsTmpItself(t *testing.T) {
	tempRoots := platformTempRoots()
	if len(tempRoots) == 0 {
		t.Skip("当前平台没有临时根")
	}
	policy := sandbox.New(sandbox.WithRoots(tempRoots[0]), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if profile.PrivateTmp {
		t.Fatal("策略根就是临时目录时私有 /tmp 必须禁用")
	}
	if !strings.Contains(profile.TmpDisabledReason, "disabled") {
		t.Fatalf("诊断没有说明禁用原因：%q", profile.TmpDisabledReason)
	}
	args := BwrapArgs(policy)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--tmpfs" && args[i+1] == tempRoots[0] {
			t.Fatalf("已经禁用私有 tmpfs，却仍然渲染了 --tmpfs：%v", args)
		}
	}
}

// 真机：工作区在 /tmp 下时，沙箱内能看到并写入工作区文件。
func TestBwrapWorkspaceUnderTmpStaysVisible(t *testing.T) {
	runner := localBwrap(t)
	ws := t.TempDir()
	marker := filepath.Join(ws, "marker.txt")
	if err := os.WriteFile(marker, []byte("hi"), 0o644); err != nil {
		t.Fatalf("prep: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(ws), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	out, err := runBwrap(t, runner, policy, "/bin/sh", "-c", "cat "+marker+" && echo x > "+filepath.Join(ws, "new.txt")+" && echo WROTE")
	if err != nil || !strings.Contains(out, "WROTE") {
		t.Fatalf("工作区在 /tmp 下时不可见/不可写：err=%v\n%s", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(ws, "new.txt")); statErr != nil {
		t.Fatalf("沙箱内的写入没有落到宿主工作区：%v", statErr)
	}
}

// 工作区不在 /tmp 下时，私有 /tmp 仍然启用（不误伤默认能力）。
func TestBwrapKeepsPrivateTmpOutsideTmp(t *testing.T) {
	ws, err := os.MkdirTemp("/data/zeokeel-rig/.audit-scratch", "ws-")
	if err != nil {
		t.Skipf("无法在 /tmp 之外建目录：%v", err)
	}
	defer os.RemoveAll(ws)
	policy := sandbox.New(sandbox.WithRoots(ws), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	profile, buildErr := BuildProfile(policy)
	if buildErr != nil {
		t.Fatalf("BuildProfile: %v", buildErr)
	}
	if !profile.PrivateTmp {
		t.Fatalf("工作区不在 /tmp 下时私有 /tmp 不该被禁用：%s", profile.TmpDisabledReason)
	}
}
