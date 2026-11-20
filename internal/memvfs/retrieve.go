package memvfs

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/memsearch"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// Find 是单查询检索：不做意图分析、不扩写，其余管线与 Search 一致。
func (s *Store) Find(ctx context.Context, query memoryvfs.Query) (memoryvfs.Result, error) {
	return s.search(ctx, query, false)
}

// Search 在 Mode 为 thinking/context 时允许意图分析与多查询融合。
func (s *Store) Search(ctx context.Context, query memoryvfs.Query) (memoryvfs.Result, error) {
	return s.search(ctx, query, true)
}

func (s *Store) search(ctx context.Context, query memoryvfs.Query, allowPlan bool) (memoryvfs.Result, error) {
	if err := ctxErr(ctx); err != nil {
		return memoryvfs.Result{}, err
	}
	started := time.Now()
	// PageSize / RecallTokens 是后端默认值：调用方显式给了就不覆盖。
	if query.Limit <= 0 && s.opts.PageSize > 0 {
		query.Limit = s.opts.PageSize
	}
	if query.TokenBudget <= 0 && query.Mode == memoryvfs.ModeContext && s.opts.RecallTokens > 0 {
		query.TokenBudget = s.opts.RecallTokens
	}
	query = memoryvfs.NormalizeQuery(query)
	query = s.embedQuery(ctx, query)

	plan := s.embedPlan(ctx, s.buildPlan(ctx, query, allowPlan), query)

	if err := s.rlock(); err != nil {
		return memoryvfs.Result{}, err
	}
	candidates := s.collectLocked(plan, query)
	s.mu.RUnlock()

	// 先做绝对压缩，再精排：精排分与 BM25 尾部分数因此处于同一量级，阈值不会
	// 出现"精排头部被丢、弱尾部被留下"的反向选择。
	candidates = normalizeScores(candidates)
	// 阈值只作用在归一化后的 BM25 单一尺度上，且在精排之前：精排器的分数量纲
	// 由宿主决定，拿它跟 BM25 尾部比会误杀精排第一名。
	if strings.TrimSpace(query.Text) != "" || len(query.Vector) > 0 {
		candidates = applyThreshold(candidates, query.Threshold)
	}
	// 精排只重排头部块，尾部保持原顺序跟在后面（两套尺度不混排）。
	candidates = s.rerank(ctx, query, candidates)

	total := len(candidates)
	page := paginate(candidates, query.Offset, query.Limit)
	entries, tokens := s.applyBodyPolicy(page, query)

	return memoryvfs.Result{
		Entries: entries,
		Total:   total,
		Tokens:  tokens,
		Plan:    plan,
		Took:    time.Since(started),
	}, nil
}

// embedQuery 在配置了 Embedder 且调用方没给向量时补上查询向量。没有 Embedder
// 或调用失败时显式关闭向量通道，绝不用 hash 向量顶替真实向量空间。
func (s *Store) embedQuery(ctx context.Context, query memoryvfs.Query) memoryvfs.Query {
	if s.dimension > 0 && len(query.Vector) == s.dimension {
		return query
	}
	text := strings.TrimSpace(query.Text)
	if text == "" || s.opts.Embedder == nil || s.dimension <= 0 {
		query.DisableVector = true
		return query
	}
	vectors, err := s.opts.Embedder.Embed(ctx, []string{text})
	if err != nil || len(vectors) != 1 || len(vectors[0]) != s.dimension {
		query.DisableVector = true
		return query
	}
	query.Vector = append([]float32(nil), vectors[0]...)
	return query
}

// embedPlan 给意图分析产出的扩展查询补向量；失败时该查询显式关闭向量通道。
func (s *Store) embedPlan(ctx context.Context, plan []memoryvfs.TypedQuery, base memoryvfs.Query) []memoryvfs.TypedQuery {
	for index := range plan {
		if plan[index].Text == base.Text && !base.DisableVector && len(base.Vector) == s.dimension && s.dimension > 0 {
			plan[index].Vector = base.Vector
			continue
		}
		if s.opts.Embedder == nil || s.dimension <= 0 || base.DisableVector {
			continue
		}
		text := strings.TrimSpace(plan[index].Text)
		if text == "" {
			continue
		}
		vectors, err := s.opts.Embedder.Embed(ctx, []string{text})
		if err != nil || len(vectors) != 1 || len(vectors[0]) != s.dimension {
			continue
		}
		plan[index].Vector = append([]float32(nil), vectors[0]...)
	}
	return plan
}

