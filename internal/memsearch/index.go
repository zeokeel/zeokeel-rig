// Package memsearch 提供记忆检索的打分与排序：分词、倒排索引、BM25、可选向量召回
// 与 RRF/加权融合。它不碰磁盘，只对内存里的条目集合工作。
//
// 并发约定：索引的**读**是并发安全的（Search/DocFreq/Len 可以并行调用）；写入
// （Put/SetVector/Delete）由调用方串行——写入会重建该条目的词项与倒排投稿。
//
// 打分口径：
//   - BM25（k1=1.2、b=0.75、idf=log(1+(N-df+0.5)/(df+0.5))），词频按字段加权
//     title 3 / summary(L0) 2 / overview(L1) 1.5 / content(L2) 1；文档长度用未加权
//     token 数（与 v1 行为一致，便于对照）。
//   - 有向量时默认用 RRF（k=60）融合 BM25 与余弦排名；也可选加权归一化融合。
//     条目没有向量时向量榜为空，RRF 退化为 BM25 排名的倒数名次分（排序不变，绝对值变小）。
//   - 空查询（无文本、无向量）按 Updated 降序返回，Level 记为 L1。
//
// Search 只做过滤与排序，**不截断**：Offset/Limit 由调用方分页；TokenBudget 与
// Threshold 也不在本层应用（Threshold 更适合在 rerank 之后按调用方口径判断）。
package memsearch

import (
	"math"
	"sort"
	"sync"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

const (
	// 字段权重：标题最强，其次摘要、概览、正文。
	weightTitle    = 3.0
	weightSummary  = 2.0
	weightOverview = 1.5
	weightContent  = 1.0

	// bm25K1/bm25B 是 BM25 的词频饱和与长度归一参数。
	bm25K1 = 1.2
	bm25B  = 0.75

	// recentScoreScale 把 Updated 的纳秒时间戳压缩成可比较的分数
	// （只用于"最近条目"排序，没有绝对含义）。
	recentScoreScale = 1e18
)

// Doc 是一条可检索条目。terms/length 是派生状态，由 Put 维护。
type Doc struct {
	Entry  memoryvfs.Entry
	Vector []float32

	terms  map[string]*termFreq
	length float64
}

// termFreq 是一个词项在某条目内各字段的出现次数。
type termFreq struct {
	title    int
	summary  int
	overview int
	content  int
}

// weighted 返回字段加权后的词频。
func (tf *termFreq) weighted() float64 {
	return weightTitle*float64(tf.title) + weightSummary*float64(tf.summary) +
		weightOverview*float64(tf.overview) + weightContent*float64(tf.content)
}

// level 返回该词项命中的最高优先级层级：title/summary=L0，overview=L1，content=L2。
func (tf *termFreq) level() memoryvfs.Level {
	switch {
	case tf.title > 0 || tf.summary > 0:
		return memoryvfs.Level0
	case tf.overview > 0:
		return memoryvfs.Level1
	default:
		return memoryvfs.Level2
	}
}

// Hit 是一条命中。Level 是命中字段决定的层级（多字段命中取最高优先级，即最小层号）。
type Hit struct {
	URI   memoryvfs.URI
	Score float64
	Level memoryvfs.Level
}

// Options 是索引的可调参数。零值可用：权重 1/1、无向量、RRF(k=60)。
type Options struct {
	BM25Weight   float64
	VectorWeight float64
	Dimension    int
	Fusion       FusionMode
	RRFK         float64
}

// Index 是内存索引。零值不可用，用 New 或 NewWithOptions。
type Index struct {
	mu           sync.RWMutex
	docs         map[memoryvfs.URI]*Doc
	postings     map[string]map[memoryvfs.URI]*termFreq
	docFreq      map[string]int
	totalLength  float64
	bm25Weight   float64
	vectorWeight float64
	dimension    int
	fusion       FusionMode
	rrfK         float64
}

// New 创建索引。dimension<=0 表示不启用向量；两个权重同时为 0 时使用默认值 1/1。
// 融合模式默认 RRF(k=60)。
func New(bm25Weight, vectorWeight float64, dimension int) *Index {
	return NewWithOptions(Options{
		BM25Weight:   bm25Weight,
		VectorWeight: vectorWeight,
		Dimension:    dimension,
	})
}

// NewWithOptions 按 Options 创建索引，并为零值补默认值。
func NewWithOptions(options Options) *Index {
	bm25Weight, vectorWeight := options.BM25Weight, options.VectorWeight
	if bm25Weight <= 0 && vectorWeight <= 0 {
		bm25Weight, vectorWeight = 1, 1
	}
	fusion := options.Fusion
	if fusion != FusionWeighted {
		fusion = FusionRRF
	}
	k := options.RRFK
	if k <= 0 {
		k = DefaultRRFK
	}
	dimension := options.Dimension
	if dimension < 0 {
		dimension = 0
	}
	return &Index{
		docs:         make(map[memoryvfs.URI]*Doc),
		postings:     make(map[string]map[memoryvfs.URI]*termFreq),
		docFreq:      make(map[string]int),
		bm25Weight:   bm25Weight,
		vectorWeight: vectorWeight,
		dimension:    dimension,
		fusion:       fusion,
		rrfK:         k,
	}
}

// Len 返回条目数。
func (i *Index) Len() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.docs)
}

