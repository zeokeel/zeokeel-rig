package filewatch

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// 本文件测试冲突检测助手 Changed：给定一份旧快照，返回"自那以后变了什么"。

// Changed 的语义：from 为 nil 时相当于"全部都是新增"。
func TestChangedFromNilReturnsAllCreated(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "a")
	writeFile(t, filepath.Join(root, "sub", "b.txt"), "bb")

	got, err := Changed(root, nil)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Changed = %+v, want 2 项", got)
	}
	wantPaths := []string{
		filepath.Join(root, "a.txt"),
		filepath.Join(root, "sub", "b.txt"),
	}
	sort.Strings(wantPaths)
	for i, ev := range got {
		if ev.Kind != KindCreated {
			t.Fatalf("第 %d 项 kind = %q, want created", i, ev.Kind)
		}
		if ev.Path != wantPaths[i] {
			t.Fatalf("第 %d 项 path = %q, want %q（必须字典序）", i, ev.Path, wantPaths[i])
		}
	}
}

// 修改与删除都被识别，且结果按字典序。
func TestChangedDetectsModifyAndRemove(t *testing.T) {
	root := t.TempDir()
	mod := filepath.Join(root, "mod.txt")
	del := filepath.Join(root, "zz-del.txt")
	writeFile(t, mod, "v1")
	writeFile(t, del, "x")

	from := snapshotOf(t, root, false)
	if len(from) != 2 {
		t.Fatalf("基线快照 = %v, want 2 项", from)
	}

	writeFile(t, mod, "v2-longer")
	if err := os.Remove(del); err != nil {
		t.Fatalf("删除: %v", err)
	}
	writeFile(t, filepath.Join(root, "new.txt"), "n")

	got, err := Changed(root, from)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	kinds := map[string]EventKind{}
	for _, ev := range got {
		kinds[ev.Path] = ev.Kind
	}
	if kinds[mod] != KindModified {
		t.Fatalf("%s 的 kind = %q, want modified（全部: %+v）", mod, kinds[mod], got)
	}
	if kinds[del] != KindRemoved {
		t.Fatalf("%s 的 kind = %q, want removed", del, kinds[del])
	}
	if kinds[filepath.Join(root, "new.txt")] != KindCreated {
		t.Fatalf("新增文件未被识别: %+v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Path > got[i].Path {
			t.Fatalf("结果未按字典序: %q > %q", got[i-1].Path, got[i].Path)
		}
	}
}

// 什么都没变：返回空。
func TestChangedNoChanges(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "a")

	w, _ := startWatcher(t, root, nil)
	from := w.Snapshot()

	got, err := Changed(root, from)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Changed = %+v, want 空", got)
	}
}

// Changed 与 Watcher 使用同一套默认 Ignore。
func TestChangedRespectsIgnore(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "index"), "x")
	writeFile(t, filepath.Join(root, "node_modules", "p", "i.js"), "x")
	writeFile(t, filepath.Join(root, "main.go"), "x")

	got, err := Changed(root, nil)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if len(got) != 1 || filepath.Base(got[0].Path) != "main.go" {
		t.Fatalf("Changed = %+v, want 只有 main.go", got)
	}
}

// 快照带摘要时，size 与 mtime 都没变的内容改动也能被发现。
func TestChangedWithDigestSnapshot(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "same.txt")
	writeFile(t, target, "aaaa")

	w, _ := startWatcher(t, root, func(o *Options) { o.Digest = true })
	from := w.Snapshot()
	if from[target].Digest == "" {
		t.Fatalf("快照缺少摘要: %+v", from)
	}

	rewriteKeepingMtime(t, target, "bbbb")

	got, err := Changed(root, from)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if len(got) != 1 || got[0].Kind != KindModified {
		t.Fatalf("Changed = %+v, want 一条 modified", got)
	}
	if got[0].Digest == "" || got[0].Digest == from[target].Digest {
		t.Fatalf("摘要未变化: 旧=%q 新=%q", from[target].Digest, got[0].Digest)
	}
}

// 快照不带摘要时按 size+mtime 比较：同样的改动不可见（与 Watcher 的取舍一致）。
func TestChangedWithoutDigestMissesSameSizeChange(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "same.txt")
	writeFile(t, target, "aaaa")

	w, _ := startWatcher(t, root, nil)
	from := w.Snapshot()
	if from[target].Digest != "" {
		t.Fatalf("未开启 Digest 却有摘要: %+v", from[target])
	}

	rewriteKeepingMtime(t, target, "bbbb")

	got, err := Changed(root, from)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Changed = %+v, want 空（无摘要时无法发现）", got)
	}
}

// root 不存在时返回错误。
func TestChangedMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	if _, err := Changed(root, nil); err == nil {
		t.Fatal("不存在的 root 应报错")
	}
}

// Changed 的事件时间字段是文件的 mtime（供上游展示"何时被改"）。
func TestChangedCarriesModTime(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a.txt")
	writeFile(t, target, "a")

	got, err := Changed(root, nil)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Changed = %+v", got)
	}
	if got[0].ModTime.IsZero() {
		t.Fatal("ModTime 未填")
	}
	if time.Since(got[0].ModTime) > time.Hour {
		t.Fatalf("ModTime 看起来不对: %v", got[0].ModTime)
	}
}

// snapshotOf 启动一个 watcher 取基线快照（走公开路径，不依赖内部实现）。
func snapshotOf(t *testing.T, root string, digest bool) map[string]FileInfo {
	t.Helper()
	w, _ := startWatcher(t, root, func(o *Options) { o.Digest = digest })
	return w.Snapshot()
}
