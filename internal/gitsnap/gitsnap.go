package gitsnap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// ErrNotGitRepo 表示目标目录不是 git 仓库（或其工作区）。
//
// 调用方拿到它应当**降级**为"不提供撤销"，而不是自己另写一套文件快照。
var ErrNotGitRepo = errors.New("gitsnap: not a git repository")

// 变更类型。
const (
	OpAdd    = "add"
	OpModify = "modify"
	OpDelete = "delete"
	OpRename = "rename"
)

// FileChange 是一次快照对比里的一条路径级变更。
type FileChange struct {
	// Path 是相对仓库根的斜杠路径（rename 时是目标路径）。
	Path string
	// OldPath 只在 rename 时非空（源路径）。
	OldPath string
	// Op 是 add、modify、delete 或 rename。
	Op string
	// Added/Removed 是行数（二进制文件为 0）。
	Added, Removed int
	// TypeChange 非空表示路径类型变了：file->symlink、symlink->file、symlink->symlink。
	TypeChange string
	// ModeOnly 表示内容没动、只有权限位（可执行位）变了。
	ModeOnly bool
	// Reason 非空表示该变更无法回退（例如子模块）。回退会跳过并如实报告。
	Reason string
	// Reverted 报告这次回退是否真的恢复了它（dry-run 与跳过的变更都是 false）。
	Reverted bool
}

// 默认保留的引用数量：超过就修剪最旧的。
const defaultRefRetain = 128

// refPrefix 是隐藏快照引用所在的名字空间（不碰 refs/heads、refs/stash）。
const refPrefix = "refs/rig/undo/"

