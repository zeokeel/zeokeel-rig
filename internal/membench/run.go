package membench

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/memsearch"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// RecallK 是 recall@K 的 K。
const RecallK = 5

// hashDimension 是 hybrid 臂的 hash 向量维度。
const hashDimension = 64

// Metrics 是一个对照组在一组查询上的指标。
type Metrics struct {
	Queries           int     `json:"queries"`
	RecallAt5         float64 `json:"recall_at_5"`
	MRR               float64 `json:"mrr"`
	AvgInjectedTokens float64 `json:"avg_injected_tokens"`
	P50LatencyMS      float64 `json:"p50_latency_ms"`
	P95LatencyMS      float64 `json:"p95_latency_ms"`
}

// Arm 是一个对照组。All 是全量查询（easy+hard），Easy/Hard 是分子集口径。
type Arm struct {
	Name string `json:"name"`
	Note string `json:"note,omitempty"`
	// BuildMS 是这条臂的索引构建耗时（进程内、单机测量；不进断言）。
	BuildMS float64 `json:"index_build_ms"`
	All     Metrics `json:"all"`
	Easy    Metrics `json:"easy"`
	Hard    Metrics `json:"hard"`
}

// LayerPoint 是分层注入曲线上的一个点。
type LayerPoint struct {
	Subset            string  `json:"subset"`
	Level             string  `json:"level"`
	RecallAt5         float64 `json:"recall_at_5"`
	AvgInjectedTokens float64 `json:"avg_injected_tokens"`
}

// DatasetSummary 是报告里的数据集概要。
type DatasetSummary struct {
	Sessions            int `json:"sessions"`
	TurnsPerSession     int `json:"turns_per_session"`
	Facts               int `json:"facts"`
	HardFacts           int `json:"hard_facts"`
	Distractors         int `json:"distractors"`
	Twins               int `json:"twins"`
	Queries             int `json:"queries"`
	RewriteQueries      int `json:"rewrite_queries"`
	IntentQueries       int `json:"intent_queries"`
	SemanticQueries     int `json:"semantic_queries"`
	VerbatimQueries     int `json:"verbatim_queries"`
	HardVerbatimQueries int `json:"hard_verbatim_queries"`
}

// IndexInfo 让"数据"不只覆盖查询侧：条目数量、落盘量级、向量占用与索引构建耗时。
type IndexInfo struct {
	Docs            int     `json:"docs"`
	WriteBytes      int64   `json:"write_bytes"`
	VectorCount     int     `json:"vector_count"`
	VectorBytes     int     `json:"vector_bytes"`
	BaselineBuildMS float64 `json:"baseline_build_ms"`
	BM25BuildMS     float64 `json:"bm25_build_ms"`
	HybridBuildMS   float64 `json:"hybrid_build_ms"`
}

// Report 是一次基准的完整结果（可 JSON 序列化；同一 seed 除计时字段外逐字节可复现）。
type Report struct {
	Seed       int64          `json:"seed"`
	Dataset    DatasetSummary `json:"dataset"`
	Index      IndexInfo      `json:"index"`
	Arms       []Arm          `json:"arms"`
	LayerCurve []LayerPoint   `json:"layer_curve"`
	Notes      []string       `json:"notes"`
}

