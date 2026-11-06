// Package schedule provides the schedule_* tools and an in-memory
// v1.Scheduler implementation.
//
// A host may supply a durable v1.Scheduler through Tools; when it is nil the
// tools fall back to NewScheduler, a concurrency-safe process-local scheduler
// that is lost on restart.
//
// The tools are pure CRUD over task descriptions. The schedule string (a cron
// expression, interval, or host-defined value) is stored verbatim: evaluating
// it and firing tasks belongs to the host, so the tools never compute or
// rewrite next-run times. A host-provided NextRun is recorded and displayed as
// is.
//
//	schedule_create {name, prompt, schedule?, enabled?}
//	schedule_list
//	schedule_update {id, name?, prompt?, schedule?, enabled?}
//	schedule_delete {id}
package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

const (
	// ToolScheduleCreate is the create tool name.
	ToolScheduleCreate = "schedule_create"
	// ToolScheduleList is the list tool name.
	ToolScheduleList = "schedule_list"
	// ToolScheduleUpdate is the update tool name.
	ToolScheduleUpdate = "schedule_update"
	// ToolScheduleDelete is the delete tool name.
	ToolScheduleDelete = "schedule_delete"
	// DefaultMaxOutputBytes caps one tool response.
	DefaultMaxOutputBytes = 32 << 10
)

// ErrTaskNotFound is returned by the in-memory scheduler when an update or
// delete names an unknown task ID.
var ErrTaskNotFound = errors.New("schedule: task not found")

// MemoryScheduler is a concurrency-safe in-memory v1.Scheduler. Create assigns
// an ID ("task-<n>") and creation time when they are missing; NextRun is stored
// as provided (zero means "not recorded"; the host computes real run times).
type MemoryScheduler struct {
	mu    sync.Mutex
	seq   uint64
	tasks []v1.ScheduledTask
}

var _ v1.Scheduler = (*MemoryScheduler)(nil)

// NewScheduler returns an empty in-memory scheduler.
func NewScheduler() *MemoryScheduler { return &MemoryScheduler{} }

// Create implements v1.Scheduler.
func (s *MemoryScheduler) Create(_ context.Context, task v1.ScheduledTask) (v1.ScheduledTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task.ID = strings.TrimSpace(task.ID)
	if task.ID == "" {
		s.seq++
		task.ID = fmt.Sprintf("task-%d", s.seq)
	}
	for _, existing := range s.tasks {
		if existing.ID == task.ID {
			return v1.ScheduledTask{}, fmt.Errorf("schedule: task %q already exists", task.ID)
		}
	}
	if task.Created.IsZero() {
		task.Created = time.Now().UTC()
	}
	s.tasks = append(s.tasks, task)
	return task, nil
}

// List implements v1.Scheduler. It returns a copy sorted by creation time and
// then ID.
func (s *MemoryScheduler) List(context.Context) ([]v1.ScheduledTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]v1.ScheduledTask(nil), s.tasks...)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Update implements v1.Scheduler. The task is matched by ID; a missing Created
// is preserved from the stored task and NextRun is stored as provided.
func (s *MemoryScheduler) Update(_ context.Context, task v1.ScheduledTask) (v1.ScheduledTask, error) {
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
	return v1.ScheduledTask{}, fmt.Errorf("%w: %q", ErrTaskNotFound, task.ID)
}

// Delete implements v1.Scheduler.
func (s *MemoryScheduler) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, existing := range s.tasks {
		if existing.ID != id {
			continue
		}
		s.tasks = append(s.tasks[:index], s.tasks[index+1:]...)
		return nil
	}
	return fmt.Errorf("%w: %q", ErrTaskNotFound, id)
}

// Tools returns the four schedule tools bound to scheduler. A nil scheduler
// uses a fresh in-memory scheduler.
func Tools(scheduler v1.Scheduler) []v1.Tool {
	if scheduler == nil {
		scheduler = NewScheduler()
	}
	return []v1.Tool{
		&tool{name: ToolScheduleCreate, scheduler: scheduler},
		&tool{name: ToolScheduleList, scheduler: scheduler},
		&tool{name: ToolScheduleUpdate, scheduler: scheduler},
		&tool{name: ToolScheduleDelete, scheduler: scheduler},
	}
}

// tool is one schedule_* tool. The four tools share the implementation and
// differ by name/description/schema.
type tool struct {
	name      string
	scheduler v1.Scheduler
}

var _ v1.Tool = (*tool)(nil)

// Name implements v1.Tool.
func (t *tool) Name() string { return t.name }