// IsRepo 报告 dir 是否位于一个 git 工作区内。
func IsRepo(ctx context.Context, dir string) (bool, error) {
	if err := ensureDir(dir); err != nil {
		return false, err
	}
	out, err := git(dir).with("rev-parse", "--is-inside-work-tree").run(ctx, nil)
	if err != nil {
		if gitError(err, "not a git repository") {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// Snapshot 把工作区当前状态拍成一个隐藏提交，返回自描述的快照 ID。
//
// 它不移动 HEAD、不创建/切换分支、不改用户暂存区：所有写入都落在 `.git/objects`
// 与 `refs/rig/undo/` 下。
func Snapshot(ctx context.Context, repoDir string) (string, error) {
	root, prefix, err := requireRepo(ctx, repoDir)
	if err != nil {
		return "", err
	}
	tree, err := worktreeTree(ctx, root, prefix)
	if err != nil {
		return "", err
	}
	head, hasHead, err := headCommit(ctx, root)
	if err != nil {
		return "", err
	}
	base := ""
	if hasHead {
		headTree, treeErr := revParse(ctx, root, head+"^{tree}")
		if treeErr != nil {
			return "", treeErr
		}
		base = head
		if headTree == tree {
			// 工作区与 HEAD 完全一致：直接拿 HEAD 当"工作区状态"提交，省一个对象。
			base = head
		} else {
			base, err = commitTree(ctx, root, tree, []string{head}, "rig-undo snapshot")
			if err != nil {
				return "", err
			}
		}
	} else {
		// 还没有任何提交（git init 之后）：造一个无父提交承载当前工作区树。
		base, err = commitTree(ctx, root, tree, nil, "rig-undo snapshot (unborn)")
		if err != nil {
			return "", err
		}
	}

	untracked, err := untrackedPaths(ctx, root, prefix)
	if err != nil {
		return "", err
	}
	listBlob, err := hashObject(ctx, root, joinNUL(untracked))
	if err != nil {
		return "", err
	}
	meta, err := metaTree(ctx, root, listBlob)
	if err != nil {
		return "", err
	}
	id, err := commitTree(ctx, root, meta, []string{base}, "rig-undo snapshot metadata")
	if err != nil {
		return "", err
	}
	ref := fmt.Sprintf("%s%020d-%s", refPrefix, time.Now().UnixNano(), randomToken())
	if err := updateRef(ctx, root, ref, id); err != nil {
		return "", err
	}
	_ = pruneRefs(ctx, root, defaultRefRetain)
	return id, nil
}

// Diff 返回快照 id 与**当前工作区**之间的路径级变更（按路径排序）。
//
// 未跟踪文件同样参与对比：快照里存的未跟踪文件在回合里被改动会报 modify，回合里新建
// 的文件会报 add（回退时删除它们）。
func Diff(ctx context.Context, repoDir, id string) ([]FileChange, error) {
	root, prefix, err := requireRepo(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	return diffScoped(ctx, root, prefix, id)
}

// Revert 把工作区恢复到快照 id 的状态，返回被处理的变更。
func Revert(ctx context.Context, repoDir, id string) ([]FileChange, error) {
	return revertScoped(ctx, repoDir, id, nil)
}

// RevertPath 只恢复快照里与该路径有关的变更（相对仓库根，越界报错）。
func RevertPath(ctx context.Context, repoDir, id, target string) ([]FileChange, error) {
	cleaned, err := cleanRelative(target)
	if err != nil {
		return nil, err
	}
	return revertScoped(ctx, repoDir, id, []string{cleaned})
}

// RevertPaths 只恢复给定的一组路径（相对 repoDir，越界报错）。
//
// 调用方通常先用 Diff 拿到变更、按自己的策略过滤，再把要恢复的路径传进来——这样
// "被忽略的路径"不会因为一次全量 Revert 而被顺带改掉。
func RevertPaths(ctx context.Context, repoDir, id string, targets []string) ([]FileChange, error) {
	if len(targets) == 0 {
		return []FileChange{}, nil
	}
	cleaned := make([]string, 0, len(targets))
	for _, target := range targets {
		item, err := cleanRelative(target)
		if err != nil {
			return nil, err
		}
		cleaned = append(cleaned, item)
	}
	return revertScoped(ctx, repoDir, id, cleaned)
}

// Paths 返回快照里记录的路径列表（相对仓库根，含当时的未跟踪文件）。
func Paths(ctx context.Context, repoDir, id string) ([]string, error) {
	root, prefix, err := requireRepo(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	base, err := snapshotBase(ctx, root, id)
	if err != nil {
		return nil, err
	}
	args := []string{"ls-tree", "-r", "--name-only", "-z", base}
	if prefix != "" {
		args = append(args, "--", prefix)
	}
	out, err := git(root).with(args...).run(ctx, nil)
	if err != nil {
		return nil, err
	}
	paths := splitNUL(out)
	sort.Strings(paths)
	return paths, nil
}

// Prune 只保留最近 keep 个隐藏快照引用（keep<=0 时不动）。
func Prune(ctx context.Context, repoDir string, keep int) error {
	root, _, err := requireRepo(ctx, repoDir)
	if err != nil {
		return err
	}
	return pruneRefs(ctx, root, keep)
}

// --- 内部实现 ---------------------------------------------------------------

// requireRepo 校验 dir 在 git 工作区内，并返回仓库根与"相对根的路径前缀"。
//
// 前缀让 undo 可以只覆盖工作区根（可能是仓库的子目录），不会顺手回退整个仓库。
func requireRepo(ctx context.Context, dir string) (root, prefix string, err error) {
	ok, err := IsRepo(ctx, dir)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", fmt.Errorf("%w: %q", ErrNotGitRepo, dir)
	}
	out, err := git(dir).with("rev-parse", "--show-toplevel").run(ctx, nil)
	if err != nil {
		return "", "", err
	}
	root = strings.TrimSpace(string(out))
	out, err = git(dir).with("rev-parse", "--show-prefix").run(ctx, nil)
	if err != nil {
		return "", "", err
	}
	prefix = strings.TrimSpace(string(out))
	return root, prefix, nil
}

// worktreeTree 生成"当前工作区"的树对象 ID。
//
// 关键点：用**用户 index 的副本**做临时 index 再 `add -A`。副本保留了 stat 缓存，
// 因此没变的文件不会被重新哈希（每回合快照才有可接受的成本）；而 `.gitignore`、
// 未跟踪文件、删除、权限位、符号链接全部由 git 自己处理。
func worktreeTree(ctx context.Context, root, prefix string) (string, error) {
	indexPath, cleanup, err := tempIndex()
	if err != nil {
		return "", err
	}
	defer cleanup()
	if real, pathErr := gitIndexPath(ctx, root); pathErr == nil {
		if data, readErr := os.ReadFile(real); readErr == nil {
			if writeErr := os.WriteFile(indexPath, data, 0o600); writeErr != nil {
				return "", fmt.Errorf("gitsnap: seed temp index: %w", writeErr)
			}
			// 关键：把副本的 mtime 设成**原索引的 mtime**。
			//
			// git 的"racy git"规则是：索引条目若 mtime >= 索引文件自身的 mtime，就
			// 视为可能在同一时间片里被改写，必须重新哈希内容。副本是刚创建的（mtime
			// 是现在），所有条目都会被认为"比索引旧"从而跳过重哈希——于是
			// "同一时间片内、大小不变的改写"（测试里 i1→i2 这种）会被漏判成没变。
			// 保留原索引的 mtime 就把判定权交回给 git 本身。
			if info, statErr := os.Stat(real); statErr == nil {
				if chtErr := os.Chtimes(indexPath, info.ModTime(), info.ModTime()); chtErr != nil {
					return "", fmt.Errorf("gitsnap: timestamp temp index: %w", chtErr)
				}
			}
		}
	}
	env := "GIT_INDEX_FILE=" + indexPath
	pathspec := "."
	if prefix != "" {
		pathspec = prefix
	}
	if _, err := git(root).withEnv(env).with("add", "-A", "--", pathspec).run(ctx, nil); err != nil {
		return "", err
	}
	out, err := git(root).withEnv(env).with("write-tree").run(ctx, nil)
	if err != nil {
		return "", err
	}
	tree := strings.TrimSpace(string(out))
	if tree == "" {
		return "", errors.New("gitsnap: git write-tree returned no tree")
	}
	return tree, nil
}

// gitIndexPath 返回真实 index 的绝对路径（兼容 worktree / 自定义 GIT_DIR）。
func gitIndexPath(ctx context.Context, root string) (string, error) {
	out, err := git(root).with("rev-parse", "--git-path", "index").run(ctx, nil)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(string(out))
	if !filepath.IsAbs(name) {
		name = filepath.Join(root, name)
	}
	return filepath.Clean(name), nil
}

// headCommit 返回 HEAD 提交；还没有任何提交（git init 之后）时 hasHead=false。
//
// 用 `--verify --quiet` 探测：仓库没有提交时它不打印任何东西并以非零退出，因此
// "解析失败"就等于"空仓库"。
func headCommit(ctx context.Context, root string) (string, bool, error) {
	out, err := git(root).with("rev-parse", "--verify", "--quiet", "HEAD^{commit}").run(ctx, nil)
	if err != nil {
		return "", false, nil
	}
	head := strings.TrimSpace(string(out))
	return head, head != "", nil
}

// untrackedPaths 返回工作区里未跟踪且未被忽略的路径（受 prefix 限制）。
func untrackedPaths(ctx context.Context, root, prefix string) ([]string, error) {
	args := []string{"ls-files", "-o", "--exclude-standard", "-z"}
	if prefix != "" {
		args = append(args, "--", prefix)
	}
	out, err := git(root).with(args...).run(ctx, nil)
	if err != nil {
		return nil, err
	}
	paths := splitNUL(out)
	sort.Strings(paths)
	return paths, nil
}

// hashObject 把数据写成 blob，返回其对象 ID。
func hashObject(ctx context.Context, root string, data []byte) (string, error) {
	out, err := git(root).with("hash-object", "-w", "--stdin").run(ctx, data)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// metaTree 构造一个只含 `untracked` 条目的树（内容 = 未跟踪名单 blob）。
func metaTree(ctx context.Context, root, blob string) (string, error) {
	indexPath, cleanup, err := tempIndex()
	if err != nil {
		return "", err
	}
	defer cleanup()
	env := "GIT_INDEX_FILE=" + indexPath
	if _, err := git(root).withEnv(env).with("update-index", "--add", "--cacheinfo", "100644,"+blob+",untracked").run(ctx, nil); err != nil {
		return "", err
	}
	out, err := git(root).withEnv(env).with("write-tree").run(ctx, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// commitTree 创建提交对象（不移动任何引用）。
func commitTree(ctx context.Context, root, tree string, parents []string, message string) (string, error) {
	args := []string{"commit-tree", tree}
	for _, parent := range parents {
		args = append(args, "-p", parent)
	}
	args = append(args, "-m", message)
	out, err := git(root).withEnv(identityEnv...).with(args...).run(ctx, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// updateRef 写入隐藏引用。
func updateRef(ctx context.Context, root, ref, id string) error {
	_, err := git(root).with("update-ref", ref, id).run(ctx, nil)
	return err
}

// revParse 解析一个 revision 表达式。
func revParse(ctx context.Context, root, spec string) (string, error) {
	out, err := git(root).with("rev-parse", "--verify", spec).run(ctx, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// snapshotBase 返回快照对应的"工作区状态"提交（元数据提交的父提交）。
func snapshotBase(ctx context.Context, root, id string) (string, error) {
	commit, err := revParse(ctx, root, id+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("gitsnap: unknown snapshot %q: %w", id, err)
	}
	parents, err := commitParents(ctx, root, commit)
	if err != nil {
		return "", err
	}
	if len(parents) == 0 {
		return commit, nil
	}
	return parents[0], nil
}

// commitParents 返回提交的父提交列表。
func commitParents(ctx context.Context, root, commit string) ([]string, error) {
	out, err := git(root).with("rev-list", "--parents", "-n", "1", commit).run(ctx, nil)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(out))
	if len(fields) <= 1 {
		return nil, nil
	}
	return fields[1:], nil
}

// untrackedAtSnapshot 读取快照里记录的未跟踪名单。
func untrackedAtSnapshot(ctx context.Context, root, id string) ([]string, error) {
	commit, err := revParse(ctx, root, id+"^{commit}")
	if err != nil {
		return nil, err
	}
	out, err := git(root).with("cat-file", "blob", commit+":untracked").run(ctx, nil)
	if err != nil {
		return nil, nil // 旧快照/异常对象：当作"没有记录"
	}
	paths := splitNUL(out)
	sort.Strings(paths)
	return paths, nil
}

// diffScoped 是 Diff 的实现（root/prefix 已知）。
func diffScoped(ctx context.Context, root, prefix, id string) ([]FileChange, error) {
	base, err := snapshotBase(ctx, root, id)
	if err != nil {
		return nil, err
	}
	tree, err := worktreeTree(ctx, root, prefix)
	if err != nil {
		return nil, err
	}
	rawArgs := []string{"diff-tree", "-r", "--raw", "-z", "-M", base, tree}
	numArgs := []string{"diff-tree", "-r", "--numstat", "-z", "-M", base, tree}
	if prefix != "" {
		rawArgs = append(rawArgs, "--", prefix)
		numArgs = append(numArgs, "--", prefix)
	}
	rawOut, err := git(root).with(rawArgs...).run(ctx, nil)
	if err != nil {
		return nil, err
	}
	numOut, err := git(root).with(numArgs...).run(ctx, nil)
	if err != nil {
		return nil, err
	}
	rawEntries, err := parseRaw(rawOut)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]numStat, len(rawEntries))
	for _, stat := range parseNumstat(numOut) {
		counts[stat.path] = stat
	}
	changes := make([]FileChange, 0, len(rawEntries))
	for _, entry := range rawEntries {
		change := FileChange{Path: entry.path, OldPath: entry.oldPath}
		switch entry.status {
		case 'A':
			change.Op = OpAdd
		case 'D':
			change.Op = OpDelete
		case 'R':
			change.Op = OpRename
		case 'M', 'T':
			change.Op = OpModify
		case 'C':
			// 复制：目标路径是新增。
			change.Op = OpAdd
		default:
			change.Op = OpModify
		}
		if stat, ok := counts[change.Path]; ok {
			change.Added, change.Removed = stat.added, stat.removed
		}
		change.TypeChange = typeChangeOf(entry.oldMode, entry.newMode)
		if change.Op == OpModify && entry.oldSHA == entry.newSHA && entry.oldMode != entry.newMode {
			change.ModeOnly = true
			change.Added, change.Removed = 0, 0
		}
		if entry.oldMode == "160000" || entry.newMode == "160000" {
			change.Reason = "submodule: pointer changes cannot be reverted here"
		}
		changes = append(changes, change)
	}
	if prefix != "" {
		for index := range changes {
			changes[index].Path = trimPrefixPath(changes[index].Path, prefix)
			changes[index].OldPath = trimPrefixPath(changes[index].OldPath, prefix)
		}
	}
	sort.Slice(changes, func(left, right int) bool { return changes[left].Path < changes[right].Path })
	return changes, nil
}

// trimPrefixPath 把仓库根相对路径换成工作区根相对路径。
func trimPrefixPath(rel, prefix string) string {
	if rel == "" || prefix == "" {
		return rel
	}
	return strings.TrimPrefix(rel, prefix)
}

// revertScoped 是 Revert/RevertPath 的实现。
func revertScoped(ctx context.Context, repoDir, id string, only []string) ([]FileChange, error) {
	root, prefix, err := requireRepo(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	changes, err := diffScoped(ctx, root, prefix, id)
	if err != nil {
		return nil, err
	}
	if only != nil {
		wanted := make(map[string]bool, len(only))
		for _, target := range only {
			wanted[target] = true
		}
		filtered := make([]FileChange, 0, len(changes))
		for _, change := range changes {
			if wanted[change.Path] || wanted[change.OldPath] {
				filtered = append(filtered, change)
			}
		}
		changes = filtered
	}
	base, err := snapshotBase(ctx, root, id)
	if err != nil {
		return nil, err
	}

	// 当时未跟踪的路径要恢复成"未跟踪"：内容写回工作区，但绝不进暂存区。
	untrackedAt, err := untrackedAtSnapshot(ctx, root, id)
	if err != nil {
		return nil, err
	}
	wasUntracked := make(map[string]bool, len(untrackedAt))
	for _, rel := range untrackedAt {
		wasUntracked[rel] = true
	}

	var restoreTracked, restoreUntracked, remove, unstage []string
	for _, change := range changes {
		if change.Reason != "" {
			continue
		}
		target := change.Path
		if change.Op == OpRename {
			target = change.OldPath
		}
		repoRel := joinPrefix(prefix, target)
		switch {
		case change.Op == OpAdd:
			remove = append(remove, repoRel)
			unstage = append(unstage, repoRel)
		case wasUntracked[repoRel]:
			restoreUntracked = append(restoreUntracked, repoRel)
			// 回合里被 add 过的话，退回"未跟踪"。
			unstage = append(unstage, repoRel)
		default:
			restoreTracked = append(restoreTracked, repoRel)
		}
		if change.Op == OpRename {
			remove = append(remove, joinPrefix(prefix, change.Path))
			unstage = append(unstage, joinPrefix(prefix, change.Path))
		}
	}
	if len(restoreTracked) > 0 {
		if err := checkoutPaths(ctx, root, base, restoreTracked); err != nil {
			return nil, err
		}
	}
	if len(restoreUntracked) > 0 {
		if err := checkoutIndexPaths(ctx, root, base, restoreUntracked); err != nil {
			return nil, err
		}
	}
	if len(remove) > 0 {
		if err := removePaths(ctx, root, remove); err != nil {
			return nil, err
		}
	}
	if len(unstage) > 0 {
		if err := unstagePaths(ctx, root, unstage); err != nil {
			return nil, err
		}
	}
	applied := make([]FileChange, 0, len(changes))
	for _, change := range changes {
		if change.Reason == "" {
			change.Reverted = true
		}
		applied = append(applied, change)
	}
	return applied, nil
}

// checkoutPaths 从快照恢复指定路径（工作区 + 暂存区）。
//
// 用 `--pathspec-from-file` 避免命令行长度限制；路径列表由我们自己的 diff 产出，不含
// 用户输入。
func checkoutPaths(ctx context.Context, root, base string, paths []string) error {
	file, cleanup, err := tempFileWithData(joinNUL(paths))
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = git(root).with("checkout", base,
		"--pathspec-from-file="+file, "--pathspec-file-nul").run(ctx, nil)
	return err
}

// removePaths 删除"回合里新建"的路径（只动工作区文件）。
func removePaths(ctx context.Context, root string, paths []string) error {
	for _, rel := range paths {
		absolute := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.Remove(absolute); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("gitsnap: remove %q: %w", rel, err)
		}
	}
	return nil
}

// unstagePaths 把路径从暂存区移除（不存在于索引时是 no-op）。
//
// 回退"回合里新增的文件"与"回合里被 add 过的未跟踪文件"时都要做这一步，否则
// `git status` 会留下一条凭空出现的暂存记录。
func unstagePaths(ctx context.Context, root string, paths []string) error {
	file, cleanup, err := tempFileWithData(joinNUL(paths))
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = git(root).with("rm", "--cached", "-f", "-q", "--ignore-unmatch",
		"--pathspec-from-file="+file, "--pathspec-file-nul").run(ctx, nil)
	return err
}

// checkoutIndexPaths 只用**临时 index** 把路径写回工作区（不碰用户暂存区）。
//
// 用于"快照时未跟踪"的路径：它们要恢复内容但保持未跟踪。
func checkoutIndexPaths(ctx context.Context, root, base string, paths []string) error {
	indexPath, cleanup, err := tempIndex()
	if err != nil {
		return err
	}
	defer cleanup()
	env := "GIT_INDEX_FILE=" + indexPath
	if _, err := git(root).withEnv(env).with("read-tree", base).run(ctx, nil); err != nil {
		return err
	}
	_, err = git(root).withEnv(env).with("checkout-index", "-f", "-z", "--stdin").run(ctx, joinNUL(paths))
	return err
}

// joinPrefix 把工作区根相对路径换成仓库根相对路径。
func joinPrefix(prefix, rel string) string {
	if prefix == "" || rel == "" {
		return rel
	}
	return prefix + rel
}

// pruneRefs 只保留最近 keep 个隐藏引用。
func pruneRefs(ctx context.Context, root string, keep int) error {
	if keep <= 0 {
		return nil
	}
	out, err := git(root).with("for-each-ref", "--format=%(refname)", "--sort=refname", refPrefix).run(ctx, nil)
	if err != nil {
		return err
	}
	refs := strings.Fields(string(out))
	sort.Strings(refs)
	excess := len(refs) - keep
	for index := 0; index < excess; index++ {
		if _, err := git(root).with("update-ref", "-d", refs[index]).run(ctx, nil); err != nil {
			return err
		}
	}
	return nil
}

// cleanRelative 校验并规范化"相对仓库根的路径"。
func cleanRelative(target string) (string, error) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return "", errors.New("gitsnap: path is empty")
	}
	native := filepath.FromSlash(trimmed)
	if filepath.IsAbs(native) || strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, `\`) {
		return "", fmt.Errorf("gitsnap: absolute paths are not allowed: %q", target)
	}
	cleaned := path.Clean(filepath.ToSlash(native))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("gitsnap: path %q escapes the repository", target)
	}
	return cleaned, nil
}

// tempFileWithData 在系统临时目录写一个数据文件（避免污染工作区）。
func tempFileWithData(data []byte) (string, func(), error) {
	file, err := os.CreateTemp("", "gitsnap-*.list")
	if err != nil {
		return "", nil, fmt.Errorf("gitsnap: create list file: %w", err)
	}
	name := file.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, fmt.Errorf("gitsnap: write list file: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("gitsnap: close list file: %w", err)
	}
	return name, cleanup, nil
}

// randomToken 生成 6 字节随机串（引用名后缀；失败时用时间戳兜底）。
func randomToken() string {
	var buffer [6]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano()&0xffffffffffff)
	}
	return hex.EncodeToString(buffer[:])
}