// Run 在 seed 生成的数据集上跑完全部对照组，easy/hard 两个子集分别出指标。
func Run(seed int64) Report {
	dataset := Generate(seed)
	byURI := make(map[memoryvfs.URI]Doc, len(dataset.Docs))
	entries := make([]memoryvfs.Entry, 0, len(dataset.Docs))
	var writeBytes int64
	for _, doc := range dataset.Docs {
		byURI[doc.URI] = doc
		entries = append(entries, entryOf(doc))
		writeBytes += int64(len(doc.Title) + len(doc.Summary) + len(doc.Overview) + len(doc.Content))
	}

	baselineStart := time.Now()
	baseline := newWikiSubstringIndex(dataset.Docs)
	baselineBuild := millis(time.Since(baselineStart))

	bm25 := memsearch.New(1, 0, 0)
	bm25Start := time.Now()
	for _, entry := range entries {
		bm25.Put(entry)
	}
	bm25Build := millis(time.Since(bm25Start))

	hybrid := memsearch.NewWithOptions(memsearch.Options{
		BM25Weight:   1,
		VectorWeight: 1,
		Dimension:    hashDimension,
	})
	hybridStart := time.Now()
	for _, entry := range entries {
		hybrid.Put(entry)
		hybrid.SetVector(entry.URI, memsearch.HashVector(vectorText(entry), hashDimension))
	}
	hybridBuild := millis(time.Since(hybridStart))

	baselineLists, baselineLatency := collect(dataset, func(query Query) []rankedHit {
		return baseline.search(query.Text)
	})
	bm25Lists, bm25Latency := collect(dataset, func(query Query) []rankedHit {
		return searchRanked(bm25, query, false)
	})
	hybridLists, hybridLatency := collect(dataset, func(query Query) []rankedHit {
		return searchRanked(hybrid, query, true)
	})

	// 四条臂的注入口径：
	//   baseline/bm25/hybrid 只注入 L0（旧 wiki 索引行的口径）；
	//   hybrid-layer 按命中层级注入，验证"命中 L0 就不用付正文的钱"。
	injectL0 := injectSummary(byURI)
	arms := []Arm{
		{
			Name:    "baseline-wiki-substring",
			BuildMS: baselineBuild,
			Note: "旧 wiki 等价语义：大小写不敏感子串，标题>摘要>正文，最多 20 条；注入 L0。" +
				"（内存里预存小写字段，真实 FileWiki 还多一层读盘；延迟只做量级参考）",
			All:  measure(dataset, baselineLists, baselineLatency, "", injectL0),
			Easy: measure(dataset, baselineLists, baselineLatency, SubsetEasy, injectL0),
			Hard: measure(dataset, baselineLists, baselineLatency, SubsetHard, injectL0),
		},
		{
			Name:    "bm25",
			BuildMS: bm25Build,
			Note:    "memsearch 倒排索引 + 字段加权 BM25（k1=1.2, b=0.75）；注入 L0",
			All:     measure(dataset, bm25Lists, bm25Latency, "", injectL0),
			Easy:    measure(dataset, bm25Lists, bm25Latency, SubsetEasy, injectL0),
			Hard:    measure(dataset, bm25Lists, bm25Latency, SubsetHard, injectL0),
		},
		{
			Name:    "hybrid-hash-rrf",
			BuildMS: hybridBuild,
			Note:    "BM25 + 确定性 hash 词袋向量，RRF(k=60) 融合；注入 L0",
			All:     measure(dataset, hybridLists, hybridLatency, "", injectL0),
			Easy:    measure(dataset, hybridLists, hybridLatency, SubsetEasy, injectL0),
			Hard:    measure(dataset, hybridLists, hybridLatency, SubsetHard, injectL0),
		},
		{
			Name:    "hybrid-layer",
			BuildMS: hybridBuild,
			Note:    "同一份混合排名，按命中层级注入：L0 命中只取摘要，L1/L2 才升级",
			All: measure(dataset, hybridLists, hybridLatency, "",
				func(hits []rankedHit) int { return injectByLevel(hits, byURI) }),
			Easy: measure(dataset, hybridLists, hybridLatency, SubsetEasy,
				func(hits []rankedHit) int { return injectByLevel(hits, byURI) }),
			Hard: measure(dataset, hybridLists, hybridLatency, SubsetHard,
				func(hits []rankedHit) int { return injectByLevel(hits, byURI) }),
		},
	}

	return Report{
		Seed:    seed,
		Dataset: summarizeDataset(dataset),
		Index: IndexInfo{
			Docs:            len(dataset.Docs),
			WriteBytes:      writeBytes,
			VectorCount:     len(dataset.Docs),
			VectorBytes:     len(dataset.Docs) * hashDimension * 4,
			BaselineBuildMS: baselineBuild,
			BM25BuildMS:     bm25Build,
			HybridBuildMS:   hybridBuild,
		},
		Arms: arms,
		LayerCurve: append(
			buildLayerCurve(dataset, hybridLists, byURI, SubsetEasy),
			buildLayerCurve(dataset, hybridLists, byURI, SubsetHard)...),
		Notes: []string{
			"easy 集（查询都带唯一实体名）验证的是结构化字段 + BM25 相对旧子串语义的排序收益；" +
				"它不证明语义理解。",
			"hard 集（120 条只有 2-3 个描述性属性、没有实体名，且每条 gold 都配一条只有实体不同的" +
				"近重复干扰句）暴露的是纯词法检索的上限：属性词在池子里大量复用，词法信号无法区分 " +
				"gold 与近重复句，BM25 会明显掉到远低于 easy 的水平。真实语义差距需要宿主 Embedder/Reranker。",
			"hybrid 臂使用 memsearch.HashVector（确定性 hash 词袋）：只演示向量通道/RRF 管线，" +
				"不代表真实语义增益；它甚至会把共享主题词的干扰句抬进前列。",
			"近重复干扰句与 gold 的 L0/L1 文本逐字相同（只有正文实体不同），BM25 分数相同，先后由 URI " +
				"稳定排序决定——这正是纯词法系统的真实处境。",
			"干扰句的 Updated 晚于对应 gold（模拟最近写入的相似记录），baseline 的 Updated 排序更容易被顶掉。",
			"延迟与索引构建耗时是单机进程内测量，随机器负载浮动，不进断言；其余字段同 seed 逐字节可复现。",
			fmt.Sprintf("tokens 用 memoryvfs.EstimateTokens 估算（CJK 1 token/字，其余 4 字节/token）；"+
				"write bytes 是 %d 条条目四字段 UTF-8 字节总和。", len(dataset.Docs)),
		},
	}
}

