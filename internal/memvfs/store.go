// Package memvfs 是 memoryvfs.Store 的默认实现：一份落到宿主状态目录的文件后端
// （entries/dirs/sessions/snapshots/trash/meta/index），以及一份语义完全一致的
// 进程内后端（OpenMemory）。
//
// # 为什么一条一文件
//
// 记忆条目之间几乎没有跨条目的写依赖，一条一文件让"写一条"变成"写一个文件"：
// 临时文件 + rename + fsync（文件与目录）保证崩溃不会留下半条记录；某一条损坏
// 也只影响它自己——Open 会跳过并记录诊断，而不是让整个记忆库打不开。
//
// # 目录 sidecar
//
// 目录没有正文，但需要 L0/L1 与 freshness（子项总数、采样数、待刷新变更数）。
// 这些数据存在 dirs/<path>.json，子项变更时父目录的 PendingChanges 递增，重新
// 打开存储（或显式刷新）时重建并清零。
//
// # 并发
//
// 所有方法并发安全：读用 RWMutex，写串行。写路径先落盘再更新内存，因此磁盘
// 出错时内存不会领先于磁盘。Close 之后所有方法返回 memoryvfs.ErrClosed。
package memvfs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/memsearch"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// 磁盘布局常量。
const (
	entriesDirName   = "entries"
	dirsDirName      = "dirs"
	sessionsDirName  = "sessions"
	snapshotsDirName = "snapshots"
	trashDirName     = "trash"
	indexDirName     = "index"
	metaFileName     = "meta.json"
	vectorsFileName  = "vectors.json"

	layoutPerm = 0o700
	filePerm   = 0o600

	metaVersion = 1
	// PackVersion 是 Pack 的当前格式版本。
	packVersion = 1

	// autoDirTag 标记"摘要是自动生成的"，刷新时可重新生成。
	autoDirTag = "auto"
	// sampleLimit 是目录 sidecar 的默认采样上限。
	sampleLimit = 32
	// sessionsPrefix 是会话归档的 URI 前缀。
	sessionsPrefix = memoryvfs.Scheme + "sessions/"
	// rulesRootURI 是规则的默认根目录。
	rulesRootURI = memoryvfs.URI(memoryvfs.Scheme + "user/self/rules")
)

// storeMeta 是 meta.json 的内容：格式版本与会话检查点。
type storeMeta struct {
	Version     int                  `json:"version"`
	Checkpoints map[string]time.Time `json:"checkpoints,omitempty"`
}

// storedSnapshot 是 snapshots/<id>.json 的内容。
type storedSnapshot struct {
	Snapshot memoryvfs.Snapshot `json:"snapshot"`
	Entries  []memoryvfs.Entry  `json:"entries"`
	Dirs     []memoryvfs.Entry  `json:"dirs"`
}

// vectorsFile 是 index/vectors.json 的内容。
type vectorsFile struct {
	Dimension int                  `json:"dimension"`
	Vectors   map[string][]float32 `json:"vectors"`
}

// Store 是文件/内存后端。零值不可用，用 Open 或 OpenMemory。
type Store struct {
	dir       string
	opts      memoryvfs.Options
	dimension int

	mu     sync.RWMutex
	closed bool

	entries map[memoryvfs.URI]memoryvfs.Entry
	dirs    map[memoryvfs.URI]memoryvfs.Entry
	index   *memsearch.Index
	vectors map[memoryvfs.URI][]float32
	meta    storeMeta

	// pending 与 snapshots 只在进程内后端使用（dir == ""）。
	pending   map[string][]memoryvfs.Message
	snapshots map[string]storedSnapshot

	issues []error
}

var _ memoryvfs.Store = (*Store)(nil)

// Open 打开（必要时创建）dir 作为存储根。目录与文件权限固定为 0700/0600。
//
// 磁盘上的个别条目损坏不会让 Open 失败：损坏文件被跳过并记录为诊断，其它条目
// 照常可用（见 LoadIssues）。
func Open(dir string, opts ...memoryvfs.Option) (*Store, error) {
	trimmed := strings.TrimSpace(dir)
	if trimmed == "" {
		return nil, errors.New("memvfs: storage directory is empty")
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, fmt.Errorf("memvfs: resolve storage directory %q: %w", dir, err)
	}
	store := buildStore(filepath.Clean(absolute), memoryvfs.ApplyOptions(opts...))
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

// OpenMemory 返回进程内后端：语义与文件后端一致，但不落盘（测试与无盘环境用）。
func OpenMemory(opts ...memoryvfs.Option) *Store {
	store := buildStore("", memoryvfs.ApplyOptions(opts...))
	store.buildIndex(nil)
	return store
}

func buildStore(dir string, options memoryvfs.Options) *Store {
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	dimension := 0
	if options.Embedder != nil {
		dimension = options.Embedder.Dimension()
	}
	return &Store{
		dir:       dir,
		opts:      options,
		dimension: dimension,
		entries:   make(map[memoryvfs.URI]memoryvfs.Entry),
		dirs:      make(map[memoryvfs.URI]memoryvfs.Entry),
		index:     memsearch.New(options.BM25Weight, options.VectorWeight, dimension),
		vectors:   make(map[memoryvfs.URI][]float32),
		meta:      storeMeta{Version: metaVersion, Checkpoints: make(map[string]time.Time)},
		pending:   make(map[string][]memoryvfs.Message),
		snapshots: make(map[string]storedSnapshot),
	}
}

// Dir 返回存储根目录；进程内后端返回空串。
func (s *Store) Dir() string { return s.dir }

// LoadIssues 返回加载期间被跳过/隔离的问题文件诊断。它不阻塞使用，只用于
// 让宿主把这些损坏暴露给用户或日志。
func (s *Store) LoadIssues() []error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]error, len(s.issues))
	copy(out, s.issues)
	return out
}

