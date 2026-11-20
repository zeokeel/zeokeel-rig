package undo

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zeokeel/zeokeel-rig/internal/gitsnap"
)

// ToolName 是 undo 工具面向模型的稳定名称。
const ToolName = "undo"

// 保留的限额常量（历史 API）。
//
// 说明：git 快照不再需要"单文件上限 / 总字节上限 / 文件数上限"——内容存在对象库里，
// SDK 不把它们读进内存。常量保留是为了不破坏既有宿主的编译；Options 里的对应字段
// 现在是 no-op。
const (
	// DefaultMaxFileBytes 曾经是单文件备份上限。
	DefaultMaxFileBytes int64 = 4 << 20
	// DefaultMaxTotalBytes 曾经是一次快照的内容总预算。
	DefaultMaxTotalBytes int64 = 64 << 20
	// DefaultMaxFiles 曾经是允许跟踪的文件数上限。
	DefaultMaxFiles = 20000
	// DefaultRetain 是 Tracker 默认保留的回合数。
	DefaultRetain = 10
	// DefaultRefRetain 是 `refs/rig/undo/*` 默认保留的快照引用数（与 gitsnap 一致）。
	DefaultRefRetain = 128
)

// DefaultIgnore 是历史默认忽略表。
//
// 已废弃：git 快照以 `.gitignore` 为准，本表不再默认生效（否则会拦掉仓库里**已被
// 跟踪**的 build/dist 目录，与 git 语义矛盾）。保留变量仅为了源码兼容。
var DefaultIgnore = []string{".git", "node_modules", "vendor", "dist", "build"}

// ErrNotGitRepo 表示工作区不是 git 仓库：调用方应当降级为"不提供撤销"。
var ErrNotGitRepo = gitsnap.ErrNotGitRepo

// ErrRootReplaced 是历史错误（非 git 实现里用于识别"工作区根被换掉"）。
//
// 已废弃：现在由 git 自己保证仓库身份，本包不再返回它。保留声明仅为源码兼容。
var ErrRootReplaced = errors.New("undo: the workspace root was replaced after the snapshot was taken")

// Options 配置 undo 的行为。
type Options struct {
	// Ignore 是**追加**的忽略规则：命中的路径不会被报告、也不会被回退。
	// 不含 "/" 的规则按任意层级的名字匹配（"cache" 命中 a/cache/x）；含 "/" 的规则
	// 按工作区相对路径前缀匹配（"docs/generated"）。末尾的 "/" 会被忽略。
	//
	// 注意：`.gitignore` 始终由 git 自己处理（被忽略的文件既不进快照也不被删除）；
	// 这里只额外排除**已经被 git 跟踪**的路径。
	Ignore []string
	// MaxFileBytes/MaxTotalBytes/MaxFiles 已废弃（no-op）：git 快照不把内容读进内存。
	MaxFileBytes  int64
	MaxTotalBytes int64
	MaxFiles      int
	// Retain 是 Tracker 保留的回合数（默认 DefaultRetain）。
	Retain int
	// RefRetain 是 `refs/rig/undo/*` 保留的快照引用数（默认 DefaultRefRetain=128，
	// 0 表示用默认值）。
	//
	// 引用多不等于占空间多（它们共享对象），但个人客户端通常只需要最近若干个回合可
	// 撤销；把它调小可以让 `git gc` 更早回收旧快照。修剪发生在每次 Take 之后。
	RefRetain int
	// Dir 是回合号持久化目录（宿主给；kit 默认 <Root>/.rig/undo）。
	//
	// 非空时回合号写进 Dir/state.json，进程重启后继续递增而不是回到 1；为空表示只记
	// 在进程内。git 后端只在这里存回合号（快照仍在 git 对象库里），文件后端
	// （NewFileTracker）用 FileOptions.Dir 同时存快照与回合号。
	//
	// 注意：Dir 落在工作区里时，后端会**按实际状态目录**自动补上一条忽略规则
	// （Dir == Root 时是 state.json，Dir 在 Root 内时是它的相对路径），避免状态文件
	// 自己成为一次"回合改动"；kit 另把 `.rig` 固定加进忽略表（spill 也用那个目录）。
	Dir string
}