// Description implements v1.Tool.
func (t *tool) Description() string {
	switch t.name {
	case ToolScheduleCreate:
		return `Create a scheduled task description. The schedule string is stored verbatim (for example a cron expression like "@daily" or "*/15 * * * *", or a host-defined interval) and must be evaluated by the host scheduler.

Arguments: "name" and "prompt" are required; "schedule" is optional (stored as-is); "enabled" defaults to true. Returns the created task ID and its fields.`
	case ToolScheduleList:
		return `List all scheduled tasks as a table with columns ID, NAME, SCHEDULE, ENABLED and NEXT RUN. NEXT RUN shows the recorded next-run time (RFC 3339) or "-" when the host has not computed one. Takes no arguments.`
	case ToolScheduleUpdate:
		return `Update one scheduled task in place. "id" is required and at least one of "name", "prompt", "schedule" or "enabled" must be provided. Fields that are omitted are left unchanged; the schedule string is stored verbatim and NextRun is left to the host. Unknown IDs are an error.`
	default:
		return `Delete one scheduled task by "id". Unknown IDs are an error. Takes no other arguments.`
	}
}

// Schema implements v1.Tool.
func (t *tool) Schema() map[string]any {
	switch t.name {
	case ToolScheduleCreate:
		return objectSchema(map[string]any{
			"name":     stringProp("Human-readable task name (required)."),
			"prompt":   stringProp("Prompt to run when the task fires (required)."),
			"schedule": stringProp("Schedule expression stored verbatim, for example \"@daily\" or \"*/15 * * * *\"."),
			"enabled":  boolProp("Whether the task is enabled (default true)."),
		}, "name", "prompt")
	case ToolScheduleList:
		return objectSchema(map[string]any{})
	case ToolScheduleUpdate:
		return objectSchema(map[string]any{
			"id":       stringProp("Task ID to update (required)."),
			"name":     stringProp("New human-readable name."),
			"prompt":   stringProp("New prompt."),
			"schedule": stringProp("New schedule expression (stored verbatim)."),
			"enabled":  boolProp("New enabled state."),
		}, "id")
	default:
		return objectSchema(map[string]any{
			"id": stringProp("Task ID to delete (required)."),
		}, "id")
	}
}

// createArgs, updateArgs and deleteArgs are the decoded argument objects.
type createArgs struct {
	Name     string `json:"name"`
	Prompt   string `json:"prompt"`
	Schedule string `json:"schedule"`
	Enabled  *bool  `json:"enabled"`
}

type updateArgs struct {
	ID       string  `json:"id"`
	Name     *string `json:"name"`
	Prompt   *string `json:"prompt"`
	Schedule *string `json:"schedule"`
	Enabled  *bool   `json:"enabled"`
}

type deleteArgs struct {
	ID string `json:"id"`
}

// Invoke implements v1.Tool.
func (t *tool) Invoke(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return v1.ToolResult{}, err
	}
	var (
		result v1.ToolResult
		err    error
	)
	switch t.name {
	case ToolScheduleCreate:
		var a createArgs
		if err := decodeArgs(call, &a); err != nil {
			return v1.ToolResult{}, err
		}
		result = t.create(ctx, a)
	case ToolScheduleList:
		var a struct{}
		if err := decodeArgs(call, &a); err != nil {
			return v1.ToolResult{}, err
		}
		result = t.list(ctx)
	case ToolScheduleUpdate:
		var a updateArgs
		if err := decodeArgs(call, &a); err != nil {
			return v1.ToolResult{}, err
		}
		result = t.update(ctx, a)
	case ToolScheduleDelete:
		var a deleteArgs
		if err := decodeArgs(call, &a); err != nil {
			return v1.ToolResult{}, err
		}
		result = t.delete(ctx, a)
	default:
		return errorResult("schedule: unknown tool %q", t.name), nil
	}
	return result, err
}

// create implements schedule_create.
func (t *tool) create(ctx context.Context, a createArgs) v1.ToolResult {
	name := strings.TrimSpace(a.Name)
	prompt := strings.TrimSpace(a.Prompt)
	if name == "" {
		return errorResult("%s: name is required", ToolScheduleCreate)
	}
	if prompt == "" {
		return errorResult("%s: prompt is required", ToolScheduleCreate)
	}
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	task := v1.ScheduledTask{
		Name:     name,
		Prompt:   prompt,
		Schedule: strings.TrimSpace(a.Schedule),
		Enabled:  enabled,
	}
	created, err := t.scheduler.Create(ctx, task)
	if err != nil {
		return errorResult("%s: %v", ToolScheduleCreate, err)
	}
	return textResult(fmt.Sprintf("created schedule task %s (name=%s, schedule=%s, enabled=%t)",
		created.ID, created.Name, displaySchedule(created.Schedule), created.Enabled))
}

