package memvfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zeokeel/zeokeel-rig/internal/diffpreview"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// Snapshot 把当前 entries+dirs 的清单写到 snapshots/<id>.json。
func (s *Store) Snapshot(ctx context.Context, label string) (memoryvfs.Snapshot, error) {
	if err := ctxErr(ctx); err != nil {
		return memoryvfs.Snapshot{}, err
	}
	if err := s.lock(); err != nil {
		return memoryvfs.Snapshot{}, err
	}
	defer s.mu.Unlock()

	entries := make([]memoryvfs.Entry, 0, len(s.entries))
	for _, uri := range s.memoryURIsLocked() {
		entries = append(entries, s.entries[uri].Clone())
	}
	dirs := make([]memoryvfs.Entry, 0, len(s.dirs))
	for uri, entry := range s.dirs {
		if uri == memoryvfs.RootURI {
			continue
		}
		dirs = append(dirs, entry.Clone())
	}
	sort.Slice(dirs, func(a, b int) bool { return dirs[a].URI < dirs[b].URI })

	now := s.now()
	id := fmt.Sprintf("%d-%s", now.UnixNano(), randomHex(4))
	stored := storedSnapshot{
		Snapshot: memoryvfs.Snapshot{
			ID:      id,
			Label:   strings.TrimSpace(label),
			Created: now,
			Entries: len(entries),
		},
		Entries: entries,
		Dirs:    dirs,
	}
	data, err := json.Marshal(stored)
	if err != nil {
		return memoryvfs.Snapshot{}, fmt.Errorf("memvfs: encode snapshot: %w", err)
	}
	stored.Snapshot.Bytes = int64(len(data))

	if s.dir == "" {
		s.snapshots[id] = stored
	} else {
		path := filepath.Join(s.dir, snapshotsDirName, id+".json")
		if err := writeFileAtomic(path, data); err != nil {
			return memoryvfs.Snapshot{}, err
		}
	}
	return stored.Snapshot, nil
}

// Snapshots 列出全部快照（按创建时间、ID 升序）。
func (s *Store) Snapshots(ctx context.Context) ([]memoryvfs.Snapshot, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if err := s.rlock(); err != nil {
		return nil, err
	}
	defer s.mu.RUnlock()
	var out []memoryvfs.Snapshot
	if s.dir == "" {
		for _, stored := range s.snapshots {
			out = append(out, stored.Snapshot)
		}
	} else {
		dir := filepath.Join(s.dir, snapshotsDirName)
		names, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("memvfs: list snapshots: %w", err)
		}
		for _, name := range names {
			if name.IsDir() || !strings.HasSuffix(name.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(name.Name(), ".json")
			stored, err := s.readSnapshotFileLocked(id)
			if err != nil {
				continue
			}
			out = append(out, stored.Snapshot)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].Created.Equal(out[b].Created) {
			return out[a].Created.Before(out[b].Created)
		}
		return out[a].ID < out[b].ID
	})
	return out, nil
}

// Diff 返回两个快照之间条目清单的统一 diff。
func (s *Store) Diff(ctx context.Context, from, to string) (string, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	if err := s.rlock(); err != nil {
		return "", err
	}
	defer s.mu.RUnlock()
	before, err := s.loadSnapshotLocked(from)
	if err != nil {
		return "", err
	}
	after, err := s.loadSnapshotLocked(to)
	if err != nil {
		return "", err
	}
	oldEntries := snapshotEntriesMap(before.Entries)
	newEntries := snapshotEntriesMap(after.Entries)
	oldDirs := snapshotEntriesMap(before.Dirs)
	newDirs := snapshotEntriesMap(after.Dirs)

	uris := make([]string, 0, len(oldEntries)+len(newEntries)+len(oldDirs)+len(newDirs))
	seen := make(map[string]struct{})
	for _, set := range []map[string]memoryvfs.Entry{oldEntries, newEntries, oldDirs, newDirs} {
		for raw := range set {
			if _, ok := seen[raw]; ok {
				continue
			}
			seen[raw] = struct{}{}
			uris = append(uris, raw)
		}
	}
	sort.Strings(uris)

	var builder strings.Builder
	for _, raw := range uris {
		oldEntry, hasOld := oldEntries[raw]
		if !hasOld {
			oldEntry, hasOld = oldDirs[raw]
		}
		newEntry, hasNew := newEntries[raw]
		if !hasNew {
			newEntry, hasNew = newDirs[raw]
		}
		if hasOld && hasNew && entriesEqual(oldEntry, newEntry) {
			continue
		}
		oldJSON, err := json.MarshalIndent(oldEntry, "", "  ")
		if err != nil {
			return "", fmt.Errorf("memvfs: encode snapshot entry %s: %w", raw, err)
		}
		newJSON, err := json.MarshalIndent(newEntry, "", "  ")
		if err != nil {
			return "", fmt.Errorf("memvfs: encode snapshot entry %s: %w", raw, err)
		}
		var oldBytes, newBytes []byte
		if hasOld {
			oldBytes = oldJSON
		}
		if hasNew {
			newBytes = newJSON
		}
		preview, err := diffpreview.Preview(raw, oldBytes, newBytes)
		if err != nil {
			return "", fmt.Errorf("memvfs: diff %s: %w", raw, err)
		}
		if preview == "" {
			continue
		}
		builder.WriteString(preview)
		if !strings.HasSuffix(preview, "\n") {
			builder.WriteString("\n")
		}
	}
	return builder.String(), nil
}

