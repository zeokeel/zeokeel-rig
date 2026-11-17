package filewatch

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// 本文件是 Watcher 的行为测试：真实临时目录 + 极短 interval/debounce，
// 断言"轮询 → 去抖 → 事件"这条链的可观察语义。

const (
	testInterval = 10 * time.Millisecond
	testDebounce = 40 * time.Millisecond
	// testWait 是"应当出现事件"的等待上限（CI 抖动也够用）。
	testWait = 3 * time.Second
	// quiet 是"不应出现事件"的观察窗口：至少覆盖若干个轮询周期 + 去抖窗口。
	quiet = 150 * time.Millisecond
)

// fastOptions 返回测试共用的极短时序；mutate 可覆盖单项。
func fastOptions(mutate func(*Options)) Options {
	opts := Options{Interval: testInterval, Debounce: testDebounce}
	if mutate != nil {
		mutate(&opts)
	}
	return opts
}

// startWatcher 建 watcher 并启动；测试结束自动取消并关闭。
func startWatcher(t *testing.T, root string, mutate func(*Options)) (*Watcher, <-chan Event) {
	t.Helper()
	w, err := New(root, fastOptions(mutate))
	if err != nil {
		t.Fatalf("New(%q): %v", root, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = w.Close()
	})
	w.Start(ctx)
	return w, w.Events()
}

// writeFile 写入文件（自动建父目录）。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件 %s: %v", path, err)
	}
}

// waitEvent 等待一条事件。
func waitEvent(t *testing.T, ch <-chan Event, timeout time.Duration) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("事件通道已关闭，但仍期待事件")
		}
		return ev
	case <-time.After(timeout):
		t.Fatalf("等待事件超时（%v）", timeout)
	}
	return Event{}
}

// expectNoEvent 断言在 d 窗口内没有任何事件。
func expectNoEvent(t *testing.T, ch <-chan Event, d time.Duration) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("事件通道已关闭")
		}
		t.Fatalf("不应有事件，却收到 %+v", ev)
	case <-time.After(d):
	}
}

// 基线：Start 时的既有文件不产出任何事件。
func TestBaselineEmitsNothing(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "a")
	writeFile(t, filepath.Join(root, "sub", "b.txt"), "b")

	w, ch := startWatcher(t, root, nil)
	expectNoEvent(t, ch, quiet)

	snap := w.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("基线快照有 %d 项, want 2: %v", len(snap), snap)
	}
	if _, ok := snap[filepath.Join(root, "sub", "b.txt")]; !ok {
		t.Fatalf("快照缺少嵌套文件: %v", snap)
	}
}

// 创建 / 修改 / 删除三种事件。
func TestCreateModifyRemove(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "keep.txt"), "1")

	_, ch := startWatcher(t, root, nil)

	created := filepath.Join(root, "new.txt")
	writeFile(t, created, "hello")
	ev := waitEvent(t, ch, testWait)
	if ev.Kind != KindCreated || ev.Path != created {
		t.Fatalf("创建事件 = %+v, want created %s", ev, created)
	}
	if ev.Size != 5 {
		t.Fatalf("创建事件 Size = %d, want 5", ev.Size)
	}
	if !filepath.IsAbs(ev.Path) {
		t.Fatalf("Path 必须是绝对路径: %q", ev.Path)
	}

	writeFile(t, created, "hello world")
	ev = waitEvent(t, ch, testWait)
	if ev.Kind != KindModified || ev.Path != created || ev.Size != int64(len("hello world")) {
		t.Fatalf("修改事件 = %+v", ev)
	}

	if err := os.Remove(created); err != nil {
		t.Fatalf("删除: %v", err)
	}
	ev = waitEvent(t, ch, testWait)
	if ev.Kind != KindRemoved || ev.Path != created {
		t.Fatalf("删除事件 = %+v", ev)
	}
}

