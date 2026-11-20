package spillfile_test

// 文件型溢出存储的契约测试。
//
// 这三件事必须被守住，否则模型会看到"结果已存起来"然后永久丢失：
//   - Save 写下的句柄能 Load 回来；
//   - 回收只作用于同一作用域（一个会话的 MaxFiles 不会删掉另一个会话的句柄）；
//   - 句柄不能被当成路径用（Load 只读本目录里的普通文件）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/spillfile"
)

// newStore 在临时目录下建一个存储，返回存储与目录路径。
func newStore(t *testing.T) (*spillfile.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "spill")
	store, err := spillfile.New(dir)
	if err != nil {
		t.Fatalf("New(%q) = %v，want nil error", dir, err)
	}
	return store, dir
}

// storedNames 返回目录里的文件名（用于断言回收结果）。
func storedNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	return names
}

// TestNewCreatesDirectory 断言目录不存在时 New 自己建（0755），宿主不必先 MkdirAll。
func TestNewCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "spill")
	if _, err := spillfile.New(dir); err != nil {
		t.Fatalf("New: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%q 不是目录", dir)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Fatalf("目录权限 = %o，want 755", perm)
	}
}

// TestSaveLoadRoundTrip 断言 Save 的句柄能原样读回，且句柄只是文件名。
func TestSaveLoadRoundTrip(t *testing.T) {
	store, dir := newStore(t)
	ctx := context.Background()
	content := strings.Repeat("一行业务输出\n", 500)

	ref, err := store.Save(ctx, rig.ToolCall{ID: "call-1", Name: "bash"}, content)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if ref.Bytes != len(content) {
		t.Fatalf("ref.Bytes = %d，want %d", ref.Bytes, len(content))
	}
	if ref.ID == "" {
		t.Fatal("Save 返回空句柄：模型没有可传回的 ID")
	}
	if strings.ContainsRune(ref.ID, filepath.Separator) {
		t.Fatalf("句柄 %q 含路径分隔符：宿主换目录后句柄就失效了", ref.ID)
	}
	if !strings.HasPrefix(ref.ID, "spill.") || !strings.HasSuffix(ref.ID, ".txt") {
		t.Fatalf("句柄 %q 不符合 spill.<scope>.<纳秒>-<随机>.txt 的形状", ref.ID)
	}
	if names := storedNames(t, dir); len(names) != 1 || names[0] != ref.ID {
		t.Fatalf("目录内容 = %v，want 只有 %q", names, ref.ID)
	}

	got, err := store.Load(ctx, ref.ID)
	if err != nil {
		t.Fatalf("Load(%q): %v", ref.ID, err)
	}
	if got != content {
		t.Fatalf("读回 %d 字节，want %d 字节", len(got), len(content))
	}
}

// TestSaveUsesScopeAndGlobalBucket 断言文件名带作用域，空作用域落 global 桶。
func TestSaveUsesScopeAndGlobalBucket(t *testing.T) {
	store, _ := newStore(t)
	scoped := rig.WithSpillScope(context.Background(), "session-a")
	global := context.Background()

	aRef, err := store.Save(scoped, rig.ToolCall{Name: "bash"}, "A")
	if err != nil {
		t.Fatalf("Save(作用域 session-a): %v", err)
	}
	if !strings.HasPrefix(aRef.ID, "spill.session-a.") {
		t.Fatalf("句柄 %q 没带作用域前缀 spill.session-a.", aRef.ID)
	}
	gRef, err := store.Save(global, rig.ToolCall{Name: "bash"}, "G")
	if err != nil {
		t.Fatalf("Save(空作用域): %v", err)
	}
	if !strings.HasPrefix(gRef.ID, "spill.global.") {
		t.Fatalf("空作用域的句柄 %q 没落 global 桶", gRef.ID)
	}
}

// TestLoadRejectsUnsafeIDs 断言句柄不能当路径用：分隔符与 ".." 一律拒绝。
func TestLoadRejectsUnsafeIDs(t *testing.T) {
	store, dir := newStore(t)
	ctx := context.Background()
	// 本目录外放一个"能读到就说明越界了"的文件。
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	for _, id := range []string{
		"../outside.txt",
		filepath.Join("..", "outside.txt"),
		"a/b.txt",
		"a\\b.txt",
		"..",
		outside,
	} {
		if _, err := store.Load(ctx, id); err == nil {
			t.Errorf("Load(%q) = nil error，want 拒绝", id)
		} else if errors.Is(err, rig.ErrSpillNotFound) {
			t.Errorf("Load(%q) = ErrSpillNotFound，want 明确的「不安全句柄」错误", id)
		}
	}
}

