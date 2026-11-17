package undo

// 包内测试：结果文案与参数解析。这里只钉"模型/用户看到什么"，不碰 git。

import (
	"strings"
	"testing"
)

// TestRenderResultShapes 钉住结果文案的稳定形状。
func TestRenderResultShapes(t *testing.T) {
	cases := []struct {
		name    string
		turn    int
		dryRun  bool
		path    string
		changes []Change
		want    string
	}{
		{
			name: "no changes",
			turn: 1,
			want: "undo: no changes from turn 1\n",
		},
		{
			name: "modify and add",
			turn: 2,
			changes: []Change{
				{Path: "src/a.go", Op: "modify", Added: 1, Removed: 1, Reverted: true},
				{Path: "docs/new.md", Op: "add", Added: 4, Reverted: true},
			},
			want: "undo: reverted 2 change(s) from turn 2\n  M src/a.go (+1/-1)\n  A docs/new.md (+4/-0)\n",
		},
		{
			name:    "delete",
			turn:    3,
			changes: []Change{{Path: "gone.txt", Op: "delete", Removed: 2, Reverted: true}},
			want:    "undo: reverted 1 change(s) from turn 3\n  D gone.txt (+0/-2)\n",
		},
		{
			name:    "rename",
			turn:    4,
			changes: []Change{{Path: "new.txt", OldPath: "old.txt", Op: "rename", Reverted: true}},
			want:    "undo: reverted 1 change(s) from turn 4\n  R old.txt -> new.txt\n",
		},
		{
			name:    "type change",
			turn:    5,
			changes: []Change{{Path: "link", Op: "modify", TypeChange: "file->symlink", Reverted: true}},
			want:    "undo: reverted 1 change(s) from turn 5\n  M link (type: file->symlink)\n",
		},
		{
			name: "skipped",
			turn: 6,
			changes: []Change{
				{Path: "ok.txt", Op: "modify", Added: 1, Reverted: true},
				{Path: "sub", Op: "modify", Reason: "submodule: pointer changes cannot be reverted here"},
			},
			want: "undo: reverted 1 change(s) from turn 6, skipped 1\n" +
				"  M ok.txt (+1/-0)\n" +
				"warning: 1 change(s) could not be reverted and were left as-is:\n" +
				"  ! sub: submodule: pointer changes cannot be reverted here\n",
		},
		{
			name:    "dry run",
			turn:    7,
			dryRun:  true,
			changes: []Change{{Path: "a.txt", Op: "modify", Added: 2, Removed: 1}},
			want:    "undo: would revert 1 change(s) from turn 7 (dry run; nothing was written)\n  M a.txt (+2/-1)\n",
		},
		{
			name:    "path filter",
			turn:    8,
			path:    "src/a.go",
			changes: []Change{{Path: "src/a.go", Op: "modify", Added: 1, Reverted: true}},
			want:    "undo: reverted 1 change(s) from turn 8 for path src/a.go\n  M src/a.go (+1/-0)\n",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := renderResult(testCase.turn, testCase.dryRun, testCase.path, testCase.changes)
			if got != testCase.want {
				t.Fatalf("renderResult:\n got=%q\nwant=%q", got, testCase.want)
			}
		})
	}
}

// TestRenderCannotBeInjectedByFilename 证明文件名里的控制字符不能伪造结果行。
func TestRenderCannotBeInjectedByFilename(t *testing.T) {
	changes := []Change{{Path: "evil\n  A main.go (+99)", Op: "add", Added: 1, Reverted: true}}
	rendered := renderResult(1, false, "", changes)
	if strings.Count(rendered, "\n") != 2 {
		t.Fatalf("文件名注入改变了行数: %q", rendered)
	}
	if strings.Contains(rendered, "evil\n") {
		t.Fatalf("控制字符没有被抹平: %q", rendered)
	}
}

// TestNormalizeIgnoreAndMatching 钉住忽略规则的语义。
func TestNormalizeIgnoreAndMatching(t *testing.T) {
	normalized := normalizeIgnore([]string{" cache/ ", "/abs", "docs/generated/", "", "."})
	if strings.Join(normalized, ",") != "cache,/abs,docs/generated" {
		t.Fatalf("normalizeIgnore = %v", normalized)
	}
	options := Options{}.withDefaults()
	options.Ignore = normalizeIgnore([]string{"cache", "docs/generated"})
	for _, hit := range []string{"cache", "cache/x.txt", "a/cache/x.txt", "docs/generated", "docs/generated/x.go"} {
		if !options.ignored(hit) {
			t.Fatalf("ignored(%q) = false, want true", hit)
		}
	}
	for _, miss := range []string{"caches", "src/cache.go", "docs/other/x.go"} {
		if options.ignored(miss) {
			t.Fatalf("ignored(%q) = true, want false", miss)
		}
	}
}

// TestOptionsDefaults 钉住 Options 的默认值与非默认值。
func TestOptionsDefaults(t *testing.T) {
	options := Options{}.withDefaults()
	if options.Retain != DefaultRetain {
		t.Fatalf("Retain = %d, want %d", options.Retain, DefaultRetain)
	}
	explicit := Options{Retain: 3}.withDefaults()
	if explicit.Retain != 3 {
		t.Fatalf("显式 Retain 被覆盖: %d", explicit.Retain)
	}
	// 历史字段仍然存在（源码兼容），但已经不参与任何决策。
	_ = Options{MaxFileBytes: 1, MaxTotalBytes: 2, MaxFiles: 3}
	_ = DefaultMaxFileBytes + DefaultMaxTotalBytes + int64(DefaultMaxFiles)
	_ = DefaultIgnore
	_ = ErrRootReplaced
}

// TestResolvePathRejectsEscapes 钉住路径校验。
func TestResolvePathRejectsEscapes(t *testing.T) {
	snapshot := &Snapshot{root: "/ws"}
	for _, bad := range []string{"", "  ", "/etc/passwd", `C:\x`, "\\\\server\\share", "..", "../x", "."} {
		if _, err := snapshot.resolvePath(bad); err == nil {
			t.Fatalf("resolvePath(%q) accepted", bad)
		}
	}
	for _, good := range []string{"a.txt", "sub/a.txt", "./sub/a.txt"} {
		if got, err := snapshot.resolvePath(good); err != nil || got == "" {
			t.Fatalf("resolvePath(%q) = %q, %v", good, got, err)
		}
	}
}
