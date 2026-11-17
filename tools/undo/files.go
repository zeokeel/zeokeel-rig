package undo

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
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

// currentFileState 是"当前状态"里一个文件的信息。小文件会带内容（用于哈希与行数），
// 大文件只带元数据。
type currentFileState struct {
	path       string
	size       int64
	mode       fs.FileMode
	modTime    time.Time
	content    []byte
	hasContent bool
	// hash/hasHash 是"没有缓冲内容"时的替代比较依据：流式算出内容哈希，
	// 这样即使超出缓冲预算也能**准确**判断是否改动，而不是一律按"已改动"处理。
	hash    [sha256.Size]byte
	hasHash bool
}

// scanCurrent 用与 Take 相同的忽略规则遍历当前工作区。
//
// 内存边界：与 Take 一样受 Options.MaxTotalBytes 约束。超出预算的文件不再缓冲
// 内容，改为流式算哈希（32KiB 固定缓冲）——否则 Changes/Revert 的内存会随
// **当前**工作区线性增长，而"撤销"往往正是发生在工作区刚被搞大的时候。
func (s *Snapshot) scanCurrent() (map[string]*currentFileState, error) {
	files := make(map[string]*currentFileState)
	var buffered int64
	walkErr := filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("undo: walk %q: %w", path, err)
		}
		rel, relErr := filepath.Rel(s.root, path)
		if relErr != nil {
			return fmt.Errorf("undo: locate %q: %w", path, relErr)
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if s.options.ignored(rel, entry.IsDir()) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasPrefix(entry.Name(), tempPrefix) {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("undo: inspect %q: %w", path, infoErr)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		state := &currentFileState{
			path:    path,
			size:    info.Size(),
			mode:    info.Mode() & modeBits,
			modTime: info.ModTime(),
		}
		switch {
		case info.Size() > s.options.MaxFileBytes:
			// 超过单文件上限：只看大小/时间，不读内容。
		case buffered+info.Size() <= s.options.MaxTotalBytes:
			if data, readErr := os.ReadFile(path); readErr == nil {
				state.content = data
				state.hasContent = true
				buffered += int64(len(data))
			}
		default:
			// 缓冲预算用尽：流式哈希（不占额外内存）。
			if sum, hashErr := hashFile(path); hashErr == nil {
				state.hash = sum
				state.hasHash = true
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

// countCurrentLines 统计一个当前文件的行数：有内容时直接数，大文件流式数。
func countCurrentLines(state *currentFileState) (int, error) {
	if state == nil {
		return 0, nil
	}
	if state.hasContent {
		return lineCount(state.content), nil
	}
	return countFileLines(state.path)
}

// diffCurrentAgainstSnapshot 在大小不同（因此可能读不进内存）时计算差异行数。
func diffCurrentAgainstSnapshot(snapshotEntry *entry, currentFile *currentFileState) (added, removed int, err error) {
	removed = lineCount(snapshotEntry.content)
	if currentFile.hasContent {
		added, removed = diffLines(snapshotEntry.content, currentFile.content)
		return added, removed, nil
	}
	added, err = countFileLines(currentFile.path)
	if err != nil {
		return 0, 0, err
	}
	return added, removed, nil
}

// readPreImage 读取一个路径在回退前的状态，供失败回滚使用。
func readPreImage(path string) (content []byte, mode fs.FileMode, exists bool, err error) {
	info, statErr := os.Lstat(path)
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return nil, 0, false, nil
		}
		return nil, 0, false, statErr
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return nil, 0, false, readErr
	}
	return data, info.Mode() & modeBits, true, nil
}

// statMode 只读一个路径的权限位（不读内容），供"仅权限变更"的回退使用。
func statMode(path string) (fs.FileMode, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return info.Mode() & modeBits, true, nil
}

// writeFileAtomic 用"同目录临时文件 + rename"写入内容，避免出现半写文件。
//
// 先创建父目录（回退被删除的文件时可能整棵子树都不在了）；临时文件名带 tempPrefix，
// 因此即使进程崩溃残留，也不会被下一次快照当成工作区内容。
func writeFileAtomic(path string, content []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if mode == 0 {
		mode = 0o644
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, path)
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

// countFileLines 流式统计文件行数（用于超过内存上限的大文件）。
func countFileLines(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("undo: count lines of %q: %w", path, err)
	}
	defer file.Close()

	count := 0
	pending := false
	buffer := make([]byte, 32<<10)
	for {
		read, readErr := file.Read(buffer)
		for index := 0; index < read; index++ {
			if buffer[index] == '\n' {
				count++
				pending = false
				continue
			}
			pending = true
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return 0, fmt.Errorf("undo: count lines of %q: %w", path, readErr)
		}
	}
	if pending {
		count++
	}
	return count, nil
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

// hashFile 流式算一个文件的 sha256（固定 32KiB 缓冲，不随文件大小增长内存）。
func hashFile(path string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	file, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	if _, err := io.CopyBuffer(hasher, file, make([]byte, 32<<10)); err != nil {
		return sum, err
	}
	copy(sum[:], hasher.Sum(nil))
	return sum, nil
}