// Checkpoint 返回某个会话最后一次成功提交的时间。
func (s *Store) Checkpoint(sessionID string) (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	at, ok := s.meta.Checkpoints[sessionID]
	return at, ok
}

// Close 关闭存储。它是幂等的：重复调用返回 nil；之后的其它方法返回 ErrClosed。
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	err := s.flushLocked()
	s.mu.Unlock()
	return err
}

// flushLocked 在关闭前把派生数据（向量缓存）落盘，并顺手把 trash/snapshots
// 裁到固定份数。条目本身每次写都已 fsync。
func (s *Store) flushLocked() error {
	if s.dir == "" {
		return nil
	}
	var err error
	if s.opts.Embedder != nil {
		err = s.writeVectorsLocked()
	}
	s.pruneAuxLocked()
	return err
}

// pruneAuxLocked 只清理辅助目录（trash/snapshots），不淘汰任何记忆/会话。
func (s *Store) pruneAuxLocked() {
	if s.dir == "" {
		return
	}
	for _, spec := range []struct {
		dir  string
		keep int
	}{{trashDirName, maxTrashFiles}, {snapshotsDirName, maxSnapshotFiles}} {
		_, _ = pruneOldestFiles(filepath.Join(s.dir, spec.dir), spec.keep)
	}
}

// ---------------------------------------------------------------------------
// 加载
// ---------------------------------------------------------------------------

func (s *Store) load() error {
	if s.dir == "" {
		s.buildIndex(nil)
		return nil
	}
	s.recoverInterruptedSwap()
	for _, name := range []string{entriesDirName, dirsDirName, sessionsDirName, snapshotsDirName, trashDirName, indexDirName} {
		if err := os.MkdirAll(filepath.Join(s.dir, name), layoutPerm); err != nil {
			return fmt.Errorf("memvfs: create %s directory: %w", name, err)
		}
	}
	s.loadMeta()
	s.loadEntryFiles()
	s.loadDirFiles()
	s.ensureImplicitDirs()
	s.buildIndex(s.loadVectors())
	if err := s.rebuildDirSidecarsLocked(); err != nil {
		return err
	}
	return nil
}

// recoverInterruptedSwap 处理 Restore 两次 rename 之间崩溃留下的 <dir>.old：
// 如果 live 目录不存在，就把 .old 换回来；否则 .old 只是上一代副本，保留在
// 原地（不静默删除），并记一条诊断。
func (s *Store) recoverInterruptedSwap() {
	for _, name := range []string{entriesDirName, dirsDirName} {
		live := filepath.Join(s.dir, name)
		old := live + ".old"
		if _, err := os.Stat(live); errors.Is(err, os.ErrNotExist) {
			if _, oldErr := os.Stat(old); oldErr == nil {
				if renameErr := os.Rename(old, live); renameErr == nil {
					s.issues = append(s.issues, fmt.Errorf("memvfs: recovered interrupted restore from %s", filepath.Base(old)))
					_ = syncDir(s.dir)
					continue
				}
			}
			continue
		}
		if _, err := os.Stat(old); err == nil {
			// live 与 .old 同时存在：把 .old 移进 trash 保留（不静默删除），
			// 避免下一次 Restore 的清理把它连同"未加载的损坏条目"一起删掉。
			trashRoot := filepath.Join(s.dir, trashDirName)
			if mkErr := os.MkdirAll(trashRoot, layoutPerm); mkErr == nil {
				if dest, mkErr := os.MkdirTemp(trashRoot, "stale-"+name+"-"); mkErr == nil {
					if renameErr := os.Rename(old, filepath.Join(dest, "old")); renameErr == nil {
						s.issues = append(s.issues, fmt.Errorf("memvfs: moved stale %s to trash for manual recovery", filepath.Base(old)))
						continue
					}
				}
			}
			s.issues = append(s.issues, fmt.Errorf("memvfs: stale %s left by an interrupted restore; kept for manual recovery", filepath.Base(old)))
		}
	}
}

