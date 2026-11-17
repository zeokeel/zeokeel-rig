package undo

// 内部实现的单元测试：默认值、忽略规则匹配、行级计数、结果渲染与路径判断。
// 这些函数决定"回退什么"和"模型看到什么"，因此单独钉住，避免只靠端到端用例间接覆盖。

import (
	"strings"
	"testing"
)

// TestOptionsDefaults 证明零值 Options 被填上文档化默认值，且用户忽略规则是追加的。
func TestOptionsDefaults(t *testing.T) {
	options := Options{}.withDefaults()
	if options.MaxFileBytes != DefaultMaxFileBytes {
		t.Fatalf("MaxFileBytes = %d, want %d", options.MaxFileBytes, DefaultMaxFileBytes)
	}
	if options.MaxTotalBytes != DefaultMaxTotalBytes {
		t.Fatalf("MaxTotalBytes = %d, want %d", options.MaxTotalBytes, DefaultMaxTotalBytes)
	}
	if options.MaxFiles != DefaultMaxFiles {
		t.Fatalf("MaxFiles = %d, want %d", options.MaxFiles, DefaultMaxFiles)
	}
	if options.Retain != DefaultRetain {
		t.Fatalf("Retain = %d, want %d", options.Retain, DefaultRetain)
	}
	for _, want := range DefaultIgnore {
		if !options.ignored(want+"/x.txt", false) || !options.ignored(want, true) {
			t.Fatalf("default ignore rule %q is not applied", want)
		}
	}
	custom := Options{Ignore: []string{"tmp-cache"}}.withDefaults()
	if !custom.ignored("tmp-cache/x.txt", false) {
		t.Fatal("custom ignore rule was dropped")
	}
	if !custom.ignored(".git/HEAD", false) {
		t.Fatal("custom ignore rules must append to the defaults, not replace them")
	}
	explicit := Options{MaxFileBytes: 7, MaxTotalBytes: 9, MaxFiles: 3, Retain: 2}.withDefaults()
	if explicit.MaxFileBytes != 7 || explicit.MaxTotalBytes != 9 || explicit.MaxFiles != 3 || explicit.Retain != 2 {
		t.Fatalf("explicit values were overwritten: %+v", explicit)
	}
}

// TestIgnoredMatching 覆盖目录名、相对路径前缀与近似前缀三种匹配。
func TestIgnoredMatching(t *testing.T) {
	options := Options{Ignore: []string{"cache/", "docs/generated"}}.withDefaults()
	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"node_modules", true, true},
		{"node_modules/pkg/index.js", false, true},
		{"a/node_modules/index.js", false, true},
		{"cache", true, true},
		{"cache/file.bin", false, true},
		{"docs/generated", true, true},
		{"docs/generated/api.md", false, true},
		{"docs/generated-extra.md", false, false},
		{"src/main.go", false, false},
		{"vendor.go", false, false},
	}
	for _, tc := range cases {
		if got := options.ignored(tc.rel, tc.isDir); got != tc.want {
			t.Fatalf("ignored(%q, %v) = %v, want %v", tc.rel, tc.isDir, got, tc.want)
		}
	}
}

// TestLineCount 钉住行数语义：末尾换行不额外算一行，空内容是 0 行。
func TestLineCount(t *testing.T) {
	cases := map[string]int{
		"":             0,
		"a":            1,
		"a\n":          1,
		"a\nb":         2,
		"a\nb\n":       2,
		"\n":           1,
		"a\r\nb\r\n":   2,
		"one\ntwo\n\n": 3,
	}
	for content, want := range cases {
		if got := lineCount([]byte(content)); got != want {
			t.Fatalf("lineCount(%q) = %d, want %d", content, got, want)
		}
	}
}

// TestDiffLines 证明 +/- 行数来自行级 LCS。
func TestDiffLines(t *testing.T) {
	cases := []struct {
		name           string
		before, after  string
		added, removed int
	}{
		{"identical", "a\nb\n", "a\nb\n", 0, 0},
		{"pure addition", "a\n", "a\nb\n", 1, 0},
		{"pure removal", "a\nb\n", "a\n", 0, 1},
		{"replace", "one\ntwo\nthree\n", "one\nTWO\nthree\n", 1, 1},
		{"middle insert", "a\nc\n", "a\nb\nc\n", 1, 0},
		{"empty before", "", "x\ny\n", 2, 0},
		{"empty after", "x\ny\n", "", 0, 2},
	}
	for _, tc := range cases {
		added, removed := diffLines([]byte(tc.before), []byte(tc.after))
		if added != tc.added || removed != tc.removed {
			t.Fatalf("%s: diffLines = (+%d/-%d), want (+%d/-%d)", tc.name, added, removed, tc.added, tc.removed)
		}
	}
}

