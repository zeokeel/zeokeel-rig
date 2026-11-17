package undo

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
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
	// tempPrefix 是回退写入时临时项的前缀；遍历时忽略它们，避免崩溃残留被当成
	// 工作区内容。
	tempPrefix = ".rig-undo-"
)

// DefaultIgnore 是始终忽略的目录/文件规则。用户的 Options.Ignore 追加在这之后，
// 而不是替换它们——否则一次误配置就可能把整个 .git 目录拍进快照。
var DefaultIgnore = []string{".git", "node_modules", "vendor", "dist", "build"}

// ErrRootReplaced 表示工作区根在快照之后被替换（变成符号链接、或换成了另一个目录）。
// 此时回退被拒绝：继续写会把快照内容倒进一个完全不同的工作区。
var ErrRootReplaced = errors.New("undo: the workspace root was replaced after the snapshot was taken")

// rootCookieName 是"根实例 cookie"的名字（Linux 上写作扩展属性 user.rig-undo-root）。
//
// 为什么需要它：只比较 (dev, ino) 有洞——删除目录后在**同一父目录**重建时，文件系统
// （ext4 实测 100% 复用）会把刚释放的 inode 分配给新目录，于是新目录与旧目录
// (dev, ino) 完全相同，守卫会误判"还是同一个工作区"，把快照内容倒进冒名目录。
// cookie 是写在根 inode 上的随机值：新目录不可能带着旧的 cookie，因此"读不到/不一致"
// 就说明根被换掉了。它用 xattr 存放，不产生目录项、不进快照、也不会被回退删除。
const rootCookieName = "rig-undo-root"

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
	// TypeChange 非空表示"路径类型变了"，取值为：
	//
	//	"file->symlink"   快照是普通文件，现在是符号链接
	//	"symlink->file"   快照是符号链接，现在是普通文件
	//	"symlink->symlink" 两侧都是符号链接，但目标文本不同
	//
	// 此时 Added/Removed 没有意义（结果为 0），回退会**还原快照里的类型**。
	TypeChange string
	// HardlinkOf 非空表示该路径与 HardlinkOf 共享同一个 inode，而本次回退没有
	// 恢复它：回退把 HardlinkOf 换成了新 inode（rename 语义），因此这条路径仍
	// 保留回合里的内容，硬链接关系也被断开。它是警告，不是"已回退"。
	HardlinkOf string
	// modeOnly 表示"只有权限变了，内容一字节没动"：回退只需 chmod，不必重写文件。
	modeOnly bool
}

// entryKind 是快照里一个路径的类型。
type entryKind uint8

const (
	// kindNone 是零值（"路径不存在"），只用于内部状态。
	kindNone entryKind = iota
	// kindFile 是普通文件。
	kindFile
	// kindSymlink 是符号链接（记录目标文本，不读目标内容）。
	kindSymlink
)

// String 返回类型的可读名（用于 TypeChange 文案与诊断）。
func (k entryKind) String() string {
	switch k {
	case kindFile:
		return "file"
	case kindSymlink:
		return "symlink"
	}
	return "none"
}

// entry 是快照里一个路径的记录。content 为 nil 表示没有备份内容（reason 说明原因）。
type entry struct {
	kind       entryKind
	size       int64
	mode       fs.FileMode
	modTime    time.Time
	hash       [sha256.Size]byte
	content    []byte
	reason     string
	linkTarget string
	// dev/ino 是 inode 身份（Unix 才有）。同一 inode 的多个路径（硬链接）靠它识别。
	dev, ino uint64
	hasInode bool
}

// Snapshot 是某一时刻的工作区状态。它由 Take 生成，之后只读（Revert 不改快照，
// 因此可以重复回退；第二次回退通常没有变更）。
type Snapshot struct {
	root    string
	options Options
	entries map[string]*entry
	files   int
	bytes   int64
	// 快照时的工作区根身份：用于发现"根被换掉"（换成符号链接或另一个目录）。
	rootDev, rootIno uint64
	rootHasInode     bool
	// rootToken 是根实例 cookie（见 rootCookieName）。空串表示平台/文件系统不支持
	// xattr，此时只保留 (dev,ino) 校验。
	rootToken string
}