func (s *Store) loadMeta() {
	path := filepath.Join(s.dir, metaFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.issues = append(s.issues, fmt.Errorf("memvfs: read %s: %w", path, err))
		}
		return
	}
	var meta storeMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		s.issues = append(s.issues, fmt.Errorf("memvfs: decode %s: %w", path, err))
		return
	}
	if meta.Version == 0 {
		meta.Version = metaVersion
	}
	if meta.Checkpoints == nil {
		meta.Checkpoints = make(map[string]time.Time)
	}
	s.meta = meta
}

// layoutPathURI 由 entries/dirs 下的文件路径反推 URI，用于校验"文件名与内容
// 里的 uri 一致"。
func layoutPathURI(root, path string) (memoryvfs.URI, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	rel = strings.TrimSuffix(rel, ".json")
	rel = filepath.ToSlash(rel)
	return memoryvfs.ParseURI(memoryvfs.Scheme + rel)
}

// loadEntryFiles 逐个解码 entries/ 下的 JSON。单个文件失败只记录诊断并跳过。
func (s *Store) loadEntryFiles() {
	root := filepath.Join(s.dir, entriesDirName)
	_ = filepath.WalkDir(root, func(path string, dirent os.DirEntry, err error) error {
		if err != nil {
			s.issues = append(s.issues, fmt.Errorf("memvfs: walk %s: %w", path, err))
			return nil
		}
		if dirent.IsDir() || !strings.HasSuffix(dirent.Name(), ".json") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			s.issues = append(s.issues, fmt.Errorf("memvfs: read %s: %w", path, readErr))
			return nil
		}
		var entry memoryvfs.Entry
		if decodeErr := json.Unmarshal(data, &entry); decodeErr != nil {
			s.issues = append(s.issues, fmt.Errorf("memvfs: decode %s: %w", path, decodeErr))
			return nil
		}
		uri, uriErr := memoryvfs.ParseURI(string(entry.URI))
		if uriErr != nil {
			s.issues = append(s.issues, fmt.Errorf("memvfs: %s has invalid uri %q: %w", path, entry.URI, uriErr))
			return nil
		}
		if expected, expErr := layoutPathURI(root, path); expErr != nil || expected != uri {
			// 文件名与内容 URI 不一致时不要载入：否则 Delete 按 URI 反推的路径删不到
			// 这个文件，下次 Open 还会把它当"幽灵条目"复活。
			s.issues = append(s.issues, fmt.Errorf("memvfs: %s declares %s but path implies another uri; quarantined (not loaded)", path, uri))
			return nil
		}
		entry.URI = uri
		if entry.Kind == memoryvfs.KindDir {
			// 目录条目放错位置：当作 sidecar 处理，不丢数据。
			if _, exists := s.dirs[uri]; exists {
				s.issues = append(s.issues, fmt.Errorf("memvfs: duplicate directory entry %s; keeping the first", uri))
				return nil
			}
			s.dirs[uri] = entry
			return nil
		}
		if _, exists := s.entries[uri]; exists {
			s.issues = append(s.issues, fmt.Errorf("memvfs: duplicate entry %s; keeping the first", uri))
			return nil
		}
		s.entries[uri] = entry
		return nil
	})
}

func (s *Store) loadDirFiles() {
	root := filepath.Join(s.dir, dirsDirName)
	_ = filepath.WalkDir(root, func(path string, dirent os.DirEntry, err error) error {
		if err != nil {
			s.issues = append(s.issues, fmt.Errorf("memvfs: walk %s: %w", path, err))
			return nil
		}
		if dirent.IsDir() || !strings.HasSuffix(dirent.Name(), ".json") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			s.issues = append(s.issues, fmt.Errorf("memvfs: read %s: %w", path, readErr))
			return nil
		}
		var dir memoryvfs.Entry
		if decodeErr := json.Unmarshal(data, &dir); decodeErr != nil {
			s.issues = append(s.issues, fmt.Errorf("memvfs: decode %s: %w", path, decodeErr))
			return nil
		}
		uri, uriErr := memoryvfs.ParseURI(string(dir.URI))
		if uriErr != nil {
			s.issues = append(s.issues, fmt.Errorf("memvfs: %s has invalid uri %q: %w", path, dir.URI, uriErr))
			return nil
		}
		if expected, expErr := layoutPathURI(root, path); expErr != nil || expected != uri {
			s.issues = append(s.issues, fmt.Errorf("memvfs: %s declares %s but path implies another uri; skipped", path, uri))
			return nil
		}
		dir.URI = uri
		dir.Kind = memoryvfs.KindDir
		if _, exists := s.dirs[uri]; exists {
			s.issues = append(s.issues, fmt.Errorf("memvfs: duplicate directory sidecar %s; keeping the first", uri))
			return nil
		}
		s.dirs[uri] = dir
		return nil
	})
}