// Dimension 返回向量维度（0 表示不启用向量）。
func (i *Index) Dimension() int { return i.dimension }

// DocFreq 返回包含该词项的条目数（df）。term 是 Tokenize 的输出，不是查询原文。
func (i *Index) DocFreq(term string) int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.docFreq[term]
}

// PostingURIs 返回包含该词项的 URI（升序）。没有该词项时返回 nil。
func (i *Index) PostingURIs(term string) []memoryvfs.URI {
	i.mu.RLock()
	defer i.mu.RUnlock()
	posting := i.postings[term]
	if len(posting) == 0 {
		return nil
	}
	out := make([]memoryvfs.URI, 0, len(posting))
	for uri := range posting {
		out = append(out, uri)
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}

// Put 插入或替换条目（保留已设置的向量）。写入由调用方串行。
func (i *Index) Put(entry memoryvfs.Entry) {
	i.mu.Lock()
	defer i.mu.Unlock()
	doc := i.docs[entry.URI]
	if doc == nil {
		doc = &Doc{}
		i.docs[entry.URI] = doc
	}
	i.detachLocked(doc)
	doc.Entry = entry
	doc.terms, doc.length = analyzeEntry(entry)
	i.attachLocked(doc)
}

// SetVector 设置条目的向量；维度不符或条目不存在时忽略（调用方负责重建）。
func (i *Index) SetVector(uri memoryvfs.URI, vector []float32) {
	i.mu.Lock()
	defer i.mu.Unlock()
	doc := i.docs[uri]
	if doc == nil || i.dimension <= 0 || len(vector) != i.dimension {
		return
	}
	doc.Vector = vector
}

// Delete 删除条目并同步更新倒排与 df。
func (i *Index) Delete(uri memoryvfs.URI) {
	i.mu.Lock()
	defer i.mu.Unlock()
	doc := i.docs[uri]
	if doc == nil {
		return
	}
	i.detachLocked(doc)
	delete(i.docs, uri)
}

// detachLocked 把条目的词项从全局倒排里撤下来（不删除 docs 里的条目）。
func (i *Index) detachLocked(doc *Doc) {
	for term := range doc.terms {
		if posting := i.postings[term]; posting != nil {
			delete(posting, doc.Entry.URI)
			if len(posting) == 0 {
				delete(i.postings, term)
			}
		}
		if i.docFreq[term] > 0 {
			i.docFreq[term]--
			if i.docFreq[term] == 0 {
				delete(i.docFreq, term)
			}
		}
	}
	i.totalLength -= doc.length
	doc.terms = nil
	doc.length = 0
}

// attachLocked 把条目的词项挂到全局倒排上。
func (i *Index) attachLocked(doc *Doc) {
	for term, freq := range doc.terms {
		posting := i.postings[term]
		if posting == nil {
			posting = make(map[memoryvfs.URI]*termFreq)
			i.postings[term] = posting
		}
		posting[doc.Entry.URI] = freq
		i.docFreq[term]++
	}
	i.totalLength += doc.length
}

// analyzeEntry 切分四个字段，返回词项→字段频次与文档长度（未加权 token 数）。
func analyzeEntry(entry memoryvfs.Entry) (map[string]*termFreq, float64) {
	terms := make(map[string]*termFreq)
	length := 0.0
	add := func(text string, bump func(*termFreq)) {
		for _, token := range Tokenize(text) {
			length++
			freq := terms[token]
			if freq == nil {
				freq = &termFreq{}
				terms[token] = freq
			}
			bump(freq)
		}
	}
	add(entry.Title, func(freq *termFreq) { freq.title++ })
	add(entry.Summary, func(freq *termFreq) { freq.summary++ })
	add(entry.Overview, func(freq *termFreq) { freq.overview++ })
	add(entry.Content, func(freq *termFreq) { freq.content++ })
	return terms, length
}

// Search 按查询过滤并排序，返回全部命中（不截断，分页由调用方按 Offset/Limit 做）。
func (i *Index) Search(query memoryvfs.Query) []Hit {
	query = memoryvfs.NormalizeQuery(query)
	i.mu.RLock()
	defer i.mu.RUnlock()

	terms := uniqueTokens(Tokenize(query.Text))
	queryVector, vectorReady := i.vectorChannelLocked(query, len(terms) > 0)
	if query.Text == "" && !vectorReady {
		return i.recentLocked(query)
	}
	if len(terms) == 0 && !vectorReady {
		return nil
	}

	candidates := i.candidatesLocked(query, terms, vectorReady)
	if len(candidates) == 0 {
		return nil
	}

	bm25Ranked := i.bm25RankedLocked(candidates, terms)
	var scores map[memoryvfs.URI]float64
	if vectorReady {
		vectorRanked := i.vectorRankedLocked(candidates, queryVector)
		if i.fusion == FusionWeighted {
			scores = fuseWeighted(bm25Ranked, vectorRanked, i.bm25Weight, i.vectorWeight)
		} else {
			scores = fuseRRF(bm25Ranked, vectorRanked, i.rrfK, i.bm25Weight, i.vectorWeight)
		}
	} else {
		scores = make(map[memoryvfs.URI]float64, len(bm25Ranked))
		for _, item := range bm25Ranked {
			scores[item.uri] = item.score
		}
	}

	hits := make([]Hit, 0, len(scores))
	for uri, score := range scores {
		if score <= 0 {
			continue
		}
		level := i.levelLocked(uri, terms)
		if !levelAllowed(query.Levels, level) {
			continue
		}
		hits = append(hits, Hit{URI: uri, Score: score, Level: level})
	}
	sortHits(hits)
	return hits
}

// vectorChannelLocked 决定这次查询是否走向量通道，并给出查询向量。
//
// 规则：Query.Vector 优先；为空且维度>0 时用确定性 hash 向量（仅测试/无 Embedder 时）；
// 维度不符的显式向量直接跳过（不拿 hash 向量顶替调用方的向量）。
func (i *Index) vectorChannelLocked(query memoryvfs.Query, hasTerms bool) ([]float32, bool) {
	if i.dimension <= 0 || i.vectorWeight <= 0 {
		return nil, false
	}
	if len(query.Vector) > 0 {
		if len(query.Vector) != i.dimension {
			return nil, false
		}
		return query.Vector, true
	}
	if query.DisableVector || !hasTerms {
		return nil, false
	}
	// hash 向量只用于测试/管线演示：真实文档向量与词袋向量不在同一空间，
	// 默认绝不混用（否则会把无关条目当"向量相似"召回）。
	if !query.UseHashVector {
		return nil, false
	}
	return HashVector(query.Text, i.dimension), true
}

// candidatesLocked 收集候选条目：BM25 走倒排，向量通道走全量扫描（只取维度匹配的）。
func (i *Index) candidatesLocked(query memoryvfs.Query, terms []string, vectorReady bool) []*Doc {
	seen := make(map[memoryvfs.URI]struct{}, len(i.docs))
	var candidates []*Doc
	add := func(doc *Doc) {
		if _, ok := seen[doc.Entry.URI]; ok {
			return
		}
		seen[doc.Entry.URI] = struct{}{}
		candidates = append(candidates, doc)
	}
	for _, term := range terms {
		for uri := range i.postings[term] {
			doc := i.docs[uri]
			if doc == nil || !matchFilters(doc.Entry, query) {
				continue
			}
			add(doc)
		}
	}
	if vectorReady {
		for _, doc := range i.docs {
			if len(doc.Vector) != i.dimension || !matchFilters(doc.Entry, query) {
				continue
			}
			add(doc)
		}
	}
	return candidates
}

// bm25RankedLocked 对候选条目算 BM25（只统计候选内出现的查询词项）。
func (i *Index) bm25RankedLocked(candidates []*Doc, terms []string) []rankedDoc {
	if len(terms) == 0 || len(i.docs) == 0 {
		return nil
	}
	docCount := float64(len(i.docs))
	avg := 1.0
	if i.totalLength > 0 {
		avg = i.totalLength / docCount
		if avg <= 0 {
			avg = 1
		}
	}
	out := make([]rankedDoc, 0, len(candidates))
	for _, doc := range candidates {
		score := 0.0
		for _, term := range terms {
			freq := doc.terms[term]
			if freq == nil {
				continue
			}
			tf := freq.weighted()
			if tf <= 0 {
				continue
			}
			df := float64(i.docFreq[term])
			if df <= 0 {
				continue
			}
			idf := math.Log(1 + (docCount-df+0.5)/(df+0.5))
			denom := tf + bm25K1*(1-bm25B+bm25B*doc.length/avg)
			score += idf * tf * (bm25K1 + 1) / denom
		}
		if score > 0 {
			out = append(out, rankedDoc{uri: doc.Entry.URI, score: score})
		}
	}
	return out
}

// vectorRankedLocked 对候选条目算余弦（维度不符的向量跳过）。
func (i *Index) vectorRankedLocked(candidates []*Doc, queryVector []float32) []rankedDoc {
	out := make([]rankedDoc, 0, len(candidates))
	for _, doc := range candidates {
		if len(doc.Vector) != i.dimension {
			continue
		}
		out = append(out, rankedDoc{uri: doc.Entry.URI, score: cosine(queryVector, doc.Vector)})
	}
	return out
}

// levelLocked 按命中字段决定层级：多字段命中取最高优先级（最小层号）。
// 完全没有词项命中（纯向量召回）时给 L2——需要正文才能解释这次命中。
func (i *Index) levelLocked(uri memoryvfs.URI, terms []string) memoryvfs.Level {
	level := memoryvfs.Level2
	for _, term := range terms {
		freq := i.postings[term][uri]
		if freq == nil {
			continue
		}
		if candidate := freq.level(); candidate < level {
			level = candidate
		}
	}
	return level
}

// recentLocked 实现空查询：按 Updated 降序（同时间按 URI 升序）。
func (i *Index) recentLocked(query memoryvfs.Query) []Hit {
	hits := make([]Hit, 0, len(i.docs))
	for _, doc := range i.docs {
		if !matchFilters(doc.Entry, query) {
			continue
		}
		if !levelAllowed(query.Levels, memoryvfs.Level1) {
			continue
		}
		hits = append(hits, Hit{
			URI:   doc.Entry.URI,
			Score: float64(doc.Entry.Updated.UnixNano()) / recentScoreScale,
			Level: memoryvfs.Level1,
		})
	}
	sortHits(hits)
	return hits
}

func sortHits(hits []Hit) {
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].Score != hits[b].Score {
			return hits[a].Score > hits[b].Score
		}
		return hits[a].URI < hits[b].URI
	})
}