// 去抖：窗口内同一路径的连续变化只产出一条，且以最后一次为准。
func TestDebounceMergesRapidChanges(t *testing.T) {
	root := t.TempDir()
	_, ch := startWatcher(t, root, func(o *Options) {
		o.Interval = 10 * time.Millisecond
		o.Debounce = 120 * time.Millisecond
	})

	target := filepath.Join(root, "burst.txt")
	for i := 0; i < 5; i++ {
		writeFile(t, target, string(make([]byte, i+1)))
		time.Sleep(5 * time.Millisecond)
	}
	final := "final-content"
	writeFile(t, target, final)

	ev := waitEvent(t, ch, testWait)
	if ev.Kind != KindCreated || ev.Path != target {
		t.Fatalf("事件 = %+v, want created %s", ev, target)
	}
	if ev.Size != int64(len(final)) {
		t.Fatalf("Size = %d, want %d（去抖后应取最后一次）", ev.Size, len(final))
	}
	// 去抖窗口内不得再有第二条同类事件。
	expectNoEvent(t, ch, 300*time.Millisecond)
}

// 两个去抖窗口之间的变化各自产出事件。
func TestSeparateDebounceWindowsEmitSeparately(t *testing.T) {
	root := t.TempDir()
	_, ch := startWatcher(t, root, nil)

	target := filepath.Join(root, "twice.txt")
	writeFile(t, target, "v1")
	if ev := waitEvent(t, ch, testWait); ev.Kind != KindCreated {
		t.Fatalf("第一次 = %+v, want created", ev)
	}

	time.Sleep(2 * (testInterval + testDebounce))

	writeFile(t, target, "v2-longer")
	if ev := waitEvent(t, ch, testWait); ev.Kind != KindModified {
		t.Fatalf("第二次 = %+v, want modified", ev)
	}
}

// 默认 Ignore：.git / node_modules 下的变化不产出事件、也不进快照。
func TestIgnoreDefaults(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "config"), "x")
	writeFile(t, filepath.Join(root, "node_modules", "pkg", "index.js"), "x")
	writeFile(t, filepath.Join(root, "src", "main.go"), "x")

	w, ch := startWatcher(t, root, nil)
	snap := w.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("快照 = %v, want 只有 src/main.go", snap)
	}

	writeFile(t, filepath.Join(root, ".git", "HEAD"), "y")
	writeFile(t, filepath.Join(root, "node_modules", "pkg", "other.js"), "y")
	expectNoEvent(t, ch, quiet)

	writeFile(t, filepath.Join(root, "src", "util.go"), "y")
	if ev := waitEvent(t, ch, testWait); filepath.Base(ev.Path) != "util.go" {
		t.Fatalf("事件 = %+v, want src/util.go", ev)
	}
}

// 自定义 Ignore：目录名与相对路径前缀两种写法。
func TestIgnoreCustomNamesAndPrefixes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "gen", "a.go"), "x")
	writeFile(t, filepath.Join(root, "internal", "gen", "b.go"), "x")
	writeFile(t, filepath.Join(root, "keep", "c.go"), "x")

	w, _ := startWatcher(t, root, func(o *Options) {
		o.Ignore = []string{"gen", "keep/gen2"}
	})
	snap := w.Snapshot()
	for path := range snap {
		if filepath.Base(filepath.Dir(path)) == "gen" {
			t.Fatalf("Ignore 未生效: %v", path)
		}
	}
	if len(snap) != 1 {
		t.Fatalf("快照 = %v, want 只有 keep/c.go", snap)
	}
}

// MaxFiles 上限：只跟踪前 N 个，Notice 只报一次，且不报错、不中断。
func TestMaxFilesCapAndNoticeOnce(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 10; i++ {
		writeFile(t, filepath.Join(root, "f"+string(rune('a'+i))+".txt"), "x")
	}

	var mu sync.Mutex
	var notices []string
	notice := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		notices = append(notices, format)
	}

	w, ch := startWatcher(t, root, func(o *Options) {
		o.MaxFiles = 3
		o.Notice = notice
	})

	if got := len(w.Snapshot()); got != 3 {
		t.Fatalf("快照有 %d 项, want 上限 3", got)
	}
	expectNoEvent(t, ch, quiet)

	mu.Lock()
	n := len(notices)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("Notice 被调用 %d 次, want 恰好 1 次: %v", n, notices)
	}

	// 未跟踪的文件即使变化也不产出事件。
	writeFile(t, filepath.Join(root, "z-untracked.txt"), "x")
	expectNoEvent(t, ch, quiet)

	// 未超上限时不该有 Notice。
	var mu2 sync.Mutex
	notices2 := 0
	w2, _ := startWatcher(t, root, func(o *Options) {
		o.MaxFiles = 100
		o.Notice = func(string, ...any) { mu2.Lock(); notices2++; mu2.Unlock() }
	})
	_ = w2
	time.Sleep(quiet)
	mu2.Lock()
	n2 := notices2
	mu2.Unlock()
	if n2 != 0 {
		t.Fatalf("未超上限却有 %d 次 Notice", n2)
	}
}

