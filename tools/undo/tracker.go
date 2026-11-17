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

// Tracker 为工作区保存最近若干份回合快照，供 undo 工具使用。它并发安全：
// 回合号分配、快照存取与淘汰都在锁内，只有文件遍历（Take）不在锁内，因此多个并发
// 会话可以同时拍快照而不会互相阻塞。
type Tracker struct {
	mu        sync.Mutex
	root      string
	options   Options
	retain    int
	next      int
	snapshots map[int]*Snapshot
}

// NewTracker 创建一个绑定到 root 的 Tracker。opts 里的 Retain 决定保留多少个回合。
func NewTracker(root string, opts Options) *Tracker {
	options := opts.withDefaults()
	return &Tracker{
		root:      root,
		options:   options,
		retain:    options.Retain,
		snapshots: make(map[int]*Snapshot),
	}
}

// Begin 拍一份新快照并返回它的回合号（从 1 开始递增）。超过保留上限时丢弃编号最小的
// 快照。Take 失败时回合号仍被消耗（之后的回合号不会回退），错误原样返回。
func (t *Tracker) Begin() (int, error) {
	return t.BeginContext(context.Background())
}

// BeginContext 是带取消的 Begin。
//
// 快照本身是一次 `git stash create` 语义的调用（只哈希变化的文件），不会移动 HEAD；
// 失败（例如工作区不是 git 仓库）只影响 undo，宿主应当记录后继续本回合。
func (t *Tracker) BeginContext(ctx context.Context) (int, error) {
	if t == nil {
		return 0, errors.New("undo: nil tracker")
	}
	t.mu.Lock()
	t.next++
	turn := t.next
	t.mu.Unlock()

	snapshot, err := TakeContext(ctx, t.root, t.options)
	if err != nil {
		return 0, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.snapshots[turn] = snapshot
	for len(t.snapshots) > t.retain {
		oldest := 0
		for number := range t.snapshots {
			if oldest == 0 || number < oldest {
				oldest = number
			}
		}
		delete(t.snapshots, oldest)
	}
	return turn, nil
}

// Prune 只保留最近 keep 个快照引用（keep<=0 时不动）。它不影响内存里的回合映射。
func (t *Tracker) Prune(keep int) error {
	if t == nil {
		return errors.New("undo: nil tracker")
	}
	return Prune(context.Background(), t.root, keep)
}

// Turns 返回当前保留的回合号（升序副本）。
func (t *Tracker) Turns() []int {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.turnsLocked()
}

// turnsLocked 返回升序回合号（调用方持锁）。
func (t *Tracker) turnsLocked() []int {
	turns := make([]int, 0, len(t.snapshots))
	for number := range t.snapshots {
		turns = append(turns, number)
	}
	sort.Ints(turns)
	return turns
}

// Resolve 把 turn<=0 解析成"最近一次回合号"，并校验该回合仍然可用。
func (t *Tracker) Resolve(turn int) (int, error) {
	if t == nil {
		return 0, errors.New("undo: nil tracker")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.resolveLocked(turn)
}

// resolveLocked 是 Resolve 的实现（调用方持锁）。
func (t *Tracker) resolveLocked(turn int) (int, error) {
	if len(t.snapshots) == 0 {
		return 0, errors.New("undo: no snapshot is available; call Begin before the turn starts")
	}
	if turn <= 0 {
		latest := 0
		for number := range t.snapshots {
			if number > latest {
				latest = number
			}
		}
		return latest, nil
	}
	if _, ok := t.snapshots[turn]; !ok {
		return 0, fmt.Errorf("undo: turn %d is not available (kept turns: %v)", turn, t.turnsLocked())
	}
	return turn, nil
}

// snapshotFor 取出指定回合的快照与其回合号。
func (t *Tracker) snapshotFor(turn int) (*Snapshot, int, error) {
	if t == nil {
		return nil, 0, errors.New("undo: nil tracker")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	resolved, err := t.resolveLocked(turn)
	if err != nil {
		return nil, 0, err
	}
	snapshot := t.snapshots[resolved]
	if snapshot == nil {
		return nil, 0, fmt.Errorf("undo: turn %d is not available (kept turns: %v)", turn, t.turnsLocked())
	}
	return snapshot, resolved, nil
}

// Changes 返回指定回合相对当前状态的差异（不落盘）。turn<=0 表示最近一次；
// path 非空时只报告该路径。
func (t *Tracker) Changes(turn int, path string) ([]Change, error) {
	return t.ChangesContext(context.Background(), turn, path)
}

// ChangesContext 是带取消的 Changes。
func (t *Tracker) ChangesContext(ctx context.Context, turn int, path string) ([]Change, error) {
	snapshot, _, err := t.snapshotFor(turn)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(path) == "" {
		return snapshot.ChangesContext(ctx)
	}
	return snapshot.ChangesForPathContext(ctx, path)
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
	snapshot, _, err := t.snapshotFor(turn)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(path) == "" {
		return snapshot.RevertContext(ctx)
	}
	return snapshot.RevertPathContext(ctx, path)
}

// --- 工具 -----------------------------------------------------------------

// description 是模型可见的工具描述（稳定文案）。
//
// 定位：这是**面向用户的撤销按钮**。宿主通常在 UI 上放一个"撤销上一回合"，由用户点击；
// 模型只应在用户明确说"撤销/回退刚才的改动"时调用它，不要主动用它收拾自己的改动。
const description = "Undo the file changes made since the start of a turn (a user-facing undo button). " +
	"Call it only when the user explicitly asks to undo or revert recent changes; do not use it on your own " +
	"initiative to tidy up edits. Works on git repositories: the host snapshots the worktree at the start of " +
	"each turn, and this tool restores it. Without arguments it undoes the most recent turn; pass turn to target " +
	"an earlier one, dry_run to only preview, and path to limit the undo to one workspace-relative path. " +
	"Files ignored by .gitignore are never touched, and nothing outside the workspace is affected."

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
