package schedule

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

const (
	// ToolScheduleCreate 是创建工具名。
	ToolScheduleCreate = "schedule_create"
	// ToolScheduleList 是列表工具名。
	ToolScheduleList = "schedule_list"
	// ToolScheduleUpdate 是更新工具名。
	ToolScheduleUpdate = "schedule_update"
	// ToolScheduleDelete 是删除工具名。
	ToolScheduleDelete = "schedule_delete"
	// DefaultMaxOutputBytes 限制单次工具响应。
	DefaultMaxOutputBytes = 32 << 10
)

// ErrTaskNotFound 是进程内 scheduler 在更新或删除未知任务 ID 时返回的错误。
var ErrTaskNotFound = errors.New("schedule: task not found")

// MemoryScheduler 是并发安全的进程内 rig.Scheduler。Create 在缺失时分配 ID
// （"task-<n>"）与创建时间；NextRun 按传入值保存（零值表示“未记录”；真实运行时间由
// 宿主计算）。
type MemoryScheduler struct {
	mu    sync.Mutex
	seq   uint64
	tasks []rig.ScheduledTask
}

var _ rig.Scheduler = (*MemoryScheduler)(nil)

// NewScheduler 返回一个空的进程内 scheduler。
func NewScheduler() *MemoryScheduler { return &MemoryScheduler{} }

// Create 实现 rig.Scheduler。
func (s *MemoryScheduler) Create(_ context.Context, task rig.ScheduledTask) (rig.ScheduledTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task.ID = strings.TrimSpace(task.ID)
	if task.ID == "" {
		s.seq++
		task.ID = fmt.Sprintf("task-%d", s.seq)
	}
	for _, existing := range s.tasks {
		if existing.ID == task.ID {
			return rig.ScheduledTask{}, fmt.Errorf("schedule: task %q already exists", task.ID)
		}
	}
	if task.Created.IsZero() {
		task.Created = time.Now().UTC()
	}
	s.tasks = append(s.tasks, task)
	return task, nil
}

// List 实现 rig.Scheduler，返回按创建时间、再按 ID 排序的副本。
func (s *MemoryScheduler) List(context.Context) ([]rig.ScheduledTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.tasks)
	slices.SortStableFunc(out, func(a, b rig.ScheduledTask) int {
		return cmp.Or(a.Created.Compare(b.Created), strings.Compare(a.ID, b.ID))
	})
	return out, nil
}

// Update 实现 rig.Scheduler。任务按 ID 匹配；缺失的 Created 会从已存任务保留，NextRun
// 按传入值保存。
func (s *MemoryScheduler) Update(_ context.Context, task rig.ScheduledTask) (rig.ScheduledTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, existing := range s.tasks {
		if existing.ID != task.ID {
			continue
		}
		if task.Created.IsZero() {
			task.Created = existing.Created
		}
		s.tasks[index] = task
		return task, nil
	}
	return rig.ScheduledTask{}, fmt.Errorf("%w: %q", ErrTaskNotFound, task.ID)
}

// Delete 实现 rig.Scheduler。
func (s *MemoryScheduler) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, existing := range s.tasks {
		if existing.ID != id {
			continue
		}
		s.tasks = slices.Delete(s.tasks, index, index+1)
		return nil
	}
	return fmt.Errorf("%w: %q", ErrTaskNotFound, id)
}

// Tools 返回绑定到 scheduler 的四个 schedule 工具。scheduler 为 nil 时使用一个新的
// 进程内 scheduler。
func Tools(scheduler rig.Scheduler) []rig.Tool {
	if scheduler == nil {
		scheduler = NewScheduler()
	}
	return []rig.Tool{
		&tool{name: ToolScheduleCreate, scheduler: scheduler},
		&tool{name: ToolScheduleList, scheduler: scheduler},
		&tool{name: ToolScheduleUpdate, scheduler: scheduler},
		&tool{name: ToolScheduleDelete, scheduler: scheduler},
	}
}

// tool 是一个 schedule_* 工具。四个工具共享实现，只有 name/description/schema 不同。
type tool struct {
	name      string
	scheduler rig.Scheduler
}

var _ rig.Tool = (*tool)(nil)

// Name 实现 rig.Tool。
func (t *tool) Name() string { return t.name }