// Root 返回规范化后的工作区根。
func (s *Snapshot) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// Files 返回快照跟踪的路径数（普通文件 + 符号链接，含未备份内容的文件）。
func (s *Snapshot) Files() int {
	if s == nil {
		return 0
	}
	return s.files
}

// Bytes 返回已备份内容的字节总数（符号链接不占内容预算）。
func (s *Snapshot) Bytes() int64 {
	if s == nil {
		return 0
	}
	return s.bytes
}

// Take 遍历 root 并拍一份快照。root 必须存在且是目录。
//
// 所有文件访问都经 internal/fsguard 的 traversal-safe 句柄；符号链接只记录目标文本，
// 绝不读取目标内容。
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
	if err := snapshot.recordRootIdentity(); err != nil {
		return nil, err
	}
	handle, err := openRoot(snapshot.root)
	if err != nil {
		return nil, fmt.Errorf("undo: open workspace root %q: %w", resolved, err)
	}
	defer func() { _ = handle.Close() }()

	// 根实例 cookie：同一根上的多次 Take 必须沿用同一个 token（否则旧快照会把
	// "下一回合拍的新快照"误判成根被换掉）。
	token := randomToken() + randomToken()
	createdCookie := false
	if existing, cookieErr := getRootCookie(snapshot.root); cookieErr == nil && existing != "" {
		token = existing
	} else if setErr := setRootCookie(snapshot.root, token); setErr == nil {
		createdCookie = true
	} else {
		// 平台/文件系统不支持：降级为 (dev,ino) 校验，绝不让 Take 失败。
		token = ""
	}
	snapshot.rootToken = token

	if err := snapshot.captureWith(handle); err != nil {
		if createdCookie {
			_ = removeRootCookie(snapshot.root)
		}
		return nil, err
	}
	return snapshot, nil
}