// uniqueTokens 去掉重复词项（BM25 的 df 与候选集合只按词项去重；tf 由倒排给出）。
func uniqueTokens(tokens []string) []string {
	if len(tokens) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}

// matchFilters 应用 Scope（URI 段前缀）、Kinds、Types、Tags 过滤。
func matchFilters(entry memoryvfs.Entry, query memoryvfs.Query) bool {
	if !query.Scope.IsZero() && query.Scope != memoryvfs.RootURI && !entry.URI.IsUnder(query.Scope) {
		return false
	}
	if len(query.Kinds) > 0 && !containsKind(query.Kinds, entry.Kind) {
		return false
	}
	if len(query.Types) > 0 && !containsString(query.Types, entry.Type) {
		return false
	}
	for key, want := range query.Tags {
		if entry.Tags[key] != want {
			return false
		}
	}
	return true
}

// levelAllowed 判断命中层级是否在 Query.Levels 里；空列表或 LevelAny 表示不限。
func levelAllowed(levels []memoryvfs.Level, level memoryvfs.Level) bool {
	if len(levels) == 0 {
		return true
	}
	for _, item := range levels {
		if item == memoryvfs.LevelAny || item == level {
			return true
		}
	}
	return false
}

func containsKind(kinds []memoryvfs.Kind, kind memoryvfs.Kind) bool {
	for _, item := range kinds {
		if item == kind {
			return true
		}
	}
	return false
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
