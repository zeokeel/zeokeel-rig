package team

// 共享团队任务板：进程内 rig.TeamTaskManager 以及 team_task_create/list/get/update
// 工具。本文件是增量能力：既有成员 Manager、其工具与旧版工具保持不变。

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// 共享任务状态。它们是 TeamTask.Status 的模型可见取值。
const (
	// TaskStatusPending 表示尚无人认领的任务。
	TaskStatusPending = "pending"
	// TaskStatusInProgress 表示已被 owner 认领的任务。
	TaskStatusInProgress = "in_progress"
	// TaskStatusCompleted 表示已完成的任务。
	TaskStatusCompleted = "completed"
)

// 共享任务板的工具名。
const (
	// ToolTeamTaskCreate 创建一个共享任务。
	ToolTeamTaskCreate = "team_task_create"
	// ToolTeamTaskList 列出共享任务（可过滤）。
	ToolTeamTaskList = "team_task_list"
	// ToolTeamTaskGet 展示一个共享任务的详情。
	ToolTeamTaskGet = "team_task_get"
	// ToolTeamTaskUpdate 通过 action 变更一个共享任务。
	ToolTeamTaskUpdate = "team_task_update"
)

// 任务板默认值与限制。
const (
	// DefaultMaxTasks 是一个 TaskManager 默认存储的任务数。
	DefaultMaxTasks = 256
	// taskIDPrefix 是生成任务 id 的前缀（"task-1" 等）。
	taskIDPrefix = "task-"
	// maxTaskTableOutput 限制单次任务工具响应（32 KiB）。
	maxTaskTableOutput = 32 << 10
)

// 进程内任务管理器返回的哨兵错误。工具失败会包含其文本；宿主可用 errors.Is 匹配。
var (
	// ErrTaskNotFound 表示 id 未匹配任何任务。
	ErrTaskNotFound = errors.New("team task not found")
	// ErrTaskExists 表示 Create 使用了重复的显式 id。
	ErrTaskExists = errors.New("team task already exists")
	// ErrTaskBoardFull 表示 Create 超出管理器的任务上限。
	ErrTaskBoardFull = errors.New("team task board is full")
	// ErrTaskSubjectRequired 表示 subject 缺失或为空。
	ErrTaskSubjectRequired = errors.New("team task subject is required")
	// ErrTaskInvalidStatus 表示状态不是 pending/in_progress/completed。
	ErrTaskInvalidStatus = errors.New("invalid team task status")
	// ErrTaskOwnerRequired 表示 in_progress 任务没有 owner。
	ErrTaskOwnerRequired = errors.New("team task owner is required")
	// ErrTaskDependencyNotFound 表示 BlockedBy 引用了不存在的任务。
	ErrTaskDependencyNotFound = errors.New("team task dependency references an unknown task")
	// ErrTaskDependencyCycle 表示直接或传递的自依赖。
	ErrTaskDependencyCycle = errors.New("team task dependency cycle")
	// ErrTaskNoChanges 表示空的 TeamTaskUpdate。
	ErrTaskNoChanges = errors.New("team task update has no changes")
)

// TaskManagerOption 配置 TaskManager。
type TaskManagerOption func(*TaskManager)

// WithMaxTasks 限制管理器存储的任务数（非正数保留 DefaultMaxTasks）。
func WithMaxTasks(max int) TaskManagerOption {
	return func(manager *TaskManager) {
		if max > 0 {
			manager.maxTasks = max
		}
	}
}

// TaskManager 是并发安全的进程内 rig.TeamTaskManager。任务保留创建时间；List 按 Created
// （再按 id）排序返回。
type TaskManager struct {
	mu       sync.Mutex
	seq      uint64
	maxTasks int
	tasks    map[string]rig.TeamTask
}

var _ rig.TeamTaskManager = (*TaskManager)(nil)

// NewTaskManager 返回一个空的进程内任务板（默认 256 条任务）。
func NewTaskManager(opts ...TaskManagerOption) *TaskManager {
	manager := &TaskManager{maxTasks: DefaultMaxTasks, tasks: make(map[string]rig.TeamTask)}
	for _, opt := range opts {
		if opt != nil {
			opt(manager)
		}
	}
	return manager
}