// recordRootIdentity 记录工作区根的身份（Lstat + inode）。
//
// 用 Lstat 是刻意的：resolveRoot 已经把用户给的根解析成真实路径，因此快照之后
// 该路径若变成符号链接，就说明"根被换掉了"。
func (s *Snapshot) recordRootIdentity() error {
	info, err := os.Lstat(s.root)
	if err != nil {
		return fmt.Errorf("undo: workspace root %q: %w", s.root, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: %q is not a directory", ErrRootReplaced, s.root)
	}
	dev, ino, ok := inodeOf(info)
	s.rootDev, s.rootIno, s.rootHasInode = dev, ino, ok
	return nil
}

// openRoot 打开工作区根句柄，并校验根没有被替换。
//
// 校验放在每次打开时做：Changes/Revert 都会重新打开，因此"回合之间根被换成符号
// 链接/另一个目录"会被立刻发现，而不是把快照内容写进一个完全不同的工作区。
func (s *Snapshot) openRoot() (fileRoot, error) {
	info, err := os.Lstat(s.root)
	if err != nil {
		return nil, fmt.Errorf("undo: workspace root %q: %w", s.root, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %q is now a symbolic link", ErrRootReplaced, s.root)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %q is no longer a directory", ErrRootReplaced, s.root)
	}
	if dev, ino, ok := inodeOf(info); ok && s.rootHasInode && (dev != s.rootDev || ino != s.rootIno) {
		return nil, fmt.Errorf("%w: %q now refers to a different directory", ErrRootReplaced, s.root)
	}
	handle, err := openRoot(s.root)
	if err != nil {
		return nil, fmt.Errorf("undo: open workspace root %q: %w", s.root, err)
	}
	return handle, nil
}

// captureWith 遍历工作区并填充 entries。cookie 校验不在这里做：Take 自己刚写完 cookie。
func (s *Snapshot) captureWith(handle fileRoot) error {
	var total int64
	count := 0
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
		if dirEntry.IsDir() {
			return nil
		}
		if !isTempName(dirEntry.Name()) {
			// 继续；临时项在下面统一跳过。
		} else {
			return nil
		}

		// 类型与元数据都经根句柄取（WalkDir 只负责列名字）。
		info, infoErr := handle.Lstat(rel)
		if infoErr != nil {
			if errNotExist(infoErr) {
				return nil // 扫描期间消失
			}
			return fmt.Errorf("undo: inspect %q: %w", rel, infoErr)
		}

		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			if count >= s.options.MaxFiles {
				return s.tooManyFilesError()
			}
			count++
			item := &entry{
				kind:    kindSymlink,
				size:    info.Size(),
				mode:    info.Mode() & modeBits,
				modTime: info.ModTime(),
			}
			// 只读链接文本（不跟随），因此指向根外的链接也不会被读出内容。
			target, linkErr := handle.Readlink(rel)
			if linkErr != nil {
				item.reason = skipReasonReadError(linkErr.Error())
			} else {
				item.linkTarget = target
			}
			item.dev, item.ino, item.hasInode = inodeOf(info)
			s.entries[rel] = item
			return nil
		case info.Mode().IsRegular():
			// 继续走下面的普通文件逻辑。
		default:
			// 设备、管道、套接字：不跟踪（既不该备份，也无法还原）。
			return nil
		}

		if count >= s.options.MaxFiles {
			return s.tooManyFilesError()
		}
		count++

		item := &entry{
			kind:    kindFile,
			size:    info.Size(),
			mode:    info.Mode() & modeBits,
			modTime: info.ModTime(),
		}
		item.dev, item.ino, item.hasInode = inodeOf(info)
		switch {
		case info.Size() > s.options.MaxFileBytes:
			item.reason = skipReasonTooLarge(info.Size(), s.options.MaxFileBytes)
		default:
			data, readErr := handle.ReadFile(rel)
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

// tooManyFilesError 是超过 Options.MaxFiles 的统一错误。
func (s *Snapshot) tooManyFilesError() error {
	return fmt.Errorf("undo: workspace %q has more than %d entries; raise Options.MaxFiles or narrow the workspace",
		s.root, s.options.MaxFiles)
}

// isTempName 报告文件名是否是我们自己的临时项（回退写入的残留）。
func isTempName(name string) bool {
	return strings.HasPrefix(name, tempPrefix) || strings.HasPrefix(name, ".fsguard-")
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
	handle, err := s.openRootIfSnapshot()
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Close() }()
	changes, _, err := s.filteredChangesWith(handle, path)
	return changes, err
}

// filteredChangesWith 是 filteredChanges 的句柄版（供 revert 复用同一个句柄）。
func (s *Snapshot) filteredChangesWith(handle fileRoot, path string) ([]Change, map[string]*currentFileState, error) {
	if s == nil {
		return nil, nil, errors.New("undo: nil snapshot")
	}
	filter, err := s.resolvePath(path)
	if err != nil {
		return nil, nil, err
	}
	if _, tracked := s.entries[filter]; !tracked {
		// 未经根句柄的 Lstat：它会拒绝父目录符号链接逃逸（fsguard.ErrEscape），
		// 因此"过滤路径穿过一个指向根外的链接"在这里就会失败。
		info, statErr := handle.Lstat(filter)
		if statErr != nil {
			if errNotExist(statErr) {
				return nil, nil, fmt.Errorf("undo: path %q is not tracked by this snapshot", path)
			}
			return nil, nil, fmt.Errorf("undo: path %q: %w", path, statErr)
		}
		if info.IsDir() {
			// 目录不支持"整体回退"：目录本身不在快照里，静默返回空变更只会让调用方
			// 以为撤销成功了。
			return nil, nil, fmt.Errorf("undo: path %q is a directory; pass the file path to revert", path)
		}
	}
	return s.changesWith(handle, filter)
}

// openRootIfSnapshot 校验非 nil、复核根身份、再打开根句柄。
//
// 这是 Changes/ChangesForPath/Revert/RevertPath 四个入口的**共同第一站**：根身份
// 校验因此一定发生在"没有变更就返回空"这类提前返回之前。根不可信时任何结果都不可信，
// 包括"恰好没有变更"。
func (s *Snapshot) openRootIfSnapshot() (fileRoot, error) {
	if s == nil {
		return nil, errors.New("undo: nil snapshot")
	}
	handle, err := s.openRoot()
	if err != nil {
		return nil, err
	}
	if err := s.verifyRootCookie(); err != nil {
		_ = handle.Close()
		return nil, err
	}
	return handle, nil
}

// verifyRootCookie 确认根目录仍然带着本快照的 cookie。
//
// (dev,ino) 挡不住"同 inode 复用"（删掉目录后在原地重建会拿到同一个 inode），
// cookie 是第二道、也是唯一能识别这种替换的信号。
func (s *Snapshot) verifyRootCookie() error {
	if s == nil || s.rootToken == "" {
		return nil
	}
	token, err := getRootCookie(s.root)
	if err != nil || token == "" {
		return fmt.Errorf("%w: no root cookie on %q (the directory was replaced)", ErrRootReplaced, s.root)
	}
	if token != s.rootToken {
		return fmt.Errorf("%w: root cookie mismatch on %q", ErrRootReplaced, s.root)
	}
	return nil
}

// changes 对比当前状态与快照；filter 非空时只保留该路径。
func (s *Snapshot) changes(filter string) ([]Change, error) {
	handle, err := s.openRootIfSnapshot()
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Close() }()
	changes, _, err := s.changesWith(handle, filter)
	return changes, err
}

