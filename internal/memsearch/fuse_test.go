package memsearch

import (
	"math"
	"testing"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

func TestRRFFusesTwoRankings(t *testing.T) {
	a := mustURI(t, "mem://user/self/facts/a")
	b := mustURI(t, "mem://user/self/facts/b")
	c := mustURI(t, "mem://user/self/facts/c")

	bm25 := []rankedDoc{{uri: a, score: 9}, {uri: b, score: 5}, {uri: c, score: 1}}
	vector := []rankedDoc{{uri: b, score: 1}, {uri: c, score: 0.5}, {uri: a, score: 0}}

	fused := fuseRRF(bm25, vector, DefaultRRFK, 1, 1)
	wantOrder := []memoryvfs.URI{b, a, c}
	for idx := 0; idx < len(wantOrder)-1; idx++ {
		if fused[wantOrder[idx]] <= fused[wantOrder[idx+1]] {
			t.Fatalf("RRF order wrong: %v (want %s > %s > %s)", fused, b, a, c)
		}
	}
	// b 在 BM25 榜第 2、向量榜第 1 → 1/(60+2) + 1/(60+1)。
	if got, want := fused[b], 61.0/62.0+1.0; math.Abs(got-want) > 1e-12 {
		t.Fatalf("fused[b] = %v, want %v", got, want)
	}
}

func TestRRFHandlesSingleSignal(t *testing.T) {
	a := mustURI(t, "mem://user/self/facts/a")
	b := mustURI(t, "mem://user/self/facts/b")
	fused := fuseRRF([]rankedDoc{{uri: a, score: 9}, {uri: b, score: 1}}, nil, DefaultRRFK, 1, 1)
	if fused[a] <= fused[b] {
		t.Fatalf("fused = %v, want a above b", fused)
	}
	if got, want := fused[a], 1.0; math.Abs(got-want) > 1e-12 {
		t.Fatalf("fused[a] = %v, want %v", got, want)
	}
}

func TestRRFRespectsWeights(t *testing.T) {
	a := mustURI(t, "mem://user/self/facts/a")
	b := mustURI(t, "mem://user/self/facts/b")
	bm25 := []rankedDoc{{uri: a, score: 9}, {uri: b, score: 1}}
	vector := []rankedDoc{{uri: b, score: 1}, {uri: a, score: 0}}

	// 向量权重压到 0，排序完全由 BM25 决定。
	fused := fuseRRF(bm25, vector, DefaultRRFK, 1, 0)
	if fused[a] <= fused[b] {
		t.Fatalf("fused = %v, want a above b when vector weight is 0", fused)
	}
	// 向量权重为 0 时 b 只保留 BM25 那一项（第 2 名 → 1/(k+2)）。
	if got, want := fused[b], 61.0/62.0; math.Abs(got-want) > 1e-12 {
		t.Fatalf("fused[b] = %v, want %v", got, want)
	}
}

func TestWeightedFusionNormalizesScores(t *testing.T) {
	a := mustURI(t, "mem://user/self/facts/a")
	b := mustURI(t, "mem://user/self/facts/b")
	bm25 := []rankedDoc{{uri: a, score: 10}, {uri: b, score: 5}}
	vector := []rankedDoc{{uri: b, score: 1}, {uri: a, score: 0}}

	// bm25 归一化后 a=1、b=0.5；向量 a=0、b=1 → 等权时 b 更高。
	equal := fuseWeighted(bm25, vector, 1, 1)
	if equal[b] <= equal[a] {
		t.Fatalf("equal weights: %v, want b above a", equal)
	}
	if got, want := equal[a], 1.0; math.Abs(got-want) > 1e-12 {
		t.Fatalf("equal[a] = %v, want %v", got, want)
	}
	if got, want := equal[b], 1.5; math.Abs(got-want) > 1e-12 {
		t.Fatalf("equal[b] = %v, want %v", got, want)
	}

	// BM25 权重抬高到 3 后 a 反超。
	heavyBM25 := fuseWeighted(bm25, vector, 3, 1)
	if heavyBM25[a] <= heavyBM25[b] {
		t.Fatalf("bm25-heavy: %v, want a above b", heavyBM25)
	}
	if got, want := heavyBM25[a], 3.0; math.Abs(got-want) > 1e-12 {
		t.Fatalf("heavyBM25[a] = %v, want %v", got, want)
	}
	if got, want := heavyBM25[b], 2.5; math.Abs(got-want) > 1e-12 {
		t.Fatalf("heavyBM25[b] = %v, want %v", got, want)
	}
}

func TestWeightedFusionHandlesDegenerateScores(t *testing.T) {
	a := mustURI(t, "mem://user/self/facts/a")
	// 全是 0 或负分时归一化不产生 NaN/Inf。
	fused := fuseWeighted(
		[]rankedDoc{{uri: a, score: 0}},
		[]rankedDoc{{uri: a, score: -1}},
		1, 1,
	)
	if math.IsNaN(fused[a]) || math.IsInf(fused[a], 0) {
		t.Fatalf("fused = %v, want a finite number", fused[a])
	}
	if fused[a] != 0 {
		t.Fatalf("fused = %v, want 0", fused[a])
	}
}

func TestDefaultFusionIsRRF(t *testing.T) {
	// BM25 排名 A>B>C，向量排名 B>C>A，RRF 会把 B 抬到第一（纯 BM25 是 A 第一）。
	entries := []memoryvfs.Entry{
		doc(t, "mem://user/self/facts/a", "", "", "", "alpha alpha alpha alpha alpha"),
		doc(t, "mem://user/self/facts/b", "", "", "", "alpha alpha"),
		doc(t, "mem://user/self/facts/c", "", "", "", "alpha"),
	}
	index := NewWithOptions(Options{BM25Weight: 1, VectorWeight: 1, Dimension: 2})
	for _, entry := range entries {
		index.Put(entry)
	}
	index.SetVector(entries[0].URI, []float32{0, 1})
	index.SetVector(entries[1].URI, []float32{1, 0})
	index.SetVector(entries[2].URI, []float32{0.5, 0.8660254})

	bm25Only := New(1, 0, 0)
	for _, entry := range entries {
		bm25Only.Put(entry)
	}
	if top := bm25Only.Search(memoryvfs.Query{Text: "alpha"}); top[0].URI != entries[0].URI {
		t.Fatalf("BM25-only top = %s, want %s", top[0].URI, entries[0].URI)
	}

	hits := index.Search(memoryvfs.Query{Text: "alpha", Vector: []float32{1, 0}})
	if len(hits) != 3 {
		t.Fatalf("hits = %#v, want 3", urisOf(hits))
	}
	if hits[0].URI != entries[1].URI {
		t.Fatalf("fused top = %s, want %s (RRF promotes the vector winner)", hits[0].URI, entries[1].URI)
	}
	if want := 61.0/62.0 + 1.0; math.Abs(hits[0].Score-want) > 1e-12 {
		t.Fatalf("top score = %v, want %v", hits[0].Score, want)
	}
}

func TestNewWithOptionsDefaults(t *testing.T) {
	index := NewWithOptions(Options{})
	if index.fusion != FusionRRF {
		t.Fatalf("default fusion = %q, want %q", index.fusion, FusionRRF)
	}
	if index.rrfK != DefaultRRFK {
		t.Fatalf("default k = %v, want %v", index.rrfK, DefaultRRFK)
	}
	if index.bm25Weight != 1 || index.vectorWeight != 1 {
		t.Fatalf("weights = %v/%v, want 1/1", index.bm25Weight, index.vectorWeight)
	}

	zero := New(0, 0, 0)
	if zero.bm25Weight != 1 || zero.vectorWeight != 1 {
		t.Fatalf("New(0,0,0) weights = %v/%v, want 1/1", zero.bm25Weight, zero.vectorWeight)
	}

	custom := NewWithOptions(Options{BM25Weight: 3, VectorWeight: 0.5, Dimension: 8, Fusion: FusionWeighted, RRFK: 10})
	if custom.bm25Weight != 3 || custom.vectorWeight != 0.5 || custom.dimension != 8 {
		t.Fatalf("custom options not applied: %+v", custom)
	}
	if custom.fusion != FusionWeighted || custom.rrfK != 10 {
		t.Fatalf("custom fusion = %q k=%v", custom.fusion, custom.rrfK)
	}
	// 未知融合模式回退到 RRF。
	unknown := NewWithOptions(Options{Fusion: FusionMode("bogus")})
	if unknown.fusion != FusionRRF {
		t.Fatalf("unknown fusion = %q, want %q", unknown.fusion, FusionRRF)
	}
}
