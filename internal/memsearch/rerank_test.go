package memsearch

import (
	"context"
	"errors"
	"testing"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

type fakeReranker struct {
	scores   []float64
	err      error
	gotQuery string
	gotDocs  []string
	calls    int
}

func (f *fakeReranker) Rerank(ctx context.Context, query string, documents []string) ([]float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.calls++
	f.gotQuery = query
	f.gotDocs = append([]string(nil), documents...)
	if f.err != nil {
		return nil, f.err
	}
	return f.scores, nil
}

func hitsFixture(t *testing.T) []Hit {
	t.Helper()
	return []Hit{
		{URI: mustURI(t, "mem://user/self/facts/a"), Score: 4, Level: memoryvfs.Level0},
		{URI: mustURI(t, "mem://user/self/facts/b"), Score: 3, Level: memoryvfs.Level1},
		{URI: mustURI(t, "mem://user/self/facts/c"), Score: 2, Level: memoryvfs.Level2},
		{URI: mustURI(t, "mem://user/self/facts/d"), Score: 1, Level: memoryvfs.Level2},
	}
}

func TestRerankReordersCandidates(t *testing.T) {
	hits := hitsFixture(t)
	docs := map[memoryvfs.URI]string{
		hits[0].URI: "doc a", hits[1].URI: "doc b", hits[2].URI: "doc c", hits[3].URI: "doc d",
	}
	reranker := &fakeReranker{scores: []float64{0.1, 0.2, 0.9, 0.5}}

	// limit=2 → 只把前 4 个（2×limit）交给 reranker，并按新分数重排。
	got, err := Rerank(context.Background(), "query", hits, docs, reranker, 2)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if reranker.gotQuery != "query" {
		t.Fatalf("reranker query = %q", reranker.gotQuery)
	}
	if len(reranker.gotDocs) != 4 {
		t.Fatalf("reranker docs = %#v, want 4 candidates", reranker.gotDocs)
	}
	if reranker.gotDocs[0] != "doc a" || reranker.gotDocs[3] != "doc d" {
		t.Fatalf("reranker docs = %#v", reranker.gotDocs)
	}
	wantOrder := []memoryvfs.URI{hits[2].URI, hits[3].URI, hits[1].URI, hits[0].URI}
	for idx, uri := range wantOrder {
		if got[idx].URI != uri {
			t.Fatalf("rank %d = %s, want %s (%#v)", idx, got[idx].URI, uri, got)
		}
	}
	if got[0].Score != 0.9 {
		t.Fatalf("top score = %v, want 0.9", got[0].Score)
	}
	// Level 必须跟着命中的文档走，不能因为重排而丢失。
	if got[0].Level != memoryvfs.Level2 {
		t.Fatalf("level = %s, want L2", got[0].Level)
	}
}

func TestRerankKeepsHitsBeyondCandidates(t *testing.T) {
	hits := hitsFixture(t)
	docs := map[memoryvfs.URI]string{}
	reranker := &fakeReranker{scores: []float64{0.1, 0.4}}

	got, err := Rerank(context.Background(), "query", hits, docs, reranker, 1)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(got) != len(hits) {
		t.Fatalf("len = %d, want %d", len(got), len(hits))
	}
	// 前两个交换位置，其余保持原顺序。
	if got[0].URI != hits[1].URI || got[1].URI != hits[0].URI {
		t.Fatalf("candidate block = %#v", got[:2])
	}
	if got[2].URI != hits[2].URI || got[3].URI != hits[3].URI {
		t.Fatalf("tail = %#v", got[2:])
	}
	if got[2].Score != hits[2].Score || got[3].Score != hits[3].Score {
		t.Fatalf("tail scores changed: %#v", got[2:])
	}
}

func TestRerankPropagatesErrorAndKeepsOriginalOrder(t *testing.T) {
	hits := hitsFixture(t)
	boom := errors.New("reranker down")
	reranker := &fakeReranker{err: boom}

	got, err := Rerank(context.Background(), "query", hits, nil, reranker, 2)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v (must not be swallowed)", err, boom)
	}
	for idx := range hits {
		if got[idx].URI != hits[idx].URI || got[idx].Score != hits[idx].Score {
			t.Fatalf("fallback changed hit %d: %#v", idx, got[idx])
		}
	}
}

func TestRerankRejectsScoreLengthMismatch(t *testing.T) {
	hits := hitsFixture(t)
	reranker := &fakeReranker{scores: []float64{1}}

	got, err := Rerank(context.Background(), "query", hits, nil, reranker, 2)
	if err == nil {
		t.Fatalf("Rerank should fail when the reranker returns the wrong number of scores")
	}
	if len(got) != len(hits) || got[0].URI != hits[0].URI {
		t.Fatalf("fallback = %#v, want the original order", got)
	}
}

func TestRerankWithoutRerankerIsIdentity(t *testing.T) {
	hits := hitsFixture(t)
	got, err := Rerank(context.Background(), "query", hits, nil, nil, 2)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	for idx := range hits {
		if got[idx] != hits[idx] {
			t.Fatalf("hit %d = %#v, want %#v", idx, got[idx], hits[idx])
		}
	}
}

func TestRerankEmptyHitsAndZeroLimit(t *testing.T) {
	if got, err := Rerank(context.Background(), "query", nil, nil, &fakeReranker{}, 5); err != nil || len(got) != 0 {
		t.Fatalf("Rerank(empty) = %#v, %v", got, err)
	}
	hits := hitsFixture(t)
	reranker := &fakeReranker{scores: []float64{0.1, 0.2, 0.3, 0.4}}
	got, err := Rerank(context.Background(), "query", hits, nil, reranker, 0)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if reranker.calls != 1 || len(reranker.gotDocs) != 4 {
		t.Fatalf("limit<=0 should rerank every hit, got %d docs", len(reranker.gotDocs))
	}
	if got[0].URI != hits[3].URI {
		t.Fatalf("top = %s, want %s", got[0].URI, hits[3].URI)
	}
}

func TestRerankHonorsContextCancellation(t *testing.T) {
	hits := hitsFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Rerank(ctx, "query", hits, nil, &fakeReranker{scores: []float64{1, 2, 3, 4}}, 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRerankTieBreaksByURI(t *testing.T) {
	hits := hitsFixture(t)
	reranker := &fakeReranker{scores: []float64{1, 1, 1, 1}}
	got, err := Rerank(context.Background(), "query", hits, nil, reranker, 4)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	for idx := 1; idx < len(got); idx++ {
		if got[idx-1].URI > got[idx].URI {
			t.Fatalf("ties not sorted by URI: %#v", got)
		}
	}
}
