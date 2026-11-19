//go:build !windows

package osrunner

// unix 专属的 DSH 对齐断言：Landlock 与 Seatbelt 只在 unix 上存在，它们的渲染里
// 出现 `/`、`/tmp`、`/dev/null` 这类 POSIX 路径字面量——在 Windows 上这些断言没有
// 意义（`filepath.Join` 也会把 `/` 变成 `\\`），因此整份文件按 `!windows` 构建。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

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
func TestDSHParitySeatbeltProfileReadOnly(t *testing.T) {
	workspace, _ := dshParityRoots(t)
	got := SeatbeltProfile(sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeReadOnly)))
	want := `(version 1) (allow default) (deny file-write*) (allow file-write* (literal "/dev/null"))`
	if got != want {
		t.Fatalf("SeatbeltProfile(read-only)\n got = %q\nwant = %q", got, want)
	}
}

// TestDSHParitySeatbeltProfileWorkspaceWrite 断言 workspace-write 的 SBPL 等于
// DSH 的字符序列。
//
// 允许的差异只有一处：**root 的顺序**。DSH 的 `writableRoots()` 是
// workspace → /tmp → os.tmpdir()，本包渲染的也是这个顺序；由于 `t.Setenv("TMPDIR")`
// 让两个临时根互不相同，这里按"前缀完全相等 + 三个 subpath 按序出现"断言，
// 同时用集合再校验一次（防止重复或遗漏）。
func TestDSHParitySeatbeltProfileWorkspaceWrite(t *testing.T) {
	workspace, tmpdir := dshParityRoots(t)
	policy := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	got := SeatbeltProfile(policy)
	want := `(version 1) (allow default) (deny file-write*) (allow file-write* (literal "/dev/null")) ` +
		`(allow file-write* (subpath "` + workspace + `") (subpath "/tmp") (subpath "` + tmpdir + `"))`
	if got != want {
		t.Fatalf("SeatbeltProfile(workspace-write)\n got = %q\nwant = %q", got, want)
	}
	// 集合校验：三个 subpath 各出现一次，别的都没有。
	if n := strings.Count(got, "(subpath "); n != 3 {
		t.Fatalf("subpath 数量 = %d, want 3: %q", n, got)
	}
}

// TestDSHParitySeatbeltProfileNoRigOnlyRules 断言 DSH 之外的 SBPL 规则一条都不回来。
func TestDSHParitySeatbeltProfileNoRigOnlyRules(t *testing.T) {
	workspace, _ := dshParityRoots(t)
	policy := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeWorkspaceWrite), sandbox.WithNetwork(false))
	got := SeatbeltProfile(policy)
	for _, removed := range []string{
		"(allow pseudo-tty)",
		"/dev/ptmx",
		"/dev/tty",
		"/dev/ttys",
		"system-fcntl",
		"user-preference-write",
		"(deny network*)",
		"(deny file-write* (subpath",
	} {
		if strings.Contains(got, removed) {
			t.Fatalf("SeatbeltProfile 里仍有 DSH 之外的规则 %q:\n%s", removed, got)
		}
	}
}

// TestDSHParitySeatbeltProfileEscapesQuotes 保留"路径含引号/反斜杠不会撑破 SBPL"的
// 纯健壮性断言（DSH 的字符串渲染同样需要这句）。
func TestDSHParitySeatbeltProfileEscapesQuotes(t *testing.T) {
	base := t.TempDir()
	weird := filepath.Join(base, `a"b\c(d)e;f`)
	if err := os.Mkdir(weird, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got := SeatbeltProfile(sandbox.New(sandbox.WithRoots(weird), sandbox.WithMode(sandbox.ModeWorkspaceWrite)))
	if !strings.Contains(got, `a\"b\\c(d)e;f`) {
		t.Fatalf("SBPL 里的路径没有正确转义：%s", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("SBPL 里出现了裸换行：%q", got)
	}
}

// TestDSHParityWindowsGrantShape 断言 Windows 的授权形状：read-only 不授权任何根，
// workspace-write 授权 {ws, 本次私有 temp}。