// Digest 开启：size 与 mtime 都不变时也能发现内容变化。
func TestDigestDetectsSameSizeSameMtimeChange(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "same.txt")
	writeFile(t, target, "aaaa")

	_, ch := startWatcher(t, root, func(o *Options) { o.Digest = true })

	rewriteKeepingMtime(t, target, "bbbb")
	ev := waitEvent(t, ch, testWait)
	if ev.Kind != KindModified || ev.Path != target {
		t.Fatalf("事件 = %+v, want modified %s", ev, target)
	}
	if ev.Digest == "" {
		t.Fatal("Digest 开启时事件必须带摘要")
	}
	if ev.Size != 4 {
		t.Fatalf("Size = %d, want 4", ev.Size)
	}
}

// Digest 关闭：同样的"size 与 mtime 都不变"的变化无法被发现（这是刻意取舍）。
func TestNoDigestMissesSameSizeSameMtimeChange(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "same.txt")
	writeFile(t, target, "aaaa")

	_, ch := startWatcher(t, root, nil) // Digest 默认 false

	rewriteKeepingMtime(t, target, "bbbb")
	expectNoEvent(t, ch, quiet)
}

// mtime 不变但 size 变化 → 必须报 modified（不能只靠 mtime）。
func TestSizeChangeWithSameMtimeIsModified(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "grow.txt")
	writeFile(t, target, "a")

	_, ch := startWatcher(t, root, nil)

	rewriteKeepingMtime(t, target, "aaaa")
	ev := waitEvent(t, ch, testWait)
	if ev.Kind != KindModified || ev.Size != 4 {
		t.Fatalf("事件 = %+v, want modified size=4", ev)
	}
}

// 符号链接：不跟随；链接本身作为条目出现，且不带摘要。
func TestSymlinkNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 创建符号链接需要额外权限")
	}
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, outside, "outside")

	w, ch := startWatcher(t, root, func(o *Options) { o.Digest = true })

	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("建符号链接: %v", err)
	}
	ev := waitEvent(t, ch, testWait)
	if ev.Path != link || ev.Kind != KindCreated {
		t.Fatalf("事件 = %+v, want created %s", ev, link)
	}
	if ev.Digest != "" {
		t.Fatalf("符号链接不应带摘要: %q", ev.Digest)
	}
	if ev.Size == int64(len("outside")) {
		t.Fatalf("Size = %d 看起来是跟随后的目标大小", ev.Size)
	}

	// 修改链接目标（在 root 之外）不应产出任何事件。
	writeFile(t, outside, "outside-changed")
	expectNoEvent(t, ch, quiet)

	if fi, ok := w.Snapshot()[link]; !ok || fi.Digest != "" {
		t.Fatalf("快照里的符号链接 = %+v ok=%v", fi, ok)
	}
}

// 指向 root 内目录的符号链接同样不被遍历。
func TestSymlinkToDirNotDescended(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 创建符号链接需要额外权限")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "real", "a.txt"), "x")

	w, ch := startWatcher(t, root, nil)
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatalf("建符号链接: %v", err)
	}
	waitEvent(t, ch, testWait) // alias 本身

	writeFile(t, filepath.Join(root, "real", "b.txt"), "y")
	ev := waitEvent(t, ch, testWait)
	if filepath.Dir(ev.Path) != filepath.Join(root, "real") {
		t.Fatalf("事件路径 = %q, want 只出现在 real/ 下（alias 不应被遍历）", ev.Path)
	}
	for path := range w.Snapshot() {
		if filepath.Dir(path) == filepath.Join(root, "alias") {
			t.Fatalf("快照出现经由符号链接的路径: %q", path)
		}
	}
}

