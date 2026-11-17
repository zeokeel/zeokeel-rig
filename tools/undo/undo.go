package undo

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ToolName 是 undo 工具面向模型的稳定名称。
const ToolName = "undo"

// 默认限额。
const (
	// DefaultMaxFileBytes 是单文件备份上限（4 MiB）：更大的文件只记录元数据。
	DefaultMaxFileBytes int64 = 4 << 20
	// DefaultMaxTotalBytes 是一次快照允许备份的总字节数（64 MiB）。
	DefaultMaxTotalBytes int64 = 64 << 20
	// DefaultMaxFiles 是一次快照允许跟踪的文件数；超过时 Take 直接报错，绝不
	// 静默生成"部分快照"（部分快照会把未记录的文件误判为新增，回退时删掉）。
	DefaultMaxFiles = 20000
	// DefaultRetain 是 Tracker 保留的回合数。
	DefaultRetain = 10
	// tempPrefix 是回退写入时临时文件的前缀；遍历时忽略它们，避免崩溃残留被当成
	// 工作区内容。
	tempPrefix = ".rig-undo-"
)

// DefaultIgnore 是始终忽略的目录/文件规则。用户的 Options.Ignore 追加在这之后，
// 而不是替换它们——否则一次误配置就可能把整个 .git 目录拍进快照。
var DefaultIgnore = []string{".git", "node_modules", "vendor", "dist", "build"}

// Options 配置快照与 Tracker 的限制。零值表示全部使用默认值。
type Options struct {
	// Ignore 是追加的忽略规则：不含 "/" 的规则按任意层级的名字匹配（"cache"
	// 命中 a/cache/x），含 "/" 的规则按工作区相对路径前缀匹配（"docs/generated"）。
	// 末尾的 "/" 会被忽略。默认规则（DefaultIgnore）始终生效。
	Ignore []string
	// MaxFileBytes 是单文件备份上限；超过的文件只记录元数据，回退时报不可回退。
	MaxFileBytes int64
	// MaxTotalBytes 是一次快照允许备份的内容总字节数。
	MaxTotalBytes int64
	// MaxFiles 是允许跟踪的文件数上限。
	MaxFiles int
	// Retain 是 Tracker 保留的回合数（默认 DefaultRetain）。
	Retain int
}

