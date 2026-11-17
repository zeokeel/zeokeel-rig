package filewatch

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// defaultIgnore 返回默认忽略列表（目录名，任意深度匹配）。
func defaultIgnore() []string {
	return []string{".git", "node_modules", "vendor", "dist", "build"}
}

// normalizeIgnore 归一化用户给的忽略规则：统一成斜杠、去掉 ./ 前缀与首尾斜杠。
//
// 规则两种形态：
//   - 不含 "/"：匹配任意深度的同名目录或文件（".git"、"node_modules"）；
//   - 含 "/"：按相对 root 的路径前缀匹配（"internal/gen" 命中该子树）。
func normalizeIgnore(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		rule := filepath.ToSlash(strings.TrimSpace(entry))
		rule = strings.TrimPrefix(rule, "./")
		rule = strings.Trim(rule, "/")
		if rule == "" || rule == "." {
			continue
		}
		out = append(out, rule)
	}
	return out
}

// scanner 是一次遍历 root 的执行体。它不持有任何可变状态，便于复用与单测。
type scanner struct {
	root     string
	ignore   []string
	maxFiles int // <=0 表示不限
	digest   bool
}

// scan 遍历 root，返回路径 → 状态以及"是否因 MaxFiles 触顶而提前停止"。
//
// 语义：
//   - 不跟随符号链接（WalkDir 默认），链接本身作为条目出现；
//   - Ignore 命中的目录整棵跳过；
//   - 单个条目不可读（权限等）时跳过该条目，不让整次扫描失败；
//   - 只有 root 本身不可访问才返回错误；
//   - 触顶时按 WalkDir 的字典序遍历顺序保留前 N 个条目，之后的直接放弃
//     （SkipAll）。保留集合通常稳定，但有个已知副作用：若前 N 个里删掉一个，
//     下一轮就会多纳入一个原本在范围外的条目，于是它会以 created 出现。
//     因此触顶时应把 created 事件当作"可能不准"来用（Notice 已提示过触顶）。
func (s *scanner) scan() (map[string]FileInfo, bool, error) {
	files := make(map[string]FileInfo)
	truncated := false

	err := filepath.WalkDir(s.root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if current == s.root {
				return walkErr
			}
			return nil
		}
		if current == s.root {
			return nil
		}
		rel, relErr := filepath.Rel(s.root, current)
		if relErr != nil {
			return nil
		}
		if s.ignored(rel) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil
		}
		state := FileInfo{Size: info.Size(), ModTime: info.ModTime()}
		if s.digest && info.Mode().IsRegular() {
			if sum, sumErr := DigestFile(current); sumErr == nil {
				state.Digest = sum
			}
		}
		files[current] = state
		if s.maxFiles > 0 && len(files) >= s.maxFiles {
			truncated = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return files, truncated, nil
}

// ignored 报告相对路径是否命中忽略规则。
func (s *scanner) ignored(rel string) bool {
	slashRel := filepath.ToSlash(rel)
	base := path.Base(slashRel)
	for _, rule := range s.ignore {
		if strings.Contains(rule, "/") {
			if slashRel == rule || strings.HasPrefix(slashRel, rule+"/") {
				return true
			}
			continue
		}
		if base == rule {
			return true
		}
	}
	return false
}

// digestBufferPool 复用 1MiB 读缓冲：Digest 开启时每个文件都要读一遍，
// 按文件分配会让大仓库的 GC 压力变得不必要地高。
var digestBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 1<<20)
		return &buf
	},
}

// DigestFile 返回一个**普通文件**内容的 sha256（小写 hex）。
//
// 口径与 Watcher 的 Snapshot 完全一致：符号链接与目录不是普通文件，直接返回
// 错误（Snapshot 也不会为它们填摘要）。1MiB 读缓冲并复用，适合逐路径调用。
func DigestFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("filewatch: %q 不是普通文件（%s）", path, info.Mode().Type())
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	hasher := sha256.New()
	bufPtr := digestBufferPool.Get().(*[]byte)
	defer digestBufferPool.Put(bufPtr)
	if _, err := io.CopyBuffer(hasher, file, *bufPtr); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// sameFileInfo 报告两份状态是否等价。