// Close 之后 Events 被关闭。
func TestCloseClosesChannel(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, fastOptions(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.Start(context.Background())
	ch := w.Events()

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("Close 后仍收到事件（通道未关闭）")
		}
	case <-time.After(testWait):
		t.Fatal("Close 后通道没有关闭")
	}
}

// Close 幂等；未 Start 也能安全 Close。
func TestCloseIdempotentAndBeforeStart(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, fastOptions(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := w.Close(); err != nil {
			t.Fatalf("第 %d 次 Close: %v", i+1, err)
		}
	}
	// 未 Start 就 Close：通道也必须关闭。
	ch := w.Events()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("收到事件")
		}
	case <-time.After(testWait):
		t.Fatal("未 Start 的 Close 没有关闭通道")
	}
	// Close 之后再 Start 不应 panic，也不应复活。
	w.Start(context.Background())
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("Close 之后 Start 复活了 watcher")
		}
	case <-time.After(quiet):
	}
}

// Start 幂等：重复调用不产生第二条事件流/重复事件。
func TestStartIdempotent(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, fastOptions(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(func() { _ = w.Close() })

	w.Start(ctx)
	w.Start(ctx)
	w.Start(context.Background())

	ch := w.Events()
	writeFile(t, filepath.Join(root, "once.txt"), "x")
	if ev := waitEvent(t, ch, testWait); ev.Kind != KindCreated {
		t.Fatalf("事件 = %+v", ev)
	}
	expectNoEvent(t, ch, quiet) // 只应有一条
}

// ctx 取消即停止：通道最终关闭，Snapshot 仍可读。
func TestContextCancelStops(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, fastOptions(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	ch := w.Events()

	writeFile(t, filepath.Join(root, "before-cancel.txt"), "x")
	waitEvent(t, ch, testWait)

	cancel()
	deadline := time.After(testWait)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				if len(w.Snapshot()) == 0 {
					t.Fatal("Snapshot 在取消后不可用")
				}
				return
			}
		case <-deadline:
			t.Fatal("取消后通道没有关闭")
		}
	}
}

// 空目录：不产出事件，快照为空。
func TestEmptyDir(t *testing.T) {
	root := t.TempDir()
	w, ch := startWatcher(t, root, nil)
	expectNoEvent(t, ch, quiet)
	if snap := w.Snapshot(); len(snap) != 0 {
		t.Fatalf("空目录快照 = %v", snap)
	}
}

// Snapshot 是拷贝：外部修改不影响内部状态。
func TestSnapshotIsCopy(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a.txt")
	writeFile(t, target, "a")

	w, _ := startWatcher(t, root, nil)
	snap := w.Snapshot()
	snap[target] = FileInfo{Size: 999}
	delete(snap, target)

	snap2 := w.Snapshot()
	fi, ok := snap2[target]
	if !ok || fi.Size != 1 {
		t.Fatalf("内部状态被外部修改: %+v ok=%v", fi, ok)
	}
}

// 选项默认值。
func TestOptionDefaults(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = w.Close() }()

	if w.interval != DefaultInterval {
		t.Fatalf("interval = %v, want %v", w.interval, DefaultInterval)
	}
	if w.debounce != DefaultDebounce {
		t.Fatalf("debounce = %v, want %v", w.debounce, DefaultDebounce)
	}
	if w.maxFiles != DefaultMaxFiles {
		t.Fatalf("maxFiles = %d, want %d", w.maxFiles, DefaultMaxFiles)
	}
	if w.digest {
		t.Fatal("Digest 默认必须为 false")
	}
	want := []string{".git", "node_modules", "vendor", "dist", "build"}
	if len(w.ignore) != len(want) {
		t.Fatalf("ignore = %v, want %v", w.ignore, want)
	}
}

// New 的入参校验。
func TestNewValidation(t *testing.T) {
	if _, err := New("", Options{}); err == nil {
		t.Fatal("空 root 应报错")
	}
	if _, err := New(filepath.Join(t.TempDir(), "missing"), Options{}); err == nil {
		t.Fatal("不存在的 root 应报错")
	}
	file := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, file, "x")
	if _, err := New(file, Options{}); err == nil {
		t.Fatal("root 是文件时应报错")
	}
}

