package undo

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/fsguard"
)

// modeBits 是回退要保留的权限位：普通权限 + setuid/setgid/sticky。
//
// 只取 Perm() 会静默丢掉 setuid/setgid/sticky，回退之后可执行文件的语义就变了。
const modeBits = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// 不备份内容的统一原因前缀。模型与测试都靠它识别"只能报告、无法回退"的变更。
const (
	skipReasonBinary = "not-backed-up: binary file (contains NUL)"
)

// skipReasonTooLarge 描述超过单文件上限的文件。
func skipReasonTooLarge(size, limit int64) string {
	return fmt.Sprintf("not-backed-up: file is %d bytes (limit %d)", size, limit)
}

// skipReasonBudget 描述超出总备份预算的文件。
func skipReasonBudget(have, need, limit int64) string {
	return fmt.Sprintf("not-backed-up: total backup budget %d bytes would be exceeded (%d + %d > %d)", limit, have, need, limit)
}

// skipReasonReadError 描述读不出来的文件。
func skipReasonReadError(detail string) string {
	return "not-backed-up: read error: " + detail
}

// containsNUL 报告内容是否像二进制（含 NUL 字节）。
func containsNUL(content []byte) bool {
	for _, b := range content {
		if b == 0 {
			return true
		}
	}
	return false
}

// currentFileState 是"当前状态"里一个路径的信息。小文件会带内容（用于哈希与行数），
// 大文件只带元数据与流式哈希。
type currentFileState struct {
	relative string
	kind     entryKind
	size     int64
	mode     fs.FileMode
	modTime  time.Time
	content  []byte
	// hasContent 表示 content 有效；hasHash 表示"没有缓冲内容"时的替代比较依据：
	// 流式算出内容哈希，这样即使超出缓冲预算也能**准确**判断是否改动。
	hasContent bool
	hash       [sha256.Size]byte
	hasHash    bool
	// linkTarget 是符号链接的原始目标文本（kind == kindSymlink 时有效）。
	linkTarget string
	// dev/ino 是 inode 身份（硬链接识别；非 Unix 平台为 false）。
	dev, ino uint64
	hasInode bool
}

// rel 返回相对工作区根的斜杠路径。
func (c *currentFileState) rel() string {
	if c == nil {
		return ""
	}
	return c.relative
}

// scanCurrent 用与 Take 相同的忽略规则遍历当前工作区。
//
// 内存边界：与 Take 一样受 Options.MaxTotalBytes 约束。超出预算的文件不再缓冲
// 内容，改为经根句柄做有界读取后算哈希——否则 Changes/Revert 的内存会随**当前**
// 工作区线性增长，而"撤销"往往正是发生在工作区刚被搞大的时候。
func (s *Snapshot) scanCurrent() (map[string]*currentFileState, error) {
	handle, err := s.openRootIfSnapshot()
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Close() }()
	return s.scanCurrentWith(handle)
}