// TestLoadMissingReturnsNotFound 断言不存在的句柄返回 ErrSpillNotFound（spill_read
// 靠它把"没有这条结果"与真实故障分开）。
func TestLoadMissingReturnsNotFound(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.Load(context.Background(), "spill.global.1-abc.txt"); !errors.Is(err, rig.ErrSpillNotFound) {
		t.Fatalf("Load(不存在) = %v，want rig.ErrSpillNotFound", err)
	}
}

// TestGCKeepsNewest 断言同作用域内按文件名时间戳保留最新 keep 个。
func TestGCKeepsNewest(t *testing.T) {
	store, dir := newStore(t)
	ctx := rig.WithSpillScope(context.Background(), "session-a")
	refs := make([]string, 0, 3)
	for _, content := range []string{"一", "二", "三"} {
		ref, err := store.Save(ctx, rig.ToolCall{Name: "bash"}, content)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		refs = append(refs, ref.ID)
	}

	dropped, err := store.GC(ctx, 2)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("GC 删了 %d 个，want 1", dropped)
	}
	if _, err := store.Load(ctx, refs[0]); !errors.Is(err, rig.ErrSpillNotFound) {
		t.Fatalf("最旧的句柄还在（Load = %v）：回收没有保留最新 keep 个", err)
	}
	for _, id := range refs[1:] {
		if _, err := store.Load(ctx, id); err != nil {
			t.Fatalf("最新的句柄 %q 被删了：%v", id, err)
		}
	}
	if names := storedNames(t, dir); len(names) != 2 {
		t.Fatalf("目录里剩 %d 个文件，want 2（%v）", len(names), names)
	}
}

// TestGCKeepsNewestUsesNameTimestamps 断言排序看的是文件名里的纳秒时间戳：
// 一批 Save 落在同一秒内时，保留的仍是最新的那几个，且保留的就是最后写入的。
func TestGCKeepsNewestUsesNameTimestamps(t *testing.T) {
	store, dir := newStore(t)
	ctx := context.Background()
	refs := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		ref, err := store.Save(ctx, rig.ToolCall{Name: "bash"}, strings.Repeat("x", i+1))
		if err != nil {
			t.Fatalf("Save #%d: %v", i, err)
		}
		refs = append(refs, ref.ID)
	}
	if dropped, err := store.GC(ctx, 3); err != nil || dropped != 2 {
		t.Fatalf("GC(ctx, 3) = (%d, %v)，want (2, nil)", dropped, err)
	}
	kept := storedNames(t, dir)
	if len(kept) != 3 {
		t.Fatalf("目录里剩 %d 个文件，want 3（%v）", len(kept), kept)
	}
	remaining := make(map[string]bool, len(kept))
	for _, name := range kept {
		remaining[name] = true
	}
	for _, id := range refs[:2] {
		if remaining[id] {
			t.Fatalf("旧的 %q 没被回收（保留的是 %v）", id, kept)
		}
	}
	for _, id := range refs[2:] {
		if !remaining[id] {
			t.Fatalf("最新的 %q 被回收了（保留的是 %v）", id, kept)
		}
	}
}

// TestGCEscopesAreIndependent 断言 A 的 GC 不动 B 的条目（多会话共用一个目录）。
func TestGCEscopesAreIndependent(t *testing.T) {
	store, _ := newStore(t)
	ctxA := rig.WithSpillScope(context.Background(), "session-a")
	ctxB := rig.WithSpillScope(context.Background(), "session-b")

	bRefs := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		if _, err := store.Save(ctxA, rig.ToolCall{Name: "bash"}, "A"); err != nil {
			t.Fatalf("Save(A): %v", err)
		}
		ref, err := store.Save(ctxB, rig.ToolCall{Name: "bash"}, "B")
		if err != nil {
			t.Fatalf("Save(B): %v", err)
		}
		bRefs = append(bRefs, ref.ID)
	}

	if dropped, err := store.GC(ctxA, 1); err != nil || dropped != 2 {
		t.Fatalf("GC(A, 1) = (%d, %v)，want (2, nil)", dropped, err)
	}
	for _, id := range bRefs {
		if _, err := store.Load(ctxB, id); err != nil {
			t.Fatalf("作用域 B 的句柄 %q 被 A 的 GC 删了：%v", id, err)
		}
	}
}