func millis(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / 1e6
}

// entryOf 把基准文档变成 memoryvfs 条目。
func entryOf(doc Doc) memoryvfs.Entry {
	return memoryvfs.Entry{
		URI:      doc.URI,
		Kind:     memoryvfs.KindMemory,
		Type:     doc.Category,
		Title:    doc.Title,
		Summary:  doc.Summary,
		Overview: doc.Overview,
		Content:  doc.Content,
		Updated:  doc.Updated,
	}
}

// vectorText 是喂给 hash 向量的文本（四个字段全给）。
func vectorText(entry memoryvfs.Entry) string {
	return strings.Join([]string{entry.Title, entry.Summary, entry.Overview, entry.Content}, " ")
}

// searchRanked 用 memsearch 检索并转成统一形态。
func searchRanked(index *memsearch.Index, query Query, useHash bool) []rankedHit {
	hits := index.Search(memoryvfs.Query{Text: query.Text, UseHashVector: useHash})
	out := make([]rankedHit, 0, len(hits))
	for _, hit := range hits {
		out = append(out, rankedHit{URI: hit.URI, Level: hit.Level})
	}
	return out
}

// collect 对每条查询跑一次检索，返回命中列表与耗时（毫秒）。
func collect(dataset Dataset, search func(Query) []rankedHit) ([][]rankedHit, []float64) {
	lists := make([][]rankedHit, len(dataset.Queries))
	latencies := make([]float64, len(dataset.Queries))
	for idx, query := range dataset.Queries {
		start := time.Now()
		lists[idx] = search(query)
		latencies[idx] = millis(time.Since(start))
	}
	return lists, latencies
}

// measure 计算某个子集上的 recall@K、MRR、注入 token 与延迟分位数。
// subset 为空表示全量查询。
func measure(dataset Dataset, lists [][]rankedHit, latencies []float64, subset string, tokens func([]rankedHit) int) Metrics {
	selected := make([]int, 0, len(dataset.Queries))
	for idx, query := range dataset.Queries {
		if subset != "" && query.Subset != subset {
			continue
		}
		selected = append(selected, idx)
	}
	if len(selected) == 0 {
		return Metrics{}
	}
	recalled := 0
	rrSum := 0.0
	tokenSum := 0
	queryLatency := make([]float64, 0, len(selected))
	for _, idx := range selected {
		rank := rankOf(lists[idx], dataset.Queries[idx].Gold)
		if rank > 0 {
			rrSum += 1 / float64(rank)
			if rank <= RecallK {
				recalled++
			}
		}
		tokenSum += tokens(topK(lists[idx], RecallK))
		if idx < len(latencies) {
			queryLatency = append(queryLatency, latencies[idx])
		}
	}
	return Metrics{
		Queries:           len(selected),
		RecallAt5:         float64(recalled) / float64(len(selected)),
		MRR:               rrSum / float64(len(selected)),
		AvgInjectedTokens: float64(tokenSum) / float64(len(selected)),
		P50LatencyMS:      percentile(queryLatency, 50),
		P95LatencyMS:      percentile(queryLatency, 95),
	}
}

