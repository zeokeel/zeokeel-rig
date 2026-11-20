package memvfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig/internal/memextract"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// CompileRules 把指令（或来源条目）编译成规则条目，写到 mem://user/self/rules 下。
//
// 每条规则的 URI 是 `<scenario>-<hash8>`，hash 取自内容，因此同样的指令重复编译
// 会落到同一批 URI（覆盖写，不会越编越多）。返回值是可作为规则根的目录条目。
func (s *Store) CompileRules(ctx context.Context, instruction string, sources []memoryvfs.URI) (memoryvfs.Entry, error) {
	if err := ctxErr(ctx); err != nil {
		return memoryvfs.Entry{}, err
	}
	if strings.TrimSpace(instruction) == "" && len(sources) == 0 {
		return memoryvfs.Entry{}, fmt.Errorf("%w: instruction and sources are both empty", memoryvfs.ErrInvalidEntry)
	}
	// 读来源用读锁，写回用写锁；Summarizer/Embedder 在锁外调用。
	if err := s.rlock(); err != nil {
		return memoryvfs.Entry{}, err
	}
	sourceTexts := make([]string, 0, len(sources))
	for _, source := range sources {
		parsed, err := memoryvfs.ParseURI(string(source))
		if err != nil {
			s.mu.RUnlock()
			return memoryvfs.Entry{}, err
		}
		entry, err := s.statLocked(parsed)
		if err != nil {
			s.mu.RUnlock()
			return memoryvfs.Entry{}, err
		}
		text := entry.Summary
		if trimmed := strings.TrimSpace(entry.Content); trimmed != "" {
			if text != "" {
				text += "\n"
			}
			text += trimmed
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		sourceTexts = append(sourceTexts, text)
	}
	s.mu.RUnlock()

	type preparedRule struct {
		entry  memoryvfs.Entry
		vector []float32
	}
	prepared := make([]preparedRule, 0, 4)
	for _, draft := range memextract.CompileRules(instruction, sourceTexts) {
		scenario := sanitizeSlug(draft.Scenario)
		if scenario == "" {
			scenario = "general"
		}
		sum := sha256.Sum256([]byte(draft.Scenario + "\x00" + draft.Content))
		hash := hex.EncodeToString(sum[:])[:8]
		uri, err := memoryvfs.ParseURI(fmt.Sprintf("%s%s-%s", memoryvfs.Scheme+"user/self/rules/", scenario, hash))
		if err != nil {
			return memoryvfs.Entry{}, err
		}
		summary, _ := memoryvfs.TruncateBytes(memoryvfs.CollapseWhitespace(draft.Content), 200)
		entry := memoryvfs.Entry{
			URI:     uri,
			Kind:    memoryvfs.KindRule,
			Type:    memoryvfs.TypeRules,
			Title:   scenario,
			Summary: summary,
			Content: draft.Content,
			Tags:    map[string]string{"scenario": draft.Scenario},
			Source:  "rules:" + scenario,
		}
		entry = s.prepareEntry(ctx, entry)
		prepared = append(prepared, preparedRule{entry: entry, vector: s.embedEntry(ctx, entry)})
	}

	if err := s.lock(); err != nil {
		return memoryvfs.Entry{}, err
	}
	defer s.mu.Unlock()
	for _, rule := range prepared {
		if _, err := s.writeLockedPrepared(ctx, rule.entry, rule.vector); err != nil {
			return memoryvfs.Entry{}, err
		}
	}
	if _, ok := s.dirs[rulesRootURI]; !ok {
		if _, err := s.mkdirLocked(rulesRootURI, "behaviour rules"); err != nil {
			return memoryvfs.Entry{}, err
		}
	}
	return s.statLocked(rulesRootURI)
}

// SearchRules 读出全部规则，用 memextract.ScoreRule 打分，按概率降序返回。
// threshold<=0 表示不过滤。
func (s *Store) SearchRules(ctx context.Context, taskContext string, threshold float64) ([]memoryvfs.Rule, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if err := s.rlock(); err != nil {
		return nil, err
	}
	defer s.mu.RUnlock()
	rules := make([]memoryvfs.Rule, 0, 8)
	for uri, entry := range s.entries {
		if entry.Kind != memoryvfs.KindRule && entry.Type != memoryvfs.TypeRules {
			continue
		}
		score := memextract.ScoreRule(taskContext, entry.Content)
		if threshold > 0 && score < threshold {
			continue
		}
		scenario := strings.TrimSpace(entry.Tags["scenario"])
		if scenario == "" {
			scenario = entry.Title
		}
		rules = append(rules, memoryvfs.Rule{
			URI:         uri,
			Content:     entry.Content,
			Scenario:    scenario,
			Probability: score,
		})
	}
	memextract.SortRules(rules)
	return rules, nil
}

// mkdirLocked 是 Mkdir 的无锁版本（供已持锁的调用方使用）。
func (s *Store) mkdirLocked(uri memoryvfs.URI, description string) (memoryvfs.Entry, error) {
	if _, exists := s.entries[uri]; exists {
		return memoryvfs.Entry{}, fmt.Errorf("%w: %s is an entry", memoryvfs.ErrExists, uri)
	}
	if err := s.checkDirChainLocked(uri); err != nil {
		return memoryvfs.Entry{}, err
	}
	entry, exists := s.dirs[uri]
	if !exists {
		entry = memoryvfs.Entry{URI: uri, Kind: memoryvfs.KindDir, Title: dirTitle(uri)}
	}
	entry.Kind = memoryvfs.KindDir
	if entry.Tags == nil {
		entry.Tags = map[string]string{}
	}
	if trimmed := strings.TrimSpace(description); trimmed != "" {
		summary, _ := memoryvfs.TruncateBytes(memoryvfs.CollapseWhitespace(trimmed), memoryvfs.MaxSummaryBytes)
		entry.Summary = summary
		delete(entry.Tags, autoDirTag)
	} else if strings.TrimSpace(entry.Summary) == "" {
		entry.Tags[autoDirTag] = "1"
	}
	s.dirs[uri] = entry
	if err := s.refreshDirLocked(uri); err != nil {
		return memoryvfs.Entry{}, err
	}
	if err := s.touchParentsLocked(uri); err != nil {
		return memoryvfs.Entry{}, err
	}
	return s.statLocked(uri)
}