//
// 先比 size 再比 mtime：mtime 精度不足（秒级文件系统、被刻意恢复的 mtime）时
// size 仍能救回来；两者都可能骗人时才需要 digest。
func sameFileInfo(a, b FileInfo, digest bool) bool {
	if a.Size != b.Size {
		return false
	}
	if !a.ModTime.Equal(b.ModTime) {
		return false
	}
	if digest && a.Digest != b.Digest {
		return false
	}
	return true
}

// diffPaths 返回"已见状态"与本次扫描不同的所有路径（含新增与消失）。
func diffPaths(seen, cur map[string]FileInfo, digest bool) []string {
	out := make([]string, 0, 8)
	for path, state := range cur {
		old, ok := seen[path]
		if !ok || !sameFileInfo(old, state, digest) {
			out = append(out, path)
		}
	}
	for path := range seen {
		if _, ok := cur[path]; !ok {
			out = append(out, path)
		}
	}
	return out
}

// eventFor 把"路径在两个映射之间的差异"渲染成事件。
func eventFor(path string, seen, cur map[string]FileInfo, digest bool) (Event, bool) {
	old, existed := seen[path]
	state, exists := cur[path]
	return eventForOne(path, old, existed, state, exists, digest)
}

// eventForOne 是 eventFor 的单路径版本，供 ChangedPaths 等 O(len(paths)) 场景复用。
//
// 第二个返回值为 false 表示没有净变化（例如路径既不在旧快照里、盘上也没有）。
func eventForOne(path string, old FileInfo, existed bool, state FileInfo, exists bool, digest bool) (Event, bool) {
	switch {
	case exists && !existed:
		return Event{Path: path, Kind: KindCreated, Size: state.Size, ModTime: state.ModTime, Digest: state.Digest}, true
	case exists && existed:
		if sameFileInfo(old, state, digest) {
			return Event{}, false
		}
		return Event{Path: path, Kind: KindModified, Size: state.Size, ModTime: state.ModTime, Digest: state.Digest}, true
	case !exists && existed:
		// 已删除：保留最后已知的 size/mtime/摘要，方便调用方展示。
		return Event{Path: path, Kind: KindRemoved, Size: old.Size, ModTime: old.ModTime, Digest: old.Digest}, true
	default:
		return Event{}, false
	}
}

// diffEvents 对两份状态做全量比较，返回按字典序排序的事件。
func diffEvents(seen, cur map[string]FileInfo, digest bool) []Event {
	paths := make([]string, 0, len(seen)+len(cur))
	for path := range cur {
		paths = append(paths, path)
	}
	for path := range seen {
		if _, ok := cur[path]; !ok {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	out := make([]Event, 0, len(paths))
	for _, path := range paths {
		if ev, ok := eventFor(path, seen, cur, digest); ok {
			out = append(out, ev)
		}
	}
	return out
}

// pathState 是单路径查询的结果类别。
type pathState int

const (
	// pathAbsent 表示盘上不存在该路径。
	pathAbsent pathState = iota
	// pathDirectory 表示该路径是目录（Watcher 不跟踪目录，查询时跳过）。
	pathDirectory
	// pathEntry 表示该路径是一个可跟踪条目（普通文件、符号链接、设备等）。
	pathEntry
)

// statPath 读取单个路径的状态。
//
// 用 Lstat（不跟随符号链接），因此"改符号链接的目标"不会被当成变化——与
// Snapshot 的口径一致。wantDigest 只对普通文件生效。
func statPath(abs string, wantDigest bool) (FileInfo, pathState, error) {
	info, err := os.Lstat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return FileInfo{}, pathAbsent, nil
		}
		return FileInfo{}, pathAbsent, err
	}
	if info.IsDir() {
		return FileInfo{}, pathDirectory, nil
	}
	state := FileInfo{Size: info.Size(), ModTime: info.ModTime()}
	if wantDigest && info.Mode().IsRegular() {
		if sum, sumErr := DigestFile(abs); sumErr == nil {
			state.Digest = sum
		}
	}
	return state, pathEntry, nil
}
