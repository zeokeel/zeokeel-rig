package memvfs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/memsearch"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// 固定时钟：所有时间断言都基于它，测试不依赖真实时间。
var testNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func testClock() time.Time { return testNow }

// newStore 在临时目录里打开一个文件后端。
func newStore(t *testing.T, opts ...memoryvfs.Option) *Store {
	t.Helper()
	return newStoreAt(t, t.TempDir(), opts...)
}

func newStoreAt(t *testing.T, dir string, opts ...memoryvfs.Option) *Store {
	t.Helper()
	store, err := Open(dir, opts...)
	if err != nil {
		t.Fatalf("Open(%q): %v", dir, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newMemStore 打开进程内后端。
func newMemStore(t *testing.T, opts ...memoryvfs.Option) *Store {
	t.Helper()
	store := OpenMemory(opts...)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func memEntry(raw, typ, summary, content string) memoryvfs.Entry {
	return memoryvfs.Entry{
		URI:     memoryvfs.URI(raw),
		Kind:    memoryvfs.KindMemory,
		Type:    typ,
		Summary: summary,
		Content: content,
	}
}

func mustWrite(t *testing.T, store *Store, entry memoryvfs.Entry) memoryvfs.Entry {
	t.Helper()
	written, err := store.Write(context.Background(), entry)
	if err != nil {
		t.Fatalf("Write(%s): %v", entry.URI, err)
	}
	return written
}

func mustStat(t *testing.T, store *Store, raw string) memoryvfs.Entry {
	t.Helper()
	entry, err := store.Stat(context.Background(), memoryvfs.URI(raw))
	if err != nil {
		t.Fatalf("Stat(%s): %v", raw, err)
	}
	return entry
}

func mustRead(t *testing.T, store *Store, raw string, level memoryvfs.Level) memoryvfs.Entry {
	t.Helper()
	entry, err := store.Read(context.Background(), memoryvfs.URI(raw), level)
	if err != nil {
		t.Fatalf("Read(%s): %v", raw, err)
	}
	return entry
}

// fakeEmbedder 是确定性的哈希嵌入：同文本同向量，不同维度可切换。
type fakeEmbedder struct {
	dimension int
	err       error
	calls     int
}

func (f *fakeEmbedder) Dimension() int { return f.dimension }

func (f *fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for index, text := range texts {
		vector := make([]float32, f.dimension)
		for _, token := range memsearch.Tokenize(text) {
			hash := uint64(1469598103934665603)
			for i := 0; i < len(token); i++ {
				hash ^= uint64(token[i])
				hash *= 1099511628211
			}
			vector[hash%uint64(f.dimension)]++
		}
		out[index] = vector
	}
	return out, nil
}

// fakeIntent 返回固定的一组 TypedQuery。
type fakeIntent struct {
	queries []memoryvfs.TypedQuery
	err     error
}

func (f *fakeIntent) Analyze(ctx context.Context, _ string, _ []memoryvfs.Message) ([]memoryvfs.TypedQuery, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.queries, nil
}

// failingExtractor 永远返回错误，用来验证"提交失败保留 pending"。
type failingExtractor struct{ err error }

func (f *failingExtractor) Extract(context.Context, memoryvfs.Session) ([]memoryvfs.Candidate, error) {
	return nil, f.err
}

// fixedExtractor 返回预设候选。
type fixedExtractor struct{ candidates []memoryvfs.Candidate }

func (f *fixedExtractor) Extract(context.Context, memoryvfs.Session) ([]memoryvfs.Candidate, error) {
	return f.candidates, nil
}

// mapSummarizer 返回固定摘要，用来验证 Summarizer 注入。
type mapSummarizer struct {
	summary  string
	overview string
	err      error
}

func (m *mapSummarizer) Summarize(context.Context, memoryvfs.Entry) (string, string, error) {
	return m.summary, m.overview, m.err
}

func wantErrIs(t *testing.T, err, target error, context string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("%s: got error %v, want %v", context, err, target)
	}
}
