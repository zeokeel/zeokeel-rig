//go:build !windows

package osrunner

// Seatbelt 的 SBPL **渲染**是纯字符串逻辑：darwin 上真跑 sandbox-exec，linux 上
// 也能断言渲染结果。期望值一律从 `platformTempRoots()` 与 `canonicalForTest()`
// 推导，**不硬编码** `/tmp`——macOS 上它是 `/private/tmp`，硬编码必假红。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

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
// 期望值**不能硬编码** `/tmp`：macOS 上 `platformTempRoots()` 给 `/private/tmp`
// 与 `/private/var/folders/...`（同 `/tmp` 与 `os.tmpdir()`），硬编码必然假红。
// 这里用被测代码自己的来源 + `canonicalForTest` 生成期望串：工作区在前，平台临时
// 目录在后（与 DSH 的 `writableRoots()` 顺序一致），去重保序。
func TestDSHParitySeatbeltProfileWorkspaceWrite(t *testing.T) {
	workspace, _ := dshParityRoots(t)
	policy := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	got := SeatbeltProfile(policy)

	subpaths := []string{canonicalForTest(workspace)}
	for _, temp := range platformTempRoots() {
		duplicate := false
		for _, existing := range subpaths {
			if samePathForTest(existing, temp) || sandbox.PathEqual(existing, temp) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			subpaths = append(subpaths, temp)
		}
	}
	parts := make([]string, 0, len(subpaths))
	for _, path := range subpaths {
		parts = append(parts, `(subpath "`+path+`")`)
	}
	want := `(version 1) (allow default) (deny file-write*) (allow file-write* (literal "/dev/null")) ` +
		`(allow file-write* ` + strings.Join(parts, " ") + `)`
	if got != want {
		t.Fatalf("SeatbeltProfile(workspace-write)\n got = %q\nwant = %q", got, want)
	}
	// 集合校验：每个 subpath 各出现一次，数量等于期望集合大小。
	if n := strings.Count(got, "(subpath "); n != len(subpaths) {
		t.Fatalf("subpath 数量 = %d, want %d: %q", n, len(subpaths), got)
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