// buildPlan 产出最多 3 条查询：quick 只有原查询；thinking/context 走 IntentAnalyzer，
// 没有 analyzer 时用"查询 + 最近会话关键词"扩展。
func (s *Store) buildPlan(ctx context.Context, query memoryvfs.Query, allowPlan bool) []memoryvfs.TypedQuery {
	if !allowPlan || (query.Mode != memoryvfs.ModeThinking && query.Mode != memoryvfs.ModeContext) {
		return []memoryvfs.TypedQuery{{Text: query.Text}}
	}
	recent := s.recentMessages(query.SessionID)
	if s.opts.Intent != nil {
		queries, err := s.opts.Intent.Analyze(ctx, query.Text, recent)
		if err == nil {
			filtered := make([]memoryvfs.TypedQuery, 0, len(queries))
			for _, typed := range queries {
				if strings.TrimSpace(typed.Text) == "" {
					continue
				}
				filtered = append(filtered, typed)
				if len(filtered) == 3 {
					break
				}
			}
			if len(filtered) > 0 {
				return filtered
			}
		}
	}
	plan := []memoryvfs.TypedQuery{{Text: query.Text}}
	keywords := keywordsFromMessages(recent, query.Text)
	if len(keywords) > 0 {
		expanded := strings.TrimSpace(query.Text + " " + strings.Join(keywords, " "))
		if expanded != strings.TrimSpace(query.Text) {
			plan = append(plan, memoryvfs.TypedQuery{Text: expanded, Priority: 1})
		}
	}
	return plan
}

// recentMessages 取最近一次会话的待提交消息（会话级查询用 SessionID 精确指定）。
func (s *Store) recentMessages(sessionID string) []memoryvfs.Message {
	if err := s.rlock(); err != nil {
		return nil
	}
	defer s.mu.RUnlock()
	if strings.TrimSpace(sessionID) != "" {
		messages, err := s.readPendingLocked(sessionID)
		if err != nil {
			return nil
		}
		return trimMessages(messages, 12)
	}
	ids := s.sessionIDsLocked()
	if len(ids) == 0 {
		return nil
	}
	best := ""
	var bestAt time.Time
	for _, id := range ids {
		at := s.sessionUpdatedLocked(id)
		if best == "" || at.After(bestAt) {
			best, bestAt = id, at
		}
	}
	messages, err := s.readPendingLocked(best)
	if err != nil {
		return nil
	}
	return trimMessages(messages, 12)
}

func trimMessages(messages []memoryvfs.Message, limit int) []memoryvfs.Message {
	if len(messages) <= limit {
		return messages
	}
	return messages[len(messages)-limit:]
}

// keywordsFromMessages 从会话消息里挑出高频、且不与查询重复的词项。
func keywordsFromMessages(messages []memoryvfs.Message, query string) []string {
	if len(messages) == 0 {
		return nil
	}
	skip := make(map[string]struct{})
	for _, token := range memsearch.Tokenize(query) {
		skip[token] = struct{}{}
	}
	counts := make(map[string]int)
	var order []string
	for _, message := range messages {
		for _, token := range memsearch.Tokenize(message.Content) {
			if _, seen := skip[token]; seen {
				continue
			}
			if len(token) < 3 && !hasNonASCII(token) {
				continue
			}
			if counts[token] == 0 {
				order = append(order, token)
			}
			counts[token]++
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		if counts[order[a]] != counts[order[b]] {
			return counts[order[a]] > counts[order[b]]
		}
		return order[a] < order[b]
	})
	if len(order) > 4 {
		order = order[:4]
	}
	return order
}

func hasNonASCII(text string) bool {
	for _, r := range text {
		if r > 127 {
			return true
		}
	}
	return false
}

// collectLocked 执行计划里的每条查询，按 URI 去重取最高分，并做层级过滤。
func (s *Store) collectLocked(plan []memoryvfs.TypedQuery, base memoryvfs.Query) []memoryvfs.Entry {
	merged := make(map[memoryvfs.URI]memsearch.Hit, len(plan)*4)
	for _, typed := range plan {
		text := typed.Text
		if strings.TrimSpace(text) == "" && strings.TrimSpace(base.Text) != "" {
			continue
		}
		query := base
		query.Text = text
		if typed.Kind != "" {
			query.Kinds = []memoryvfs.Kind{typed.Kind}
		}
		if typed.Type != "" {
			query.Types = []string{typed.Type}
		}
		query.Vector = typed.Vector
		query.UseHashVector = false
		if s.dimension <= 0 || len(query.Vector) != s.dimension {
			query.DisableVector = true
		}
		for _, hit := range s.index.Search(query) {
			if existing, ok := merged[hit.URI]; !ok || hit.Score > existing.Score {
				merged[hit.URI] = hit
			}
		}
	}
	entries := make([]memoryvfs.Entry, 0, len(merged))
	for uri, hit := range merged {
		stored, ok := s.entries[uri]
		if !ok {
			continue
		}
		entry := stored.Clone()
		entry.Score = hit.Score
		entry.MatchedLevel = hit.Level
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(a, b int) bool {
		if entries[a].Score != entries[b].Score {
			return entries[a].Score > entries[b].Score
		}
		return entries[a].URI < entries[b].URI
	})
	return entries
}