// Restore 读快照并原子替换当前状态（内存与磁盘）。
func (s *Store) Restore(ctx context.Context, id string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := s.lock(); err != nil {
		return err
	}
	stored, err := s.loadSnapshotLocked(id)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	for _, entry := range stored.Entries {
		if err := memoryvfs.ValidateEntry(entry); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("%w: snapshot %s has an invalid entry %s: %v", memoryvfs.ErrCorrupt, id, entry.URI, err)
		}
	}
	previousVectors := s.vectors
	if err := s.replaceStateLocked(stored); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	// Restore 会清空向量缓存；启用 Embedder 时在锁外重算（Rebuild 自己拿锁）。
	if s.opts.Embedder != nil && s.dimension > 0 {
		if err := s.Rebuild(ctx); err != nil {
			// 重算失败至少保留"恢复后仍存在的条目"的旧向量，别把已有向量清空。
			if lockErr := s.lock(); lockErr == nil {
				kept := make(map[memoryvfs.URI][]float32)
				for uri, vector := range previousVectors {
					if _, ok := s.entries[uri]; ok {
						kept[uri] = vector
					}
				}
				s.vectors = kept
				s.buildIndex(kept)
				if s.dir != "" {
					_ = s.writeVectorsLocked()
				}
				s.mu.Unlock()
			}
			return fmt.Errorf("memvfs: restore %s: rebuild vectors: %w", id, err)
		}
	}
	return nil
}