// Create 实现 rig.TeamTaskManager。缺失的 id 会生成（"task-N"），缺失的 status 默认为
// pending，Created/Updated 默认为当前时间。BlockedBy id 必须存在；write scopes 会 trim
// 并去重。
func (m *TaskManager) Create(ctx context.Context, task rig.TeamTask) (rig.TeamTask, error) {
	if err := contextError(ctx); err != nil {
		return rig.TeamTask{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tasks == nil {
		m.tasks = make(map[string]rig.TeamTask)
	}
	if len(m.tasks) >= m.maxTasks {
		return rig.TeamTask{}, fmt.Errorf("%w (limit %d)", ErrTaskBoardFull, m.maxTasks)
	}

	task.ID = strings.TrimSpace(task.ID)
	task.Subject = strings.TrimSpace(task.Subject)
	if task.Subject == "" {
		return rig.TeamTask{}, ErrTaskSubjectRequired
	}
	status := strings.ToLower(strings.TrimSpace(task.Status))
	if status == "" {
		status = TaskStatusPending
	}
	if !validTaskStatus(status) {
		return rig.TeamTask{}, invalidTaskStatusError(task.Status)
	}
	task.Status = status
	task.Owner = strings.TrimSpace(task.Owner)
	if status == TaskStatusInProgress && task.Owner == "" {
		return rig.TeamTask{}, fmt.Errorf("%w: in_progress tasks need an owner", ErrTaskOwnerRequired)
	}
	// 在分配 id 之前先校验依赖，使被拒绝的 Create 不消耗任务号。
	dependencies, err := m.cleanDependenciesLocked("", task.BlockedBy)
	if err != nil {
		return rig.TeamTask{}, err
	}
	if task.ID != "" {
		if _, exists := m.tasks[task.ID]; exists {
			return rig.TeamTask{}, fmt.Errorf("%w: %q", ErrTaskExists, task.ID)
		}
	} else {
		m.seq++
		task.ID = taskIDPrefix + strconv.FormatUint(m.seq, 10)
	}
	if slices.Contains(dependencies, task.ID) {
		return rig.TeamTask{}, fmt.Errorf("%w: task %q cannot depend on itself", ErrTaskDependencyCycle, task.ID)
	}
	task.BlockedBy = dependencies
	task.WriteScopes = cleanStringList(task.WriteScopes)
	now := time.Now().UTC()
	if task.Created.IsZero() {
		task.Created = now
	}
	task.Updated = task.Created
	m.tasks[task.ID] = cloneTask(task)
	return cloneTask(task), nil
}

// List 实现 rig.TeamTaskManager。过滤字段可选；设置 Status 时必须是已知状态之一。
func (m *TaskManager) List(ctx context.Context, filter rig.TeamTaskFilter) ([]rig.TeamTask, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	status := strings.ToLower(strings.TrimSpace(filter.Status))
	if status != "" && !validTaskStatus(status) {
		return nil, invalidTaskStatusError(filter.Status)
	}
	owner := strings.TrimSpace(filter.Owner)
	tasks := make([]rig.TeamTask, 0, len(m.tasks))
	for _, task := range m.tasks {
		if status != "" && task.Status != status {
			continue
		}
		if owner != "" && task.Owner != owner {
			continue
		}
		tasks = append(tasks, cloneTask(task))
	}
	sortTasks(tasks)
	return tasks, nil
}

// Get 实现 rig.TeamTaskManager。
func (m *TaskManager) Get(ctx context.Context, id string) (rig.TeamTask, error) {
	if err := contextError(ctx); err != nil {
		return rig.TeamTask{}, err
	}
	id = strings.TrimSpace(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return rig.TeamTask{}, fmt.Errorf("%w: %q", ErrTaskNotFound, id)
	}
	return cloneTask(task), nil
}

// Update 实现 rig.TeamTaskManager。Nil 字段保持不变；新增依赖会校验存在性与直接/传递
// 环，in_progress 任务始终保留 owner。不产生任何变化的更新返回 ErrTaskNoChanges。
func (m *TaskManager) Update(ctx context.Context, id string, update rig.TeamTaskUpdate) (rig.TeamTask, error) {
	if err := contextError(ctx); err != nil {
		return rig.TeamTask{}, err
	}
	id = strings.TrimSpace(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return rig.TeamTask{}, fmt.Errorf("%w: %q", ErrTaskNotFound, id)
	}

	changed := false
	if update.Subject != nil {
		subject := strings.TrimSpace(*update.Subject)
		if subject == "" {
			return rig.TeamTask{}, ErrTaskSubjectRequired
		}
		if task.Subject != subject {
			task.Subject = subject
			changed = true
		}
	}
	if update.Description != nil {
		description := strings.TrimSpace(*update.Description)
		if task.Description != description {
			task.Description = description
			changed = true
		}
	}
	if update.Status != nil {
		status := strings.ToLower(strings.TrimSpace(*update.Status))
		if !validTaskStatus(status) {
			return rig.TeamTask{}, invalidTaskStatusError(*update.Status)
		}
		if task.Status != status {
			task.Status = status
			changed = true
		}
	}
	if update.Owner != nil {
		owner := strings.TrimSpace(*update.Owner)
		if task.Owner != owner {
			task.Owner = owner
			changed = true
		}
	}
	if update.WriteScopes != nil {
		scopes := cleanStringList(*update.WriteScopes)
		if !equalStrings(task.WriteScopes, scopes) {
			task.WriteScopes = scopes
			changed = true
		}
	}
	if len(update.AddBlocked) > 0 || len(update.DelBlocked) > 0 {
		dependencies := applyDependencyChanges(task.BlockedBy, update.DelBlocked, update.AddBlocked)
		cleaned, err := m.cleanDependenciesLocked(task.ID, dependencies)
		if err != nil {
			return rig.TeamTask{}, err
		}
		if err := m.checkDependencyCycleLocked(task.ID, cleaned); err != nil {
			return rig.TeamTask{}, err
		}
		if !equalStrings(task.BlockedBy, cleaned) {
			task.BlockedBy = cleaned
			changed = true
		}
	}
	if !changed {
		return rig.TeamTask{}, ErrTaskNoChanges
	}
	if task.Status == TaskStatusInProgress && task.Owner == "" {
		return rig.TeamTask{}, fmt.Errorf("%w: in_progress tasks need an owner", ErrTaskOwnerRequired)
	}
	now := time.Now().UTC()
	if task.Created.IsZero() {
		task.Created = now
	}
	task.Updated = now
	m.tasks[task.ID] = cloneTask(task)
	return cloneTask(task), nil
}

// cleanDependenciesLocked 对依赖 id 做 trim/去重，并校验每个都存在且不是任务自身。
func (m *TaskManager) cleanDependenciesLocked(taskID string, dependencies []string) ([]string, error) {
	cleaned := cleanStringList(dependencies)
	for _, dependency := range cleaned {
		if dependency == taskID {
			return nil, fmt.Errorf("%w: task %q cannot depend on itself", ErrTaskDependencyCycle, taskID)
		}
		if _, ok := m.tasks[dependency]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrTaskDependencyNotFound, dependency)
		}
	}
	return cleaned, nil
}