// Description 实现 rig.Tool。
func (t *tool) Description() string {
	switch t.name {
	case ToolScheduleCreate:
		return `Create a scheduled task/reminder. "prompt" is required and "name" (legacy) or "title" (DSH name, at most 120 characters) supplies the task name. Supply at most one timing selector: after_seconds (positive whole-second delay), at (strictly future RFC 3339 instant with offset, or an object with date/time/time_zone), every_seconds (fixed interval, at least 60), daily (time/time_zone), weekly (time/time_zone/weekdays as ISO 1=Monday..7=Sunday), or cron (five-field Vixie expression/time_zone). The selector is validated and canonicalized to a stable schedule string evaluated by the host; the legacy free-form "schedule" string is stored verbatim instead. "enabled" defaults to true. Returns the created task ID and its fields.`
	case ToolScheduleList:
		return `List all scheduled tasks as a table with columns ID, NAME, SCHEDULE, ENABLED and NEXT RUN. SCHEDULE is the canonical selector form or the legacy schedule string; NEXT RUN shows the recorded next-run time (RFC 3339) or "-" when the host has not computed one. Takes no arguments.`
	case ToolScheduleUpdate:
		return `Update one scheduled task in place. "id" is required and at least one of "name", "title", "prompt", "enabled" or one timing selector (at, every_seconds, daily, weekly, cron, or the legacy "schedule" string) must be provided. Fields that are omitted are left unchanged. after_seconds is create-only; supply at, every_seconds, daily, weekly or cron to change the timing. Unknown IDs are an error.`
	default:
		return `Delete one scheduled task by "id". Unknown IDs are an error. Takes no other arguments.`
	}
}

// Schema 实现 rig.Tool。
func (t *tool) Schema() map[string]any {
	switch t.name {
	case ToolScheduleCreate:
		return toolutil.ObjectSchema(withProperties(map[string]any{
			"name":     toolutil.StringProp("Legacy task name; name or title is required."),
			"title":    toolutil.StringProp(fmt.Sprintf("DSH task name, at most %d characters; name or title is required.", MaxTitleLength)),
			"prompt":   toolutil.StringProp("Prompt to run when the task fires (required)."),
			"enabled":  toolutil.BooleanProp("Whether the task is enabled (default true)."),
			"schedule": toolutil.StringProp("Legacy free-form schedule expression stored verbatim, for example \"@daily\" or \"*/15 * * * *\"."),
		}, createSelectorSchema()), "prompt")
	case ToolScheduleList:
		return toolutil.ObjectSchema(map[string]any{})
	case ToolScheduleUpdate:
		return toolutil.ObjectSchema(withProperties(map[string]any{
			"id":       toolutil.StringProp("Task ID to update (required)."),
			"name":     toolutil.StringProp("New legacy human-readable name."),
			"title":    toolutil.StringProp(fmt.Sprintf("New DSH task name, at most %d characters.", MaxTitleLength)),
			"prompt":   toolutil.StringProp("New prompt."),
			"enabled":  toolutil.BooleanProp("New enabled state."),
			"schedule": toolutil.StringProp("New legacy schedule expression (stored verbatim; an empty string clears it)."),
		}, updateSelectorSchema()), "id")
	default:
		return toolutil.ObjectSchema(map[string]any{
			"id": toolutil.StringProp("Task ID to delete (required)."),
		}, "id")
	}
}

// withProperties 把 extra 复制进 base 并返回 base。
func withProperties(base, extra map[string]any) map[string]any {
	for key, value := range extra {
		base[key] = value
	}
	return base
}

// createSelectorSchema 返回 schedule_create 的 DSH 类型化时间属性（update 的全集加上
// create 独有的 after_seconds）。
func createSelectorSchema() map[string]any {
	properties := updateSelectorSchema()
	properties["after_seconds"] = toolutil.IntegerProp("One-shot delay in whole seconds; must be positive. Create-only: change a relative delay by creating a new task.")
	return properties
}

// updateSelectorSchema 返回 schedule_create 与 schedule_update 共享的 DSH 类型化时间
// 属性。
func updateSelectorSchema() map[string]any {
	return map[string]any{
		"at": map[string]any{
			"description": "One-shot absolute target: an RFC 3339 date-time string with an explicit offset, or an object with date (YYYY-MM-DD), time (HH:MM:SS with optional 1-3 fractional digits) and time_zone. Must be strictly in the future.",
			"oneOf": []any{
				map[string]any{"type": "string"},
				toolutil.ObjectSchema(map[string]any{
					"date":      toolutil.StringProp("ISO calendar date, for example 2026-12-01."),
					"time":      toolutil.StringProp("Local time HH:MM:SS with optional 1-3 fractional digits."),
					"time_zone": toolutil.StringProp("UTC or IANA Area/Location, for example Asia/Shanghai."),
				}, "date", "time", "time_zone"),
			},
		},
		"every_seconds": toolutil.IntegerProp(fmt.Sprintf("Fixed-rate interval in whole seconds, at least %d.", MinEveryIntervalSeconds)),
		"daily": toolutil.ObjectSchema(map[string]any{
			"time":      toolutil.StringProp("Local HH:MM:SS time with optional 1-3 fractional digits, for example 23:00:00."),
			"time_zone": toolutil.StringProp("UTC or IANA Area/Location, for example Asia/Shanghai."),
		}, "time", "time_zone"),
		"weekly": toolutil.ObjectSchema(map[string]any{
			"time":      toolutil.StringProp("Local HH:MM:SS time with optional 1-3 fractional digits, for example 09:00:00."),
			"time_zone": toolutil.StringProp("UTC or IANA Area/Location, for example Asia/Shanghai."),
			"weekdays": map[string]any{
				"type":        "array",
				"description": "ISO weekdays, Monday 1 through Sunday 7, without repetitions.",
				"items":       map[string]any{"type": "integer"},
			},
		}, "time", "time_zone", "weekdays"),
		"cron": toolutil.ObjectSchema(map[string]any{
			"expression": toolutil.StringProp(`Five-field Vixie cron expression, for example "*/15 9-17 * * 1-5".`),
			"time_zone":  toolutil.StringProp("UTC or IANA Area/Location, for example Asia/Shanghai."),
		}, "expression", "time_zone"),
	}
}