// changesWith 是 changes 的句柄版：同时返回当前状态（供硬链接影响面分析使用）。
func (s *Snapshot) changesWith(handle fileRoot, filter string) ([]Change, map[string]*currentFileState, error) {
	if s == nil {
		return nil, nil, errors.New("undo: nil snapshot")
	}
	current, err := s.scanCurrentWith(handle)
	if err != nil {
		return nil, nil, err
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
			added, countErr := countCurrentLines(handle, s.options.MaxFileBytes, currentFile)
			if countErr != nil {
				return nil, nil, countErr
			}
			changes = append(changes, Change{Path: rel, Op: "add", Added: added})
		case currentFile == nil:
			if snapshotEntry.reason != "" {
				changes = append(changes, Change{Path: rel, Op: "delete", Reason: snapshotEntry.reason})
				continue
			}
			removed := 0
			if snapshotEntry.kind == kindFile {
				removed = lineCount(snapshotEntry.content)
			}
			changes = append(changes, Change{Path: rel, Op: "delete", Removed: removed})
		default:
			change, modified := compareEntry(snapshotEntry, currentFile)
			if modified {
				changes = append(changes, change)
			}
		}
	}
	// 硬链接影响面在**对比阶段**就生成，因此 Changes（dry-run）与 Revert 报告完全一致。
	changes = append(changes, s.hardlinkSiblings(changes, current)...)
	sort.Slice(changes, func(left, right int) bool { return changes[left].Path < changes[right].Path })
	return changes, current, nil
}