// withDefaults 返回填好默认值、并已把默认忽略规则合并进 Ignore 的副本。
func (o Options) withDefaults() Options {
	filled := o
	if filled.MaxFileBytes <= 0 {
		filled.MaxFileBytes = DefaultMaxFileBytes
	}
	if filled.MaxTotalBytes <= 0 {
		filled.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if filled.MaxFiles <= 0 {
		filled.MaxFiles = DefaultMaxFiles
	}
	if filled.Retain <= 0 {
		filled.Retain = DefaultRetain
	}
	rules := make([]string, 0, len(DefaultIgnore)+len(o.Ignore))
	rules = append(rules, DefaultIgnore...)
	rules = append(rules, o.Ignore...)
	filled.Ignore = normalizeIgnore(rules)
	return filled
}

// normalizeIgnore 规范化忽略规则：去空白、统一斜杠、去掉末尾 "/" 与空项。
func normalizeIgnore(rules []string) []string {
	out := make([]string, 0, len(rules))
	for _, rule := range rules {
		rule = filepath.ToSlash(strings.TrimSpace(rule))
		rule = strings.TrimSuffix(rule, "/")
		if rule == "" || rule == "." {
			continue
		}
		out = append(out, rule)
	}
	return out
}

// ignored 报告相对路径（斜杠形式）是否命中忽略规则。
func (o Options) ignored(rel string, isDir bool) bool {
	rel = filepath.ToSlash(rel)
	for _, rule := range o.Ignore {
		if strings.Contains(rule, "/") {
			if rel == rule || strings.HasPrefix(rel, rule+"/") {
				return true
			}
			continue
		}
		// 单段规则：命中同名文件/目录，或路径中任意一层同名目录。
		if rel == rule ||
			strings.HasPrefix(rel, rule+"/") ||
			strings.HasSuffix(rel, "/"+rule) ||
			strings.Contains(rel, "/"+rule+"/") {
			return true
		}
	}
	return false
}

// Change 是一处相对快照的差异。Op 取 "add"、"modify" 或 "delete"。
//
// Added/Removed 是行数：add 时 Added 是新文件的行数；delete 时 Removed 是快照里该文件
// 的行数；modify 时按行级 LCS 计算（超大文件退化为整体行数，见 diffLines）。这些数字
// 描述的是"相对快照的差异"，也就是 Revert 将要抹平的东西。
type Change struct {
	// Path 是相对工作区根的斜杠路径。
	Path string
	// Op 是 add、modify 或 delete。
	Op string
	// Added/Removed 是相对快照的增删行数。
	Added, Removed int
	// Reverted 报告这次调用是否真的把它写回了（dry-run 与跳过的变更都是 false）。
	Reverted bool
	// Reason 非空表示该变更不可回退（例如 "not-backed-up: binary file (contains NUL)"）。
	Reason string
	// modeOnly 表示"只有权限变了，内容一字节没动"：回退只需 chmod，不必重写文件。
	modeOnly bool
}

// entry 是快照里一个文件的记录。content 为 nil 表示没有备份内容（reason 说明原因）。
type entry struct {
	size    int64
	mode    fs.FileMode
	modTime time.Time
	hash    [sha256.Size]byte
	content []byte
	reason  string
}

// Snapshot 是某一时刻的工作区状态。它由 Take 生成，之后只读（Revert 不改快照，
// 因此可以重复回退；第二次回退通常没有变更）。
type Snapshot struct {
	root    string
	options Options
	entries map[string]*entry
	files   int
	bytes   int64
}

// Root 返回规范化后的工作区根。
func (s *Snapshot) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// Files 返回快照跟踪的普通文件数（含未备份内容的文件）。
func (s *Snapshot) Files() int {
	if s == nil {
		return 0
	}
	return s.files
}

// Bytes 返回已备份内容的字节总数。
func (s *Snapshot) Bytes() int64 {
	if s == nil {
		return 0
	}
	return s.bytes
}

// Take 遍历 root 并拍一份快照。root 必须存在且是目录。
//
// 超过 Options.MaxFiles 时返回错误（不生成部分快照）；单个文件读不出来时把它记为
// "not-backed-up: read error"，继续遍历（其余文件仍可回退）。
func Take(root string, opts Options) (*Snapshot, error) {
	options := opts.withDefaults()
	resolved, err := resolveRoot(root)
	if err != nil {
		return nil, err
	}
	snapshot := &Snapshot{
		root:    resolved,
		options: options,
		entries: make(map[string]*entry),
	}
	if err := snapshot.capture(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// capture 遍历工作区并填充 entries。
func (s *Snapshot) capture() error {
	var total int64
	count := 0
	walkErr := filepath.WalkDir(s.root, func(path string, dirEntry fs.DirEntry, err error) error {
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
		if s.options.ignored(rel, dirEntry.IsDir()) {
			if dirEntry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if dirEntry.IsDir() {
			return nil
		}
		if strings.HasPrefix(dirEntry.Name(), tempPrefix) {
			return nil
		}
		info, infoErr := dirEntry.Info()
		if infoErr != nil {
			return fmt.Errorf("undo: inspect %q: %w", path, infoErr)
		}
		// 符号链接、设备、管道等一律不跟踪：回退它们要么不安全，要么没有意义。
		if !info.Mode().IsRegular() {
			return nil
		}
		if count >= s.options.MaxFiles {
			return fmt.Errorf("undo: workspace %q has more than %d files; raise Options.MaxFiles or narrow the workspace",
				s.root, s.options.MaxFiles)
		}
		count++

		item := &entry{size: info.Size(), mode: info.Mode() & modeBits, modTime: info.ModTime()}
		switch {
		case info.Size() > s.options.MaxFileBytes:
			item.reason = skipReasonTooLarge(info.Size(), s.options.MaxFileBytes)
		default:
			data, readErr := os.ReadFile(path)
			switch {
			case readErr != nil:
				item.reason = skipReasonReadError(readErr.Error())
			case containsNUL(data):
				item.reason = skipReasonBinary
			case total+int64(len(data)) > s.options.MaxTotalBytes:
				item.reason = skipReasonBudget(total, int64(len(data)), s.options.MaxTotalBytes)
			default:
				item.content = data
				item.hash = sha256.Sum256(data)
				total += int64(len(data))
			}
		}
		s.entries[rel] = item
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	s.files = len(s.entries)
	s.bytes = total
	return nil
}

// Changes 返回相对快照的全部差异（按路径排序）。无差异时返回空切片。
func (s *Snapshot) Changes() ([]Change, error) {
	return s.changes("")
}

// ChangesForPath 返回某一个路径的差异（相对工作区根，越界报错）。
func (s *Snapshot) ChangesForPath(path string) ([]Change, error) {
	return s.filteredChanges(path)
}

// Revert 把工作区恢复到快照状态，返回被处理（含跳过）的变更。
//
// 全有或全无：任何文件写入失败都会把已恢复的文件改回原状并返回错误。
func (s *Snapshot) Revert() ([]Change, error) {
	return s.revert("")
}

// RevertPath 只回退一个路径（相对工作区根）。该路径必须被快照跟踪或当前存在，
// 否则返回错误。
func (s *Snapshot) RevertPath(path string) ([]Change, error) {
	return s.revert(path)
}

// filteredChanges 校验并解析过滤路径，再对比当前状态。
func (s *Snapshot) filteredChanges(path string) ([]Change, error) {
	if s == nil {
		return nil, errors.New("undo: nil snapshot")
	}
	filter, err := s.resolvePath(path)
	if err != nil {
		return nil, err
	}
	if _, tracked := s.entries[filter]; !tracked {
		info, statErr := os.Lstat(filepath.Join(s.root, filepath.FromSlash(filter)))
		if statErr != nil {
			return nil, fmt.Errorf("undo: path %q is not tracked by this snapshot", path)
		}
		if info.IsDir() {
			// 目录不支持"整体回退"：目录本身不在快照里，静默返回空变更只会让调用方
			// 以为撤销成功了。
			return nil, fmt.Errorf("undo: path %q is a directory; pass the file path to revert", path)
		}
	}
	return s.changes(filter)
}

// changes 对比当前状态与快照；filter 非空时只保留该路径。
func (s *Snapshot) changes(filter string) ([]Change, error) {
	if s == nil {
		return nil, errors.New("undo: nil snapshot")
	}
	current, err := s.scanCurrent()
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(s.entries)+len(current))
	for rel := range s.entries {
		paths = append(paths, rel)
	}
	for rel := range current {
		if _, tracked := s.entries[rel]; !tracked {
			paths = append(paths, rel)
		}
	}
	sort.Strings(paths)

	changes := make([]Change, 0, len(paths))
	for _, rel := range paths {
		if filter != "" && rel != filter {
			continue
		}
		snapshotEntry := s.entries[rel]
		currentFile := current[rel]
		switch {
		case snapshotEntry == nil:
			added, countErr := countCurrentLines(currentFile)
			if countErr != nil {
				return nil, countErr
			}
			changes = append(changes, Change{Path: rel, Op: "add", Added: added})
		case currentFile == nil:
			if snapshotEntry.reason != "" {
				changes = append(changes, Change{Path: rel, Op: "delete", Reason: snapshotEntry.reason})
				continue
			}
			changes = append(changes, Change{Path: rel, Op: "delete", Removed: lineCount(snapshotEntry.content)})
		default:
			change, modified, compareErr := compareEntry(rel, snapshotEntry, currentFile)
			if compareErr != nil {
				return nil, compareErr
			}
			if modified {
				changes = append(changes, change)
			}
		}
	}
	return changes, nil
}

// compareEntry 判断一个两边都存在的文件是否被改动，并算出差异行数。
func compareEntry(rel string, snapshotEntry *entry, currentFile *currentFileState) (Change, bool, error) {
	change := Change{Path: rel, Op: "modify"}
	if snapshotEntry.reason != "" {
		// 没有备份内容：只能用大小 + mtime 判断是否被碰过。
		if currentFile.size == snapshotEntry.size && currentFile.modTime.Equal(snapshotEntry.modTime) {
			return change, false, nil
		}
		change.Reason = snapshotEntry.reason
		return change, true, nil
	}
	contentChanged := false
	switch {
	case currentFile.size != snapshotEntry.size:
		added, removed, err := diffCurrentAgainstSnapshot(snapshotEntry, currentFile)
		if err != nil {
			return change, false, err
		}
		change.Added, change.Removed = added, removed
		contentChanged = true
	case currentFile.hasContent:
		if sha256.Sum256(currentFile.content) == snapshotEntry.hash {
			break
		}
		change.Added, change.Removed = diffLines(snapshotEntry.content, currentFile.content)
		contentChanged = true
	case currentFile.hasHash:
		// 没有缓冲内容但有流式哈希：仍然可以准确判断（不会误报、也不会漏报）。
		if currentFile.hash == snapshotEntry.hash {
			break
		}
		change.Added, change.Removed = 0, lineCount(snapshotEntry.content)
		contentChanged = true
	default:
		// 既没内容也没哈希（超过 MaxFileBytes）：无法比较，按已改动处理。
		change.Added, change.Removed = 0, lineCount(snapshotEntry.content)
		contentChanged = true
	}
	if contentChanged {
		return change, true, nil
	}
	if snapshotEntry.mode != currentFile.mode {
		// 只有权限变了。这必须算一次变更：否则 chmod 过的文件在 undo 之后
		// 权限仍然是被改坏的样子（文档承诺"内容与权限"都回退）。
		change.modeOnly = true
		return change, true, nil
	}
	return change, false, nil
}

// revert 是 Revert/RevertPath 的实现：先算变更，再把可回退的部分原子地写回。
func (s *Snapshot) revert(filter string) ([]Change, error) {
	var changes []Change
	var err error
	if filter == "" {
		changes, err = s.changes("")
	} else {
		changes, err = s.filteredChanges(filter)
	}
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return []Change{}, nil
	}

	actions := make([]action, 0, len(changes))
	for _, change := range changes {
		if change.Reason != "" {
			continue
		}
		item := s.entries[change.Path]
		if change.Op == "add" {
			actions = append(actions, action{change: change, removeFile: true})
			continue
		}
		if change.modeOnly {
			actions = append(actions, action{change: change, modeOnly: true, mode: item.mode})
			continue
		}
		actions = append(actions, action{change: change, content: item.content, mode: item.mode})
	}
	applied, err := s.execute(actions)
	if err != nil {
		return nil, err
	}

	appliedByPath := make(map[string]Change, len(applied))
	for _, change := range applied {
		appliedByPath[change.Path] = change
	}
	// 按原顺序（已按路径排序）合并"已回退"与"跳过"，跳过项保留 Reason 供结果文案使用。
	merged := make([]Change, 0, len(changes))
	for _, change := range changes {
		if applied, ok := appliedByPath[change.Path]; ok {
			merged = append(merged, applied)
			continue
		}
		merged = append(merged, change)
	}
	return merged, nil
}

// action 是一次待执行的恢复动作。
type action struct {
	change     Change
	removeFile bool
	modeOnly   bool
	content    []byte
	mode       fs.FileMode
}

// execute 逐文件落盘，失败时把已执行的动作按原样回滚。
func (s *Snapshot) execute(actions []action) ([]Change, error) {
	applied := make([]Change, 0, len(actions))
	undoSteps := make([]func() error, 0, len(actions))

	rollback := func(cause error) error {
		var failures []error
		for index := len(undoSteps) - 1; index >= 0; index-- {
			if undoErr := undoSteps[index](); undoErr != nil {
				failures = append(failures, undoErr)
			}
		}
		if len(failures) > 0 {
			return fmt.Errorf("%w (rollback incomplete: %v)", cause, errors.Join(failures...))
		}
		if len(undoSteps) > 0 {
			return fmt.Errorf("%w (already-reverted files were rolled back)", cause)
		}
		return cause
	}

	for _, act := range actions {
		target := filepath.Join(s.root, filepath.FromSlash(act.change.Path))
		var previous []byte
		var previousMode fs.FileMode
		var exists bool
		var readErr error
		if act.modeOnly {
			previousMode, exists, readErr = statMode(target)
		} else {
			previous, previousMode, exists, readErr = readPreImage(target)
		}
		if readErr != nil {
			return nil, rollback(fmt.Errorf("undo: read %q: %w", act.change.Path, readErr))
		}
		switch {
		case act.removeFile:
			if removeErr := os.Remove(target); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
				return nil, rollback(fmt.Errorf("undo: remove %q: %w", act.change.Path, removeErr))
			}
			if exists {
				// 文件在扫描之后、执行之前就消失了：回滚必须是 no-op，
				// 否则 writeFileAtomic 会凭空造出一个 0 字节文件。
				undoSteps = append(undoSteps, func() error {
					return writeFileAtomic(target, previous, previousMode)
				})
			}
		case act.modeOnly:
			if chmodErr := os.Chmod(target, act.mode); chmodErr != nil {
				return nil, rollback(fmt.Errorf("undo: restore mode of %q: %w", act.change.Path, chmodErr))
			}
			undoSteps = append(undoSteps, func() error {
				if !exists {
					return nil
				}
				return os.Chmod(target, previousMode)
			})
		default:
			if writeErr := writeFileAtomic(target, act.content, act.mode); writeErr != nil {
				return nil, rollback(fmt.Errorf("undo: restore %q: %w", act.change.Path, writeErr))
			}
			undoSteps = append(undoSteps, func() error {
				if !exists {
					if removeErr := os.Remove(target); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
						return removeErr
					}
					return nil
				}
				return writeFileAtomic(target, previous, previousMode)
			})
		}
		act.change.Reverted = true
		applied = append(applied, act.change)
	}
	return applied, nil
}
