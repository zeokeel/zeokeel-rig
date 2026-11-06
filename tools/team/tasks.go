package team

// Shared team task board: an in-memory v1.TeamTaskManager plus the
// team_task_create/list/get/update tools. This file is additive: the existing
// member Manager, its tools and the legacy tools are untouched.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// Shared task statuses. They are the model-facing values of TeamTask.Status.
const (
	// TaskStatusPending is a task nobody has claimed yet.
	TaskStatusPending = "pending"
	// TaskStatusInProgress is a task claimed by an owner.
	TaskStatusInProgress = "in_progress"
	// TaskStatusCompleted is a finished task.
	TaskStatusCompleted = "completed"
)

// Tool names of the shared task board.
const (
	// ToolTeamTaskCreate creates one shared task.
	ToolTeamTaskCreate = "team_task_create"
	// ToolTeamTaskList lists shared tasks (optionally filtered).
	ToolTeamTaskList = "team_task_list"
	// ToolTeamTaskGet shows one shared task in detail.
	ToolTeamTaskGet = "team_task_get"
	// ToolTeamTaskUpdate mutates one shared task through an action.
	ToolTeamTaskUpdate = "team_task_update"
)

// Task board defaults and limits.
const (
	// DefaultMaxTasks is the default number of tasks one TaskManager stores.
	DefaultMaxTasks = 256
	// taskIDPrefix is the prefix of generated task ids ("task-1", ...).
	taskIDPrefix = "task-"
	// maxTaskTableOutput caps one task tool response (32 KiB).
	maxTaskTableOutput = 32 << 10
)

// Sentinel errors returned by the in-memory task manager. Tool failures include
// their text; hosts can match them with errors.Is.
var (
	// ErrTaskNotFound reports an id that matches no task.
	ErrTaskNotFound = errors.New("team task not found")
	// ErrTaskExists reports a Create with a duplicate explicit id.
	ErrTaskExists = errors.New("team task already exists")
	// ErrTaskBoardFull reports a Create beyond the manager's task limit.
	ErrTaskBoardFull = errors.New("team task board is full")
	// ErrTaskSubjectRequired reports a missing/blank subject.
	ErrTaskSubjectRequired = errors.New("team task subject is required")
	// ErrTaskInvalidStatus reports a status outside pending/in_progress/completed.
	ErrTaskInvalidStatus = errors.New("invalid team task status")
	// ErrTaskOwnerRequired reports in_progress without an owner.
	ErrTaskOwnerRequired = errors.New("team task owner is required")
	// ErrTaskDependencyNotFound reports a BlockedBy id that does not exist.
	ErrTaskDependencyNotFound = errors.New("team task dependency references an unknown task")
	// ErrTaskDependencyCycle reports a direct or transitive self-dependency.
	ErrTaskDependencyCycle = errors.New("team task dependency cycle")
	// ErrTaskNoChanges reports an empty TeamTaskUpdate.
	ErrTaskNoChanges = errors.New("team task update has no changes")
)

// TaskManagerOption configures a TaskManager.
type TaskManagerOption func(*TaskManager)

// WithMaxTasks bounds how many tasks the manager stores (non-positive keeps
// DefaultMaxTasks).
func WithMaxTasks(max int) TaskManagerOption {
	return func(manager *TaskManager) {
		if max > 0 {
			manager.maxTasks = max
		}
	}
}

// TaskManager is a concurrency-safe in-memory v1.TeamTaskManager. Tasks keep
// their creation time; List returns them ordered by Created (then by id).
type TaskManager struct {
	mu       sync.Mutex
	seq      uint64
	maxTasks int
	tasks    map[string]v1.TeamTask
}

var _ v1.TeamTaskManager = (*TaskManager)(nil)

// NewTaskManager returns an empty in-memory task board (256 tasks by default).
func NewTaskManager(opts ...TaskManagerOption) *TaskManager {
	manager := &TaskManager{maxTasks: DefaultMaxTasks, tasks: make(map[string]v1.TeamTask)}
	for _, opt := range opts {
		if opt != nil {
			opt(manager)
		}
	}
	return manager
}