// TestRenderResult 逐字钉住工具结果文案。
func TestRenderResult(t *testing.T) {
	changes := []Change{
		{Path: "a.txt", Op: "modify", Added: 1, Removed: 2, Reverted: true},
		{Path: "b.txt", Op: "add", Added: 3, Removed: 0, Reverted: true},
		{Path: "c.txt", Op: "delete", Added: 0, Removed: 4, Reverted: true},
	}
	cases := []struct {
		name    string
		turn    int
		dryRun  bool
		path    string
		changes []Change
		want    string
	}{
		{
			name: "reverted", turn: 2, changes: changes,
			want: "undo: reverted 3 change(s) from turn 2\n" +
				"  M a.txt (+1/-2)\n  A b.txt (+3/-0)\n  D c.txt (+0/-4)\n",
		},
		{
			name: "dry run", turn: 2, dryRun: true, changes: changes,
			want: "undo: would revert 3 change(s) from turn 2 (dry run; nothing was written)\n" +
				"  M a.txt (+1/-2)\n  A b.txt (+3/-0)\n  D c.txt (+0/-4)\n",
		},
		{
			name: "path filter", turn: 1, path: "a.txt", changes: changes[:1],
			want: "undo: reverted 1 change(s) from turn 1 for path a.txt\n  M a.txt (+1/-2)\n",
		},
		{
			name: "no changes", turn: 3,
			want: "undo: no changes from turn 3\n",
		},
		{
			name: "no changes dry run", turn: 3, dryRun: true,
			want: "undo: no changes from turn 3 (dry run; nothing was written)\n",
		},
		{
			name: "skipped",
			turn: 4,
			changes: []Change{
				{Path: "blob.bin", Op: "modify", Reason: "not-backed-up: binary file (contains NUL)"},
			},
			want: "undo: reverted 0 change(s) from turn 4, skipped 1\n" +
				"warning: 1 change(s) were not backed up and were left as-is:\n" +
				"  ! blob.bin: not-backed-up: binary file (contains NUL)\n",
		},
		{
			name: "type change",
			turn: 6,
			changes: []Change{
				{Path: "a.txt", Op: "modify", TypeChange: "file->symlink", Reverted: true},
			},
			want: "undo: reverted 1 change(s) from turn 6\n  M a.txt (type: file->symlink)\n",
		},
		{
			name: "hardlink affected",
			turn: 7,
			changes: []Change{
				{Path: "a.txt", Op: "modify", Added: 1, Removed: 1, Reverted: true},
				{Path: "b.txt", Op: "modify", HardlinkOf: "a.txt"},
			},
			want: "undo: reverted 1 change(s) from turn 7, affected 1\n" +
				"  M a.txt (+1/-1)\n" +
				"warning: 1 path(s) share an inode with a rewritten path and were left as-is:\n" +
				"  ! b.txt: shares an inode with a.txt; the hard link was broken by the revert\n",
		},
		{
			name: "mixed",
			turn: 5,
			changes: []Change{
				{Path: "a.txt", Op: "modify", Added: 1, Removed: 1, Reverted: true},
				{Path: "z.bin", Op: "modify", Reason: "not-backed-up: file too large"},
			},
			want: "undo: reverted 1 change(s) from turn 5, skipped 1\n" +
				"  M a.txt (+1/-1)\n" +
				"warning: 1 change(s) were not backed up and were left as-is:\n" +
				"  ! z.bin: not-backed-up: file too large\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderResult(tc.turn, tc.dryRun, tc.path, tc.changes); got != tc.want {
				t.Fatalf("renderResult mismatch:\ngot  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestPathPredicates 覆盖跨平台绝对路径判断（词法层唯一还需要的路径判断）。
func TestPathPredicates(t *testing.T) {
	for _, absolute := range []string{"/etc/passwd", `C:\x`, "C:/x", `\\server\share`, "//server/share"} {
		if !isAbsoluteAnyOS(absolute) {
			t.Fatalf("isAbsoluteAnyOS(%q) = false, want true", absolute)
		}
	}
	// 单字母 + 冒号按 Windows 盘符处理（"a:b.txt" 在 Windows 上是"驱动相对路径"），
	// 因此它属于危险前缀；这里只列真正相对的路径。
	for _, relative := range []string{"a.txt", "./a.txt", "sub/a.txt", "docs/a:b.txt"} {
		if isAbsoluteAnyOS(relative) {
			t.Fatalf("isAbsoluteAnyOS(%q) = true, want false", relative)
		}
	}
	if !isAbsoluteAnyOS("a:b.txt") {
		t.Fatal(`isAbsoluteAnyOS("a:b.txt") = false, want true (Windows drive-relative path)`)
	}
}

// TestSkipReasonPrefix 证明所有"不备份"原因都带统一前缀（方便模型识别与测试断言）。
func TestSkipReasonPrefix(t *testing.T) {
	reasons := []string{
		skipReasonBinary,
		skipReasonTooLarge(10, 4),
		skipReasonBudget(10, 4, 8),
		skipReasonReadError("boom"),
	}
	for _, reason := range reasons {
		if !strings.HasPrefix(reason, "not-backed-up: ") {
			t.Fatalf("reason = %q, want the not-backed-up prefix", reason)
		}
	}
}
