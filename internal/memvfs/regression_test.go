package memvfs

// 审计 r1 的回归守护：零 At 幂等、Restore 崩溃窗口自愈、阈值可表达无命中、
// 没有 Embedder 时不得用 hash 向量污染召回。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

func TestCaptureZeroAtIsIdempotent(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	message := memoryvfs.Message{Role: "user", Content: "remember the launch date"}
	for attempt := 0; attempt < 3; attempt++ {
		if err := store.Capture(ctx, "s1", message); err != nil {
			t.Fatalf("Capture #%d: %v", attempt, err)
		}
	}
	state, err := store.Pending(ctx, "s1")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if state.Pending != 1 {
		t.Fatalf("零 At 消息重复 Capture 后 pending = %d, want 1", state.Pending)
	}
}

func TestOpenRecoversInterruptedRestore(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if _, err := store.Write(ctx, memoryvfs.Entry{
		URI: "mem://user/self/memories/preferences/theme", Kind: memoryvfs.KindMemory,
		Type: "preferences", Summary: "dark mode", Content: "dark mode",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 模拟 Restore 在 live → .old 之后、staging → live 之前崩溃。
	entries := filepath.Join(dir, entriesDirName)
	old := entries + ".old"
	if err := os.Rename(entries, old); err != nil {
		t.Fatalf("rename entries: %v", err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after interrupted restore: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if _, err := reopened.Stat(ctx, "mem://user/self/memories/preferences/theme"); err != nil {
		t.Fatalf("重启后条目丢失（.old 没有恢复）: %v", err)
	}
}

func TestCommitSurvivesQuotaFull(t *testing.T) {
	store := newStore(t, memoryvfs.WithMaxEntries(1))
	ctx := context.Background()
	if _, err := store.Write(ctx, memoryvfs.Entry{
		URI: "mem://user/self/memories/preferences/fill", Kind: memoryvfs.KindMemory,
		Type: "preferences", Summary: "fill the quota", Content: "x",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := store.Capture(ctx, "s-quota", memoryvfs.Message{
		Role: "user", Content: "I prefer dark mode for the editor please",
	}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	result, err := store.Commit(ctx, "s-quota", memoryvfs.CommitOptions{Extract: true})
	if err != nil {
		t.Fatalf("配额满时 Commit 不应失败: %v", err)
	}
	if result.Skipped == 0 {
		t.Fatalf("配额满时抽取候选应被跳过: %+v", result)
	}
	state, err := store.Pending(ctx, "s-quota")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if state.Pending != 0 {
		t.Fatalf("Commit 成功后 pending 应清空，得到 %d", state.Pending)
	}
	if _, err := store.Stat(ctx, result.Archive); err != nil {
		t.Fatalf("归档缺失: %v", err)
	}
}

type constantEmbedder struct{ dim int }

func (e constantEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for index := range texts {
		vector := make([]float32, e.dim)
		vector[0] = 1
		out[index] = vector
	}
	return out, nil
}
func (e constantEmbedder) Dimension() int { return e.dim }

func TestVectorChannelIsReachable(t *testing.T) {
	store := newStore(t, memoryvfs.WithEmbedder(constantEmbedder{dim: 4}))
	ctx := context.Background()
	if _, err := store.Write(ctx, memoryvfs.Entry{
		URI: "mem://user/self/memories/preferences/alpha", Kind: memoryvfs.KindMemory,
		Type: "preferences", Summary: "alpha", Content: "alpha",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// 查询与条目没有词项重叠；只有向量通道能召回。
	result, err := store.Find(ctx, memoryvfs.Query{Text: "gamma", Limit: 5})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(result.Entries) == 0 {
		t.Fatal("Store 的向量通道不可达：查询向量被丢弃")
	}
}

func TestConcurrentAppendKeepsEveryUpdate(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	uri := memoryvfs.URI("mem://user/self/memories/events/log")
	if _, err := store.Write(ctx, memoryvfs.Entry{URI: uri, Kind: memoryvfs.KindMemory, Type: "events", Summary: "log", Content: "base"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	const writers = 12
	done := make(chan error, writers)
	for index := 0; index < writers; index++ {
		go func(index int) {
			marker := fmt.Sprintf("writer-%02d", index)
			for attempt := 0; attempt < 20; attempt++ {
				_, err := store.Append(ctx, uri, marker)
				if err == nil {
					done <- nil
					return
				}
				if !errors.Is(err, memoryvfs.ErrConflict) {
					done <- err
					return
				}
			}
			done <- fmt.Errorf("writer %d: too many conflicts", index)
		}(index)
	}
	for index := 0; index < writers; index++ {
		if err := <-done; err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	entry, err := store.Read(ctx, uri, memoryvfs.Level2)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for index := 0; index < writers; index++ {
		if !strings.Contains(entry.Content, fmt.Sprintf("writer-%02d", index)) {
			t.Fatalf("并发 Append 丢了 writer-%02d:\n%s", index, entry.Content)
		}
	}
}

type flatReranker struct{ score float64 }

func (r flatReranker) Rerank(ctx context.Context, query string, documents []string) ([]float64, error) {
	out := make([]float64, len(documents))
	for index := range out {
		out[index] = r.score
	}
	return out, nil
}

func TestRerankScoresAreSquashed(t *testing.T) {
	store := newStore(t, memoryvfs.WithReranker(flatReranker{score: 5}))
	ctx := context.Background()
	for _, name := range []string{"alpha-one", "alpha-two"} {
		if _, err := store.Write(ctx, memoryvfs.Entry{
			URI: memoryvfs.URI("mem://user/self/memories/preferences/" + name), Kind: memoryvfs.KindMemory,
			Type: "preferences", Summary: "alpha " + name, Content: "alpha " + name,
		}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	result, err := store.Find(ctx, memoryvfs.Query{Text: "alpha", Limit: 1})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(result.Entries) == 0 {
		t.Fatal("no hit")
	}
	// 精排器返回 5.0；压缩后应为 5/6≈0.833（若走 BM25 则在 0.3 左右）。
	if score := result.Entries[0].Score; score < 0.82 || score > 0.84 {
		t.Fatalf("精排分未按 score/(1+score) 压缩，Score=%v", score)
	}
}

func TestRerankHeadStaysAheadOfTail(t *testing.T) {
	store := newStore(t, memoryvfs.WithReranker(flatReranker{score: 0.05}))
	ctx := context.Background()
	for index := 0; index < 8; index++ {
		name := fmt.Sprintf("zeta-%d", index)
		if _, err := store.Write(ctx, memoryvfs.Entry{
			URI: memoryvfs.URI("mem://user/self/memories/preferences/" + name), Kind: memoryvfs.KindMemory,
			Type: "preferences", Summary: "zeta " + name, Content: "zeta " + name,
		}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	// limit=1 → 精排只处理前 2 条；其余尾部不能被混排到精排第一名之前。
	result, err := store.Find(ctx, memoryvfs.Query{Text: "zeta", Limit: 1})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(result.Entries) == 0 {
		t.Fatal("no hit")
	}
	// 所有候选都以 zeta- 开头；必须断言"具体是精排块的第一名"，否则全局混排
	// 把尾部 zeta-2 顶上来时这条用例仍会绿。
	if got := result.Entries[0].URI.Base(); got != "zeta-0" {
		t.Fatalf("精排头被尾部挤出首页: got %s, want zeta-0", got)
	}
}

func TestConcurrentAppendWithEmbedderKeepsEveryUpdate(t *testing.T) {
	store := newStore(t, memoryvfs.WithEmbedder(constantEmbedder{dim: 4}))
	ctx := context.Background()
	uri := memoryvfs.URI("mem://user/self/memories/events/log-embed")
	if _, err := store.Write(ctx, memoryvfs.Entry{URI: uri, Kind: memoryvfs.KindMemory, Type: "events", Summary: "log", Content: "base"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	const writers = 4
	done := make(chan error, writers)
	for index := 0; index < writers; index++ {
		go func(index int) {
			marker := fmt.Sprintf("cas-%02d", index)
			for attempt := 0; attempt < 40; attempt++ {
				_, err := store.Append(ctx, uri, marker)
				if err == nil {
					done <- nil
					return
				}
				if !errors.Is(err, memoryvfs.ErrConflict) {
					done <- err
					return
				}
			}
			done <- fmt.Errorf("writer %d: too many conflicts", index)
		}(index)
	}
	for index := 0; index < writers; index++ {
		if err := <-done; err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	entry, err := store.Read(ctx, uri, memoryvfs.Level2)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for index := 0; index < writers; index++ {
		if !strings.Contains(entry.Content, fmt.Sprintf("cas-%02d", index)) {
			t.Fatalf("带 Embedder 的 CAS 分支丢 cas-%02d:\n%s", index, entry.Content)
		}
	}
}

func TestRestoreRebuildsVectors(t *testing.T) {
	store := newStore(t, memoryvfs.WithEmbedder(constantEmbedder{dim: 4}))
	ctx := context.Background()
	if _, err := store.Write(ctx, memoryvfs.Entry{
		URI: "mem://user/self/memories/preferences/alpha", Kind: memoryvfs.KindMemory,
		Type: "preferences", Summary: "alpha", Content: "alpha",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	snapshot, err := store.Snapshot(ctx, "before")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := store.Delete(ctx, "mem://user/self/memories/preferences/alpha"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Restore(ctx, snapshot.ID); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// 查询与条目无词项重叠：只有向量通道能召回，说明 Restore 后向量已重算。
	result, err := store.Find(ctx, memoryvfs.Query{Text: "gamma", Limit: 5})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(result.Entries) == 0 {
		t.Fatal("Restore 后向量缓存为空，向量通道失效")
	}
}

func TestReadRejectsInvalidLevel(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if _, err := store.Write(ctx, memoryvfs.Entry{
		URI: "mem://user/self/memories/preferences/level", Kind: memoryvfs.KindMemory,
		Type: "preferences", Summary: "level", Content: "body",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := store.Read(ctx, "mem://user/self/memories/preferences/level", memoryvfs.Level(7)); !errors.Is(err, memoryvfs.ErrInvalidEntry) {
		t.Fatalf("invalid level 应返回 ErrInvalidEntry，得到 %v", err)
	}
}

func TestSessionSegmentsDoNotCollide(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	seen := make(map[memoryvfs.URI]string)
	for _, id := range []string{"ws 1", "ws-1", "Foo", "foo"} {
		if err := store.Capture(ctx, id, memoryvfs.Message{Role: "user", Content: "remember " + id}); err != nil {
			t.Fatalf("Capture(%q): %v", id, err)
		}
		result, err := store.Commit(ctx, id, memoryvfs.CommitOptions{})
		if err != nil {
			t.Fatalf("Commit(%q): %v", id, err)
		}
		if previous, dup := seen[result.Archive]; dup {
			t.Fatalf("session %q 与 %q 落到同一归档 %s", id, previous, result.Archive)
		}
		seen[result.Archive] = id
	}
}
func TestGCOnMemoryStoreDoesNotTouchWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	defer func() { _ = os.Chdir(previous) }()
	if err := os.MkdirAll("trash", 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join("trash", "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	store := newStore(t)
	if _, err := store.GC(context.Background(), memoryvfs.GCOptions{MaxEntries: 1}); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if _, err := os.Stat(filepath.Join("trash", "keep.txt")); err != nil {
		t.Fatalf("进程内 GC 删了 CWD 的 trash: %v", err)
	}
}

func TestGCDoesNotEvictSessionArchivesByDefault(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if err := store.Capture(ctx, "sess-gc", memoryvfs.Message{Role: "user", Content: "remember this"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	result, err := store.Commit(ctx, "sess-gc", memoryvfs.CommitOptions{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := store.GC(ctx, memoryvfs.GCOptions{TTL: time.Nanosecond}); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if _, err := store.Stat(ctx, result.Archive); err != nil {
		t.Fatalf("默认 GC 不应删除会话归档: %v", err)
	}
	if _, err := store.GC(ctx, memoryvfs.GCOptions{TTL: time.Nanosecond, IncludeSessions: true}); err != nil {
		t.Fatalf("GC include sessions: %v", err)
	}
	if _, err := store.Stat(ctx, result.Archive); !errors.Is(err, memoryvfs.ErrNotFound) {
		t.Fatalf("IncludeSessions=true 应能删除会话归档，得到 %v", err)
	}
}

type reentrantSummarizer struct{ store *Store }

func (r reentrantSummarizer) Summarize(ctx context.Context, entry memoryvfs.Entry) (string, string, error) {
	if _, err := r.store.Stats(ctx); err != nil {
		return "", "", err
	}
	return entry.Summary, entry.Overview, nil
}

func TestHostCallbacksCanReenterStore(t *testing.T) {
	store := newStore(t)
	summarizer := reentrantSummarizer{store: store}
	store.opts.Summarizer = summarizer
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		if _, err := store.CompileRules(ctx, "always run tests", nil); err != nil {
			done <- err
			return
		}
		if _, err := store.Import(ctx, memoryvfs.Pack{Version: 1, Entries: []memoryvfs.Entry{{
			URI: "mem://user/self/memories/preferences/reentrant", Kind: memoryvfs.KindMemory,
			Type: "preferences", Summary: "reentrant", Content: "body",
		}}}, memoryvfs.ImportOptions{}); err != nil {
			done <- err
			return
		}
		if err := store.Capture(ctx, "reentrant", memoryvfs.Message{Role: "user", Content: "I prefer dark mode"}); err != nil {
			done <- err
			return
		}
		_, err := store.Commit(ctx, "reentrant", memoryvfs.CommitOptions{Extract: true, WorkingMemory: true})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("callback reentry: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("宿主回调回读 Store 时死锁")
	}
}

func TestCaptureKeepsRepeatedMessagesWithDistinctKeys(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if err := store.Capture(ctx, "repeat",
		memoryvfs.Message{Role: "user", Content: "continue", Key: "run-1-0"},
		memoryvfs.Message{Role: "user", Content: "continue", Key: "run-1-2"},
	); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	result, err := store.Commit(ctx, "repeat", memoryvfs.CommitOptions{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	archive, err := store.Read(ctx, result.Archive, memoryvfs.Level2)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if strings.Count(archive.Content, "user: continue") != 2 {
		t.Fatalf("重复消息被吞:\n%s", archive.Content)
	}
}
