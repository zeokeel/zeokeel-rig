//go:build linux

package osrunner

// bwrap 是 **Linux 专属**后端：这些用例断言它与 DSH 的 argv 逐 token 一致。
// 放在 `//go:build linux` 而不是 `!windows`：macOS/darwin 上根本没有 bwrap，
// 在那儿跑这些断言只会得到"平台假设错"的假红。

import (
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// TestDSHParityBwrapArgsReadOnly 断言 read-only 的 bwrap 参数**完全等于** DSH 的
// 10 个 token（顺序也一致）。
func TestDSHParityBwrapArgsReadOnly(t *testing.T) {
	workspace, _ := dshParityRoots(t)
	policy := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeReadOnly))
	got := BwrapArgs(policy)
	want := []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--unshare-pid",
		"--proc", "/proc",
		"--die-with-parent",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("BwrapArgs(read-only)\n got = %q\nwant = %q", got, want)
	}
}

// TestDSHParityBwrapArgsWorkspaceWrite 断言 workspace-write = 10 个基础 token +
// `--tmpfs /tmp` + `--bind <ws> <ws>`。
func TestDSHParityBwrapArgsWorkspaceWrite(t *testing.T) {
	workspace, _ := dshParityRoots(t)
	policy := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	got := BwrapArgs(policy)
	want := []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--unshare-pid",
		"--proc", "/proc",
		"--die-with-parent",
		"--tmpfs", "/tmp",
		"--bind", workspace, workspace,
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("BwrapArgs(workspace-write)\n got = %q\nwant = %q", got, want)
	}
	// 只有工作区被 bind：临时目录由 --tmpfs 承载，宿主 /tmp /var/tmp 不再 bind。
	binds := 0
	for index, arg := range got {
		if arg != "--bind" {
			continue
		}
		binds++
		if index+2 < len(got) && got[index+1] != workspace {
			t.Fatalf("BwrapArgs 出现了非工作区的 --bind 目标：%q", got[index+1])
		}
	}
	if binds != 1 {
		t.Fatalf("BwrapArgs 的 --bind 数量 = %d, want 1（只 bind 工作区）: %q", binds, got)
	}
}

// TestDSHParityBwrapArgsDangerFullAccess 断言 danger-full-access 不渲染任何参数。
// TestDSHParityBwrapArgsNoRigOnlyTokens 断言 DSH 没有的加固 token 一个都不再出现。
//
// 这条是"删除清单"的可执行版本：任何人把它们加回来，不管加在哪个模式，都会红。
func TestDSHParityBwrapArgsNoRigOnlyTokens(t *testing.T) {
	workspace, _ := dshParityRoots(t)
	removed := []string{"--cap-drop", "--remount-ro", "--unshare-net", "--seccomp", "--unshare-user", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup"}
	for _, mode := range []sandbox.Mode{sandbox.ModeReadOnly, sandbox.ModeWorkspaceWrite} {
		policy := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(mode), sandbox.WithNetwork(false))
		args := BwrapArgs(policy)
		for _, token := range removed {
			for _, arg := range args {
				if arg == token {
					t.Fatalf("mode=%s: BwrapArgs 里仍有 DSH 之外的 token %q: %q", mode, token, args)
				}
			}
		}
		// AllowNetwork=false 不得改变渲染结果（DSH 不按网络生成参数）。
		withNet := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(mode), sandbox.WithNetwork(true))
		if strings.Join(BwrapArgs(withNet), "\x00") != strings.Join(args, "\x00") {
			t.Fatalf("mode=%s: AllowNetwork 影响了 BwrapArgs 渲染：%q vs %q", mode, BwrapArgs(withNet), args)
		}
	}
}
