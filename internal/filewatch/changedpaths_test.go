package filewatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件测试两个"单点查询"API：DigestFile 与 ChangedPaths。
//
// ChangedPaths 是冲突检测的热路径：它只碰调用方点名的路径（O(len(paths))），
// 不遍历全树，因此这些用例也顺带钉住"不扫全树"的可观测后果（未点名的文件
// 变化不出现）。

// DigestFile 的口径必须与 Snapshot 完全一致（同一算法、同一格式）。
func TestDigestFileMatchesSnapshotDigest(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a.txt")
	writeFile(t, target, "hello world")

	got, err := DigestFile(target)
	if err != nil {
		t.Fatalf("DigestFile: %v", err)
	}
	if len(got) != 64 {
		t.Fatalf("摘要长度 = %d, want 64（sha256 小写 hex）: %q", len(got), got)
	}
	for _, r := range got {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			t.Fatalf("摘要含非小写 hex 字符 %q: %q", r, got)
		}
	}

	w, _ := startWatcher(t, root, func(o *Options) { o.Digest = true })
	if snap := w.Snapshot()[target].Digest; snap != got {
		t.Fatalf("Snapshot 摘要 = %q, DigestFile = %q（口径必须一致）", snap, got)
	}

	// 内容变、大小不变 → 摘要必须变（这是 Digest 存在的意义）。
	rewriteKeepingMtime(t, target, strings.Repeat("x", len("hello world")))
	changed, err := DigestFile(target)
	if err != nil {
		t.Fatalf("DigestFile: %v", err)
	}
	if changed == got {
		t.Fatal("内容变了摘要没变")
	}
}

// 非普通文件（目录、符号链接）不能摘要：与 Snapshot 的"符号链接摘要为空"一致。
func TestDigestFileRejectsNonRegular(t *testing.T) {
	root := t.TempDir()
	if _, err := DigestFile(root); err == nil {
		t.Fatal("目录应报错")
	}
	target := filepath.Join(root, "real.txt")
	writeFile(t, target, "x")
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, link); err == nil {
		if _, err := DigestFile(link); err == nil {
			t.Fatal("符号链接应报错（不跟随，与 Snapshot 口径一致）")
		}
	}
	if _, err := DigestFile(filepath.Join(root, "missing.txt")); err == nil {
		t.Fatal("不存在的文件应报错")
	}
}

// 只比较点名的路径：created / modified / removed 三种都报，且按字典序。
func TestChangedPathsCreatedModifiedRemoved(t *testing.T) {
	root := t.TempDir()
	mod := filepath.Join(root, "b-mod.txt")
	del := filepath.Join(root, "c-del.txt")
	untouched := filepath.Join(root, "d-untouched.txt")
	writeFile(t, mod, "v1")
	writeFile(t, del, "x")
	writeFile(t, untouched, "keep")

	from := snapshotOf(t, root, false)
	if len(from) != 3 {
		t.Fatalf("基线快照 = %v", from)
	}

	writeFile(t, mod, "v2-longer")
	if err := os.Remove(del); err != nil {
		t.Fatalf("删除: %v", err)
	}
	writeFile(t, untouched, "changed but not asked about")
	created := filepath.Join(root, "a-created.txt")
	writeFile(t, created, "new")

	got, err := ChangedPaths(root, []string{mod, del, created}, from)
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ChangedPaths = %+v, want 3 项", got)
	}
	want := []struct {
		path string
		kind EventKind
	}{
		{created, KindCreated},
		{mod, KindModified},
		{del, KindRemoved},
	}
	for i, w := range want {
		if got[i].Path != w.path || got[i].Kind != w.kind {
			t.Fatalf("第 %d 项 = %+v, want %s %s", i, got[i], w.kind, w.path)
		}
	}
	// size/mtime 反映当前状态（removed 用最后已知值）。
	if got[1].Size != int64(len("v2-longer")) {
		t.Fatalf("modified size = %d", got[1].Size)
	}
	if got[2].Size != 1 {
		t.Fatalf("removed size = %d, want 最后已知 1", got[2].Size)
	}
}

// from 为空（nil）等价于"点名的路径全部视为 created"。
func TestChangedPathsEmptyFromAllCreated(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "sub", "b.txt")
	writeFile(t, a, "a")
	writeFile(t, b, "bb")

	for _, from := range []map[string]FileInfo{nil, {}} {
		got, err := ChangedPaths(root, []string{b, a}, from)
		if err != nil {
			t.Fatalf("ChangedPaths: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ChangedPaths = %+v, want 2 项", got)
		}
		for _, ev := range got {
			if ev.Kind != KindCreated {
				t.Fatalf("kind = %q, want created", ev.Kind)
			}
		}
		if got[0].Path != a || got[1].Path != b {
			t.Fatalf("未按字典序: %q, %q", got[0].Path, got[1].Path)
		}
	}
}

