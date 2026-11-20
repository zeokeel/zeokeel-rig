package memvfs

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// Stats 返回本地统计。Bytes 是条目与目录 sidecar 的 JSON 字节数之和（近似磁盘占用）。
func (s *Store) Stats(ctx context.Context) (memoryvfs.Stats, error) {
	if err := ctxErr(ctx); err != nil {
		return memoryvfs.Stats{}, err
	}
	if err := s.rlock(); err != nil {
		return memoryvfs.Stats{}, err
	}
	defer s.mu.RUnlock()

	stats := memoryvfs.Stats{
		ByKind: make(map[memoryvfs.Kind]int),
		ByType: make(map[string]int),
	}
	var bytesTotal int64
	for _, entry := range s.entries {
		stats.Entries++
		bytesTotal += int64(entrySize(entry))
		stats.Tokens += entry.Tokens
		stats.ByKind[entry.Kind]++
		if entry.Type != "" {
			stats.ByType[entry.Type]++
		}
		if stats.Oldest.IsZero() || entry.Created.Before(stats.Oldest) {
			stats.Oldest = entry.Created
		}
		if stats.Newest.IsZero() || entry.Updated.After(stats.Newest) {
			stats.Newest = entry.Updated
		}
	}
	for uri, dir := range s.dirs {
		if uri == memoryvfs.RootURI {
			continue
		}
		stats.Dirs++
		bytesTotal += int64(entrySize(dir))
	}
	stats.Bytes = bytesTotal
	stats.Vectors = len(s.vectors)
	stats.Sessions = len(s.sessionIDsLocked())
	stats.Snapshots = s.snapshotCountLocked()
	return stats, nil
}

func (s *Store) snapshotCountLocked() int {
	if s.dir == "" {
		return len(s.snapshots)
	}
	names, err := os.ReadDir(filepath.Join(s.dir, snapshotsDirName))
	if err != nil {
		return 0
	}
	count := 0
	for _, name := range names {
		if !name.IsDir() {
			count++
		}
	}
	return count
}

