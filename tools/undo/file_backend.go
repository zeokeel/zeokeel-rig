package undo

// 纯文件系统快照后端：不依赖 git 的工作区也能用 undo。
//
// 做法很直接：回合开始时把工作区里的普通文件复制到 `<Dir>/turns/<turn>/files/<相对路径>`
// （镜像），并写一份 `<Dir>/turns/<turn>/manifest.json`（清单）记录每个路径的权限位与
// "没快照进去的文件及原因"。**清单与镜像分属两个命名空间**：清单在回合目录里，镜像在
// 它下面的 `files/` 子目录里，所以工作区里叫 `manifest.json` 的文件不会与清单撞名。
// 回退时把清单里的文件写回工作区、删掉工作区里清单之外的（= 本回合新增的）文件、
// 补齐工作区里已丢失的（= 本回合删除的）文件；删除的文件不需要单独的墓碑记录——
// 清单本身就是"回合开始时的存在性"，工作区里缺了就说明被删了。
//
// 没有 git 就没有"哪次提交/哪棵树"可比，所以这里不做增量：每个回合复制一份完整工作区。
// 代价由 FileOptions.MaxTurns（保留几个回合）与 MaxBytes（单文件上限）封顶。
//
// 写入一律不跟随符号链接：类型判定用 Lstat，写回时先确认目标不存在或是普通文件、再以
// O_EXCL 创建；符号链接/目录/设备文件不做写入，只作为"未回退"如实报告。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// DefaultFileMaxBytes 是文件后端默认的单文件快照上限（4 MiB）。
//
// 超过它的文件只记一条"未快照"（跳过大文件而不是把内存/磁盘撑爆），undo 结果里会
// 列出来并说明没有动它。
const DefaultFileMaxBytes int64 = 4 << 20

// manifestFileName 是每个回合目录里的清单文件名。
//
// 它位于回合目录本身，而镜像内容在回合目录下的 `files/` 里：两者是**不同的命名空间**，
// 因此工作区根里叫 `manifest.json` 的文件不会覆盖（或被覆盖）清单。
const manifestFileName = "manifest.json"

// turnsDirName 是 Dir 下存放回合目录的子目录名。
const turnsDirName = "turns"

// filesDirName 是回合目录下存放镜像内容的子目录名。
const filesDirName = "files"

// FileOptions 配置纯文件系统快照后端（见 NewFileTracker）。
type FileOptions struct {
	// Dir 是快照目录（必填）。可以放在工作区里（kit 默认 <Root>/.rig/undo），也可以
	// 放在外面；放在工作区里时它自己不会被快照。
	Dir string
	// MaxBytes 是单文件快照上限，<=0 表示 DefaultFileMaxBytes。
	MaxBytes int64
	// MaxTurns 是保留的回合数，<=0 表示 DefaultRetain。
	MaxTurns int
}

// withDefaults 返回填好默认值的副本。
func (o FileOptions) withDefaults() FileOptions {
	filled := o
	filled.Dir = strings.TrimSpace(filled.Dir)
	if filled.MaxBytes <= 0 {
		filled.MaxBytes = DefaultFileMaxBytes
	}
	if filled.MaxTurns <= 0 {
		filled.MaxTurns = DefaultRetain
	}
	return filled
}

// snapshotEntry 是清单里"已快照"的一个文件。
type snapshotEntry struct {
	// Path 是相对工作区根的斜杠路径。
	Path string `json:"path"`
	// Size 是快照时的字节数。
	Size int64 `json:"size"`
	// Mode 是权限位（保留可执行位）。
	Mode uint32 `json:"mode"`
}

