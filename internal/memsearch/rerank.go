package memsearch

import (
	"context"
	"fmt"
	"sort"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// RerankCandidateFactor 决定送给 reranker 的候选倍数：只重排前 factor×limit 条。
const RerankCandidateFactor = 2

// Rerank 用宿主 Reranker 对检索结果精排。
//
// 只把前 RerankCandidateFactor×limit 条候选交给 reranker（limit<=0 表示全部），
// 重排后的候选块写回原位置，未进入候选块的命中保持原顺序与分数。
// docs 是 URI→用于重排的正文（缺失按空串处理）。
//
// 失败语义：reranker 报错或返回的分数条数不符时，返回**原始顺序**的命中**且**返回
// 错误——不吞错，由调用方决定是降级用原分数还是整次检索报错。
func Rerank(
	ctx context.Context,
	query string,
	hits []Hit,
	docs map[memoryvfs.URI]string,
	reranker memoryvfs.Reranker,
	limit int,
) ([]Hit, error) {
	fallback := func() []Hit { return append([]Hit(nil), hits...) }
	if len(hits) == 0 || reranker == nil {
		return fallback(), nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return fallback(), fmt.Errorf("memsearch: rerank canceled: %w", err)
	}

	candidates := len(hits)
	if limit > 0 && limit*RerankCandidateFactor < candidates {
		candidates = limit * RerankCandidateFactor
	}
	documents := make([]string, candidates)
	for idx := 0; idx < candidates; idx++ {
		documents[idx] = docs[hits[idx].URI]
	}

	scores, err := reranker.Rerank(ctx, query, documents)
	if err != nil {
		return fallback(), fmt.Errorf("memsearch: rerank %d candidates: %w", candidates, err)
	}
	if len(scores) != candidates {
		return fallback(), fmt.Errorf(
			"memsearch: reranker returned %d scores for %d documents", len(scores), candidates)
	}

	out := fallback()
	block := make([]Hit, candidates)
	for idx := 0; idx < candidates; idx++ {
		block[idx] = Hit{URI: out[idx].URI, Score: scores[idx], Level: out[idx].Level}
	}
	sort.SliceStable(block, func(a, b int) bool {
		if block[a].Score != block[b].Score {
			return block[a].Score > block[b].Score
		}
		return block[a].URI < block[b].URI
	})
	copy(out, block)
	return out, nil
}