// createArgs、updateArgs 与 deleteArgs 是解码后的参数对象。
type createArgs struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Prompt  string `json:"prompt"`
	Enabled *bool  `json:"enabled"`
	createTimingArgs
}

type updateArgs struct {
	ID           string          `json:"id"`
	Name         *string         `json:"name"`
	Title        *string         `json:"title"`
	Prompt       *string         `json:"prompt"`
	Enabled      *bool           `json:"enabled"`
	Schedule     *string         `json:"schedule"`
	AfterSeconds *int            `json:"after_seconds"`
	At           json.RawMessage `json:"at"`
	EverySeconds *int            `json:"every_seconds"`
	Daily        *dailyInput     `json:"daily"`
	Weekly       *weeklyInput    `json:"weekly"`
	Cron         *cronInput      `json:"cron"`
}

type deleteArgs struct {
	ID string `json:"id"`
}

// Invoke 实现 rig.Tool。
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	var (
		result rig.ToolResult
		err    error
	)
	switch t.name {
	case ToolScheduleCreate:
		var a createArgs
		if err := toolutil.DecodeArgs(call, &a); err != nil {
			return rig.ToolResult{}, err
		}
		result = t.create(ctx, a)
	case ToolScheduleList:
		var a struct{}
		if err := toolutil.DecodeArgs(call, &a); err != nil {
			return rig.ToolResult{}, err
		}
		result = t.list(ctx)
	case ToolScheduleUpdate:
		var a updateArgs
		if err := toolutil.DecodeArgs(call, &a); err != nil {
			return rig.ToolResult{}, err
		}
		result = t.update(ctx, a)
	case ToolScheduleDelete:
		var a deleteArgs
		if err := toolutil.DecodeArgs(call, &a); err != nil {
			return rig.ToolResult{}, err
		}
		result = t.delete(ctx, a)
	default:
		return toolutil.ErrorResultf("schedule: unknown tool %q", t.name), nil
	}
	return result, err
}

// create 实现 schedule_create。
func (t *tool) create(ctx context.Context, a createArgs) rig.ToolResult {
	name := strings.TrimSpace(a.Name)
	title := strings.TrimSpace(a.Title)
	if name != "" && title != "" && name != title {
		return toolutil.ErrorResultf("%s: provide name or title, not both", ToolScheduleCreate)
	}
	if title != "" {
		validated, err := validateTitle(title)
		if err != nil {
			return toolutil.ErrorResultf("%s: %v", ToolScheduleCreate, err)
		}
		title = validated
	}
	taskName := title
	if taskName == "" {
		taskName = name
	}
	if taskName == "" {
		return toolutil.ErrorResultf("%s: name or title is required", ToolScheduleCreate)
	}
	prompt := strings.TrimSpace(a.Prompt)
	if prompt == "" {
		return toolutil.ErrorResultf("%s: prompt is required", ToolScheduleCreate)
	}
	schedule, err := resolveCreateTiming(a.createTimingArgs)
	if err != nil {
		return toolutil.ErrorResultf("%s: %v", ToolScheduleCreate, err)
	}
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	task := rig.ScheduledTask{
		Name:     taskName,
		Prompt:   prompt,
		Schedule: schedule,
		Enabled:  enabled,
	}
	created, err := t.scheduler.Create(ctx, task)
	if err != nil {
		return toolutil.ErrorResultf("%s: %v", ToolScheduleCreate, err)
	}
	return toolutil.TextResult(fmt.Sprintf("created schedule task %s (name=%s, schedule=%s, enabled=%t)",
		created.ID, created.Name, displaySchedule(created.Schedule), created.Enabled))
}