// skippedEntry 是清单里"没有快照"的一个文件。
//
// Size/ModTime 是快照时的大小与修改时间：回退时据此判断它是否在本回合里被动过——
// 没动过就无需报告（它本来就在快照状态上），动过则作为"未快照、未回退"列出来。
type skippedEntry struct {
	Path    string `json:"path"`
	Reason  string `json:"reason"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
}

// fileManifest 是一个回合的快照清单。
type fileManifest struct {
	Turn    int             `json:"turn"`
	Root    string          `json:"root"`
	Entries []snapshotEntry `json:"entries"`
	Skipped []skippedEntry  `json:"skipped,omitempty"`
}

// NewFileTracker 创建绑定到 root 的 Tracker，快照后端是**纯文件系统副本**（不需要 git）。
//
// 回合号持久化在 Dir/state.json，重启后打开同一个 Dir 会从上次的回合号继续；Dir 里
// 已有的回合目录也参与推算，state.json 丢失时同样不会撞车。
//
// 目录布局（清单/状态都不在镜像命名空间里，避免与工作区文件名冲突）：
//
//	<Dir>/state.json                 回合号
//	<Dir>/turns/<turn>/manifest.json 清单
//	<Dir>/turns/<turn>/files/<rel>   镜像内容
func NewFileTracker(root string, opts FileOptions) (*Tracker, error) {
	options := opts.withDefaults()
	if options.Dir == "" {
		return nil, errors.New("undo: file snapshot directory is empty; set FileOptions.Dir")
	}
	resolved, err := resolveRoot(root)
	if err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(options.Dir)
	if err != nil {
		return nil, fmt.Errorf("undo: snapshot directory %q: %w", options.Dir, err)
	}
	dir = filepath.Clean(dir)
	if dir == resolved {
		return nil, fmt.Errorf("undo: the snapshot directory must differ from the workspace root (%q)", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("undo: create snapshot directory %q: %w", dir, err)
	}
	// 解析符号链接后再与 root 比较：否则"工作区内的快照目录"可能因为路径写法不同
	// 而匹配不上，被自己快照进去。
	if resolvedDir, evalErr := filepath.EvalSymlinks(dir); evalErr == nil {
		dir = filepath.Clean(resolvedDir)
	}
	if dir == resolved {
		return nil, fmt.Errorf("undo: the snapshot directory must differ from the workspace root (%q)", dir)
	}
	backend := &fileBackend{
		root:     resolved,
		dir:      dir,
		maxBytes: options.MaxBytes,
		retain:   options.MaxTurns,
		// 与 git 后端同一个来源：状态/快照目录落在工作区内时，遍历要跳过它。
		stateRules: stateDirIgnoreRules(resolved, dir),
	}
	next, err := loadTurnState(dir)
	if err != nil {
		return nil, err
	}
	for _, turn := range backend.Turns() {
		if turn > next {
			next = turn
		}
	}
	return &Tracker{root: resolved, dir: dir, backend: backend, next: next}, nil
}

// fileBackend 是 Backend 的文件系统实现。
type fileBackend struct {
	root     string
	dir      string
	maxBytes int64
	retain   int
	// stateRules 是"实际状态目录"的忽略规则（见 stateDirIgnoreRules）：遍历工作区时
	// 命中的目录整棵跳过，避免把快照目录自己收进快照。
	stateRules []string
	mu         sync.Mutex
}

// turnsRoot 返回存放回合目录的父目录。
func (b *fileBackend) turnsRoot() string {
	return filepath.Join(b.dir, turnsDirName)
}

// turnDir 返回回合的快照目录。
func (b *fileBackend) turnDir(turn int) string {
	return filepath.Join(b.turnsRoot(), strconv.Itoa(turn))
}

// fullPath 把工作区相对路径变成绝对路径。
func (b *fileBackend) fullPath(rel string) string {
	return filepath.Join(b.root, filepath.FromSlash(rel))
}

// copyPath 返回快照里该文件的副本路径。
//
// 副本放在回合目录的 `files/` 下，与回合目录里的清单（manifest.json）分开命名空间。
func (b *fileBackend) copyPath(turn int, rel string) string {
	return filepath.Join(b.turnDir(turn), filesDirName, filepath.FromSlash(rel))
}

// Take 实现 Backend：把工作区里的普通文件复制进 Dir/turns/<turn>/files/。
func (b *fileBackend) Take(ctx context.Context, turn int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	turnDir := b.turnDir(turn)
	if err := os.MkdirAll(turnDir, 0o700); err != nil {
		return fmt.Errorf("undo: create snapshot %q: %w", turnDir, err)
	}
	manifest := fileManifest{Turn: turn, Root: b.root}
	walkErr := b.walk(ctx, func(rel string, info fs.FileInfo) error {
		if info.Size() > b.maxBytes {
			manifest.Skipped = append(manifest.Skipped, skippedEntry{
				Path:    rel,
				Reason:  fmt.Sprintf("not-backed-up: larger than %d bytes", b.maxBytes),
				Size:    info.Size(),
				ModTime: info.ModTime().UnixNano(),
			})
			return nil
		}
		if err := copyFile(b.fullPath(rel), b.copyPath(turn, rel), info.Mode().Perm()); err != nil {
			// 单个文件读不出来不该让整份快照失败：记一条原因，回退时报告。
			manifest.Skipped = append(manifest.Skipped, skippedEntry{
				Path:    rel,
				Reason:  "not-backed-up: " + err.Error(),
				Size:    info.Size(),
				ModTime: info.ModTime().UnixNano(),
			})
			return nil
		}
		manifest.Entries = append(manifest.Entries, snapshotEntry{
			Path: rel,
			Size: info.Size(),
			Mode: uint32(info.Mode().Perm()),
		})
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	sort.Slice(manifest.Entries, func(left, right int) bool {
		return manifest.Entries[left].Path < manifest.Entries[right].Path
	})
	sort.Slice(manifest.Skipped, func(left, right int) bool {
		return manifest.Skipped[left].Path < manifest.Skipped[right].Path
	})
	if err := writeManifest(turnDir, manifest); err != nil {
		return err
	}
	return b.pruneLocked()
}

// Turns 实现 Backend。
func (b *fileBackend) Turns() []int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.turnsLocked()
}

// Changes 实现 Backend。
func (b *fileBackend) Changes(ctx context.Context, turn int, path string) ([]Change, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	filter := ""
	if path != "" {
		cleaned, err := resolveRelativePath(path)
		if err != nil {
			return nil, err
		}
		filter = cleaned
	}
	b.mu.Lock()
	manifest, err := b.manifest(turn)
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}

	changes := make([]Change, 0, len(manifest.Entries))
	known := make(map[string]snapshotEntry, len(manifest.Entries))
	// 快照里是文件、现在成了目录的路径：它下面的文件属于"被替换掉的目录"，不是本回合
	// 新增的文件，不能参与回退（否则会把用户新目录里的内容删掉）。
	var blockedDirs []string
	for _, entry := range manifest.Entries {
		known[entry.Path] = entry
		change, changed := b.diffEntry(ctx, turn, entry)
		if changed {
			changes = append(changes, change)
		}
		if change.TypeChange == typeChangeToDir {
			blockedDirs = append(blockedDirs, entry.Path)
		}
	}
	// 清单之外、但工作区里存在的文件 = 本回合新增的：回退时删除。
	untracked := make(map[string]struct{}, len(manifest.Skipped))
	for _, skip := range manifest.Skipped {
		untracked[skip.Path] = struct{}{}
	}
	addWalkErr := b.walk(ctx, func(rel string, info fs.FileInfo) error {
		if _, ok := known[rel]; ok {
			return nil
		}
		if _, ok := untracked[rel]; ok {
			return nil
		}
		if underAny(rel, blockedDirs) {
			return nil
		}
		added := 0
		if data, err := os.ReadFile(b.fullPath(rel)); err == nil {
			added = lineCount(data)
		}
		changes = append(changes, Change{Path: rel, Op: "add", Added: added})
		return nil
	})
	if addWalkErr != nil {
		return nil, addWalkErr
	}
	changes = append(changes, b.skippedChanges(manifest)...)

	if filter != "" {
		kept := make([]Change, 0, 1)
		for _, change := range changes {
			if change.Path == filter {
				kept = append(kept, change)
			}
		}
		if len(kept) == 0 {
			return nil, fmt.Errorf("undo: path %q has no changes relative to the snapshot", filter)
		}
		changes = kept
	}
	sort.Slice(changes, func(left, right int) bool { return changes[left].Path < changes[right].Path })
	return changes, nil
}

// Revert 实现 Backend。
func (b *fileBackend) Revert(ctx context.Context, turn int, path string) ([]Change, error) {
	changes, err := b.Changes(ctx, turn, path)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	manifest, manifestErr := b.manifest(turn)
	entries := make(map[string]snapshotEntry, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		entries[entry.Path] = entry
	}
	for index := range changes {
		change := &changes[index]
		if change.Reason != "" {
			// 未快照的文件：报告但不改动。
			continue
		}
		switch change.Op {
		case "add":
			if removeErr := removeRevertedAdd(b.fullPath(change.Path)); removeErr != nil {
				change.Reason = "not-reverted: " + removeErr.Error()
				continue
			}
			change.Reverted = true
		default:
			if manifestErr != nil {
				change.Reason = "not-reverted: " + manifestErr.Error()
				continue
			}
			entry, ok := entries[change.Path]
			if !ok {
				change.Reason = "not-reverted: the file is not in the snapshot manifest"
				continue
			}
			if restoreErr := restoreFile(b.root, b.copyPath(turn, change.Path), b.fullPath(change.Path), fs.FileMode(entry.Mode)); restoreErr != nil {
				change.Reason = "not-reverted: " + restoreErr.Error()
				continue
			}
			change.Reverted = true
		}
	}
	return changes, nil
}

// Prune 实现 Backend。
func (b *fileBackend) Prune(ctx context.Context, keep int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if keep <= 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	turns := b.turnsLocked()
	for len(turns) > keep {
		if err := os.RemoveAll(b.turnDir(turns[0])); err != nil {
			return fmt.Errorf("undo: remove snapshot for turn %d: %w", turns[0], err)
		}
		turns = turns[1:]
	}
	return nil
}

// typeChangeToDir 标记"快照里是文件、工作区里成了目录"的类型变化。
const typeChangeToDir = "file->dir"

// diffEntry 比较清单里的一个文件与工作区当前状态。changed 为 false 表示没动过。
//
// 类型判定一律用 Lstat：跟随符号链接会把悬空链接当成"已删除"，也会让后面的写入跑到
// 工作区外。当前路径不是普通文件时这里**不写**，只如实报告（Reason 非空 = 不可回退）。
func (b *fileBackend) diffEntry(ctx context.Context, turn int, entry snapshotEntry) (Change, bool) {
	if err := ctx.Err(); err != nil {
		return Change{}, false
	}
	current := b.fullPath(entry.Path)
	info, statErr := os.Lstat(current)
	if statErr != nil {
		if isMissingPath(statErr) {
			// 本回合删除的文件（或父路径被换成了文件/链接，路径已不可达）：内容还在
			// 快照里，回退时写回。
			return Change{Path: entry.Path, Op: "delete", Removed: b.lineCountInSnapshot(turn, entry.Path)}, true
		}
		return Change{Path: entry.Path, Op: "modify", Reason: "not-reverted: " + statErr.Error()}, true
	}
	if !info.Mode().IsRegular() {
		// 符号链接/目录/设备文件：报告类型变化，不改动它。
		return Change{
			Path:       entry.Path,
			Op:         "modify",
			TypeChange: "file->" + typeWord(info.Mode()),
			Reason: "not-reverted: the workspace path is now a " + describeFileMode(info.Mode()) +
				" (the snapshot has it as a regular file); left as-is",
		}, true
	}
	oldData, oldErr := os.ReadFile(b.copyPath(turn, entry.Path))
	if oldErr != nil {
		return Change{Path: entry.Path, Op: "modify", Reason: "not-reverted: snapshot copy is unreadable: " + oldErr.Error()}, true
	}
	newData, newErr := os.ReadFile(current)
	if newErr != nil {
		return Change{Path: entry.Path, Op: "modify", Reason: "not-reverted: " + newErr.Error()}, true
	}
	if bytes.Equal(oldData, newData) {
		if info.Mode().Perm() == fs.FileMode(entry.Mode) {
			return Change{}, false
		}
		return Change{Path: entry.Path, Op: "modify", modeOnly: true}, true
	}
	added, removed := countLineDelta(oldData, newData)
	return Change{Path: entry.Path, Op: "modify", Added: added, Removed: removed}, true
}

// lineCountInSnapshot 返回快照副本的行数（二进制视为 0）。
func (b *fileBackend) lineCountInSnapshot(turn int, rel string) int {
	data, err := os.ReadFile(b.copyPath(turn, rel))
	if err != nil {
		return 0
	}
	return lineCount(data)
}

// skippedChanges 把"本回合确实被动过的未快照文件"变成不可回退的变更。
func (b *fileBackend) skippedChanges(manifest fileManifest) []Change {
	changes := make([]Change, 0, len(manifest.Skipped))
	for _, skip := range manifest.Skipped {
		info, err := os.Lstat(b.fullPath(skip.Path))
		switch {
		case err != nil && isMissingPath(err):
			changes = append(changes, Change{Path: skip.Path, Op: "delete", Reason: skip.Reason + " (it is gone now)"})
		case err != nil:
			changes = append(changes, Change{Path: skip.Path, Op: "modify", Reason: skip.Reason})
		case info.Size() != skip.Size || info.ModTime().UnixNano() != skip.ModTime:
			changes = append(changes, Change{Path: skip.Path, Op: "modify", Reason: skip.Reason})
		}
	}
	return changes
}

// walk 遍历工作区里的普通文件。它跳过快照目录本身、`.git` 与非常规文件
// （符号链接、设备文件等不参与快照）。
func (b *fileBackend) walk(ctx context.Context, visit func(rel string, info fs.FileInfo) error) error {
	return filepath.WalkDir(b.root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// 权限/竞态导致的个别错误不打断整次遍历。
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || b.isStateDir(current) {
				return fs.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(b.root, current)
		if err != nil {
			return nil
		}
		return visit(filepath.ToSlash(rel), info)
	})
}

// isStateDir 报告一个目录是否就是（或位于）实际状态目录里。
//
// 判定用 stateDirIgnoreRules 给出的规则，与 git 后端共用同一个来源（不再单独比较
// b.dir 的绝对路径）。规则是严格前缀匹配：`<root>/snapshots` 不会把
// `<root>/sub/snapshots` 一起跳过。
func (b *fileBackend) isStateDir(current string) bool {
	rel, err := filepath.Rel(b.root, current)
	if err != nil {
		return false
	}
	return underStateRule(b.stateRules, filepath.ToSlash(rel))
}

// manifest 读回合的清单。
func (b *fileBackend) manifest(turn int) (fileManifest, error) {
	path := filepath.Join(b.turnDir(turn), manifestFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileManifest{}, fmt.Errorf("undo: turn %d has no snapshot in %q", turn, b.dir)
	}
	if err != nil {
		return fileManifest{}, fmt.Errorf("undo: read snapshot manifest for turn %d: %w", turn, err)
	}
	var manifest fileManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fileManifest{}, fmt.Errorf("undo: parse snapshot manifest for turn %d: %w", turn, err)
	}
	return manifest, nil
}

// turnsLocked 返回目录里仍然完整的回合号（升序）。调用方持锁。
func (b *fileBackend) turnsLocked() []int {
	entries, err := os.ReadDir(b.turnsRoot())
	if err != nil {
		return nil
	}
	turns := make([]int, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		turn, err := strconv.Atoi(entry.Name())
		if err != nil || turn <= 0 {
			continue
		}
		// 只有清单齐了才算一个可用回合（崩溃留下的半份快照不算）。
		if _, err := os.Stat(filepath.Join(b.turnsRoot(), entry.Name(), manifestFileName)); err != nil {
			continue
		}
		turns = append(turns, turn)
	}
	sort.Ints(turns)
	return turns
}

// pruneLocked 只保留最近 b.retain 个回合（含刚拍的这一份）。调用方持锁。
func (b *fileBackend) pruneLocked() error {
	turns := b.turnsLocked()
	for len(turns) > b.retain {
		// 刚拍的这一份是最大回合号，永远不会是被淘汰的那一份。
		oldest := turns[0]
		if err := os.RemoveAll(b.turnDir(oldest)); err != nil {
			return fmt.Errorf("undo: remove snapshot for turn %d: %w", oldest, err)
		}
		turns = turns[1:]
	}
	return nil
}

// writeManifest 原子写入清单。
func writeManifest(turnDir string, manifest fileManifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("undo: encode snapshot manifest: %w", err)
	}
	target := filepath.Join(turnDir, manifestFileName)
	temporary := target + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("undo: write snapshot manifest: %w", err)
	}
	if err := os.Rename(temporary, target); err != nil {
		return fmt.Errorf("undo: replace snapshot manifest: %w", err)
	}
	return nil
}

// copyFile 复制一个普通文件并设置权限位。
func copyFile(source, target string, mode fs.FileMode) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(target, data, mode); err != nil {
		return err
	}
	return os.Chmod(target, mode)
}

// readSnapshotCopy 读快照副本。
//
// 它是变量而不是直接写 os.ReadFile：测试需要模拟"类型检查通过之后副本才读不出来"
// （并发删除、I/O 错误），用来钉住 restoreFile 的顺序——读副本必须发生在删目标之前。
var readSnapshotCopy = os.ReadFile

// writeRestoredFile 把回退内容写进工作区。
//
// 同样是替身点：测试用它制造"目标已经删掉、写却失败"的路径，钉住文案必须说明
// "原文件已删"，而不是含糊的 left as-is。
var writeRestoredFile = writeFileExclusive

// restoreFile 把快照副本写回工作区（必要时重建父目录）并恢复权限位。
//
// 写入路径的安全前提：副本必须是普通文件；root→target 之间不能有符号链接（否则会写到
// 工作区外）；目标可以不存在或是普通文件（只读也行——删除只需要父目录可写）。任何不是
// 普通文件的目标都拒绝写入，由调用方把它报成"未回退"。
//
// 顺序是"**先读副本、再删目标**"：旧实现先 Remove 再 ReadFile，副本恰好读不出来时
// 用户的文件已经没了，却仍报 "left as-is"。现在读失败时工作区一个字节都没动，写失败
// 时文案明确说"原文件已删"——两种失败各自与实际状态一致。
func restoreFile(root, source, target string, mode fs.FileMode) error {
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("snapshot copy %q: %w", source, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("snapshot copy %q is a %s", source, describeFileMode(info.Mode()))
	}
	if err := checkNoSymlinkParent(root, target); err != nil {
		return err
	}
	data, readErr := readSnapshotCopy(source)
	if readErr != nil {
		return fmt.Errorf("snapshot copy %q is unreadable; the workspace file was left as-is: %w", source, readErr)
	}
	if err := ensureParentDir(target); err != nil {
		return err
	}
	existing, lerr := os.Lstat(target)
	switch {
	case errors.Is(lerr, fs.ErrNotExist):
		// 目标不存在：直接创建。
	case lerr != nil:
		return lerr
	case existing.IsDir():
		return fmt.Errorf("%q is a directory now (the snapshot has it as a file); remove it to restore the file", target)
	case !existing.Mode().IsRegular():
		return fmt.Errorf("%q is a %s now; refusing to write through it", target, describeFileMode(existing.Mode()))
	default:
		// 先删再以 O_EXCL 创建：删除只需要父目录可写，所以只读（0444）目标也能回退；
		// O_EXCL 保证不会顺着"检查之后被换成的符号链接"写出去。
		if err := os.Remove(target); err != nil {
			return err
		}
	}
	if err := writeRestoredFile(target, data, mode); err != nil {
		// 走到这里时目标已经被删掉：不能再让调用方以为"原文件没动"。
		return fmt.Errorf("%q was removed but could not be restored: %w", target, err)
	}
	return nil
}

// writeFileExclusive 以 O_EXCL 创建目标并写入（调用方已确认目标不存在）。
func writeFileExclusive(target string, data []byte, mode fs.FileMode) error {
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// OpenFile 的 mode 会被 umask 削位；显式恢复清单里记录的那一份。
	return os.Chmod(target, mode)
}

// removeRevertedAdd 删除本回合新增的文件。
//
// 目录不归 undo 管（目录不跟踪）：万一某条 add 的当前路径成了目录，就报错不删，
// 避免把用户/模型新建的目录内容顺手销毁。
func removeRevertedAdd(target string) error {
	info, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%q is a directory now; leaving it alone", target)
	}
	return os.Remove(target)
}

// ensureParentDir 建好 target 的父目录。
//
// MkdirAll 失败通常意味着父链上有一个组件"曾经是目录、现在成了文件/链接"（回合内换过
// 类型）：删掉挡路的那一个（只删普通文件与符号链接，目录一律不动）再重试一次。
func ensureParentDir(target string) error {
	parent := filepath.Dir(target)
	mkErr := os.MkdirAll(parent, 0o700)
	if mkErr == nil {
		return nil
	}
	current := parent
	for {
		info, lerr := os.Lstat(current)
		switch {
		case lerr == nil:
			if info.IsDir() {
				return fmt.Errorf("create parent directory for %q: %w", target, mkErr)
			}
			if !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
				return fmt.Errorf("create parent directory for %q: %q is a %s", target, current, describeFileMode(info.Mode()))
			}
			// 先删挡路的文件/链接，再建目录（顺序不能反过来：目录建不出来）。
			if rmErr := os.Remove(current); rmErr != nil {
				return fmt.Errorf("remove %q blocking the parent directory of %q: %w", current, target, rmErr)
			}
			if retry := os.MkdirAll(parent, 0o700); retry != nil {
				return fmt.Errorf("create parent directory for %q: %w", target, retry)
			}
			return nil
		case !errors.Is(lerr, fs.ErrNotExist):
			return lerr
		}
		next := filepath.Dir(current)
		if next == current {
			return fmt.Errorf("create parent directory for %q: %w", target, mkErr)
		}
		current = next
	}
}

// checkNoSymlinkParent 确认 root→target 之间的每一级目录都不是符号链接。
//
// 回退只应该写工作区里的路径；父目录是链接时写入会落到工作区外（工具却报成功），
// 所以宁可拒绝。root 本身在 NewFileTracker 里已经解析过符号链接。
func checkNoSymlinkParent(root, target string) error {
	parent, err := filepath.Rel(root, filepath.Dir(target))
	if err != nil {
		return nil
	}
	if parent == "." {
		return nil
	}
	if parent == ".." || strings.HasPrefix(parent, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%q is outside the workspace root %q", target, root)
	}
	current := root
	for _, part := range strings.Split(parent, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			// 这一级和它下面的都不存在，没有链接可跟随。
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%q is a symbolic link; refusing to write through it", current)
		}
	}
	return nil
}

// typeWord 返回路径类型在 TypeChange 里的短名（`file->symlink` 这类文案用它）。
func typeWord(mode fs.FileMode) string {
	switch {
	case mode.IsRegular():
		return "file"
	case mode.IsDir():
		return "dir"
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	}
	return "other"
}

// describeFileMode 返回给模型看的类型描述（模型可见文案保持英文）。
func describeFileMode(mode fs.FileMode) string {
	switch typeWord(mode) {
	case "file":
		return "regular file"
	case "dir":
		return "directory"
	case "symlink":
		return "symbolic link"
	}
	return "special file"
}

// underAny 报告相对路径 rel 是否落在 paths 里的某一项之下（含自身）。
func underAny(rel string, paths []string) bool {
	for _, blocked := range paths {
		if rel == blocked || strings.HasPrefix(rel, blocked+"/") {
			return true
		}
	}
	return false
}

// isMissingPath 报告 Lstat 的错误是否等价于"这个路径不存在"。
//
// 除了 fs.ErrNotExist，ENOTDIR（父路径成了文件/链接）同样意味着该路径不可达：
// 对回退来说两者一样——文件内容还在快照里，写回时要先修好父路径。
func isMissingPath(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// lineCount 返回文本行数（二进制视为 0）。
func lineCount(data []byte) int { return len(splitLines(data)) }

// splitLines 按 '\n' 切行；空内容算 0 行，末尾换行不额外算一行。
func splitLines(data []byte) []string {
	if bytes.IndexByte(data, 0) >= 0 {
		return nil
	}
	if len(data) == 0 {
		return nil
	}
	text := strings.TrimSuffix(string(data), "\n")
	return strings.Split(text, "\n")
}

// countLineDelta 粗略统计 old→new 的增删行数：去掉公共前缀与后缀行后，剩下的行数
// 分别算作新增与删除。它不做真正的最长公共子序列，对"这一处改了多少行"足够；
// 二进制文件返回 0/0。
func countLineDelta(oldData, newData []byte) (added, removed int) {
	oldLines := splitLines(oldData)
	newLines := splitLines(newData)
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}
	return len(newLines) - prefix - suffix, len(oldLines) - prefix - suffix
}