// Create implements v1.TeamTaskManager. A missing id is generated ("task-N"),
// a missing status defaults to pending, and Created/Updated default to now.
// BlockedBy ids must exist; write scopes are trimmed and deduplicated.
func (m *TaskManager) Create(ctx context.Context, task v1.TeamTask) (v1.TeamTask, error) {
	if err := contextError(ctx); err != nil {
		return v1.TeamTask{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tasks == nil {
		m.tasks = make(map[string]v1.TeamTask)
	}
	if len(m.tasks) >= m.maxTasks {
		return v1.TeamTask{}, fmt.Errorf("%w (limit %d)", ErrTaskBoardFull, m.maxTasks)
	}

	task.ID = strings.TrimSpace(task.ID)
	task.Subject = strings.TrimSpace(task.Subject)
	if task.Subject == "" {
		return v1.TeamTask{}, ErrTaskSubjectRequired
	}
	status := strings.ToLower(strings.TrimSpace(task.Status))
	if status == "" {
		status = TaskStatusPending
	}
	if !validTaskStatus(status) {
		return v1.TeamTask{}, invalidTaskStatusError(task.Status)
	}
	task.Status = status
	task.Owner = strings.TrimSpace(task.Owner)
	if status == TaskStatusInProgress && task.Owner == "" {
		return v1.TeamTask{}, fmt.Errorf("%w: in_progress tasks need an owner", ErrTaskOwnerRequired)
	}
	// Validate dependencies before allocating an id so a rejected Create does
	// not consume a task number.
	dependencies, err := m.cleanDependenciesLocked("", task.BlockedBy)
	if err != nil {
		return v1.TeamTask{}, err
	}
	if task.ID != "" {
		if _, exists := m.tasks[task.ID]; exists {
			return v1.TeamTask{}, fmt.Errorf("%w: %q", ErrTaskExists, task.ID)
		}
	} else {
		m.seq++
		task.ID = taskIDPrefix + strconv.FormatUint(m.seq, 10)
	}
	for _, dependency := range dependencies {
		if dependency == task.ID {
			return v1.TeamTask{}, fmt.Errorf("%w: task %q cannot depend on itself", ErrTaskDependencyCycle, task.ID)
		}
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

// List implements v1.TeamTaskManager. Filter fields are optional; Status must
// be one of the known statuses when set.
func (m *TaskManager) List(ctx context.Context, filter v1.TeamTaskFilter) ([]v1.TeamTask, error) {
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
	tasks := make([]v1.TeamTask, 0, len(m.tasks))
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

// Get implements v1.TeamTaskManager.
func (m *TaskManager) Get(ctx context.Context, id string) (v1.TeamTask, error) {
	if err := contextError(ctx); err != nil {
		return v1.TeamTask{}, err
	}
	id = strings.TrimSpace(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return v1.TeamTask{}, fmt.Errorf("%w: %q", ErrTaskNotFound, id)
	}
	return cloneTask(task), nil
}

// Update implements v1.TeamTaskManager. Nil fields are unchanged; dependency
// additions are validated for existence and for direct/transitive cycles, and
// an in_progress task always keeps an owner. An update that changes nothing
// returns ErrTaskNoChanges.
func (m *TaskManager) Update(ctx context.Context, id string, update v1.TeamTaskUpdate) (v1.TeamTask, error) {
	if err := contextError(ctx); err != nil {
		return v1.TeamTask{}, err
	}
	id = strings.TrimSpace(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return v1.TeamTask{}, fmt.Errorf("%w: %q", ErrTaskNotFound, id)
	}

	changed := false
	if update.Subject != nil {
		subject := strings.TrimSpace(*update.Subject)
		if subject == "" {
			return v1.TeamTask{}, ErrTaskSubjectRequired
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
			return v1.TeamTask{}, invalidTaskStatusError(*update.Status)
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
			return v1.TeamTask{}, err
		}
		if err := m.checkDependencyCycleLocked(task.ID, cleaned); err != nil {
			return v1.TeamTask{}, err
		}
		if !equalStrings(task.BlockedBy, cleaned) {
			task.BlockedBy = cleaned
			changed = true
		}
	}
	if !changed {
		return v1.TeamTask{}, ErrTaskNoChanges
	}
	if task.Status == TaskStatusInProgress && task.Owner == "" {
		return v1.TeamTask{}, fmt.Errorf("%w: in_progress tasks need an owner", ErrTaskOwnerRequired)
	}
	now := time.Now().UTC()
	if task.Created.IsZero() {
		task.Created = now
	}
	task.Updated = now
	m.tasks[task.ID] = cloneTask(task)
	return cloneTask(task), nil
}

// cleanDependenciesLocked trims/deduplicates dependency ids and validates that
// each one exists and is not the task itself.
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

// checkDependencyCycleLocked reports a cycle when any dependency depends
// (directly or transitively) on taskID.
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

// validTaskStatus reports whether status is one of the three known values.
func validTaskStatus(status string) bool {
	switch status {
	case TaskStatusPending, TaskStatusInProgress, TaskStatusCompleted:
		return true
	default:
		return false
	}
}

// invalidTaskStatusError builds one readable status error.
func invalidTaskStatusError(status string) error {
	return fmt.Errorf("%w: %q (want %s, %s or %s)", ErrTaskInvalidStatus, strings.TrimSpace(status),
		TaskStatusPending, TaskStatusInProgress, TaskStatusCompleted)
}

// applyDependencyChanges removes DelBlocked then adds AddBlocked, deduplicating.
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
			// Explicit removal wins when the same id appears in both lists.
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

// cleanStringList trims, drops empties and deduplicates strings, preserving the
// first occurrence order.
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

// equalStrings reports whether two string slices are element-wise equal.
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

// cloneTask returns a deep copy of task so callers cannot mutate stored slices.
func cloneTask(task v1.TeamTask) v1.TeamTask {
	task.BlockedBy = append([]string(nil), task.BlockedBy...)
	task.WriteScopes = append([]string(nil), task.WriteScopes...)
	return task
}

// sortTasks orders tasks by creation time, then by numeric id.
func sortTasks(tasks []v1.TeamTask) {
	sort.SliceStable(tasks, func(i, j int) bool {
		if !tasks[i].Created.Equal(tasks[j].Created) {
			return tasks[i].Created.Before(tasks[j].Created)
		}
		left, leftOK := taskIDNumber(tasks[i].ID)
		right, rightOK := taskIDNumber(tasks[j].ID)
		if leftOK && rightOK {
			return left < right
		}
		return tasks[i].ID < tasks[j].ID
	})
}

// taskIDNumber parses the numeric suffix of a generated task id.
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

// --- tools ------------------------------------------------------------------

// TaskToolNames returns the names of the shared task board tools, in the same
// order as TaskTools.
func TaskToolNames() []string {
	return []string{ToolTeamTaskCreate, ToolTeamTaskList, ToolTeamTaskGet, ToolTeamTaskUpdate}
}

// TaskTools returns the four shared task board tools bound to manager. A nil
// manager is allowed at construction time; every invocation then returns a
// readable configuration error (hosts pass NewTaskManager for the default).
func TaskTools(manager v1.TeamTaskManager) []v1.Tool {
	return []v1.Tool{
		taskCreateTool(manager),
		taskListTool(manager),
		taskGetTool(manager),
		taskUpdateTool(manager),
	}
}

// Tools returns the shared task board tools. It is an alias for TaskTools so
// hosts wiring a task manager do not have to remember the name.
func Tools(manager v1.TeamTaskManager) []v1.Tool { return TaskTools(manager) }

// requireTaskManager returns a model-facing configuration error result when the
// tool has no manager.
func requireTaskManager(toolName string, manager v1.TeamTaskManager) (v1.ToolResult, bool) {
	if manager == nil {
		return errorResult("%s: team task manager is not configured (pass NewTaskManager)", toolName), true
	}
	return v1.ToolResult{}, false
}

// taskCreateTool implements team_task_create.
func taskCreateTool(manager v1.TeamTaskManager) v1.Tool {
	return &teamTool{
		name: ToolTeamTaskCreate,
		description: "Create one shared team task on the task board. The task starts in status pending " +
			"unless it is created blocked by other tasks; dependencies must reference existing task ids. " +
			"Use team_task_update to claim/complete/reopen/release it or to edit its fields and dependencies. " +
			"Subjects must be non-empty; blockers are shown by team_task_list and team_task_get.",
		parameters: objectSchema(map[string]any{
			"subject":      stringProp("Short imperative title of the task (required, non-empty)."),
			"description":  stringProp("Longer details for the task (optional)."),
			"owner":        stringProp("Initial owner name; claiming normally sets this with team_task_update action=claim (optional)."),
			"blocked_by":   stringArrayProp("Task ids this task depends on; all must already exist."),
			"write_scopes": stringArrayProp("Workspace-relative files/directories this task expects to modify (advisory)."),
		}, "subject"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args struct {
				Subject     string   `json:"subject"`
				Description string   `json:"description"`
				Owner       string   `json:"owner"`
				BlockedBy   []string `json:"blocked_by"`
				WriteScopes []string `json:"write_scopes"`
			}
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if err := contextError(ctx); err != nil {
				return v1.ToolResult{}, err
			}
			if result, missing := requireTaskManager(ToolTeamTaskCreate, manager); missing {
				return result, nil
			}
			task, err := manager.Create(ctx, v1.TeamTask{
				Subject:     args.Subject,
				Description: args.Description,
				Owner:       args.Owner,
				BlockedBy:   args.BlockedBy,
				WriteScopes: args.WriteScopes,
			})
			if err != nil {
				return errorResult("%s: %v", ToolTeamTaskCreate, err), nil
			}
			return textResult("created shared team task\n" + formatTaskDetail(task)), nil
		},
	}
}

// taskListTool implements team_task_list.
func taskListTool(manager v1.TeamTaskManager) v1.Tool {
	return &teamTool{
		name: ToolTeamTaskList,
		description: "List shared team tasks ordered by creation time, optionally filtered by status and/or owner. " +
			"Columns: ID, STATUS, OWNER, SUBJECT, BLOCKED BY. Use team_task_get for the full description and scope details.",
		parameters: objectSchema(map[string]any{
			"status": stringEnumProp("Only tasks with this status (pending, in_progress or completed).", TaskStatusPending, TaskStatusInProgress, TaskStatusCompleted),
			"owner":  stringProp("Only tasks owned by this exact owner name."),
		}),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args struct {
				Status string `json:"status"`
				Owner  string `json:"owner"`
			}
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if err := contextError(ctx); err != nil {
				return v1.ToolResult{}, err
			}
			if result, missing := requireTaskManager(ToolTeamTaskList, manager); missing {
				return result, nil
			}
			tasks, err := manager.List(ctx, v1.TeamTaskFilter{Status: args.Status, Owner: args.Owner})
			if err != nil {
				return errorResult("%s: %v", ToolTeamTaskList, err), nil
			}
			return textResult(formatTaskTable(tasks)), nil
		},
	}
}

// taskGetTool implements team_task_get.
func taskGetTool(manager v1.TeamTaskManager) v1.Tool {
	return &teamTool{
		name: ToolTeamTaskGet,
		description: "Show one shared team task in detail: subject, status, owner, dependencies, write scopes, " +
			"timestamps and the full description. Unknown ids are reported as errors.",
		parameters: objectSchema(map[string]any{
			"id": stringProp("Task id shown by team_task_create or team_task_list (required)."),
		}, "id"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if err := contextError(ctx); err != nil {
				return v1.ToolResult{}, err
			}
			if result, missing := requireTaskManager(ToolTeamTaskGet, manager); missing {
				return result, nil
			}
			id := strings.TrimSpace(args.ID)
			if id == "" {
				return errorResult("%s: id is required", ToolTeamTaskGet), nil
			}
			task, err := manager.Get(ctx, id)
			if err != nil {
				return errorResult("%s: %v", ToolTeamTaskGet, err), nil
			}
			return textResult(formatTaskDetail(task)), nil
		},
	}
}

// taskUpdateTool implements team_task_update.
func taskUpdateTool(manager v1.TeamTaskManager) v1.Tool {
	return &teamTool{
		name: ToolTeamTaskUpdate,
		description: "Mutate one shared team task through an action: " +
			"claim (owner required; sets status in_progress and owner), complete (status completed), " +
			"reopen (status pending), release (status pending and clears owner), " +
			"edit (update subject/description/write_scopes and optional status), " +
			"set_dependencies (add_blocked_by/remove_blocked_by; cycles and unknown ids are refused), " +
			"reassign (owner required; changes the owner without claiming). " +
			"Unknown actions, ids, statuses and invalid dependency graphs are readable errors.",
		parameters: objectSchema(map[string]any{
			"id":                stringProp("Task id to update (required)."),
			"action":            stringEnumProp("Action to perform.", "claim", "complete", "reopen", "release", "edit", "set_dependencies", "reassign"),
			"subject":           stringProp("New subject (action=edit)."),
			"description":       stringProp("New description (action=edit)."),
			"status":            stringEnumProp("New status for action=edit (pending, in_progress or completed).", TaskStatusPending, TaskStatusInProgress, TaskStatusCompleted),
			"owner":             stringProp("Owner for action=claim or action=reassign (required there)."),
			"add_blocked_by":    stringArrayProp("Task ids to add as dependencies (action=set_dependencies)."),
			"remove_blocked_by": stringArrayProp("Task ids to remove from dependencies (action=set_dependencies)."),
			"write_scopes":      stringArrayProp("Replacement advisory write scopes (action=edit)."),
		}, "id", "action"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
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
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if err := contextError(ctx); err != nil {
				return v1.ToolResult{}, err
			}
			if result, missing := requireTaskManager(ToolTeamTaskUpdate, manager); missing {
				return result, nil
			}
			id := strings.TrimSpace(args.ID)
			if id == "" {
				return errorResult("%s: id is required", ToolTeamTaskUpdate), nil
			}
			action := strings.ToLower(strings.TrimSpace(args.Action))
			if action == "" {
				return errorResult("%s: action is required", ToolTeamTaskUpdate), nil
			}

			update, errResult := taskUpdateFor(action, args.Subject, args.Description, args.Status,
				args.Owner, args.AddBlockedBy, args.RemoveBlockedBy, args.WriteScopes)
			if errResult != "" {
				return errorResult("%s: %s", ToolTeamTaskUpdate, errResult), nil
			}
			if update == nil {
				return errorResult("%s: unknown action %q (claim, complete, reopen, release, edit, set_dependencies, reassign)",
					ToolTeamTaskUpdate, args.Action), nil
			}
			task, err := manager.Update(ctx, id, *update)
			if err != nil {
				return errorResult("%s: %v", ToolTeamTaskUpdate, err), nil
			}
			return textResult(fmt.Sprintf("updated shared team task %s (status=%s, owner=%s, blocked_by=%s)",
				task.ID, task.Status, displayOwner(task.Owner), displayList(task.BlockedBy))), nil
		},
	}
}

// taskUpdateFor translates one tool action into a v1.TeamTaskUpdate. It returns
// a non-empty error string for model-facing validation failures and a nil
// update for unknown actions.
func taskUpdateFor(action string, subject, description, status, owner *string,
	addBlockedBy, removeBlockedBy []string, writeScopes *[]string) (*v1.TeamTaskUpdate, string) {
	switch action {
	case "claim":
		claimOwner := strings.TrimSpace(deref(owner))
		if claimOwner == "" {
			return nil, "owner is required for action: claim"
		}
		inProgress := TaskStatusInProgress
		return &v1.TeamTaskUpdate{Status: &inProgress, Owner: &claimOwner}, ""
	case "complete":
		completed := TaskStatusCompleted
		return &v1.TeamTaskUpdate{Status: &completed}, ""
	case "reopen":
		pending := TaskStatusPending
		return &v1.TeamTaskUpdate{Status: &pending}, ""
	case "release":
		pending := TaskStatusPending
		empty := ""
		return &v1.TeamTaskUpdate{Status: &pending, Owner: &empty}, ""
	case "reassign":
		newOwner := strings.TrimSpace(deref(owner))
		if newOwner == "" {
			return nil, "owner is required for action: reassign"
		}
		return &v1.TeamTaskUpdate{Owner: &newOwner}, ""
	case "edit":
		update := &v1.TeamTaskUpdate{Subject: subject, Description: description, Status: status, WriteScopes: writeScopes}
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
		return &v1.TeamTaskUpdate{AddBlocked: addBlockedBy, DelBlocked: removeBlockedBy}, ""
	default:
		return nil, ""
	}
}

// deref returns *value, or "" for nil.
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// formatTaskTable renders the team_task_list table.
func formatTaskTable(tasks []v1.TeamTask) string {
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
	return truncateTaskOutput(strings.TrimSuffix(builder.String(), "\n"))
}

// formatTaskDetail renders one task for team_task_create/team_task_get.
func formatTaskDetail(task v1.TeamTask) string {
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
	return truncateTaskOutput(strings.TrimSuffix(builder.String(), "\n"))
}

// displayOwner renders an empty owner as "-".
func displayOwner(owner string) string {
	if strings.TrimSpace(owner) == "" {
		return "-"
	}
	return strings.TrimSpace(owner)
}

// displayList renders an empty/missing string list as "-".
func displayList(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ", ")
}

// oneLineTaskText collapses whitespace/newlines so table rows stay one line.
func oneLineTaskText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// formatTaskTime renders a task timestamp (RFC 3339, UTC) or "-" when zero.
func formatTaskTime(timestamp time.Time) string {
	if timestamp.IsZero() {
		return "-"
	}
	return timestamp.UTC().Format(time.RFC3339)
}

// truncateTaskOutput caps one task tool response at maxTaskTableOutput bytes.
func truncateTaskOutput(content string) string {
	if len(content) <= maxTaskTableOutput {
		return content
	}
	cut := maxTaskTableOutput
	for cut > 0 && (content[cut]&0xC0) == 0x80 {
		cut--
	}
	return content[:cut] + "\n[truncated: task output exceeded 32 KiB]"
}

// stringArrayProp builds one string-array property schema.
func stringArrayProp(description string) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": description,
		"items":       map[string]any{"type": "string"},
	}
}

// stringEnumProp builds one enumerated string property schema.
func stringEnumProp(description string, values ...string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": description,
		"enum":        values,
	}
}