// 路径逃出 root 必须报错（相对穿越与绝对路径两种写法）。
func TestChangedPathsRejectsEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, outside, "x")

	cases := []string{
		"../outside.txt",
		"sub/../../outside.txt",
		outside,
		filepath.Dir(root),
	}
	for _, bad := range cases {
		if _, err := ChangedPaths(root, []string{bad}, nil); err == nil {
			t.Fatalf("路径 %q 逃出 root 却未报错", bad)
		}
	}
	// root 自身也算合法查询（虽然目录不产事件），但绝不能 panic。
	if _, err := ChangedPaths(root, []string{"."}, nil); err != nil {
		t.Fatalf("查询 root 自身报错: %v", err)
	}
}

// 盘上没有、from 里也没有的路径：不报错、不产事件。
func TestChangedPathsMissingPathNoEvent(t *testing.T) {
	root := t.TempDir()
	got, err := ChangedPaths(root, []string{"nope.txt"}, nil)
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ChangedPaths = %+v, want 空", got)
	}
}

// 符号链接：不跟随（改链接目标不算变化），链接本身作为条目产出且摘要为空。
func TestChangedPathsSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, outside, "outside")
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("创建符号链接失败（Windows 需要权限）: %v", err)
	}

	w, _ := startWatcher(t, root, func(o *Options) { o.Digest = true })
	from := w.Snapshot()
	if _, ok := from[link]; !ok {
		t.Fatalf("符号链接不在快照里: %v", from)
	}
	if from[link].Digest != "" {
		t.Fatalf("快照里符号链接摘要应为空: %+v", from[link])
	}

	// 改目标（root 之外）：链接自身的 lstat 没变 → 无事件。
	writeFile(t, outside, "outside-changed")
	got, err := ChangedPaths(root, []string{link}, from)
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("改符号链接目标却报变化: %+v", got)
	}

	// 新建一个符号链接：created，且摘要为空。
	link2 := filepath.Join(root, "link2.txt")
	if err := os.Symlink(outside, link2); err != nil {
		t.Fatalf("建符号链接: %v", err)
	}
	got, err = ChangedPaths(root, []string{link2}, from)
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}
	if len(got) != 1 || got[0].Kind != KindCreated || got[0].Digest != "" {
		t.Fatalf("ChangedPaths = %+v, want created 且无摘要", got)
	}
}

// 摘要口径与 Watcher 一致：from 带摘要时能发现"同 size 同 mtime"的改写；
// 不带摘要时按 size+mtime 比较，发现不了（与 Changed 同取舍）。
func TestChangedPathsDigestParity(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "same.txt")
	writeFile(t, target, "aaaa")

	withDigest := snapshotOf(t, root, true)
	withoutDigest := snapshotOf(t, root, false)
	if withDigest[target].Digest == "" || withoutDigest[target].Digest != "" {
		t.Fatalf("基线摘要不符合预期: with=%+v without=%+v", withDigest[target], withoutDigest[target])
	}

	rewriteKeepingMtime(t, target, "bbbb")

	got, err := ChangedPaths(root, []string{target}, withDigest)
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}
	if len(got) != 1 || got[0].Kind != KindModified {
		t.Fatalf("带摘要 = %+v, want modified", got)
	}
	if got[0].Digest == fromDigest(withDigest, target) {
		t.Fatalf("事件摘要没更新: %q", got[0].Digest)
	}

	got, err = ChangedPaths(root, []string{target}, withoutDigest)
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("不带摘要 = %+v, want 空（无法发现）", got)
	}
}

// 相对路径按 root 解析；同一路径的多种拼法只报一次。
func TestChangedPathsRelativeAndDedup(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "sub", "a.txt")
	writeFile(t, target, "x")

	got, err := ChangedPaths(root, []string{"sub/a.txt", "./sub/a.txt", target, "sub//a.txt"}, nil)
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ChangedPaths = %+v, want 去重后 1 项", got)
	}
	if got[0].Path != filepath.Clean(target) || got[0].Kind != KindCreated {
		t.Fatalf("事件 = %+v", got[0])
	}
}

// fromDigest 是取快照摘要的小助手。
func fromDigest(snapshot map[string]FileInfo, path string) string {
	return snapshot[path].Digest
}