// withDefaults 返回填好默认值的副本，并规范化忽略规则。
func (o Options) withDefaults() Options {
	filled := o
	if filled.Retain <= 0 {
		filled.Retain = DefaultRetain
	}
	if filled.RefRetain <= 0 {
		filled.RefRetain = DefaultRefRetain
	}
	filled.Ignore = normalizeIgnore(o.Ignore)
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
func (o Options) ignored(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, rule := range o.Ignore {
		if strings.Contains(rule, "/") {
			if rel == rule || strings.HasPrefix(rel, rule+"/") {
				return true
			}
			continue
		}
		if rel == rule ||
			strings.HasPrefix(rel, rule+"/") ||
			strings.HasSuffix(rel, "/"+rule) ||
			strings.Contains(rel, "/"+rule+"/") {
			return true
		}
	}
	return false
}

// stateDirIgnoreRules 返回"后端状态目录落在工作区内"时需要追加的忽略规则。
//
// 两个后端共用这一个来源：git 后端的回合号写在 Options.Dir 下，文件后端的状态与快照
// 都写在 FileOptions.Dir 下。Dir 就是工作区根本身时 rel="."，规则退化成 state.json
// （git 后端只在根下写这一个文件）；Dir 在 Root 之外时不追加任何规则。
func stateDirIgnoreRules(root, dir string) []string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	absRoot, rootErr := filepath.Abs(root)
	absDir, dirErr := filepath.Abs(dir)
	if rootErr != nil || dirErr != nil {
		return nil
	}
	rel, err := filepath.Rel(filepath.Clean(absRoot), filepath.Clean(absDir))
	if err != nil {
		return nil
	}
	switch {
	case rel == ".":
		return []string{stateFileName}
	case rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)):
		return nil
	}
	return []string{filepath.ToSlash(rel)}
}

// underStateRule 报告相对路径是否就是某条状态目录规则本身、或位于它下面。
//
// 这里刻意用**严格前缀**而不是 ignoredBy 的"任意层级同名"语义：状态目录是一个具体
// 位置，`<root>/snapshots` 不能把 `<root>/sub/snapshots` 一起跳过。
func underStateRule(rules []string, rel string) bool {
	for _, rule := range rules {
		if rel == rule || strings.HasPrefix(rel, rule+"/") {
			return true
		}
	}
	return false
}

// Change 是一处相对快照的差异。Op 取 "add"、"modify"、"delete" 或 "rename"。
type Change struct {
	// Path 是相对工作区根的斜杠路径（rename 时是目标路径）。
	Path string
	// OldPath 只在 rename 时非空（源路径）。
	OldPath string
	// Op 是 add、modify、delete 或 rename。
	Op string
	// Added/Removed 是行数（二进制文件为 0）。
	Added, Removed int
	// Reverted 报告这次调用是否真的把它恢复了（dry-run 与跳过的变更都是 false）。
	Reverted bool
	// Reason 非空表示该变更不可回退（例如子模块）。
	Reason string
	// TypeChange 非空表示路径类型变了：file->symlink、symlink->file、symlink->symlink。
	TypeChange string
	// HardlinkOf 已废弃：git 不区分硬链接，本包永远留空。保留字段仅为源码兼容。
	HardlinkOf string
	// modeOnly 表示内容没动、只有可执行位变了（回退只需恢复权限）。
	modeOnly bool
}

// Snapshot 是某一时刻的工作区状态（一个 git 隐藏提交）。它只读，可以重复回退。
type Snapshot struct {
	root    string
	options Options
	id      string
	files   int
}

// Root 返回规范化后的工作区根。
func (s *Snapshot) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// ID 返回底层的 git 快照 ID（隐藏提交），供宿主诊断/记录。
func (s *Snapshot) ID() string {
	if s == nil {
		return ""
	}
	return s.id
}

// Files 返回快照记录的路径数（当时的已跟踪 + 未跟踪文件）。
func (s *Snapshot) Files() int {
	if s == nil {
		return 0
	}
	return s.files
}