// scanCurrentWith 是 scanCurrent 的句柄版。
func (s *Snapshot) scanCurrentWith(handle fileRoot) (map[string]*currentFileState, error) {
	files := make(map[string]*currentFileState)
	var buffered int64
	walkErr := filepath.WalkDir(s.root, func(absPath string, dirEntry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("undo: walk %q: %w", absPath, walkErr)
		}
		rel, relErr := filepath.Rel(s.root, absPath)
		if relErr != nil {
			return fmt.Errorf("undo: locate %q: %w", absPath, relErr)
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if s.options.ignored(rel, dirEntry.IsDir()) {
			if dirEntry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if dirEntry.IsDir() || isTempName(dirEntry.Name()) {
			return nil
		}
		info, infoErr := handle.Lstat(rel)
		if infoErr != nil {
			if errNotExist(infoErr) {
				return nil
			}
			return fmt.Errorf("undo: inspect %q: %w", rel, infoErr)
		}
		state := &currentFileState{
			relative: rel,
			size:     info.Size(),
			mode:     info.Mode() & modeBits,
			modTime:  info.ModTime(),
		}
		state.dev, state.ino, state.hasInode = inodeOf(info)
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			state.kind = kindSymlink
			if target, linkErr := handle.Readlink(rel); linkErr == nil {
				state.linkTarget = target
			}
		case info.Mode().IsRegular():
			state.kind = kindFile
		default:
			// 设备/管道/套接字：与 Take 一致，不跟踪。
			return nil
		}
		if state.kind == kindFile {
			switch {
			case info.Size() > s.options.MaxFileBytes:
				// 超过单文件上限：只看大小/时间，不读内容。
			case buffered+info.Size() <= s.options.MaxTotalBytes:
				if data, readErr := handle.ReadFile(rel); readErr == nil {
					state.content = data
					state.hasContent = true
					buffered += int64(len(data))
				}
			default:
				// 缓冲预算用尽：经根句柄有界读后算哈希（不长期占内存）。
				if sum, hashErr := hashRel(handle, rel, s.options.MaxFileBytes); hashErr == nil {
					state.hash = sum
					state.hasHash = true
				}
			}
		}
		files[rel] = state
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return files, nil
}

// countCurrentLines 统计一个当前路径的行数：有内容时直接数，大文件经根句柄有界读。
func countCurrentLines(handle fileRoot, maxBytes int64, state *currentFileState) (int, error) {
	if state == nil {
		return 0, nil
	}
	if state.hasContent {
		return lineCount(state.content), nil
	}
	if state.kind != kindFile {
		return 0, nil
	}
	return countLinesRel(handle, state.relative, maxBytes)
}

// diffCurrentAgainstSnapshot 在大小不同（因此可能读不进内存）时计算差异行数。
func diffCurrentAgainstSnapshot(currentFile *currentFileState, snapshotEntry *entry) (added, removed int, err error) {
	removed = lineCount(snapshotEntry.content)
	if currentFile.hasContent {
		added, removed = diffLines(snapshotEntry.content, currentFile.content)
		return added, removed, nil
	}
	// 没有缓冲内容：只能按"整体替换"近似（精确行级 diff 需要两侧全文）。
	return 0, removed, nil
}

// lineCount 统计内存内容里的行数：按 '\n' 分段，末尾换行不额外算一行。
func lineCount(content []byte) int {
	count := 0
	pending := false
	for _, b := range content {
		if b == '\n' {
			count++
			pending = false
			continue
		}
		pending = true
	}
	if pending {
		count++
	}
	return count
}

// diffLines 返回把 before 变成 after 的增删行数（基于行级 LCS）。
//
// 超大文件（行数乘积超过 maxDiffCells）退化为"整体替换"的近似：不做 O(n*m) 的
// LCS，避免一次回退吃掉几百 MB 内存。这只会让 +/- 数字更悲观，不影响回退内容。
func diffLines(before, after []byte) (added, removed int) {
	oldLines := splitLines(before)
	newLines := splitLines(after)
	common := lcsLength(oldLines, newLines)
	return len(newLines) - common, len(oldLines) - common
}

// maxDiffCells 限制 LCS 计算量（约 2000×2000 行）。
const maxDiffCells = 4_000_000

// splitLines 按 '\n' 切行，丢弃末尾的空段（末尾换行不算多一行）。
func splitLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	lines := strings.Split(string(content), "\n")
	if last := len(lines) - 1; last >= 0 && lines[last] == "" {
		lines = lines[:last]
	}
	return lines
}

// lcsLength 返回两个行切片的 LCS 长度（两行 DP，内存 O(min(n,m))）。
func lcsLength(before, after []string) int {
	if len(before) == 0 || len(after) == 0 {
		return 0
	}
	if len(before)*len(after) > maxDiffCells {
		// 近似：只统计"完全同序同位置"的行，其余按替换处理。
		common := 0
		limit := len(before)
		if len(after) < limit {
			limit = len(after)
		}
		for index := 0; index < limit; index++ {
			if before[index] == after[index] {
				common++
			}
		}
		return common
	}
	previous := make([]int, len(after)+1)
	current := make([]int, len(after)+1)
	for i := 1; i <= len(before); i++ {
		current[0] = 0
		for j := 1; j <= len(after); j++ {
			switch {
			case before[i-1] == after[j-1]:
				current[j] = previous[j-1] + 1
			case previous[j] >= current[j-1]:
				current[j] = previous[j]
			default:
				current[j] = current[j-1]
			}
		}
		previous, current = current, previous
	}
	return previous[len(after)]
}

// 编译期断言：fsguard 错误分类仍可被 undo 识别（避免依赖被静默改写）。
var _ = fsguard.IsEscape
