package memsearch

import (
	"sort"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// FusionMode 选择 BM25 与向量排名的融合方式。
type FusionMode string

const (
	// FusionRRF 是默认融合：Reciprocal Rank Fusion，score = Σ w/(k+rank)。
	FusionRRF FusionMode = "rrf"
	// FusionWeighted 是加权归一化融合：两路分数各自除以本路最大值后加权求和。
	FusionWeighted FusionMode = "weighted"
)

// DefaultRRFK 是 RRF 的平滑常数（与 OpenViking/常见实现一致）。
const DefaultRRFK = 60.0

// rankedDoc 是一路召回里的一条已打分结果。score 越大越相关。
type rankedDoc struct {
	uri   memoryvfs.URI
	score float64
}

// fuseRRF 用 RRF 融合两路排名。只出现在一路里的条目，另一路贡献 0。
// k<=0 时用 DefaultRRFK；权重为 0 表示该路完全不计入。
func fuseRRF(bm25Ranked, vectorRanked []rankedDoc, k, bm25Weight, vectorWeight float64) map[memoryvfs.URI]float64 {
	if k <= 0 {
		k = DefaultRRFK
	}
	out := make(map[memoryvfs.URI]float64, len(bm25Ranked)+len(vectorRanked))
	add := func(list []rankedDoc, weight float64) {
		if weight == 0 || len(list) == 0 {
			return
		}
		for rank, item := range sortedRanked(list) {
			out[item.uri] += weight / (k + float64(rank+1))
		}
	}
	add(bm25Ranked, bm25Weight)
	add(vectorRanked, vectorWeight)
	// 把 RRF 分标定到"单路第一 ≈ 1"：原始 1/(k+1) ≈ 0.016 与 BM25/余弦不在
	// 同一量级，调用方按 0..1 设阈值时会把所有命中都过滤掉。
	scale := k + 1
	for uri := range out {
		out[uri] *= scale
	}
	return out
}

// fuseWeighted 用加权归一化融合两路排名：每路除以本路最大值（负分与 0 不计入）。
func fuseWeighted(bm25Ranked, vectorRanked []rankedDoc, bm25Weight, vectorWeight float64) map[memoryvfs.URI]float64 {
	out := make(map[memoryvfs.URI]float64, len(bm25Ranked)+len(vectorRanked))
	add := func(list []rankedDoc, weight float64) {
		if weight == 0 || len(list) == 0 {
			return
		}
		peak := 0.0
		for _, item := range list {
			if item.score > peak {
				peak = item.score
			}
		}
		if peak <= 0 {
			return
		}
		for _, item := range list {
			if item.score <= 0 {
				continue
			}
			out[item.uri] += weight * item.score / peak
		}
	}
	add(bm25Ranked, bm25Weight)
	add(vectorRanked, vectorWeight)
	return out
}

// sortedRanked 返回按分数降序、同分按 URI 升序的副本（保证 RRF 排名确定）。
func sortedRanked(list []rankedDoc) []rankedDoc {
	out := make([]rankedDoc, len(list))
	copy(out, list)
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].score != out[b].score {
			return out[a].score > out[b].score
		}
		return out[a].uri < out[b].uri
	})
	return out
}