// list 实现 schedule_list。
func (t *tool) list(ctx context.Context) rig.ToolResult {
	tasks, err := t.scheduler.List(ctx)
	if err != nil {
		return toolutil.ErrorResultf("%s: %v", ToolScheduleList, err)
	}
	if len(tasks) == 0 {
		return toolutil.TextResult("no scheduled tasks")
	}
	var builder strings.Builder
	writer := tabwriter.NewWriter(&builder, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tNAME\tSCHEDULE\tENABLED\tNEXT RUN")
	for _, task := range tasks {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%t\t%s\n",
			task.ID, oneLine(task.Name), displaySchedule(task.Schedule), task.Enabled, displayNextRun(task.NextRun))
	}
	_ = writer.Flush()
	return toolutil.TextResult(toolutil.TruncateBytes(strings.TrimSuffix(builder.String(), "\n"), DefaultMaxOutputBytes, "\n[truncated: output exceeded the schedule tool limit]"))
}

// update 实现 schedule_update。
func (t *tool) update(ctx context.Context, a updateArgs) rig.ToolResult {
	id := strings.TrimSpace(a.ID)
	if id == "" {
		return toolutil.ErrorResultf("%s: id is required", ToolScheduleUpdate)
	}
	provided := a.Name != nil || a.Title != nil || a.Prompt != nil || a.Enabled != nil ||
		a.Schedule != nil || a.AfterSeconds != nil || len(a.At) > 0 ||
		a.EverySeconds != nil || a.Daily != nil || a.Weekly != nil || a.Cron != nil
	if !provided {
		return toolutil.ErrorResultf("%s: nothing to update: provide at least one of name, title, prompt, enabled, or a timing selector", ToolScheduleUpdate)
	}
	if a.Name != nil && a.Title != nil {
		return toolutil.ErrorResultf("%s: provide name or title, not both", ToolScheduleUpdate)
	}
	schedule, scheduleProvided, err := resolveUpdateTiming(a)
	if err != nil {
		return toolutil.ErrorResultf("%s: %v", ToolScheduleUpdate, err)
	}
	tasks, err := t.scheduler.List(ctx)
	if err != nil {
		return toolutil.ErrorResultf("%s: %v", ToolScheduleUpdate, err)
	}
	var current rig.ScheduledTask
	found := false
	for _, task := range tasks {
		if task.ID == id {
			current = task
			found = true
			break
		}
	}
	if !found {
		return toolutil.ErrorResultf("%s: no scheduled task with id %q", ToolScheduleUpdate, id)
	}
	if a.Name != nil {
		name := strings.TrimSpace(*a.Name)
		if name == "" {
			return toolutil.ErrorResultf("%s: name must not be empty", ToolScheduleUpdate)
		}
		current.Name = name
	}
	if a.Title != nil {
		title, err := validateTitle(*a.Title)
		if err != nil {
			return toolutil.ErrorResultf("%s: %v", ToolScheduleUpdate, err)
		}
		current.Name = title
	}
	if a.Prompt != nil {
		prompt := strings.TrimSpace(*a.Prompt)
		if prompt == "" {
			return toolutil.ErrorResultf("%s: prompt must not be empty", ToolScheduleUpdate)
		}
		current.Prompt = prompt
	}
	if scheduleProvided {
		current.Schedule = schedule
	}
	if a.Enabled != nil {
		current.Enabled = *a.Enabled
	}
	updated, err := t.scheduler.Update(ctx, current)
	if err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			return toolutil.ErrorResultf("%s: no scheduled task with id %q", ToolScheduleUpdate, id)
		}
		return toolutil.ErrorResultf("%s: %v", ToolScheduleUpdate, err)
	}
	return toolutil.TextResult(fmt.Sprintf("updated schedule task %s (name=%s, schedule=%s, enabled=%t)",
		updated.ID, updated.Name, displaySchedule(updated.Schedule), updated.Enabled))
}

// delete 实现 schedule_delete。
func (t *tool) delete(ctx context.Context, a deleteArgs) rig.ToolResult {
	id := strings.TrimSpace(a.ID)
	if id == "" {
		return toolutil.ErrorResultf("%s: id is required", ToolScheduleDelete)
	}
	if err := t.scheduler.Delete(ctx, id); err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			return toolutil.ErrorResultf("%s: no scheduled task with id %q", ToolScheduleDelete, id)
		}
		return toolutil.ErrorResultf("%s: %v", ToolScheduleDelete, err)
	}
	return toolutil.TextResult(fmt.Sprintf("deleted schedule task %s", id))
}

// displaySchedule 把空 schedule 渲染为 "-"。
func displaySchedule(schedule string) string {
	schedule = strings.TrimSpace(schedule)
	if schedule == "" {
		return "-"
	}
	return schedule
}

// displayNextRun 把零值 next-run 渲染为 "-"，否则按 UTC 的 RFC 3339 渲染。
func displayNextRun(next time.Time) string {
	if next.IsZero() {
		return "-"
	}
	return next.UTC().Format(time.RFC3339)
}

// oneLine 折叠空白/换行，使一个值保持在同一表格行内。
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