// Bytes 返回 SDK 侧持有的内容字节数：**恒为 0**。
//
// 内容存在 git 对象库里（按内容寻址、增量压缩），SDK 不缓存文件内容——大文件不会
// 被复制进内存，这也是换掉"逐文件内容备份"的原因。
func (s *Snapshot) Bytes() int64 {
	return 0
}

// Take 为 root 拍一份快照（回合开始时调用）。
//
// root 必须是 git 仓库（或其子目录）；否则返回 ErrNotGitRepo，调用方应当降级为
// "不提供撤销"。root 是子目录时快照只覆盖该子树。
func Take(root string, opts Options) (*Snapshot, error) {
	return TakeContext(context.Background(), root, opts)
}

// TakeContext 是带取消的 Take。
func TakeContext(ctx context.Context, root string, opts Options) (*Snapshot, error) {
	options := opts.withDefaults()
	resolved, err := resolveRoot(root)
	if err != nil {
		return nil, err
	}
	id, err := gitsnap.SnapshotWithRetain(ctx, resolved, options.RefRetain)
	if err != nil {
		return nil, err
	}
	paths, err := gitsnap.Paths(ctx, resolved, id)
	if err != nil {
		return nil, err
	}
	return &Snapshot{root: resolved, options: options, id: id, files: len(paths)}, nil
}

// IsGitRepo 报告 root 是否是 git 工作区（宿主可用它决定要不要提供撤销）。
func IsGitRepo(ctx context.Context, root string) (bool, error) {
	resolved, err := resolveRoot(root)
	if err != nil {
		return false, err
	}
	return gitsnap.IsRepo(ctx, resolved)
}

// Changes 返回相对快照的全部差异（按路径排序）。
func (s *Snapshot) Changes() ([]Change, error) {
	return s.ChangesContext(context.Background())
}

// ChangesContext 是带取消的 Changes。
func (s *Snapshot) ChangesContext(ctx context.Context) ([]Change, error) {
	if s == nil {
		return nil, errors.New("undo: nil snapshot")
	}
	raw, err := gitsnap.Diff(ctx, s.root, s.id)
	if err != nil {
		return nil, err
	}
	return s.convert(raw), nil
}

// ChangesForPath 返回某一个路径的差异（相对工作区根，越界报错）。
func (s *Snapshot) ChangesForPath(target string) ([]Change, error) {
	return s.ChangesForPathContext(context.Background(), target)
}

// ChangesForPathContext 是带取消的 ChangesForPath。
func (s *Snapshot) ChangesForPathContext(ctx context.Context, target string) ([]Change, error) {
	cleaned, err := s.resolvePath(target)
	if err != nil {
		return nil, err
	}
	changes, err := s.ChangesContext(ctx)
	if err != nil {
		return nil, err
	}
	filtered := make([]Change, 0, 1)
	for _, change := range changes {
		if change.Path == cleaned || change.OldPath == cleaned {
			filtered = append(filtered, change)
		}
	}
	if len(filtered) == 0 {
		return nil, fmt.Errorf("undo: path %q has no changes relative to the snapshot", target)
	}
	return filtered, nil
}

// Revert 把工作区恢复到快照状态，返回被处理的变更。
func (s *Snapshot) Revert() ([]Change, error) {
	return s.RevertContext(context.Background())
}

// RevertContext 是带取消的 Revert。
func (s *Snapshot) RevertContext(ctx context.Context) ([]Change, error) {
	return s.revert(ctx, "")
}

// RevertPath 只回退一个路径（相对工作区根）。
func (s *Snapshot) RevertPath(target string) ([]Change, error) {
	return s.RevertPathContext(context.Background(), target)
}

// RevertPathContext 是带取消的 RevertPath。
func (s *Snapshot) RevertPathContext(ctx context.Context, target string) ([]Change, error) {
	cleaned, err := s.resolvePath(target)
	if err != nil {
		return nil, err
	}
	return s.revert(ctx, cleaned)
}