// ensureImplicitDirs 为每个条目的祖先补目录 sidecar（目录由条目隐式存在）。
func (s *Store) ensureImplicitDirs() {
	for uri := range s.entries {
		for parent := uri.Parent(); parent != memoryvfs.RootURI; parent = parent.Parent() {
			if _, isEntry := s.entries[parent]; isEntry {
				// 磁盘上出现了"条目与目录同 URI"的畸形状态：保留条目，不再造目录。
				s.issues = append(s.issues, fmt.Errorf("memvfs: %s is both an entry and an ancestor of %s", parent, uri))
				continue
			}
			if _, ok := s.dirs[parent]; ok {
				continue
			}
			s.dirs[parent] = memoryvfs.Entry{
				URI:    parent,
				Kind:   memoryvfs.KindDir,
				Title:  dirTitle(parent),
				Tags:   map[string]string{autoDirTag: "1"},
				Source: "implicit",
			}
		}
	}
	for uri := range s.dirs {
		if uri == memoryvfs.RootURI {
			continue
		}
		for parent := uri.Parent(); parent != memoryvfs.RootURI; parent = parent.Parent() {
			if _, ok := s.dirs[parent]; !ok {
				s.dirs[parent] = memoryvfs.Entry{
					URI:    parent,
					Kind:   memoryvfs.KindDir,
					Title:  dirTitle(parent),
					Tags:   map[string]string{autoDirTag: "1"},
					Source: "implicit",
				}
			}
		}
	}
}

func (s *Store) buildIndex(vectors map[memoryvfs.URI][]float32) {
	s.index = memsearch.New(s.opts.BM25Weight, s.opts.VectorWeight, s.dimension)
	for _, entry := range s.entries {
		s.index.Put(entry)
	}
	s.vectors = make(map[memoryvfs.URI][]float32, len(vectors))
	for uri, vector := range vectors {
		if len(vector) != s.dimension {
			continue
		}
		s.vectors[uri] = vector
		s.index.SetVector(uri, vector)
	}
}

func (s *Store) loadVectors() map[memoryvfs.URI][]float32 {
	if s.dir == "" || s.dimension <= 0 {
		return nil
	}
	path := filepath.Join(s.dir, indexDirName, vectorsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var file vectorsFile
	if err := json.Unmarshal(data, &file); err != nil {
		s.issues = append(s.issues, fmt.Errorf("memvfs: decode %s: %w", path, err))
		return nil
	}
	if file.Dimension != s.dimension {
		// 维度变了：旧向量整体失效，重建由 Rebuild 负责。
		return nil
	}
	out := make(map[memoryvfs.URI][]float32, len(file.Vectors))
	for raw, vector := range file.Vectors {
		uri, parseErr := memoryvfs.ParseURI(raw)
		if parseErr != nil {
			continue
		}
		out[uri] = vector
	}
	return out
}

// rebuildDirSidecarsLocked 在加载后重建所有目录 sidecar：freshness 重算，待刷新
// 变更数清零。
func (s *Store) rebuildDirSidecarsLocked() error {
	uris := make([]memoryvfs.URI, 0, len(s.dirs))
	for uri := range s.dirs {
		uris = append(uris, uri)
	}
	sort.Slice(uris, func(a, b int) bool { return uris[a] < uris[b] })
	for _, uri := range uris {
		if uri == memoryvfs.RootURI {
			continue
		}
		if err := s.refreshDirLocked(uri); err != nil {
			s.issues = append(s.issues, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 锁与工具
// ---------------------------------------------------------------------------

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func (s *Store) lock() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return memoryvfs.ErrClosed
	}
	return nil
}

func (s *Store) rlock() error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return memoryvfs.ErrClosed
	}
	return nil
}

func (s *Store) now() time.Time {
	if s.opts.Now == nil {
		return time.Now().UTC()
	}
	return s.opts.Now().UTC()
}

// randomHex 返回 n 字节的随机十六进制串；随机源失败时退化为时间戳，保证唯一性
// 优先于不可预测性（这些 ID 只用于本地文件名）。
func randomHex(n int) string {
	buffer := make([]byte, n)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

func dirTitle(uri memoryvfs.URI) string {
	if uri == memoryvfs.RootURI {
		return "root"
	}
	return uri.Base()
}

func isAutoDir(entry memoryvfs.Entry) bool {
	return entry.Tags[autoDirTag] == "1"
}

// entrySize 是条目在磁盘上的近似字节数（JSON 编码长度）。
func entrySize(entry memoryvfs.Entry) int {
	data, err := json.Marshal(entry)
	if err != nil {
		return 0
	}
	return len(data)
}