// checkDependencyCycleLocked 在任一依赖（直接或传递地）依赖 taskID 时报告环。
func (m *TaskManager) checkDependencyCycleLocked(taskID string, dependencies []string) error {
	if len(dependencies) == 0 {
		return nil
	}
	type visitState uint8
	const (
		unvisited visitState = iota
		visiting
		done
	)
	states := make(map[string]visitState, len(m.tasks))
	var reaches func(current string) bool
	reaches = func(current string) bool {
		if current == taskID {
			return true
		}
		switch states[current] {
		case visiting, done:
			return false
		}
		states[current] = visiting
		for _, dependency := range m.tasks[current].BlockedBy {
			if reaches(dependency) {
				return true
			}
		}
		states[current] = done
		return false
	}
	for _, dependency := range dependencies {
		if reaches(dependency) {
			return fmt.Errorf("%w: task %q would depend on itself through %q", ErrTaskDependencyCycle, taskID, dependency)
		}
	}
	return nil
}

// validTaskStatus 判断 status 是否为三个已知取值之一。
func validTaskStatus(status string) bool {
	switch status {
	case TaskStatusPending, TaskStatusInProgress, TaskStatusCompleted:
		return true
	default:
		return false
	}
}

// invalidTaskStatusError 构造一个可读的状态错误。
func invalidTaskStatusError(status string) error {
	return fmt.Errorf("%w: %q (want %s, %s or %s)", ErrTaskInvalidStatus, strings.TrimSpace(status),
		TaskStatusPending, TaskStatusInProgress, TaskStatusCompleted)
}