// revert 是 Revert/RevertPath 的共同实现。filter 为空表示全部。
func (s *Snapshot) revert(ctx context.Context, filter string) ([]Change, error) {
	if s == nil {
		return nil, errors.New("undo: nil snapshot")
	}
	var changes []Change
	var err error
	if filter == "" {
		changes, err = s.ChangesContext(ctx)
	} else {
		changes, err = s.ChangesForPathContext(ctx, filter)
	}
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return []Change{}, nil
	}
	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		paths = append(paths, change.Path)
	}
	// 始终按"过滤后的路径清单"回退：Options.Ignore 命中的路径不能因为一次全量
	// Revert 被顺带改掉。
	raw, err := gitsnap.RevertPaths(ctx, s.root, s.id, paths)
	if err != nil {
		return nil, err
	}
	return s.convert(raw), nil
}

// convert 把 gitsnap 的变更转换成 undo 的 Change，并按 Options.Ignore 过滤。
func (s *Snapshot) convert(raw []gitsnap.FileChange) []Change {
	changes := make([]Change, 0, len(raw))
	for _, entry := range raw {
		if s.options.ignored(entry.Path) || (entry.OldPath != "" && s.options.ignored(entry.OldPath)) {
			continue
		}
		changes = append(changes, Change{
			Path:       entry.Path,
			OldPath:    entry.OldPath,
			Op:         entry.Op,
			Added:      entry.Added,
			Removed:    entry.Removed,
			Reverted:   entry.Reverted,
			Reason:     entry.Reason,
			TypeChange: entry.TypeChange,
			modeOnly:   entry.ModeOnly,
		})
	}
	sort.Slice(changes, func(left, right int) bool { return changes[left].Path < changes[right].Path })
	return changes
}

// Prune 只保留最近 keep 个 `refs/rig/undo/*` 快照引用（keep<=0 时不动）。
//
// 宿主一般不需要调用它：每次 Take 都会按 Options.RefRetain 自动修剪。它存在是为了
// 让宿主能在"用户清空历史/退出应用"时主动收紧，而不必依赖内部包。
func Prune(ctx context.Context, root string, keep int) error {
	resolved, err := resolveRoot(root)
	if err != nil {
		return err
	}
	return gitsnap.Prune(ctx, resolved, keep)
}

// resolveRoot 校验并规范化工作区根：绝对化、必须是存在的目录、解析符号链接。
func resolveRoot(root string) (string, error) {
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		return "", errors.New("undo: workspace root is empty")
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("undo: workspace root %q: %w", root, err)
	}
	absolute = filepath.Clean(absolute)
	info, err := osStat(absolute)
	if err != nil {
		return "", fmt.Errorf("undo: workspace root %q: %w", root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("undo: workspace root %q is not a directory", root)
	}
	resolved, err := evalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("undo: workspace root %q: resolve symlinks: %w", root, err)
	}
	return filepath.Clean(resolved), nil
}

// resolvePath 把工具/API 给的过滤路径解析成"相对工作区根的斜杠路径"（只做词法校验）。
//
// 保留在 Snapshot 上是为了源码兼容；实现与文件后端共用 resolveRelativePath。
func (s *Snapshot) resolvePath(target string) (string, error) { return resolveRelativePath(target) }

// resolveRelativePath 把工具/API 给的过滤路径解析成"相对工作区根的斜杠路径"
// （只做词法校验：非空、非绝对、不越界、不是根本身）。
func resolveRelativePath(target string) (string, error) {
	display := strings.TrimSpace(target)
	if display == "" {
		return "", errors.New("undo: path is empty")
	}
	if isAbsoluteAnyOS(display) {
		return "", fmt.Errorf("undo: absolute paths are not allowed: %q", display)
	}
	native := filepath.Clean(filepath.FromSlash(display))
	if native == ".." || strings.HasPrefix(native, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("undo: path %q escapes the workspace root", display)
	}
	if native == "." {
		return "", fmt.Errorf("undo: the workspace root itself cannot be reverted: %q", display)
	}
	return path.Clean(filepath.ToSlash(native)), nil
}
