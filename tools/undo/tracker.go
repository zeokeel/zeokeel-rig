package undo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// Backend 是一次回合快照的存储后端。
//
// 它只回答三个问题：怎么拍（Take）、相对快照有什么变化（Changes）、怎么回到快照
// （Revert）。回合号分配、保留上限与并发编排都在 Tracker 里，因此 kit 与 undo 工具
// 不需要判断后端类型——两个后端返回的是同一套 Change。
//
// 实现：
//
//   - gitBackend（NewTracker）：internal/gitsnap 的隐藏提交，需要 git 工作区；
//   - fileBackend（NewFileTracker）：把工作区文件复制进快照目录，不依赖 git。
//
// 后端实现本身不要求并发安全：Tracker 会在调用前后串行化需要串行的部分。
type Backend interface {
	// Take 为回合 turn 拍一份快照（回合开始时调用）。失败时该回合不可撤销。
	Take(ctx context.Context, turn int) error
	// Turns 返回仍然可用的回合号（升序）。
	Turns() []int
	// Changes 返回回合 turn 相对当前工作区的差异；path 非空时只报告该路径。
	Changes(ctx context.Context, turn int, path string) ([]Change, error)
	// Revert 回退回合 turn；path 非空时只回退该路径。
	Revert(ctx context.Context, turn int, path string) ([]Change, error)
	// Prune 只保留最近 keep 个回合（keep<=0 时不动）。
	Prune(ctx context.Context, keep int) error
}

// Tracker 为工作区保存最近若干份回合快照，供 undo 工具使用。它并发安全：
// 回合号分配与后端调用都在锁外串行化（只有"分配回合号"与"读回持久化状态"在锁内），
// 因此多个并发会话可以同时拍快照而不会互相阻塞。
type Tracker struct {
	mu      sync.Mutex
	root    string
	backend Backend
	// dir 是回合号持久化目录（空表示只记在进程内）。
	dir string
	// next 是已分配的最大回合号；stateLoaded 表示是否已从 dir 读回。
	next        int
	stateLoaded bool
}

// NewTracker 创建一个绑定到 root 的 Tracker，快照后端是 git 隐藏提交。
// opts 里的 Retain 决定保留多少个回合；opts.Dir 非空时回合号会持久化在那里
// （重启后继续递增），为空时回合号只活在进程内。
func NewTracker(root string, opts Options) *Tracker {
	options := opts.withDefaults()
	return &Tracker{
		root:    root,
		dir:     strings.TrimSpace(options.Dir),
		backend: newGitBackend(root, options),
	}
}

// Begin 拍一份新快照并返回它的回合号（从 1 开始递增，跨进程继续）。超过保留上限时
// 丢弃编号最小的快照。Take 失败时回合号仍被消耗（之后的回合号不会回退），错误原样返回。
func (t *Tracker) Begin() (int, error) {
	return t.BeginContext(context.Background())
}

// BeginContext 是带取消的 Begin。
//
// git 后端的快照本身是一次 `git stash create` 语义的调用（只哈希变化的文件），不会
// 移动 HEAD；文件后端则把工作区文件复制进快照目录。失败（例如工作区不是 git 仓库）
// 只影响 undo，宿主应当记录后继续本回合。
func (t *Tracker) BeginContext(ctx context.Context) (int, error) {
	if t == nil {
		return 0, errors.New("undo: nil tracker")
	}
	t.mu.Lock()
	if !t.stateLoaded {
		// 回合号必须先读回来再分配，否则重启后会与上一进程的回合号撞车。
		persisted, err := loadTurnState(t.dir)
		if err != nil {
			t.mu.Unlock()
			return 0, err
		}
		if persisted > t.next {
			t.next = persisted
		}
		t.stateLoaded = true
	}
	t.next++
	turn := t.next
	dir := t.dir
	t.mu.Unlock()

	// 先落盘再拍快照：即使随后崩溃，分给宿主的回合号也不会被复用。
	if err := saveTurnState(dir, turn); err != nil {
		return 0, err
	}
	if err := t.backend.Take(ctx, turn); err != nil {
		return 0, err
	}
	return turn, nil
}