// buildLayerCurve 用同一份混合排名算某子集 L0 / L0+L1 / L0+L1+L2 的 token 曲线。
func buildLayerCurve(dataset Dataset, lists [][]rankedHit, byURI map[memoryvfs.URI]Doc, subset string) []LayerPoint {
	levels := []struct {
		label string
		level memoryvfs.Level
	}{
		{"L0", memoryvfs.Level0},
		{"L0+L1", memoryvfs.Level1},
		{"L0+L1+L2", memoryvfs.Level2},
	}
	curve := make([]LayerPoint, 0, len(levels))
	for _, item := range levels {
		level := item.level
		metrics := measure(dataset, lists, nil, subset, func(hits []rankedHit) int {
			return injectUpTo(hits, byURI, level)
		})
		curve = append(curve, LayerPoint{
			Subset:            subset,
			Level:             item.label,
			RecallAt5:         metrics.RecallAt5,
			AvgInjectedTokens: metrics.AvgInjectedTokens,
		})
	}
	return curve
}

// injectSummary 只注入标题 + L0 摘要（旧 wiki 索引行的口径）。
func injectSummary(byURI map[memoryvfs.URI]Doc) func([]rankedHit) int {
	return func(hits []rankedHit) int {
		total := 0
		for _, hit := range hits {
			doc, ok := byURI[hit.URI]
			if !ok {
				continue
			}
			total += memoryvfs.EstimateTokens(doc.Title) + memoryvfs.EstimateTokens(doc.Summary)
		}
		return total
	}
}

// injectByLevel 按命中层级注入：L0 只要摘要，L1 加概览，L2 加正文。
func injectByLevel(hits []rankedHit, byURI map[memoryvfs.URI]Doc) int {
	total := 0
	for _, hit := range hits {
		doc, ok := byURI[hit.URI]
		if !ok {
			continue
		}
		total += memoryvfs.EstimateTokens(doc.Title)
		switch hit.Level {
		case memoryvfs.Level0:
			total += memoryvfs.EstimateTokens(doc.Summary)
		case memoryvfs.Level1:
			total += memoryvfs.EstimateTokens(doc.Summary) + memoryvfs.EstimateTokens(doc.Overview)
		default:
			total += memoryvfs.EstimateTokens(doc.Summary) +
				memoryvfs.EstimateTokens(doc.Overview) +
				memoryvfs.EstimateTokens(doc.Content)
		}
	}
	return total
}

// injectUpTo 注入到指定层级（含该层）。
func injectUpTo(hits []rankedHit, byURI map[memoryvfs.URI]Doc, level memoryvfs.Level) int {
	total := 0
	for _, hit := range hits {
		doc, ok := byURI[hit.URI]
		if !ok {
			continue
		}
		total += memoryvfs.EstimateTokens(doc.Title) + memoryvfs.EstimateTokens(doc.Summary)
		if level >= memoryvfs.Level1 {
			total += memoryvfs.EstimateTokens(doc.Overview)
		}
		if level >= memoryvfs.Level2 {
			total += memoryvfs.EstimateTokens(doc.Content)
		}
	}
	return total
}

func rankOf(hits []rankedHit, gold memoryvfs.URI) int {
	for idx, hit := range hits {
		if hit.URI == gold {
			return idx + 1
		}
	}
	return 0
}

func topK(hits []rankedHit, k int) []rankedHit {
	if k <= 0 || len(hits) <= k {
		return hits
	}
	return hits[:k]
}

// percentile 用 nearest-rank 算分位数；空输入返回 0。
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// summarizeDataset 汇总数据集规模与查询构成。
func summarizeDataset(dataset Dataset) DatasetSummary {
	summary := DatasetSummary{
		Sessions:        dataset.Sessions,
		TurnsPerSession: dataset.TurnsPerSession,
		Queries:         len(dataset.Queries),
	}
	for _, doc := range dataset.Docs {
		switch {
		case doc.Twin:
			summary.Twins++
		case doc.Gold && doc.Hard:
			summary.HardFacts++
		case doc.Gold:
			summary.Facts++
		default:
			summary.Distractors++
		}
	}
	for _, query := range dataset.Queries {
		switch query.Kind {
		case QueryRewrite:
			summary.RewriteQueries++
		case QueryIntent:
			summary.IntentQueries++
		case QuerySemantic:
			summary.SemanticQueries++
		}
		if query.Verbatim {
			summary.VerbatimQueries++
			if query.Subset == SubsetHard {
				summary.HardVerbatimQueries++
			}
		}
	}
	return summary
}