// applyDependencyChanges 先移除 DelBlocked 再加入 AddBlocked，并去重。
func applyDependencyChanges(existing, remove, add []string) []string {
	removed := make(map[string]struct{}, len(remove))
	for _, id := range remove {
		if id = strings.TrimSpace(id); id != "" {
			removed[id] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(existing)+len(add))
	out := make([]string, 0, len(existing)+len(add))
	for _, id := range existing {
		if _, drop := removed[id]; drop {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, id := range add {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, drop := removed[id]; drop {
			// 同一个 id 同时出现在两个列表中时，显式移除优先。
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// cleanStringList 对字符串做 trim、丢弃空值并去重，保留首次出现顺序。
func cleanStringList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// equalStrings 判断两个字符串切片是否逐元素相等。
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

// cloneTask 返回 task 的深拷贝，使调用方无法修改已存的切片。
func cloneTask(task rig.TeamTask) rig.TeamTask {
	task.BlockedBy = slices.Clone(task.BlockedBy)
	task.WriteScopes = slices.Clone(task.WriteScopes)
	return task
}

// sortTasks 按创建时间、再按数字 id 排序任务。
func sortTasks(tasks []rig.TeamTask) {
	slices.SortStableFunc(tasks, func(a, b rig.TeamTask) int {
		if order := a.Created.Compare(b.Created); order != 0 {
			return order
		}
		left, leftOK := taskIDNumber(a.ID)
		right, rightOK := taskIDNumber(b.ID)
		if leftOK && rightOK {
			return cmp.Compare(left, right)
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// taskIDNumber 解析生成任务 id 的数字后缀。
func taskIDNumber(id string) (uint64, bool) {
	if !strings.HasPrefix(id, taskIDPrefix) {
		return 0, false
	}
	number, err := strconv.ParseUint(strings.TrimPrefix(id, taskIDPrefix), 10, 64)
	if err != nil {
		return 0, false
	}
	return number, true
}

// --- 工具 ------------------------------------------------------------------

// TaskToolNames 按 TaskTools 的顺序返回共享任务板工具名。
func TaskToolNames() []string {
	return []string{ToolTeamTaskCreate, ToolTeamTaskList, ToolTeamTaskGet, ToolTeamTaskUpdate}
}

// TaskTools 返回绑定到 manager 的四个共享任务板工具。构造时允许 manager 为 nil；此时
// 每次调用返回可读的配置错误（宿主默认传入 NewTaskManager）。
func TaskTools(manager rig.TeamTaskManager) []rig.Tool {
	return []rig.Tool{
		taskCreateTool(manager),
		taskListTool(manager),
		taskGetTool(manager),
		taskUpdateTool(manager),
	}
}

// Tools 返回共享任务板工具。它是 TaskTools 的别名，便于接线任务管理器的宿主不必记住
// 另一个名字。
func Tools(manager rig.TeamTaskManager) []rig.Tool { return TaskTools(manager) }

// requireTaskManager 在工具没有 manager 时返回模型可见的配置错误结果。
func requireTaskManager(toolName string, manager rig.TeamTaskManager) (rig.ToolResult, bool) {
	if manager == nil {
		return toolutil.ErrorResultf("%s: team task manager is not configured (pass NewTaskManager)", toolName), true
	}
	return rig.ToolResult{}, false
}

// taskCreateTool 实现 team_task_create。
func taskCreateTool(manager rig.TeamTaskManager) rig.Tool {
	return &teamTool{
		name: ToolTeamTaskCreate,
		description: "Create one shared team task on the task board. The task starts in status pending " +
			"unless it is created blocked by other tasks; dependencies must reference existing task ids. " +
			"Use team_task_update to claim/complete/reopen/release it or to edit its fields and dependencies. " +
			"Subjects must be non-empty; blockers are shown by team_task_list and team_task_get.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"subject":      toolutil.StringProp("Short imperative title of the task (required, non-empty)."),
			"description":  toolutil.StringProp("Longer details for the task (optional)."),
			"owner":        toolutil.StringProp("Initial owner name; claiming normally sets this with team_task_update action=claim (optional)."),
			"blocked_by":   toolutil.StringArrayProp("Task ids this task depends on; all must already exist."),
			"write_scopes": toolutil.StringArrayProp("Workspace-relative files/directories this task expects to modify (advisory)."),
		}, "subject"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args struct {
				Subject     string   `json:"subject"`
				Description string   `json:"description"`
				Owner       string   `json:"owner"`
				BlockedBy   []string `json:"blocked_by"`
				WriteScopes []string `json:"write_scopes"`
			}
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := contextError(ctx); err != nil {
				return rig.ToolResult{}, err
			}
			if result, missing := requireTaskManager(ToolTeamTaskCreate, manager); missing {
				return result, nil
			}
			task, err := manager.Create(ctx, rig.TeamTask{
				Subject:     args.Subject,
				Description: args.Description,
				Owner:       args.Owner,
				BlockedBy:   args.BlockedBy,
				WriteScopes: args.WriteScopes,
			})
			if err != nil {
				return toolutil.ErrorResultf("%s: %v", ToolTeamTaskCreate, err), nil
			}
			return toolutil.TextResult("created shared team task\n" + formatTaskDetail(task)), nil
		},
	}
}

// taskListTool 实现 team_task_list。
func taskListTool(manager rig.TeamTaskManager) rig.Tool {
	return &teamTool{
		name: ToolTeamTaskList,
		description: "List shared team tasks ordered by creation time, optionally filtered by status and/or owner. " +
			"Columns: ID, STATUS, OWNER, SUBJECT, BLOCKED BY. Use team_task_get for the full description and scope details.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"status": stringEnumProp("Only tasks with this status (pending, in_progress or completed).", TaskStatusPending, TaskStatusInProgress, TaskStatusCompleted),
			"owner":  toolutil.StringProp("Only tasks owned by this exact owner name."),
		}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args struct {
				Status string `json:"status"`
				Owner  string `json:"owner"`
			}
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := contextError(ctx); err != nil {
				return rig.ToolResult{}, err
			}
			if result, missing := requireTaskManager(ToolTeamTaskList, manager); missing {
				return result, nil
			}
			tasks, err := manager.List(ctx, rig.TeamTaskFilter{Status: args.Status, Owner: args.Owner})
			if err != nil {
				return toolutil.ErrorResultf("%s: %v", ToolTeamTaskList, err), nil
			}
			return toolutil.TextResult(formatTaskTable(tasks)), nil
		},
	}
}

// taskGetTool 实现 team_task_get。
func taskGetTool(manager rig.TeamTaskManager) rig.Tool {
	return &teamTool{
		name: ToolTeamTaskGet,
		description: "Show one shared team task in detail: subject, status, owner, dependencies, write scopes, " +
			"timestamps and the full description. Unknown ids are reported as errors.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"id": toolutil.StringProp("Task id shown by team_task_create or team_task_list (required)."),
		}, "id"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := contextError(ctx); err != nil {
				return rig.ToolResult{}, err
			}
			if result, missing := requireTaskManager(ToolTeamTaskGet, manager); missing {
				return result, nil
			}
			id := strings.TrimSpace(args.ID)
			if id == "" {
				return toolutil.ErrorResultf("%s: id is required", ToolTeamTaskGet), nil
			}
			task, err := manager.Get(ctx, id)
			if err != nil {
				return toolutil.ErrorResultf("%s: %v", ToolTeamTaskGet, err), nil
			}
			return toolutil.TextResult(formatTaskDetail(task)), nil
		},
	}
}

// taskUpdateTool 实现 team_task_update。
func taskUpdateTool(manager rig.TeamTaskManager) rig.Tool {
	return &teamTool{
		name: ToolTeamTaskUpdate,
		description: "Mutate one shared team task through an action: " +
			"claim (owner required; sets status in_progress and owner), complete (status completed), " +
			"reopen (status pending), release (status pending and clears owner), " +
			"edit (update subject/description/write_scopes and optional status), " +
			"set_dependencies (add_blocked_by/remove_blocked_by; cycles and unknown ids are refused), " +
			"reassign (owner required; changes the owner without claiming). " +
			"Unknown actions, ids, statuses and invalid dependency graphs are readable errors.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"id":                toolutil.StringProp("Task id to update (required)."),
			"action":            stringEnumProp("Action to perform.", "claim", "complete", "reopen", "release", "edit", "set_dependencies", "reassign"),
			"subject":           toolutil.StringProp("New subject (action=edit)."),
			"description":       toolutil.StringProp("New description (action=edit)."),
			"status":            stringEnumProp("New status for action=edit (pending, in_progress or completed).", TaskStatusPending, TaskStatusInProgress, TaskStatusCompleted),
			"owner":             toolutil.StringProp("Owner for action=claim or action=reassign (required there)."),
			"add_blocked_by":    toolutil.StringArrayProp("Task ids to add as dependencies (action=set_dependencies)."),
			"remove_blocked_by": toolutil.StringArrayProp("Task ids to remove from dependencies (action=set_dependencies)."),
			"write_scopes":      toolutil.StringArrayProp("Replacement advisory write scopes (action=edit)."),
		}, "id", "action"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args struct {
				ID              string    `json:"id"`
				Action          string    `json:"action"`
				Subject         *string   `json:"subject"`
				Description     *string   `json:"description"`
				Status          *string   `json:"status"`
				Owner           *string   `json:"owner"`
				AddBlockedBy    []string  `json:"add_blocked_by"`
				RemoveBlockedBy []string  `json:"remove_blocked_by"`
				WriteScopes     *[]string `json:"write_scopes"`
			}
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := contextError(ctx); err != nil {
				return rig.ToolResult{}, err
			}
			if result, missing := requireTaskManager(ToolTeamTaskUpdate, manager); missing {
				return result, nil
			}
			id := strings.TrimSpace(args.ID)
			if id == "" {
				return toolutil.ErrorResultf("%s: id is required", ToolTeamTaskUpdate), nil
			}
			action := strings.ToLower(strings.TrimSpace(args.Action))
			if action == "" {
				return toolutil.ErrorResultf("%s: action is required", ToolTeamTaskUpdate), nil
			}

			update, errResult := taskUpdateFor(action, args.Subject, args.Description, args.Status,
				args.Owner, args.AddBlockedBy, args.RemoveBlockedBy, args.WriteScopes)
			if errResult != "" {
				return toolutil.ErrorResultf("%s: %s", ToolTeamTaskUpdate, errResult), nil
			}
			if update == nil {
				return toolutil.ErrorResultf("%s: unknown action %q (claim, complete, reopen, release, edit, set_dependencies, reassign)", ToolTeamTaskUpdate, args.Action), nil
			}
			task, err := manager.Update(ctx, id, *update)
			if err != nil {
				return toolutil.ErrorResultf("%s: %v", ToolTeamTaskUpdate, err), nil
			}
			return toolutil.TextResult(fmt.Sprintf("updated shared team task %s (status=%s, owner=%s, blocked_by=%s)",
				task.ID, task.Status, displayOwner(task.Owner), displayList(task.BlockedBy))), nil
		},
	}
}

// taskUpdateFor 把一个工具 action 翻译为 rig.TeamTaskUpdate。对模型可见的校验失败返回
// 非空错误字符串；未知 action 返回 nil update。
func taskUpdateFor(action string, subject, description, status, owner *string,
	addBlockedBy, removeBlockedBy []string, writeScopes *[]string) (*rig.TeamTaskUpdate, string) {
	switch action {
	case "claim":
		claimOwner := strings.TrimSpace(deref(owner))
		if claimOwner == "" {
			return nil, "owner is required for action: claim"
		}
		inProgress := TaskStatusInProgress
		return &rig.TeamTaskUpdate{Status: &inProgress, Owner: &claimOwner}, ""
	case "complete":
		completed := TaskStatusCompleted
		return &rig.TeamTaskUpdate{Status: &completed}, ""
	case "reopen":
		pending := TaskStatusPending
		return &rig.TeamTaskUpdate{Status: &pending}, ""
	case "release":
		pending := TaskStatusPending
		empty := ""
		return &rig.TeamTaskUpdate{Status: &pending, Owner: &empty}, ""
	case "reassign":
		newOwner := strings.TrimSpace(deref(owner))
		if newOwner == "" {
			return nil, "owner is required for action: reassign"
		}
		return &rig.TeamTaskUpdate{Owner: &newOwner}, ""
	case "edit":
		update := &rig.TeamTaskUpdate{Subject: subject, Description: description, Status: status, WriteScopes: writeScopes}
		if owner != nil {
			update.Owner = owner
		}
		if update.Subject == nil && update.Description == nil && update.Status == nil &&
			update.WriteScopes == nil && update.Owner == nil {
			return nil, "nothing to edit: provide subject, description, status, owner or write_scopes"
		}
		return update, ""
	case "set_dependencies":
		if len(cleanStringList(addBlockedBy)) == 0 && len(cleanStringList(removeBlockedBy)) == 0 {
			return nil, "nothing to update: provide add_blocked_by or remove_blocked_by"
		}
		return &rig.TeamTaskUpdate{AddBlocked: addBlockedBy, DelBlocked: removeBlockedBy}, ""
	default:
		return nil, ""
	}
}

// deref 返回 *value；nil 时返回 ""。
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// formatTaskTable 渲染 team_task_list 表格。
func formatTaskTable(tasks []rig.TeamTask) string {
	if len(tasks) == 0 {
		return "no shared team tasks"
	}
	var builder strings.Builder
	writer := tabwriter.NewWriter(&builder, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tSTATUS\tOWNER\tSUBJECT\tBLOCKED BY")
	for _, task := range tasks {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			task.ID, task.Status, displayOwner(task.Owner), oneLineTaskText(task.Subject), displayList(task.BlockedBy))
	}
	_ = writer.Flush()
	return toolutil.TruncateBytes(strings.TrimSuffix(builder.String(), "\n"), maxTaskTableOutput, "\n[truncated: task output exceeded 32 KiB]")
}

// formatTaskDetail 为 team_task_create/team_task_get 渲染单个任务。
func formatTaskDetail(task rig.TeamTask) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "id: %s\n", task.ID)
	fmt.Fprintf(&builder, "subject: %s\n", oneLineTaskText(task.Subject))
	fmt.Fprintf(&builder, "status: %s\n", task.Status)
	fmt.Fprintf(&builder, "owner: %s\n", displayOwner(task.Owner))
	fmt.Fprintf(&builder, "blocked_by: %s\n", displayList(task.BlockedBy))
	fmt.Fprintf(&builder, "write_scopes: %s\n", displayList(task.WriteScopes))
	fmt.Fprintf(&builder, "created: %s\n", formatTaskTime(task.Created))
	fmt.Fprintf(&builder, "updated: %s\n", formatTaskTime(task.Updated))
	if description := strings.TrimSpace(task.Description); description != "" {
		fmt.Fprintf(&builder, "description:\n%s\n", description)
	}
	return toolutil.TruncateBytes(strings.TrimSuffix(builder.String(), "\n"), maxTaskTableOutput, "\n[truncated: task output exceeded 32 KiB]")
}

// displayOwner 把空 owner 渲染为 "-"。
func displayOwner(owner string) string {
	if strings.TrimSpace(owner) == "" {
		return "-"
	}
	return strings.TrimSpace(owner)
}

// displayList 把空/缺失的字符串列表渲染为 "-"。
func displayList(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ", ")
}

// oneLineTaskText 折叠空白/换行，使表格行保持单行。
func oneLineTaskText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// formatTaskTime 渲染任务时间戳（RFC 3339，UTC），零值时输出 "-"。
func formatTaskTime(timestamp time.Time) string {
	if timestamp.IsZero() {
		return "-"
	}
	return timestamp.UTC().Format(time.RFC3339)
}

// stringEnumProp 构造一个枚举字符串属性 schema。
func stringEnumProp(description string, values ...string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": description,
		"enum":        values,
	}
}