// TestGCEmptyScopeIsOwnBucket 断言空作用域是一个独立桶，不是通配符。
func TestGCEmptyScopeIsOwnBucket(t *testing.T) {
	store, _ := newStore(t)
	ctxA := rig.WithSpillScope(context.Background(), "session-a")
	var aRefs []string
	for i := 0; i < 3; i++ {
		ref, err := store.Save(ctxA, rig.ToolCall{Name: "bash"}, "A")
		if err != nil {
			t.Fatalf("Save(A): %v", err)
		}
		aRefs = append(aRefs, ref.ID)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.Save(context.Background(), rig.ToolCall{Name: "bash"}, "G"); err != nil {
			t.Fatalf("Save(空作用域): %v", err)
		}
	}

	if dropped, err := store.GC(context.Background(), 1); err != nil || dropped != 2 {
		t.Fatalf("GC(空作用域, 1) = (%d, %v)，want (2, nil)", dropped, err)
	}
	for _, id := range aRefs {
		if _, err := store.Load(ctxA, id); err != nil {
			t.Fatalf("空作用域 GC 删掉了 session-a 的句柄 %q：%v", id, err)
		}
	}
}

// TestGCNonPositiveKeepIsNoop 断言 keep<=0 不做事（返回 0, nil，不删文件）。
func TestGCNonPositiveKeepIsNoop(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	ref, err := store.Save(ctx, rig.ToolCall{Name: "bash"}, "内容")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	for _, keep := range []int{0, -1} {
		dropped, err := store.GC(ctx, keep)
		if err != nil || dropped != 0 {
			t.Fatalf("GC(ctx, %d) = (%d, %v)，want (0, nil)", keep, dropped, err)
		}
	}
	if _, err := store.Load(ctx, ref.ID); err != nil {
		t.Fatalf("keep<=0 的 GC 删了文件：%v", err)
	}
}

// TestSaveConcurrent 断言并发 Save 不互相踩：句柄两两不同且都能读回。
func TestSaveConcurrent(t *testing.T) {
	store, _ := newStore(t)
	const workers = 16
	var wg sync.WaitGroup
	ids := make([]string, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := rig.WithSpillScope(context.Background(), "session-a")
			content := strings.Repeat(string(rune('a'+i)), 32)
			ref, err := store.Save(ctx, rig.ToolCall{ID: "call", Name: "bash"}, content)
			if err != nil {
				errs[i] = err
				return
			}
			ids[i] = ref.ID
			got, err := store.Load(ctx, ref.ID)
			if err != nil {
				errs[i] = err
				return
			}
			if got != content {
				errs[i] = errors.New("内容不一致")
			}
		}(i)
	}
	wg.Wait()
	seen := make(map[string]bool, workers)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if seen[ids[i]] {
			t.Fatalf("两个并发 Save 拿到同一个句柄 %q", ids[i])
		}
		seen[ids[i]] = true
	}
}

// TestDirReturnsAbsolutePath 断言 Dir 返回绝对目录：宿主用它把溢出结果的位置
// 展示在文件树里，相对路径会随进程 cwd 漂移。
func TestDirReturnsAbsolutePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spill")
	store, err := spillfile.New(dir)
	if err != nil {
		t.Fatalf("New(%q): %v", dir, err)
	}
	got := store.Dir()
	if !filepath.IsAbs(got) {
		t.Errorf("Dir() = %q，want 绝对路径", got)
	}
	if want, absErr := filepath.Abs(dir); absErr != nil {
		t.Fatalf("filepath.Abs(%q): %v", dir, absErr)
	} else if got != want {
		t.Errorf("Dir() = %q，want %q", got, want)
	}
}

// TestNewRejectsBadDirectory 覆盖构造失败的两条路径：空目录名，以及父路径是
// 普通文件（MkdirAll 必然失败）。两者都必须返回错误，而不是给宿主一个写不进去的 Store。
func TestNewRejectsBadDirectory(t *testing.T) {
	t.Run("空目录名", func(t *testing.T) {
		if store, err := spillfile.New("   "); err == nil {
			t.Fatalf("New(空串) = %v, nil；want 错误", store)
		}
	})
	t.Run("父路径是普通文件", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %q: %v", file, err)
		}
		if store, err := spillfile.New(filepath.Join(file, "spill")); err == nil {
			t.Fatalf("New(以普通文件为父路径) = %v, nil；want 错误", store)
		}
	})
}
