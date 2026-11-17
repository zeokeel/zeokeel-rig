package search

// 本文件是内容检索：BM25（k1=1.2、b=0.75）+ 多词 AND 语义 + 命中行定位。
//
// 公式（每个文档对每个查询词求和）：
//
//	idf(t)   = ln(1 + (N - df(t) + 0.5) / (df(t) + 0.5))
//	tfNorm   = tf * (k1 + 1) / (tf + k1 * (1 - b + b * dl / avgdl))
//	score(d) = Σ_{t ∈ query} idf(t) * tfNorm(t, d)
//
// 其中 N 是被索引的文档数、df(t) 是包含 t 的文档数、dl 是文档词数、avgdl 是平均
// 词数。多词查询是 AND：只有包含全部查询词的文档才进入排序。

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
)

const (
	// bm25K1 是词频饱和参数（越大越奖励重复出现）。
	bm25K1 = 1.2
	// bm25B 是文档长度归一化参数（0 = 不归一化，1 = 完全归一化）。
	bm25B = 0.75
)

// document 是一个被索引的文档。
type document struct {
	// rel 是相对工作区根的路径（"/" 分隔）。
	rel string
	// termFreq 是词项 → 出现次数。
	termFreq map[string]int
	// length 是文档总词数。
	length int
}

// bm25Index 是一次查询构建的倒排统计（不持久化，随查询丢弃）。
type bm25Index struct {
	docs          []document
	documentFreq  map[string]int
	averageLength float64
}

// newBM25Index 返回一个空索引。
func newBM25Index() *bm25Index {
	return &bm25Index{documentFreq: make(map[string]int)}
}

// add 把一份文件内容加入索引（同一词项在文档内的重复只计一次 df）。
func (index *bm25Index) add(rel string, content []byte) {
	terms := tokenizeBytes(content)
	if len(terms) == 0 {
		return
	}
	termFreq := make(map[string]int, len(terms))
	for _, term := range terms {
		termFreq[term]++
	}
	for term := range termFreq {
		index.documentFreq[term]++
	}
	index.docs = append(index.docs, document{rel: rel, termFreq: termFreq, length: len(terms)})
}

// scoredDocument 是一个命中文档及其得分。
type scoredDocument struct {
	rel   string
	score float64
}

// search 返回满足 AND 语义的文档，按 (score 降序, 路径长度升序, 路径升序) 排序。
//
// 每 scoringCheckInterval 个文档检查一次 ctx：评分是 O(docs × terms)，大语料上
// 不检查 ctx 会让"超时"退化成"算完再成功返回"。
func (index *bm25Index) search(ctx context.Context, terms []string) ([]scoredDocument, error) {
	if len(index.docs) == 0 || len(terms) == 0 {
		return nil, nil
	}
	total := float64(len(index.docs))
	averageLength := index.averageLength
	if averageLength <= 0 {
		averageLength = 1
	}
	uniqueTerms := make([]string, 0, len(terms))
	seen := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		if _, exists := seen[term]; exists {
			continue
		}
		seen[term] = struct{}{}
		uniqueTerms = append(uniqueTerms, term)
	}
	matches := make([]scoredDocument, 0, len(index.docs))
	for docIndex, doc := range index.docs {
		if docIndex%scoringCheckInterval == 0 {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
		}
		if !containsAllTerms(doc.termFreq, uniqueTerms) {
			continue
		}
		score := 0.0
		for _, term := range uniqueTerms {
			termFreq := float64(doc.termFreq[term])
			documentFreq := float64(index.documentFreq[term])
			idf := math.Log(1 + (total-documentFreq+0.5)/(documentFreq+0.5))
			norm := float64(doc.length) / averageLength
			score += idf * (termFreq * (bm25K1 + 1)) / (termFreq + bm25K1*(1-bm25B+bm25B*norm))
		}
		matches = append(matches, scoredDocument{rel: doc.rel, score: score})
	}
	sort.SliceStable(matches, func(left, right int) bool {
		if matches[left].score != matches[right].score {
			return matches[left].score > matches[right].score
		}
		leftLength := len(matches[left].rel)
		rightLength := len(matches[right].rel)
		if leftLength != rightLength {
			return leftLength < rightLength
		}
		return matches[left].rel < matches[right].rel
	})
	return matches, nil
}