// Prune 只保留最近 keep 个回合引用（keep<=0 时不动）。它不影响内存里的回合映射。
func (t *Tracker) Prune(keep int) error {
	if t == nil {
		return errors.New("undo: nil tracker")
	}
	return t.backend.Prune(context.Background(), keep)
}

// Turns 返回当前保留的回合号（升序副本）。
func (t *Tracker) Turns() []int {
	if t == nil || t.backend == nil {
		return nil
	}
	return t.backend.Turns()
}

// Resolve 把 turn<=0 解析成"最近一次回合号"，并校验该回合仍然可用。
func (t *Tracker) Resolve(turn int) (int, error) {
	if t == nil {
		return 0, errors.New("undo: nil tracker")
	}
	turns := t.backend.Turns()
	if len(turns) == 0 {
		return 0, errors.New("undo: no snapshot is available; call Begin before the turn starts")
	}
	if turn <= 0 {
		return turns[len(turns)-1], nil
	}
	for _, available := range turns {
		if available == turn {
			return turn, nil
		}
	}
	return 0, fmt.Errorf("undo: turn %d is not available (kept turns: %v)", turn, turns)
}

// Changes 返回指定回合相对当前状态的差异（不落盘）。turn<=0 表示最近一次；
// path 非空时只报告该路径。
func (t *Tracker) Changes(turn int, path string) ([]Change, error) {
	return t.ChangesContext(context.Background(), turn, path)
}

// ChangesContext 是带取消的 Changes。
func (t *Tracker) ChangesContext(ctx context.Context, turn int, path string) ([]Change, error) {
	if t == nil {
		return nil, errors.New("undo: nil tracker")
	}
	resolved, err := t.Resolve(turn)
	if err != nil {
		return nil, err
	}
	return t.backend.Changes(ctx, resolved, strings.TrimSpace(path))
}

// Revert 回退到指定回合开始时的状态。turn<=0 表示最近一次。
func (t *Tracker) Revert(turn int) ([]Change, error) {
	return t.RevertPath(turn, "")
}

// RevertPath 只回退指定路径（相对工作区根）。
func (t *Tracker) RevertPath(turn int, path string) ([]Change, error) {
	return t.RevertPathContext(context.Background(), turn, path)
}

// RevertPathContext 是带取消的 RevertPath。
func (t *Tracker) RevertPathContext(ctx context.Context, turn int, path string) ([]Change, error) {
	if t == nil {
		return nil, errors.New("undo: nil tracker")
	}
	resolved, err := t.Resolve(turn)
	if err != nil {
		return nil, err
	}
	return t.backend.Revert(ctx, resolved, strings.TrimSpace(path))
}

// --- git 后端 -------------------------------------------------------------

// gitBackend 用 internal/gitsnap 的隐藏提交保存回合快照（本包从 v0.24 起的唯一后端）。
type gitBackend struct {
	root      string
	options   Options
	retain    int
	mu        sync.Mutex
	snapshots map[int]*Snapshot
}

// newGitBackend 构造 git 后端。
func newGitBackend(root string, options Options) *gitBackend {
	return &gitBackend{
		root:      root,
		options:   options,
		retain:    options.Retain,
		snapshots: make(map[int]*Snapshot),
	}
}

// Take 实现 Backend。
func (b *gitBackend) Take(ctx context.Context, turn int) error {
	snapshot, err := TakeContext(ctx, b.root, b.options)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.snapshots[turn] = snapshot
	for len(b.snapshots) > b.retain {
		oldest := 0
		for number := range b.snapshots {
			if oldest == 0 || number < oldest {
				oldest = number
			}
		}
		delete(b.snapshots, oldest)
	}
	return nil
}

// Turns 实现 Backend。
func (b *gitBackend) Turns() []int {
	b.mu.Lock()
	defer b.mu.Unlock()
	turns := make([]int, 0, len(b.snapshots))
	for number := range b.snapshots {
		turns = append(turns, number)
	}
	sort.Ints(turns)
	return turns
}

// snapshotFor 取出指定回合的快照。
func (b *gitBackend) snapshotFor(turn int) (*Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	snapshot := b.snapshots[turn]
	if snapshot == nil {
		return nil, fmt.Errorf("undo: turn %d is not available (kept turns: %v)", turn, sortedTurns(b.snapshots))
	}
	return snapshot, nil
}