// list implements schedule_list.
func (t *tool) list(ctx context.Context) v1.ToolResult {
	tasks, err := t.scheduler.List(ctx)
	if err != nil {
		return errorResult("%s: %v", ToolScheduleList, err)
	}
	if len(tasks) == 0 {
		return textResult("no scheduled tasks")
	}
	var builder strings.Builder
	writer := tabwriter.NewWriter(&builder, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tNAME\tSCHEDULE\tENABLED\tNEXT RUN")
	for _, task := range tasks {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%t\t%s\n",
			task.ID, oneLine(task.Name), displaySchedule(task.Schedule), task.Enabled, displayNextRun(task.NextRun))
	}
	_ = writer.Flush()
	return textResult(truncateOutput(strings.TrimSuffix(builder.String(), "\n"), DefaultMaxOutputBytes))
}

// update implements schedule_update.
func (t *tool) update(ctx context.Context, a updateArgs) v1.ToolResult {
	id := strings.TrimSpace(a.ID)
	if id == "" {
		return errorResult("%s: id is required", ToolScheduleUpdate)
	}
	if a.Name == nil && a.Prompt == nil && a.Schedule == nil && a.Enabled == nil {
		return errorResult("%s: nothing to update: provide at least one of name, prompt, schedule, enabled", ToolScheduleUpdate)
	}
	tasks, err := t.scheduler.List(ctx)
	if err != nil {
		return errorResult("%s: %v", ToolScheduleUpdate, err)
	}
	var current v1.ScheduledTask
	found := false
	for _, task := range tasks {
		if task.ID == id {
			current = task
			found = true
			break
		}
	}
	if !found {
		return errorResult("%s: no scheduled task with id %q", ToolScheduleUpdate, id)
	}
	if a.Name != nil {
		name := strings.TrimSpace(*a.Name)
		if name == "" {
			return errorResult("%s: name must not be empty", ToolScheduleUpdate)
		}
		current.Name = name
	}
	if a.Prompt != nil {
		prompt := strings.TrimSpace(*a.Prompt)
		if prompt == "" {
			return errorResult("%s: prompt must not be empty", ToolScheduleUpdate)
		}
		current.Prompt = prompt
	}
	if a.Schedule != nil {
		current.Schedule = strings.TrimSpace(*a.Schedule)
	}
	if a.Enabled != nil {
		current.Enabled = *a.Enabled
	}
	updated, err := t.scheduler.Update(ctx, current)
	if err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			return errorResult("%s: no scheduled task with id %q", ToolScheduleUpdate, id)
		}
		return errorResult("%s: %v", ToolScheduleUpdate, err)
	}
	return textResult(fmt.Sprintf("updated schedule task %s (name=%s, schedule=%s, enabled=%t)",
		updated.ID, updated.Name, displaySchedule(updated.Schedule), updated.Enabled))
}

// delete implements schedule_delete.
func (t *tool) delete(ctx context.Context, a deleteArgs) v1.ToolResult {
	id := strings.TrimSpace(a.ID)
	if id == "" {
		return errorResult("%s: id is required", ToolScheduleDelete)
	}
	if err := t.scheduler.Delete(ctx, id); err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			return errorResult("%s: no scheduled task with id %q", ToolScheduleDelete, id)
		}
		return errorResult("%s: %v", ToolScheduleDelete, err)
	}
	return textResult(fmt.Sprintf("deleted schedule task %s", id))
}

// displaySchedule renders an empty schedule as "-".
func displaySchedule(schedule string) string {
	schedule = strings.TrimSpace(schedule)
	if schedule == "" {
		return "-"
	}
	return schedule
}

// displayNextRun renders a zero next-run as "-", otherwise RFC 3339 in UTC.
func displayNextRun(next time.Time) string {
	if next.IsZero() {
		return "-"
	}
	return next.UTC().Format(time.RFC3339)
}

// oneLine collapses whitespace/newlines so a value stays on one table row.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// truncateOutput clamps output to max bytes without splitting a UTF-8 rune.
func truncateOutput(content string, max int) string {
	if max <= 0 || len(content) <= max {
		return content
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	return content[:cut] + "\n[truncated: output exceeded the schedule tool limit]"
}

// objectSchema builds one tool argument JSON Schema.
func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// stringProp builds one string property schema.
func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// boolProp builds one boolean property schema.
func boolProp(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

// decodeArgs decodes one call's argument JSON. Missing arguments decode as an
// empty object.
func decodeArgs(call v1.ToolCall, target any) error {
	raw := strings.TrimSpace(call.Arguments)
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("invalid arguments JSON: %w", err)
	}
	return nil
}

// textResult builds a successful tool result.
func textResult(content string) v1.ToolResult { return v1.ToolResult{Content: content} }

// errorResult builds a model-facing tool failure.
func errorResult(format string, args ...any) v1.ToolResult {
	return v1.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}
