package memsearch

import (
	"math"
	"testing"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

func TestCosine(t *testing.T) {
	if got := cosine([]float32{1, 0}, []float32{1, 0}); math.Abs(got-1) > 1e-9 {
		t.Fatalf("cosine(same) = %v, want 1", got)
	}
	if got := cosine([]float32{1, 0}, []float32{0, 1}); math.Abs(got) > 1e-9 {
		t.Fatalf("cosine(orthogonal) = %v, want 0", got)
	}
	if got := cosine([]float32{0, 0}, []float32{1, 0}); got != 0 {
		t.Fatalf("cosine(zero) = %v, want 0", got)
	}
	if got := cosine([]float32{1, 0}, []float32{1, 0, 0}); got != 0 {
		t.Fatalf("cosine(dim mismatch) = %v, want 0", got)
	}
	if got := cosine(nil, nil); got != 0 {
		t.Fatalf("cosine(nil) = %v, want 0", got)
	}
	if got := cosine([]float32{-1, 0}, []float32{1, 0}); math.Abs(got+1) > 1e-9 {
		t.Fatalf("cosine(opposite) = %v, want -1", got)
	}
}

func TestHashVectorIsDeterministic(t *testing.T) {
	first := HashVector("alpha zephyr 部署", 16)
	second := HashVector("alpha zephyr 部署", 16)
	if len(first) != 16 || len(second) != 16 {
		t.Fatalf("dimensions = %d/%d, want 16", len(first), len(second))
	}
	for idx := range first {
		if first[idx] != second[idx] {
			t.Fatalf("hash vector differs at %d: %v vs %v", idx, first[idx], second[idx])
		}
	}
	if got := cosine(first, second); math.Abs(got-1) > 1e-9 {
		t.Fatalf("self cosine = %v, want 1", got)
	}
	other := HashVector("beta delta", 16)
	if cosine(first, other) > 0.99 {
		t.Fatalf("unrelated texts produced identical hash vectors")
	}
	if len(HashVector("", 8)) != 8 {
		t.Fatalf("empty text hash vector should still have the requested dimension")
	}
}

func TestVectorSearchRanksByQueryVector(t *testing.T) {
	similar := doc(t, "mem://user/self/facts/similar", "alpha", "alpha body", "", "alpha")
	dissimilar := doc(t, "mem://user/self/facts/dissimilar", "alpha", "alpha body", "", "alpha")
	index := NewWithOptions(Options{BM25Weight: 1, VectorWeight: 1, Dimension: 2, Fusion: FusionWeighted})
	index.Put(similar)
	index.Put(dissimilar)
	index.SetVector(similar.URI, []float32{1, 0})
	index.SetVector(dissimilar.URI, []float32{0, 1})

	// 两个文档的文本完全相同，唯一的区分信号是向量。
	hits := index.Search(memoryvfs.Query{Text: "alpha", Vector: []float32{1, 0}})
	if len(hits) != 2 {
		t.Fatalf("hits = %#v, want 2", urisOf(hits))
	}
	if hits[0].URI != similar.URI {
		t.Fatalf("top hit = %s, want %s", hits[0].URI, similar.URI)
	}
}

func TestVectorDimensionMismatchIsSkipped(t *testing.T) {
	good := doc(t, "mem://user/self/facts/good", "alpha", "alpha body", "", "alpha")
	bad := doc(t, "mem://user/self/facts/bad", "alpha", "alpha body", "", "alpha")
	index := NewWithOptions(Options{BM25Weight: 1, VectorWeight: 1, Dimension: 2, Fusion: FusionWeighted})
	index.Put(good)
	index.Put(bad)
	index.SetVector(good.URI, []float32{1, 0})
	index.SetVector(bad.URI, []float32{1, 0, 0}) // 维度不符：被忽略

	hits := index.Search(memoryvfs.Query{Text: "alpha", Vector: []float32{1, 0}})
	if len(hits) != 2 {
		t.Fatalf("hits = %#v, want both docs (BM25 still matches)", urisOf(hits))
	}
	if hits[0].URI != good.URI {
		t.Fatalf("top hit = %s, want %s (only the good vector contributes)", hits[0].URI, good.URI)
	}

	// 查询向量维度也不符时，整个向量通道关闭，不能拿 hash 向量顶替调用方的向量。
	noVector := index.Search(memoryvfs.Query{Text: "alpha", Vector: []float32{1, 0, 0, 0}})
	if len(noVector) != 2 {
		t.Fatalf("hits = %#v, want 2", urisOf(noVector))
	}
}

func TestVectorOnlySearchWithoutText(t *testing.T) {
	first := doc(t, "mem://user/self/facts/first", "first", "first body", "", "first")
	second := doc(t, "mem://user/self/facts/second", "second", "second body", "", "second")
	index := NewWithOptions(Options{BM25Weight: 1, VectorWeight: 1, Dimension: 2})
	index.Put(first)
	index.Put(second)
	index.SetVector(first.URI, []float32{1, 0})
	index.SetVector(second.URI, []float32{0, 1})

	hits := index.Search(memoryvfs.Query{Vector: []float32{0, 1}})
	if len(hits) != 2 {
		t.Fatalf("hits = %#v, want 2", urisOf(hits))
	}
	if hits[0].URI != second.URI {
		t.Fatalf("top hit = %s, want %s", hits[0].URI, second.URI)
	}
	if hits[0].Level != memoryvfs.Level2 {
		t.Fatalf("vector-only level = %s, want L2 (no term hit, whole body needed)", hits[0].Level)
	}
}

func TestExplicitQueryVectorDrivesRanking(t *testing.T) {
	heavy := doc(t, "mem://user/self/facts/heavy", "", "", "", "alpha alpha alpha")
	light := doc(t, "mem://user/self/facts/light", "", "", "", "alpha")
	index := NewWithOptions(Options{BM25Weight: 1, VectorWeight: 1, Dimension: 2, Fusion: FusionWeighted})
	index.Put(heavy)
	index.Put(light)
	index.SetVector(heavy.URI, []float32{0, 1})
	index.SetVector(light.URI, []float32{1, 0})

	// BM25 更看好 heavy，向量更看好 light；换查询向量就会换冠军，说明
	// 排序用的确实是调用方给的 Query.Vector。
	if top := index.Search(memoryvfs.Query{Text: "alpha", Vector: []float32{1, 0}}); top[0].URI != light.URI {
		t.Fatalf("top = %s, want %s", top[0].URI, light.URI)
	}
	if top := index.Search(memoryvfs.Query{Text: "alpha", Vector: []float32{0, 1}}); top[0].URI != heavy.URI {
		t.Fatalf("top = %s, want %s", top[0].URI, heavy.URI)
	}
}

func TestSearchWithoutVectorsFallsBackToBM25(t *testing.T) {
	entry := doc(t, "mem://user/self/facts/alpha", "alpha", "alpha body", "", "alpha")
	index := New(1, 1, 4)
	index.Put(entry)

	hits := index.Search(memoryvfs.Query{Text: "alpha"})
	if len(hits) != 1 {
		t.Fatalf("hits = %#v, want 1", urisOf(hits))
	}
	if hits[0].Score <= 0 {
		t.Fatalf("score = %v, want a positive BM25 score", hits[0].Score)
	}
}