// finalize 计算平均文档长度（在 add 全部完成后调用一次）。
func (index *bm25Index) finalize() {
	if len(index.docs) == 0 {
		index.averageLength = 0
		return
	}
	total := 0
	for _, doc := range index.docs {
		total += doc.length
	}
	index.averageLength = float64(total) / float64(len(index.docs))
}

// containsAllTerms 报告文档是否包含全部查询词（AND 语义）。
func containsAllTerms(termFreq map[string]int, terms []string) bool {
	for _, term := range terms {
		if termFreq[term] == 0 {
			return false
		}
	}
	return true
}

// bestHitLine 返回文件里"命中查询词最多"的那一行（并列时取最早的一行）。
//
// 命中行按查询词的字面子串计数：分词会把 camelCase 拆开，因此用字面子串匹配能
// 同时覆盖 "SearchIndex" 与 "search"。
func bestHitLine(content []byte, terms []string) (int, string, bool) {
	lines := strings.Split(string(content), "\n")
	bestLine := 0
	bestContent := ""
	bestHits := 0
	for index, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		lowered := strings.ToLower(line)
		hits := 0
		for _, term := range terms {
			if strings.Contains(lowered, term) {
				hits++
			}
		}
		if hits > bestHits {
			bestHits = hits
			bestLine = index + 1
			bestContent = line
		}
	}
	if bestHits == 0 {
		return 0, "", false
	}
	return bestLine, bestContent, true
}

// formatHit 把一条命中渲染成 `path:line:content`（内容截断到 maxSnippetRunes 字符）。
func formatHit(rel string, line int, content string) string {
	return fmt.Sprintf("%s:%d:%s", rel, line, truncateRunes(content, maxSnippetRunes))
}

// truncateRunes 把 s 截断到至多 max 个字符（rune），真正截断时追加省略号。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// searchText 在 baseDir 下建立 BM25 索引并返回前 limit 条命中行。
func (tool *tool) searchText(ctx context.Context, baseDir, query string, limit int) ([]string, int, error) {
	terms := Tokenize(query)
	if len(terms) == 0 {
		return nil, 0, nil
	}
	files, err := walkSourceFiles(ctx, tool.rootReal, baseDir, tool.hardlinkGuard)
	if err != nil {
		return nil, 0, err
	}
	index := newBM25Index()
	for _, file := range files {
		content, ok, readErr := readSourceFile(ctx, file, int64(tool.maxFileBytes))
		if readErr != nil {
			return nil, 0, readErr
		}
		if !ok {
			continue
		}
		index.add(file.rel, content)
	}
	index.finalize()
	matches, searchErr := index.search(ctx, terms)
	if searchErr != nil {
		return nil, 0, searchErr
	}
	if len(matches) == 0 {
		return nil, 0, nil
	}
	total := len(matches)
	if len(matches) > limit {
		matches = matches[:limit]
	}
	hits := make([]string, 0, len(matches))
	for _, match := range matches {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, ctxErr
		}
		file, ok := findSourceFile(files, match.rel)
		if !ok {
			continue
		}
		content, readable, readErr := readSourceFile(ctx, file, int64(tool.maxFileBytes))
		if readErr != nil {
			return nil, 0, readErr
		}
		if !readable {
			continue
		}
		line, text, found := bestHitLine(content, terms)
		if !found {
			continue
		}
		hits = append(hits, formatHit(match.rel, line, text))
	}
	return hits, total, nil
}

// findSourceFile 按相对路径查回候选文件（二分查找，files 已按 rel 排序）。
func findSourceFile(files []sourceFile, rel string) (sourceFile, bool) {
	index := sort.Search(len(files), func(index int) bool { return files[index].rel >= rel })
	if index < len(files) && files[index].rel == rel {
		return files[index], true
	}
	return sourceFile{}, false
}