// 事件路径是系统原生绝对路径（不做 ToSlash 归一）。
func TestEventPathIsNativeAbsolute(t *testing.T) {
	root := t.TempDir()
	_, ch := startWatcher(t, root, nil)
	target := filepath.Join(root, "dir", "native.txt")
	writeFile(t, target, "x")

	ev := waitEvent(t, ch, testWait)
	if !filepath.IsAbs(ev.Path) {
		t.Fatalf("Path = %q 不是绝对路径", ev.Path)
	}
	if ev.Path != filepath.Clean(target) {
		t.Fatalf("Path = %q, want %q（原生分隔符 + Clean）", ev.Path, filepath.Clean(target))
	}
}

// 并发 Snapshot 与事件消费（配合 -race）。
func TestConcurrentSnapshotAndEvents(t *testing.T) {
	root := t.TempDir()
	w, ch := startWatcher(t, root, func(o *Options) { o.Digest = true })

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = w.Snapshot()
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			writeFile(t, filepath.Join(root, "f"+string(rune('a'+i))+".txt"), "content")
		}
	}()

	// 消费 20 条事件（去抖窗口足够长，保证一条不丢）。
	seen := 0
	deadline := time.After(testWait)
	for seen < 20 {
		select {
		case _, ok := <-ch:
			if !ok {
				t.Fatal("通道提前关闭")
			}
			seen++
		case <-deadline:
			t.Fatalf("只收到 %d 条事件", seen)
		}
	}
	close(stop)
	wg.Wait()
}

// rewriteKeepingMtime 改写内容但把 mtime 恢复成改写前的值，用来构造
// "size+mtime 都相同" 的陷阱。
func rewriteKeepingMtime(t *testing.T, path, content string) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("重写: %v", err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatalf("恢复 mtime: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("无法固定 mtime（%v != %v），该用例在本文件系统上不可靠", after.ModTime(), before.ModTime())
	}
}

// Close 会把最后一个窗口内尚未冲刷的变化尽量交付出去（即使轮询周期很长）。
func TestCloseFlushesPendingChanges(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, Options{
		Interval: time.Hour, // 保证测试期间不会有任何轮询 tick
		Debounce: time.Hour,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.Start(context.Background())
	ch := w.Events()

	target := filepath.Join(root, "late.txt")
	writeFile(t, target, "x")
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	saw := false
	for ev := range ch {
		if ev.Path == target && ev.Kind == KindCreated {
			saw = true
		}
	}
	if !saw {
		t.Fatal("Close 前的变更没有交付（flushFinal 未生效）")
	}
}

// root 在运行中消失：只报一次 Notice，watcher 不崩，Snapshot 保留最后已知状态。
func TestRootDisappearedNoticesOnce(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "ws")
	writeFile(t, filepath.Join(root, "a.txt"), "x")

	var mu sync.Mutex
	var notices []string
	w, err := New(root, fastOptions(func(o *Options) {
		o.Notice = func(format string, args ...any) {
			mu.Lock()
			notices = append(notices, format)
			mu.Unlock()
		}
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { _ = w.Close(); close(done) })
	w.Start(context.Background())

	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("删除 root: %v", err)
	}
	time.Sleep(4 * testInterval) // 让至少几轮扫描失败

	mu.Lock()
	n := len(notices)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("扫描失败 Notice 次数 = %d, want 1（同一错误不应刷屏）: %v", n, notices)
	}
	if snap := w.Snapshot(); len(snap) != 1 {
		t.Fatalf("扫描失败后应保留最后已知状态, got %v", snap)
	}
}

// 显式传空 Ignore 切片表示"什么都不忽略"。
func TestIgnoreExplicitEmptyDisablesDefaults(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "config"), "x")
	writeFile(t, filepath.Join(root, "main.go"), "x")

	w, _ := startWatcher(t, root, func(o *Options) { o.Ignore = []string{} })
	if got := len(w.Snapshot()); got != 2 {
		t.Fatalf("快照有 %d 项, want 2（空 Ignore 应禁用默认忽略）", got)
	}
}
