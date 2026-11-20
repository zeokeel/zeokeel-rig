package memvfs

import (
	"context"
	"fmt"
	"sort"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// Export 收集 scope 下的全部条目（scope 为零值/根表示全部），生成可迁移的 Pack。
//
// 会话归档条目也在其中（它们就是 mem://sessions/... 下的普通条目），因此导出包
// 换一台机器导入后仍然读得到历史会话。
func (s *Store) Export(ctx context.Context, scope memoryvfs.URI) (memoryvfs.Pack, error) {
	if err := ctxErr(ctx); err != nil {
		return memoryvfs.Pack{}, err
	}
	scopeURI := memoryvfs.RootURI
	if raw := string(scope); raw != "" {
		parsed, err := memoryvfs.ParseURI(raw)
		if err != nil {
			return memoryvfs.Pack{}, err
		}
		scopeURI = parsed
	}
	if err := s.rlock(); err != nil {
		return memoryvfs.Pack{}, err
	}
	defer s.mu.RUnlock()

	entries := make([]memoryvfs.Entry, 0, len(s.entries))
	for _, uri := range s.memoryURIsLocked() {
		if scopeURI != memoryvfs.RootURI && !uri.IsUnder(scopeURI) {
			continue
		}
		entries = append(entries, s.entries[uri].Clone())
	}
	// 目录描述也是记忆的一部分（L0/L1 是检索入口），不能导出后丢失。
	for uri, dir := range s.dirs {
		if scopeURI != memoryvfs.RootURI && !uri.IsUnder(scopeURI) {
			continue
		}
		entries = append(entries, dir.Clone())
	}
	pack := memoryvfs.Pack{
		Version:   packVersion,
		Generated: s.now(),
		Scope:     scopeURI,
		Entries:   entries,
	}
	sort.SliceStable(pack.Entries, func(a, b int) bool { return pack.Entries[a].URI < pack.Entries[b].URI })
	return pack, nil
}

// Import 导入一个 Pack。Overwrite=false 时跳过已存在的条目；DryRun 只统计。
func (s *Store) Import(ctx context.Context, pack memoryvfs.Pack, opts memoryvfs.ImportOptions) (memoryvfs.ImportResult, error) {
	if err := ctxErr(ctx); err != nil {
		return memoryvfs.ImportResult{}, err
	}
	if pack.Version != packVersion {
		return memoryvfs.ImportResult{}, fmt.Errorf("%w: pack version %d is not supported (want %d)", memoryvfs.ErrInvalidEntry, pack.Version, packVersion)
	}
	// 先整包校验，避免"导到一半才发现非法条目"。
	for _, entry := range pack.Entries {
		if err := memoryvfs.ValidateEntry(entry); err != nil {
			return memoryvfs.ImportResult{}, fmt.Errorf("memvfs: import %s: %w", entry.URI, err)
		}
	}
	result := memoryvfs.ImportResult{}
	for _, entry := range pack.Entries {
		parsed, err := memoryvfs.ParseURI(string(entry.URI))
		if err != nil {
			return memoryvfs.ImportResult{}, err
		}
		entry.URI = parsed
		if entry.Kind == memoryvfs.KindDir {
			if err := s.lock(); err != nil {
				return memoryvfs.ImportResult{}, err
			}
			_, dirExists := s.dirs[parsed]
			_, entryExists := s.entries[parsed]
			if entryExists {
				s.mu.Unlock()
				result.Skipped++
				continue
			}
			if dirExists {
				if !opts.Overwrite {
					s.mu.Unlock()
					result.Skipped++
					continue
				}
				result.Updated++
				if opts.DryRun {
					s.mu.Unlock()
					continue
				}
				updated := entry.Clone()
				updated.Kind = memoryvfs.KindDir
				existingDir := s.dirs[parsed]
				updated.Created = existingDir.Created
				updated.Version = existingDir.Version + 1
				if updated.Updated.IsZero() {
					updated.Updated = s.now()
				}
				s.dirs[parsed] = updated
				err := s.persistDirLocked(updated)
				if err == nil {
					err = s.touchParentsLocked(parsed)
				}
				s.mu.Unlock()
				if err != nil {
					return memoryvfs.ImportResult{}, err
				}
				continue
			}
			result.Imported++
			if opts.DryRun {
				s.mu.Unlock()
				continue
			}
			_, err := s.mkdirLocked(parsed, entry.Summary)
			s.mu.Unlock()
			if err != nil {
				return memoryvfs.ImportResult{}, err
			}
			continue
		}
		// 逐条走公开 Write：校验/Summarizer/Embedder 都在锁外，避免宿主回调重入。
		_, statErr := s.Stat(ctx, parsed)
		exists := statErr == nil
		if exists && !opts.Overwrite {
			result.Skipped++
			continue
		}
		if exists {
			result.Updated++
		} else {
			result.Imported++
		}
		if opts.DryRun {
			continue
		}
		if _, err := s.Write(ctx, entry); err != nil {
			return memoryvfs.ImportResult{}, err
		}
	}
	return result, nil
}