// compareEntry 判断一个两边都存在的路径是否被改动，并算出差异行数。
func compareEntry(snapshotEntry *entry, currentFile *currentFileState) (Change, bool) {
	change := Change{Path: currentFile.rel(), Op: "modify"}
	if snapshotEntry.kind != currentFile.kind {
		// 类型变化（file↔symlink）：必须被识别为一次变更，而不是"类型不同所以跳过"。
		change.TypeChange = snapshotEntry.kind.String() + "->" + currentFile.kind.String()
		return change, true
	}
	if snapshotEntry.kind == kindSymlink {
		if snapshotEntry.linkTarget == currentFile.linkTarget {
			return change, false
		}
		change.TypeChange = "symlink->symlink"
		return change, true
	}
	if snapshotEntry.reason != "" {
		// 没有备份内容：只能用大小 + mtime 判断是否被碰过。
		if currentFile.size == snapshotEntry.size && currentFile.modTime.Equal(snapshotEntry.modTime) {
			return change, false
		}
		change.Reason = snapshotEntry.reason
		return change, true
	}
	contentChanged := false
	switch {
	case currentFile.size != snapshotEntry.size:
		added, removed, err := diffCurrentAgainstSnapshot(currentFile, snapshotEntry)
		if err != nil {
			change.Reason = skipReasonReadError(err.Error())
			return change, true
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
		return change, true
	}
	if snapshotEntry.mode != currentFile.mode {
		// 只有权限变了。这必须算一次变更：否则 chmod 过的文件在 undo 之后
		// 权限仍然是被改坏的样子（文档承诺"内容与权限"都回退）。
		change.modeOnly = true
		return change, true
	}
	return change, false
}

// revert 是 Revert/RevertPath 的实现：先算变更，再把可回退的部分原子地写回。
func (s *Snapshot) revert(filter string) ([]Change, error) {
	handle, err := s.openRootIfSnapshot()
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Close() }()

	var changes []Change
	if filter == "" {
		changes, _, err = s.changesWith(handle, "")
	} else {
		changes, _, err = s.filteredChangesWith(handle, filter)
	}
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return []Change{}, nil
	}

	actions := make([]action, 0, len(changes))
	for _, change := range changes {
		// 硬链接同组但未被回退的路径只是报告项（HardlinkOf），没有对应动作。
		if change.Reason != "" || change.HardlinkOf != "" {
			continue
		}
		item := s.entries[change.Path]
		switch {
		case change.Op == "add":
			// 新增路径没有快照记录：回退就是把它删掉（链接只删链接本身）。
			actions = append(actions, action{change: change, kind: actionRemovePath})
		case item == nil:
			continue // 理论上不可达：非 add 的变更一定有快照记录
		case change.modeOnly:
			actions = append(actions, action{change: change, kind: actionChmod, mode: item.mode})
		case item.kind == kindSymlink:
			actions = append(actions, action{change: change, kind: actionWriteSymlink, linkTarget: item.linkTarget})
		default:
			actions = append(actions, action{change: change, kind: actionWriteFile, content: item.content, mode: item.mode})
		}
	}
	applied, err := s.execute(handle, actions)
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
		if appliedChange, ok := appliedByPath[change.Path]; ok {
			merged = append(merged, appliedChange)
			continue
		}
		merged = append(merged, change)
	}
	// affected 条目已经在 changesWith 里生成并随 changes 一起排序，无需再合并。
	return merged, nil
}

// hardlinkSiblings 找出"与本次被改写路径共享 inode、但本身没有被回退"的路径。
//
// 回退用 rename 落盘，会把目标换成新 inode：同 inode 的其它路径因此仍保留回合里的
// 内容，硬链接关系也被断开。这必须显式报告（Change.HardlinkOf），不能静默。
//
// 输入是**对比结果**而不是动作列表：changesWith 在 Changes（dry-run）与 Revert 两条
// 路径上都调用它，因此预览与实际执行的报告严格一致。
//
// 只有 modify/delete 会真正落盘（rename 换 inode）；add 只是删掉一条路径，不会让
// 同 inode 的其它路径改内容，modeOnly 是原地 chmod（不换 inode），两者都不算被改写。
func (s *Snapshot) hardlinkSiblings(changes []Change, current map[string]*currentFileState) []Change {
	rewritten := make(map[string]bool, len(changes))
	for _, change := range changes {
		if change.Reason != "" || change.modeOnly || change.HardlinkOf != "" {
			continue
		}
		if change.Op == "modify" || change.Op == "delete" {
			rewritten[change.Path] = true
		}
	}
	if len(rewritten) == 0 || len(current) == 0 {
		return nil
	}
	reported := make(map[string]bool)
	var notes []Change
	for rel := range rewritten {
		snapshotEntry := s.entries[rel]
		if snapshotEntry == nil || !snapshotEntry.hasInode {
			continue
		}
		currentEntry := current[rel]
		if currentEntry == nil || !currentEntry.hasInode {
			continue
		}
		if currentEntry.dev != snapshotEntry.dev || currentEntry.ino != snapshotEntry.ino {
			// 回合里已经被换成新 inode：链接早就断了，没有"另一条路径被连带改写"。
			continue
		}
		for other, otherEntry := range s.entries {
			if other == rel || rewritten[other] || reported[other] {
				continue
			}
			if !otherEntry.hasInode || otherEntry.dev != snapshotEntry.dev || otherEntry.ino != snapshotEntry.ino {
				continue
			}
			otherCurrent := current[other]
			if otherCurrent == nil || !otherCurrent.hasInode {
				continue
			}
			if otherCurrent.dev != currentEntry.dev || otherCurrent.ino != currentEntry.ino {
				continue // 当前已不是同一 inode
			}
			reported[other] = true
			notes = append(notes, Change{Path: other, Op: "modify", HardlinkOf: rel})
		}
	}
	sort.Slice(notes, func(left, right int) bool { return notes[left].Path < notes[right].Path })
	return notes
}