// GC 先按 TTL 过期，再按衰减分数淘汰到 MaxEntries/MaxBytes 之下。Pinned 条目永不
// 淘汰。DryRun 只统计，不改动任何数据。
func (s *Store) GC(ctx context.Context, opts memoryvfs.GCOptions) (memoryvfs.GCResult, error) {
	if err := ctxErr(ctx); err != nil {
		return memoryvfs.GCResult{}, err
	}
	if err := s.lock(); err != nil {
		return memoryvfs.GCResult{}, err
	}
	defer s.mu.Unlock()

	now := s.now()
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = s.opts.TTL
	}
	maxEntries := opts.MaxEntries
	if maxEntries <= 0 {
		maxEntries = s.opts.MaxEntries
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = s.opts.MaxBytes
	}

	expired := make([]memoryvfs.URI, 0, 4)
	survivors := make([]memoryvfs.Entry, 0, len(s.entries))
	for uri, entry := range s.entries {
		isExpired := false
		if entry.ExpiresAt != nil && !entry.ExpiresAt.After(now) {
			isExpired = true
		}
		if !isExpired && ttl > 0 && now.Sub(entry.Updated) > ttl {
			isExpired = true
		}
		if isExpired && entry.Pinned {
			// 钉住的条目连 TTL 都不动：用户显式要求保留。
			isExpired = false
		}
		if isExpired && entry.Kind == memoryvfs.KindSession && !opts.IncludeSessions {
			isExpired = false
		}
		if isExpired {
			expired = append(expired, uri)
			continue
		}
		survivors = append(survivors, entry)
	}

	halfLife := s.opts.DecayHalfLife
	sort.SliceStable(survivors, func(a, b int) bool {
		scoreA := decayScore(survivors[a], now, halfLife)
		scoreB := decayScore(survivors[b], now, halfLife)
		if scoreA != scoreB {
			return scoreA < scoreB
		}
		return survivors[a].URI < survivors[b].URI
	})
	// 配额只按"可淘汰集合"计：Pinned 与（默认）会话条目不计入 kept/bytes，
	// 否则会话归档会把配额撑满、GC 反而把长期记忆全部淘汰。
	// Pinned 仍占配额（只是不淘汰）；会话条目默认既不占配额也不淘汰。
	accountedEntry := func(entry memoryvfs.Entry) bool {
		return opts.IncludeSessions || entry.Kind != memoryvfs.KindSession
	}
	totalBytes := int64(0)
	accounted := 0
	pinnedAccounted := 0
	for _, entry := range survivors {
		if accountedEntry(entry) {
			totalBytes += int64(entrySize(entry))
			accounted++
			if entry.Pinned {
				pinnedAccounted++
			}
		}
	}
	evicted := make([]memoryvfs.URI, 0, 4)
	for index := 0; index < len(survivors); index++ {
		// Pinned 本身已经超过 MaxEntries 时，再淘汰非 Pinned 也到不了目标；
		// 不要为了一个不可能满足的条数把其余记忆清空。
		overCount := maxEntries > 0 && accounted > maxEntries && pinnedAccounted < maxEntries
		overBytes := maxBytes > 0 && totalBytes > maxBytes
		if !overCount && !overBytes {
			break
		}
		entry := survivors[index]
		if entry.Pinned || (entry.Kind == memoryvfs.KindSession && !opts.IncludeSessions) {
			continue
		}
		evicted = append(evicted, entry.URI)
		accounted--
		totalBytes -= int64(entrySize(entry))
	}

	removedBytes := int64(0)
	for _, uri := range expired {
		entry, ok := s.entries[uri]
		if !ok {
			continue
		}
		removedBytes += int64(entrySize(entry))
		if opts.DryRun {
			continue
		}
		if err := s.deleteLocked(uri); err != nil {
			return memoryvfs.GCResult{}, err
		}
	}
	for _, uri := range evicted {
		entry, ok := s.entries[uri]
		if !ok {
			continue
		}
		removedBytes += int64(entrySize(entry))
		if opts.DryRun {
			continue
		}
		if err := s.deleteLocked(uri); err != nil {
			return memoryvfs.GCResult{}, err
		}
	}

	if !opts.DryRun && s.dir != "" {
		// trash/ 与 snapshots/ 也会无限增长：只保留最新 N 份（按 mtime）。
		// 进程内后端（dir=="")没有磁盘目录，绝不能把 CWD 的 trash/snapshots
		// 当成自己的（filepath.Join("", "trash") == "trash"）。
		for _, spec := range []struct {
			dir  string
			keep int
		}{{trashDirName, maxTrashFiles}, {snapshotsDirName, maxSnapshotFiles}} {
			bytes, pruneErr := pruneOldestFiles(filepath.Join(s.dir, spec.dir), spec.keep)
			if pruneErr != nil {
				return memoryvfs.GCResult{}, pruneErr
			}
			removedBytes += bytes
		}
	}
	result := memoryvfs.GCResult{
		Expired:      len(expired),
		Evicted:      len(evicted),
		RemovedBytes: removedBytes,
	}
	if opts.DryRun {
		result.Kept = len(s.entries) - len(expired) - len(evicted)
	} else {
		result.Kept = len(s.entries)
	}
	if result.Kept < 0 {
		result.Kept = 0
	}
	return result, nil
}

const (
	maxTrashFiles    = 20
	maxSnapshotFiles = 20
)

// pruneOldestFiles 保留 dir 下最新的 keep 个普通文件，返回删除的字节数。
func pruneOldestFiles(dir string, keep int) (int64, error) {
	if dir == "" || keep < 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	type fileInfo struct {
		path  string
		mtime time.Time
		size  int64
		isDir bool
	}
	files := make([]fileInfo, 0, len(entries))
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		files = append(files, fileInfo{path: filepath.Join(dir, entry.Name()), mtime: info.ModTime(), size: info.Size(), isDir: entry.IsDir()})
	}
	if len(files) <= keep {
		return 0, nil
	}
	sort.SliceStable(files, func(a, b int) bool { return files[a].mtime.Before(files[b].mtime) })
	var removed int64
	for _, file := range files[:len(files)-keep] {
		var err error
		if file.isDir {
			err = os.RemoveAll(file.path)
		} else {
			err = os.Remove(file.path)
		}
		if err != nil && !os.IsNotExist(err) {
			return removed, err
		}
		removed += file.size
	}
	return removed, nil
}