// Changes 实现 Backend。
func (b *gitBackend) Changes(ctx context.Context, turn int, path string) ([]Change, error) {
	snapshot, err := b.snapshotFor(turn)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return snapshot.ChangesContext(ctx)
	}
	return snapshot.ChangesForPathContext(ctx, path)
}

// Revert 实现 Backend。
func (b *gitBackend) Revert(ctx context.Context, turn int, path string) ([]Change, error) {
	snapshot, err := b.snapshotFor(turn)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return snapshot.RevertContext(ctx)
	}
	return snapshot.RevertPathContext(ctx, path)
}

// Prune 实现 Backend。
func (b *gitBackend) Prune(ctx context.Context, keep int) error {
	return Prune(ctx, b.root, keep)
}

// sortedTurns 返回快照映射里的回合号（升序），用于错误文案。
func sortedTurns(snapshots map[int]*Snapshot) []int {
	turns := make([]int, 0, len(snapshots))
	for number := range snapshots {
		turns = append(turns, number)
	}
	sort.Ints(turns)
	return turns
}

// --- 工具 -----------------------------------------------------------------

// description 是模型可见的工具描述（稳定文案）。
//
// 定位：这是**面向用户的撤销按钮**。宿主通常在 UI 上放一个"撤销上一回合"，由用户点击；
// 模型只应在用户明确说"撤销/回退刚才的改动"时调用它，不要主动用它收拾自己的改动。
const description = "Undo the file changes made since the start of a turn (a user-facing undo button). " +
	"Call it only when the user explicitly asks to undo or revert recent changes; do not use it on your own " +
	"initiative to tidy up edits. The host snapshots the worktree at the start of each turn, and this tool " +
	"restores it. Without arguments it undoes the most recent turn; pass turn to target an earlier one, " +
	"dry_run to only preview, and path to limit the undo to one workspace-relative path. " +
	"Files the snapshot skipped (for example files above the host's per-file size limit) are reported and " +
	"left as-is, and nothing outside the workspace is affected."

// tool 是 undo 的 rig.Tool 实现。
type tool struct {
	tracker *Tracker
}

// New 返回绑定到 tracker 的 undo 工具。tracker 为 nil 时工具仍可调用，但每次调用返回
// 可读错误（宿主没有配置快照存储时，不存在可回退的状态）。
func New(tracker *Tracker) rig.Tool {
	return &tool{tracker: tracker}
}

// Name 实现 rig.Tool。
func (t *tool) Name() string { return ToolName }

// Description 实现 rig.Tool。
func (t *tool) Description() string { return description }

// Schema 实现 rig.Tool。
func (t *tool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"turn":    toolutil.IntegerProp("Turn number to revert to (default: the most recent turn)."),
		"dry_run": toolutil.BooleanProp("Report what would change without touching any file."),
		"path":    toolutil.StringProp("Workspace-relative path to revert; other changed files are left alone."),
	})
}

// undoArgs 是解码后的工具参数。
type undoArgs struct {
	Turn   int    `json:"turn"`
	DryRun bool   `json:"dry_run"`
	Path   string `json:"path"`
}

// Invoke 实现 rig.Tool。
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	var args undoArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	if t.tracker == nil {
		return toolutil.ErrorResultf("%s: no snapshot tracker is configured; the host must create one with undo.NewTracker", ToolName), nil
	}
	turn, err := t.tracker.Resolve(args.Turn)
	if err != nil {
		return toolutil.ErrorResult(err), nil
	}
	path := strings.TrimSpace(args.Path)
	if args.DryRun {
		changes, dryErr := t.tracker.Changes(turn, path)
		if dryErr != nil {
			return toolutil.ErrorResult(dryErr), nil
		}
		return toolutil.TextResult(renderResult(turn, true, path, changes)), nil
	}
	changes, revertErr := t.tracker.RevertPath(turn, path)
	if revertErr != nil {
		return toolutil.ErrorResult(revertErr), nil
	}
	return toolutil.TextResult(renderResult(turn, false, path, changes)), nil
}
