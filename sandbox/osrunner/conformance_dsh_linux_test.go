//go:build linux

package osrunner

// Linux 专属的 DSH 对齐断言（landlockRunner 只存在于 Linux 构建里）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// TestDSHParityLandlockConfineArgv 断言 runner 渲染出的 helper 参数形状：
// `--ro / --rw /dev/null [--rw /tmp --rw <ws>] -- <cmd...>`。
func TestDSHParityLandlockConfineArgv(t *testing.T) {
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

// TestDSHParityLandlockGrants 断言 Landlock 的 --ro/--rw 集合与 DSH 完全一致。
func TestDSHParityLandlockGrants(t *testing.T) {
	workspace, tmpdir := dshParityRoots(t)

	readOnly, readWrite := LandlockGrants(sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeReadOnly)))
	if strings.Join(readOnly, "\x00") != "/" {
		t.Fatalf("LandlockGrants(read-only) ro = %q, want [/]", readOnly)
	}
	if strings.Join(readWrite, "\x00") != "/dev/null" {
		t.Fatalf("LandlockGrants(read-only) rw = %q, want [/dev/null]", readWrite)
	}

	readOnly, readWrite = LandlockGrants(sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeWorkspaceWrite)))
	wantWrite := []string{"/dev/null", "/tmp", tmpdir, workspace}
	if strings.Join(readWrite, "\x00") != strings.Join(wantWrite, "\x00") {
		t.Fatalf("LandlockGrants(workspace-write) rw\n got = %q\nwant = %q", readWrite, wantWrite)
	}
	if strings.Join(readOnly, "\x00") != "/" {
		t.Fatalf("LandlockGrants(workspace-write) ro = %q, want [/]", readOnly)
	}

	// 对齐后没有任何"根内只读子路径"：工作区里存在 .git 也不会产生额外的 --ro，
	// 更不会出现在 rw 之外的地方（Landlock 的并集语义本来也无法收窄）。
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	readOnly, readWrite = LandlockGrants(policy)
	for _, grant := range readOnly {
		if grant == workspace || strings.HasPrefix(grant, workspace+"/") {
			t.Fatalf("保护路径不应产生额外的 --ro 规则：ro = %q", readOnly)
		}
	}
	for _, grant := range readWrite {
		if strings.HasSuffix(grant, ".git") {
			t.Fatalf("保护路径不应出现在 rw 里：rw = %q", readWrite)
		}
	}
}

// TestDSHParitySeatbeltProfileReadOnly 断言 read-only 的 SBPL **逐字符**等于 DSH。