func (s *Store) loadSnapshotLocked(id string) (storedSnapshot, error) {
	clean := strings.TrimSpace(id)
	if clean == "" || clean != filepath.Base(clean) || strings.ContainsAny(clean, `/\`) || clean == "." || clean == ".." {
		return storedSnapshot{}, fmt.Errorf("%w: snapshot %q", memoryvfs.ErrNotFound, id)
	}
	if s.dir == "" {
		stored, ok := s.snapshots[clean]
		if !ok {
			return storedSnapshot{}, fmt.Errorf("%w: snapshot %q", memoryvfs.ErrNotFound, id)
		}
		return stored, nil
	}
	return s.readSnapshotFileLocked(clean)
}

func (s *Store) readSnapshotFileLocked(id string) (storedSnapshot, error) {
	path := filepath.Join(s.dir, snapshotsDirName, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return storedSnapshot{}, fmt.Errorf("%w: snapshot %q", memoryvfs.ErrNotFound, id)
		}
		return storedSnapshot{}, fmt.Errorf("memvfs: read snapshot %q: %w", id, err)
	}
	var stored storedSnapshot
	if err := json.Unmarshal(data, &stored); err != nil {
		return storedSnapshot{}, fmt.Errorf("%w: snapshot %q: %v", memoryvfs.ErrCorrupt, id, err)
	}
	if stored.Snapshot.ID == "" {
		return storedSnapshot{}, fmt.Errorf("%w: snapshot %q has no id", memoryvfs.ErrCorrupt, id)
	}
	if stored.Snapshot.Bytes == 0 {
		stored.Snapshot.Bytes = int64(len(data))
	}
	return stored, nil
}

// replaceStateLocked 用快照状态替换当前状态：先写 staging 目录，再原子换名。
func (s *Store) replaceStateLocked(snapshot storedSnapshot) error {
	entries := make(map[memoryvfs.URI]memoryvfs.Entry, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		uri, err := memoryvfs.ParseURI(string(entry.URI))
		if err != nil {
			return fmt.Errorf("%w: snapshot entry %q: %v", memoryvfs.ErrCorrupt, entry.URI, err)
		}
		entry.URI = uri
		entries[uri] = entry.Clone()
	}
	dirs := make(map[memoryvfs.URI]memoryvfs.Entry, len(snapshot.Dirs))
	for _, dir := range snapshot.Dirs {
		uri, err := memoryvfs.ParseURI(string(dir.URI))
		if err != nil {
			return fmt.Errorf("%w: snapshot dir %q: %v", memoryvfs.ErrCorrupt, dir.URI, err)
		}
		dir.URI = uri
		dir.Kind = memoryvfs.KindDir
		dirs[uri] = dir.Clone()
	}

	previousEntries, previousDirs := s.entries, s.dirs
	s.entries, s.dirs = entries, dirs
	s.ensureImplicitDirs()

	if s.dir != "" {
		if err := s.writeLayoutLocked(); err != nil {
			s.entries, s.dirs = previousEntries, previousDirs
			return err
		}
	}
	s.vectors = make(map[memoryvfs.URI][]float32)
	s.buildIndex(nil)
	if s.dir != "" {
		if err := s.writeVectorsLocked(); err != nil {
			return err
		}
		if err := s.rebuildDirSidecarsLocked(); err != nil {
			return err
		}
	}
	return nil
}

// writeLayoutLocked 把当前内存状态整体写到磁盘：staging 目录 + 原子换名。
func (s *Store) writeLayoutLocked() error {
	if s.dir == "" {
		return nil
	}
	stagingEntries := filepath.Join(s.dir, entriesDirName+".new")
	stagingDirs := filepath.Join(s.dir, dirsDirName+".new")
	if err := os.RemoveAll(stagingEntries); err != nil {
		return fmt.Errorf("memvfs: clear staging: %w", err)
	}
	if err := os.RemoveAll(stagingDirs); err != nil {
		return fmt.Errorf("memvfs: clear staging: %w", err)
	}
	if err := os.MkdirAll(stagingEntries, layoutPerm); err != nil {
		return fmt.Errorf("memvfs: create staging: %w", err)
	}
	if err := os.MkdirAll(stagingDirs, layoutPerm); err != nil {
		return fmt.Errorf("memvfs: create staging: %w", err)
	}
	for uri, entry := range s.entries {
		path, err := resolvePathIn(stagingEntries, "", uri, ".json")
		if err != nil {
			return err
		}
		if err := writeJSONAtomic(path, entry); err != nil {
			return err
		}
	}
	for uri, dir := range s.dirs {
		if uri == memoryvfs.RootURI {
			continue
		}
		path, err := resolvePathIn(stagingDirs, "", uri, ".json")
		if err != nil {
			return err
		}
		if err := writeJSONAtomic(path, dir); err != nil {
			return err
		}
	}
	for _, pair := range [][2]string{{entriesDirName, stagingEntries}, {dirsDirName, stagingDirs}} {
		live := filepath.Join(s.dir, pair[0])
		staging := pair[1]
		old := live + ".old"
		if err := os.RemoveAll(old); err != nil {
			return fmt.Errorf("memvfs: clear %s: %w", old, err)
		}
		if err := os.Rename(live, old); err != nil {
			return fmt.Errorf("memvfs: swap %s: %w", live, err)
		}
		if err := os.Rename(staging, live); err != nil {
			_ = os.Rename(old, live)
			return fmt.Errorf("memvfs: swap %s: %w", live, err)
		}
		// 不直接删除旧目录：里面有"加载时被跳过"的损坏/不可读文件，直接删会把
		// 可人工抢救的数据变成永久丢失。移到 trash/ 下，由 GC 按保留策略回收。
		trashRoot := filepath.Join(s.dir, trashDirName)
		if err := os.MkdirAll(trashRoot, layoutPerm); err != nil {
			return fmt.Errorf("memvfs: create trash: %w", err)
		}
		trashDir, err := os.MkdirTemp(trashRoot, "restore-"+pair[0]+"-")
		if err != nil {
			return fmt.Errorf("memvfs: create restore trash: %w", err)
		}
		trashOld := filepath.Join(trashDir, "old")
		if err := os.Rename(old, trashOld); err != nil {
			// 移不动就保留 .old（下次 Open 会记诊断），不要 RemoveAll。
			s.issues = append(s.issues, fmt.Errorf("memvfs: keep old %s after restore: %w", filepath.Base(old), err))
		}
		if err := syncDir(s.dir); err != nil {
			return err
		}
	}
	return nil
}

func snapshotEntriesMap(entries []memoryvfs.Entry) map[string]memoryvfs.Entry {
	out := make(map[string]memoryvfs.Entry, len(entries))
	for _, entry := range entries {
		out[string(entry.URI)] = entry
	}
	return out
}

func entriesEqual(a, b memoryvfs.Entry) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

// memoryURIsLocked 是 memoryURIs 的无锁版本。
func (s *Store) memoryURIsLocked() []memoryvfs.URI {
	uris := make([]memoryvfs.URI, 0, len(s.entries))
	for uri := range s.entries {
		uris = append(uris, uri)
	}
	sort.Slice(uris, func(a, b int) bool { return uris[a] < uris[b] })
	return uris
}
