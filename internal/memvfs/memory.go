package memvfs

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig/internal/memextract"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// rememberCandidates 是相似度查找时看的候选条数上限。
const rememberCandidates = 5

// Remember 做记忆写入：校验 → 同 scope 同 type 找相似条目 → 合并/去重/新建。
//
//   - 相似度 ≥ 0.90：视为重复，不写盘、返回已有条目；
//   - 0.65 ≤ 相似度 < 0.90：合并（追加正文、合并 Tags、Updated 前进、Version+1）；
//   - 其余：新建。
func (s *Store) Remember(ctx context.Context, entry memoryvfs.Entry) (memoryvfs.Entry, error) {
	if err := ctxErr(ctx); err != nil {
		return memoryvfs.Entry{}, err
	}
	// 先在锁外补 L0/L1 与向量（可能调用宿主 Summarizer/Embedder），避免回调重入。
	entry = s.prepareEntry(ctx, entry)
	vector := s.embedEntry(ctx, entry)
	if err := s.lock(); err != nil {
		return memoryvfs.Entry{}, err
	}
	written, _, err := s.rememberLocked(ctx, entry, vector)
	s.mu.Unlock()
	if err != nil {
		return memoryvfs.Entry{}, err
	}
	// 合并分支的正文变了，向量必须重算；放到锁外调用宿主 Embedder。
	if s.opts.Embedder != nil {
		mergedVector := s.embedEntry(ctx, written)
		if err := s.lock(); err != nil {
			return memoryvfs.Entry{}, err
		}
		s.vectors[written.URI] = mergedVector
		if s.dir != "" {
			err = s.writeVectorsLocked()
		}
		s.mu.Unlock()
		if err != nil {
			return memoryvfs.Entry{}, err
		}
	}
	return written, nil
}

// rememberLocked 返回（写入/命中的条目，是否新建）。
func (s *Store) rememberLocked(ctx context.Context, entry memoryvfs.Entry, vector []float32) (memoryvfs.Entry, bool, error) {
	uri, err := memoryvfs.ParseURI(string(entry.URI))
	if err != nil {
		return memoryvfs.Entry{}, false, err
	}
	entry.URI = uri
	if entry.Kind == "" {
		entry.Kind = memoryvfs.KindMemory
	}
	normalized := memoryvfs.NormalizeEntry(entry, s.opts.Now)
	if err := memoryvfs.ValidateEntry(normalized); err != nil {
		return memoryvfs.Entry{}, false, err
	}

	if best, ok := s.bestMatchLocked(normalized); ok {
		outcome := memextract.Merge(best, candidateFromEntry(normalized))
		if outcome.Merge {
			if !outcome.Append {
				return best.Clone(), false, nil
			}
			merged := outcome.Entry
			merged.URI = best.URI
			merged.Kind = best.Kind
			merged.Type = best.Type
			merged.Title = best.Title
			merged.Created = best.Created
			merged.Version = best.Version + 1
			merged.Updated = s.now()
			if strings.TrimSpace(merged.Summary) == "" {
				merged.Summary = best.Summary
			}
			merged.Tokens = merged.TokenEstimate(memoryvfs.Level2)
			if err := memoryvfs.ValidateEntry(merged); err != nil {
				return memoryvfs.Entry{}, false, err
			}
			if err := s.checkQuotaLocked(merged, true); err != nil {
				return memoryvfs.Entry{}, false, err
			}
			if err := s.persistEntryLocked(merged); err != nil {
				return memoryvfs.Entry{}, false, err
			}
			stored := merged.Clone()
			s.entries[merged.URI] = stored
			s.index.Put(stored)
			if err := s.touchParentsLocked(merged.URI); err != nil {
				return memoryvfs.Entry{}, false, err
			}
			return stored.Clone(), false, nil
		}
	}

	created, err := s.writeLockedPrepared(ctx, normalized, vector)
	if err != nil {
		return memoryvfs.Entry{}, false, err
	}
	return created, true, nil
}

// bestMatchLocked 在同一 scope、同一 type 下取相似度最高的已有条目。
func (s *Store) bestMatchLocked(entry memoryvfs.Entry) (memoryvfs.Entry, bool) {
	query := memoryvfs.Query{
		Text:  entry.Summary + " " + entry.Content,
		Scope: entry.URI.Parent(),
		Kinds: []memoryvfs.Kind{entry.Kind},
		Limit: rememberCandidates,
	}
	if entry.Kind == memoryvfs.KindMemory {
		query.Types = []string{entry.Type}
	}
	hits := s.index.Search(query)
	if len(hits) > rememberCandidates {
		hits = hits[:rememberCandidates]
	}
	var best memoryvfs.Entry
	var bestSimilarity float64
	found := false
	for _, hit := range hits {
		existing, ok := s.entries[hit.URI]
		if !ok {
			continue
		}
		similarity := memextract.Similarity(
			existing.Summary+"\n"+existing.Content,
			entry.Summary+"\n"+entry.Content,
		)
		if hit.URI == entry.URI {
			similarity = 1
		}
		if !found || similarity > bestSimilarity {
			best, bestSimilarity, found = existing, similarity, true
		}
	}
	return best, found
}

// Forget 删除记忆条目（写 trash 快照）。
func (s *Store) Forget(ctx context.Context, uri memoryvfs.URI) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := s.lock(); err != nil {
		return err
	}
	defer s.mu.Unlock()
	parsed, err := memoryvfs.ParseURI(string(uri))
	if err != nil {
		return err
	}
	if _, ok := s.entries[parsed]; !ok {
		return fmt.Errorf("%w: %s", memoryvfs.ErrNotFound, parsed)
	}
	return s.deleteLocked(parsed)
}

func candidateFromEntry(entry memoryvfs.Entry) memoryvfs.Candidate {
	return memoryvfs.Candidate{
		Kind:    entry.Kind,
		Type:    entry.Type,
		Title:   entry.Title,
		Summary: entry.Summary,
		Content: entry.Content,
		Tags:    entry.Tags,
		Links:   entry.Links,
		Source:  entry.Source,
	}
}