// decayScore 是衰减分数：exp(-age/halfLife)，越新越大。halfLife<=0 时退化为
// "越新越大、越旧越小"（便于按年龄淘汰）。
func decayScore(entry memoryvfs.Entry, now time.Time, halfLife time.Duration) float64 {
	last := entry.Updated
	if entry.LastAccess != nil && entry.LastAccess.After(last) {
		last = *entry.LastAccess
	}
	age := now.Sub(last)
	if age < 0 {
		age = 0
	}
	if halfLife <= 0 {
		return -float64(age)
	}
	return math.Exp(-float64(age) / float64(halfLife))
}

// Rebuild 清空并重建索引；配置了 Embedder 时重算全部向量并写 index/vectors.json。
// Embedder 失败时保留旧索引与旧向量文件并返回错误（不会留下半套数据）。
func (s *Store) Rebuild(ctx context.Context) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	// 宿主 Embedder 可能在内部回读 Store：先在读锁下拍快照，锁外算向量。
	if err := s.rlock(); err != nil {
		return err
	}
	snapshot := make(map[memoryvfs.URI]memoryvfs.Entry, len(s.entries))
	for uri, entry := range s.entries {
		snapshot[uri] = entry.Clone()
	}
	s.mu.RUnlock()

	uris := make([]memoryvfs.URI, 0, len(snapshot))
	for uri := range snapshot {
		uris = append(uris, uri)
	}
	sort.Slice(uris, func(a, b int) bool { return uris[a] < uris[b] })
	var vectors map[memoryvfs.URI][]float32
	if s.opts.Embedder != nil && s.dimension > 0 {
		texts := make([]string, 0, len(uris))
		for _, uri := range uris {
			texts = append(texts, s.embedText(snapshot[uri]))
		}
		computed, err := s.opts.Embedder.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("memvfs: rebuild vectors: %w", err)
		}
		if len(computed) != len(uris) {
			return fmt.Errorf("memvfs: rebuild vectors: embedder returned %d vectors for %d entries", len(computed), len(uris))
		}
		vectors = make(map[memoryvfs.URI][]float32, len(uris))
		for index, uri := range uris {
			if len(computed[index]) != s.dimension {
				return fmt.Errorf("memvfs: rebuild vectors: entry %s has dimension %d, want %d", uri, len(computed[index]), s.dimension)
			}
			vectors[uri] = append([]float32(nil), computed[index]...)
		}
	}
	if err := s.lock(); err != nil {
		return err
	}
	defer s.mu.Unlock()
	// Rebuild 快照之后、拿到写锁之前可能有新写入：它们已经算好向量，保留。
	if vectors != nil {
		for uri := range s.entries {
			if _, ok := vectors[uri]; !ok {
				if vector, ok := s.vectors[uri]; ok {
					vectors[uri] = vector
				}
			}
		}
	}
	s.buildIndex(vectors)
	if s.opts.Embedder != nil && s.dir != "" {
		return s.writeVectorsLocked()
	}
	return nil
}

// writeVectorsLocked 把当前向量缓存写到 index/vectors.json。
func (s *Store) writeVectorsLocked() error {
	if s.dir == "" {
		return nil
	}
	file := vectorsFile{Dimension: s.dimension, Vectors: make(map[string][]float32, len(s.vectors))}
	for uri, vector := range s.vectors {
		file.Vectors[string(uri)] = vector
	}
	path := filepath.Join(s.dir, indexDirName, vectorsFileName)
	return writeJSONAtomic(path, file)
}