// actionKind 是一次回退动作的类型。
type actionKind uint8

const (
	// actionWriteFile 原子写回普通文件（先删掉占位的链接，绝不写穿）。
	actionWriteFile actionKind = iota
	// actionWriteSymlink 重建符号链接（删掉占位项后按原始目标重建）。
	actionWriteSymlink
	// actionRemovePath 删除"回合里新增"的路径（文件或链接本身）。
	actionRemovePath
	// actionChmod 只恢复权限位。
	actionChmod
)

// action 是一次待执行的恢复动作。
type action struct {
	kind       actionKind
	change     Change
	content    []byte
	mode       fs.FileMode
	linkTarget string
}

// execute 逐路径落盘，失败时把已执行的动作按原样回滚。
func (s *Snapshot) execute(handle fileRoot, actions []action) ([]Change, error) {
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
		rel := act.change.Path
		previous, readErr := readPreState(handle, rel)
		if readErr != nil {
			return nil, rollback(fmt.Errorf("undo: inspect %q: %w", rel, readErr))
		}
		if applyErr := applyAction(handle, rel, act, previous); applyErr != nil {
			return nil, rollback(fmt.Errorf("undo: restore %q: %w", rel, applyErr))
		}
		undoSteps = append(undoSteps, func() error {
			return restorePreState(handle, rel, previous)
		})
		act.change.Reverted = true
		applied = append(applied, act.change)
	}
	return applied, nil
}

// applyAction 执行单个恢复动作。previous 是执行前的状态（只用于判断是否需要先删）。
func applyAction(handle fileRoot, rel string, act action, previous preState) error {
	switch act.kind {
	case actionChmod:
		if previous.kind != kindFile {
			return nil // 位置已经不是普通文件：没有权限位可恢复
		}
		return handle.Chmod(rel, act.mode)
	case actionRemovePath:
		if err := handle.Remove(rel); err != nil && !errNotExist(err) {
			return err
		}
		return nil
	case actionWriteSymlink:
		if previous.kind != kindNone {
			if err := handle.Remove(rel); err != nil && !errNotExist(err) {
				return err
			}
		}
		return writeSymlink(handle, rel, act.linkTarget)
	default: // actionWriteFile
		if previous.kind == kindSymlink {
			// 快照里是普通文件、现在是链接：先删链接再写，避免写穿到链接目标
			// （fsguard.WriteFileAtomic 的语义是"写穿最终符号链接"）。
			if err := handle.Remove(rel); err != nil && !errNotExist(err) {
				return err
			}
		}
		if parent := path.Dir(rel); parent != "." && parent != "/" {
			if err := handle.MkdirAll(parent, 0o755); err != nil {
				return err
			}
		}
		return handle.WriteFileAtomic(rel, act.content, act.mode)
	}
}