// rerank 对前 2×limit 条做精排；其余条目保持原顺序跟在后面。
func (s *Store) rerank(ctx context.Context, query memoryvfs.Query, entries []memoryvfs.Entry) []memoryvfs.Entry {
	if s.opts.Reranker == nil || len(entries) < 2 {
		return entries
	}
	head := 2 * query.Limit
	if head > len(entries) {
		head = len(entries)
	}
	if head < 2 {
		return entries
	}
	documents := make([]string, head)
	for index := 0; index < head; index++ {
		documents[index] = entries[index].Summary + "\n" + entries[index].Overview
	}
	scores, err := s.opts.Reranker.Rerank(ctx, query.Text, documents)
	if err != nil || len(scores) != head {
		return entries
	}
	headEntries := make([]memoryvfs.Entry, head)
	copy(headEntries, entries[:head])
	for index := range headEntries {
		// 精排器分数量纲不由契约限定（可能 >1）。用同一个绝对压缩函数，避免
		// Score=5.0 外泄、也避免小分时被未精排尾部反超。
		headEntries[index].Score = squashScore(scores[index])
	}
	sort.SliceStable(headEntries, func(a, b int) bool {
		if headEntries[a].Score != headEntries[b].Score {
			return headEntries[a].Score > headEntries[b].Score
		}
		return headEntries[a].URI < headEntries[b].URI
	})
	out := make([]memoryvfs.Entry, 0, len(entries))
	out = append(out, headEntries...)
	out = append(out, entries[head:]...)
	return out
}

func squashScore(score float64) float64 {
	if score <= 0 {
		return 0
	}
	return score / (1 + score)
}

// normalizeScores 用单调压缩 score/(1+score) 把任意非负原始分映射到 [0,1)。
//
// 它**不**按本次最大值归一：否则最高分恒为 1，Threshold 永远无法表达"没有
// 足够相关的命中"（0.99 也会放行一条弱命中）。压缩是绝对的、跨查询可比的
// （同一分数量级下），代价是 rerank 的 0..1 分会映射到 0..0.5。
func normalizeScores(entries []memoryvfs.Entry) []memoryvfs.Entry {
	for index := range entries {
		score := entries[index].Score
		if score <= 0 {
			entries[index].Score = 0
			continue
		}
		entries[index].Score = score / (1 + score)
	}
	return entries
}

func applyThreshold(entries []memoryvfs.Entry, threshold float64) []memoryvfs.Entry {
	if threshold <= 0 {
		return entries
	}
	kept := entries[:0]
	for _, entry := range entries {
		if entry.Score >= threshold {
			kept = append(kept, entry)
		}
	}
	return kept
}

func paginate(entries []memoryvfs.Entry, offset, limit int) []memoryvfs.Entry {
	if offset >= len(entries) {
		return nil
	}
	end := offset + limit
	if end > len(entries) {
		end = len(entries)
	}
	return entries[offset:end]
}

// applyBodyPolicy 落实 IncludeBody 与 TokenBudget：预算内按 L0→L1→L2 逐级装入，
// 装不下的条目丢弃（不超预算）。
func (s *Store) applyBodyPolicy(page []memoryvfs.Entry, query memoryvfs.Query) ([]memoryvfs.Entry, int) {
	maxLevel := memoryvfs.Level2
	if !query.IncludeBody {
		maxLevel = memoryvfs.Level1
	}
	out := make([]memoryvfs.Entry, 0, len(page))
	used := 0
	for _, entry := range page {
		if query.TokenBudget <= 0 {
			entry = trimToLevel(entry, maxLevel)
			out = append(out, entry)
			continue
		}
		chosen := memoryvfs.Level(-1)
		for _, level := range []memoryvfs.Level{memoryvfs.Level2, memoryvfs.Level1, memoryvfs.Level0} {
			if level > maxLevel {
				continue
			}
			cost := entry.TokenEstimate(level)
			if used+cost <= query.TokenBudget {
				chosen = level
				used += cost
				break
			}
		}
		if chosen < 0 {
			continue
		}
		out = append(out, trimToLevel(entry, chosen))
	}
	return out, used
}
