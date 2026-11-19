//go:build linux

package osrunner

// Linux 专属的 DSH 对齐断言（landlockRunner 只存在于 Linux 构建里）。

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// TestDSHParityLandlockConfineArgv 断言 runner 渲染出的 helper 参数形状：
// `--ro / --rw /dev/null [--rw /tmp --rw <ws>] -- <cmd...>`。
func TestDSHParityLandlockConfineArgv(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Landlock 是 Linux 后端")
	}
	workspace, tmpdir := dshParityRoots(t)
	runner := &landlockRunner{exe: "/opt/rig/host", abi: 4}
	confined, err := runner.Confine([]string{"/bin/true"}, sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeReadOnly)))
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	want := []string{"/opt/rig/host", HelperArgLandlock, "--ro", "/", "--rw", "/dev/null", "--", "/bin/true"}
	if strings.Join(confined.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("Confine(read-only)\n got = %q\nwant = %q", confined.Argv, want)
	}

	confined, err = runner.Confine([]string{"/bin/true"}, sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeWorkspaceWrite)))
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	want = []string{"/opt/rig/host", HelperArgLandlock,
		"--ro", "/",
		"--rw", "/dev/null",
		"--rw", "/tmp",
		"--rw", tmpdir,
		"--rw", workspace,
		"--", "/bin/true"}
	if strings.Join(confined.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("Confine(workspace-write)\n got = %q\nwant = %q", confined.Argv, want)
	}
	// 断网策略不得改变形状（DSH 没有 --no-net / 网络域）。
	noNet := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeReadOnly))
	confinedNet, err := runner.Confine([]string{"/bin/true"}, noNet)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	for _, arg := range confinedNet.Argv {
		if arg == "--no-net" {
			t.Fatalf("Landlock 参数里出现了 DSH 之外的 --no-net: %q", confinedNet.Argv)
		}
	}
}

// TestDSHParityNoHostSideEffectsLandlock 是 TestDSHParityNoHostSideEffects 的
// Landlock 部分：Confine 只做 argv 渲染，不得在宿主 HOME 里创建任何目录。
func TestDSHParityNoHostSideEffectsLandlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	workspace := t.TempDir()
	for _, mode := range []sandbox.Mode{sandbox.ModeReadOnly, sandbox.ModeWorkspaceWrite} {
		policy := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(mode))
		if _, err := (&landlockRunner{exe: "/opt/rig/host", abi: 4}).Confine([]string{"/bin/true"}, policy); err != nil {
			t.Fatalf("landlock Confine(%s): %v", mode, err)
		}
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("read home: %v", err)
	}
	if len(entries) != 0 {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("Confine 在宿主 HOME 里创建了目录 %v", names)
	}
}
