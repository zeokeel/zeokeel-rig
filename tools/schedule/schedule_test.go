package schedule_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/schedule"
)

// invoke runs one schedule tool call and fails on a Go error.
func invoke(t *testing.T, tool rig.Tool, arguments string) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Name: tool.Name(), Arguments: arguments})
	if err != nil {
		t.Fatalf("Invoke(%s): %v", arguments, err)
	}
	return result
}

func toolByName(t *testing.T, tools []rig.Tool, name string) rig.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found in %v", name, toolNames(tools))
	return nil
}

func toolNames(tools []rig.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	return names
}

func requireContains(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("text %q does not contain %q", text, want)
		}
	}
}

func requireError(t *testing.T, result rig.ToolResult, wants ...string) {
	t.Helper()
	if !result.IsError {
		t.Fatalf("expected a tool error, got %q", result.Content)
	}
	requireContains(t, result.Content, wants...)
}

// TestScheduleToolCRUD covers create/list/update/delete and enabled toggling.
func TestScheduleToolCRUD(t *testing.T) {
	scheduler := schedule.NewScheduler()
	tools := schedule.Tools(scheduler)
	if len(tools) != 4 {
		t.Fatalf("Tools returned %d tools, want 4: %v", len(tools), toolNames(tools))
	}
	create := toolByName(t, tools, schedule.ToolScheduleCreate)
	list := toolByName(t, tools, schedule.ToolScheduleList)
	update := toolByName(t, tools, schedule.ToolScheduleUpdate)
	deleteTool := toolByName(t, tools, schedule.ToolScheduleDelete)

	created := invoke(t, create, `{"name":"Morning digest","prompt":"Summarize changes","schedule":"@daily"}`)
	requireContains(t, created.Content, "created schedule task task-1", "Morning digest", "@daily", "enabled=true")

	disabled := invoke(t, create, `{"name":"Nightly","prompt":"Check build","schedule":"0 2 * * *","enabled":false}`)
	requireContains(t, disabled.Content, "task-2", "enabled=false")

	listed := invoke(t, list, `{}`)
	requireContains(t, listed.Content, "ID", "NAME", "SCHEDULE", "ENABLED", "NEXT RUN",
		"task-1", "Morning digest", "@daily", "true",
		"task-2", "Nightly", "0 2 * * *", "false")

	updated := invoke(t, update, `{"id":"task-1","name":"Morning brief","enabled":false}`)
	requireContains(t, updated.Content, "updated schedule task task-1", "Morning brief", "enabled=false")
	listed = invoke(t, list, `{}`)
	requireContains(t, listed.Content, "Morning brief", "false")
	if strings.Contains(listed.Content, "Morning digest") {
		t.Fatalf("old name still present after update: %q", listed.Content)
	}

	requireError(t, invoke(t, update, `{"id":"task-99","name":"x"}`), "no scheduled task")
	requireError(t, invoke(t, update, `{"id":"task-1"}`), "nothing to update")
	requireError(t, invoke(t, update, `{"name":"x"}`), "id is required")

	deleted := invoke(t, deleteTool, `{"id":"task-2"}`)
	requireContains(t, deleted.Content, "deleted schedule task task-2")
	requireError(t, invoke(t, deleteTool, `{"id":"task-2"}`), "no scheduled task")
	listed = invoke(t, list, `{}`)
	if strings.Contains(listed.Content, "Nightly") {
		t.Fatalf("deleted task still listed: %q", listed.Content)
	}
}

// TestScheduleDefaultsAndErrors covers default enabled, nil scheduler and
// argument validation.
func TestScheduleDefaultsAndErrors(t *testing.T) {
	tools := schedule.Tools(nil) // nil scheduler uses the in-memory default
	create := toolByName(t, tools, schedule.ToolScheduleCreate)
	list := toolByName(t, tools, schedule.ToolScheduleList)

	created := invoke(t, create, `{"name":"Default","prompt":"do it"}`)
	requireContains(t, created.Content, "task-1", "enabled=true", "schedule=-")
	listed := invoke(t, list, ``)
	requireContains(t, listed.Content, "task-1", "Default", "-")

	requireError(t, invoke(t, create, `{"prompt":"x"}`), "name or title is required")
	requireError(t, invoke(t, create, `{"name":"x"}`), "prompt is required")
	requireError(t, invoke(t, toolByName(t, tools, schedule.ToolScheduleDelete), `{}`), "id is required")

	if result, err := create.Invoke(context.Background(), rig.ToolCall{Arguments: `{invalid`}); err == nil {
		t.Fatalf("invalid JSON returned no Go error: %+v", result)
	}
	empty := schedule.Tools(schedule.NewScheduler())
	if got := invoke(t, toolByName(t, empty, schedule.ToolScheduleList), `{}`); got.Content != "no scheduled tasks" {
		t.Fatalf("empty list = %q", got.Content)
	}
}

// TestScheduleNextRunRecorded verifies a host-provided NextRun is recorded and
// displayed verbatim.
func TestScheduleNextRunRecorded(t *testing.T) {
	scheduler := schedule.NewScheduler()
	nextRun := time.Date(2026, 12, 1, 9, 30, 0, 0, time.UTC)
	if _, err := scheduler.Create(context.Background(), rig.ScheduledTask{
		Name:     "Future",
		Prompt:   "run later",
		Schedule: "0 9 * * *",
		Enabled:  true,
		NextRun:  nextRun,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	listed := invoke(t, toolByName(t, schedule.Tools(scheduler), schedule.ToolScheduleList), `{}`)
	requireContains(t, listed.Content, "Future", nextRun.Format(time.RFC3339))
}

// TestScheduleSchedulerDirect covers the default scheduler implementation.
func TestScheduleSchedulerDirect(t *testing.T) {
	scheduler := schedule.NewScheduler()
	ctx := context.Background()

	created, err := scheduler.Create(ctx, rig.ScheduledTask{Name: "A", Prompt: "a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID != "task-1" || created.Created.IsZero() {
		t.Fatalf("Create = %+v, want generated id/created", created)
	}
	if _, err := scheduler.Create(ctx, rig.ScheduledTask{ID: "task-1", Name: "dup", Prompt: "x"}); err == nil {
		t.Fatal("duplicate Create succeeded")
	}
	created.Name = "A2"
	updated, err := scheduler.Update(ctx, created)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != "A2" || updated.Created.IsZero() {
		t.Fatalf("Update = %+v", updated)
	}
	if _, err := scheduler.Update(ctx, rig.ScheduledTask{ID: "missing"}); !errors.Is(err, schedule.ErrTaskNotFound) {
		t.Fatalf("Update(missing) = %v, want ErrTaskNotFound", err)
	}
	if err := scheduler.Delete(ctx, "task-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := scheduler.Delete(ctx, "task-1"); !errors.Is(err, schedule.ErrTaskNotFound) {
		t.Fatalf("Delete(missing) = %v, want ErrTaskNotFound", err)
	}
}

// TestScheduleConcurrency verifies concurrent scheduler use.
func TestScheduleConcurrency(t *testing.T) {
	scheduler := schedule.NewScheduler()
	tools := schedule.Tools(scheduler)
	create := toolByName(t, tools, schedule.ToolScheduleCreate)
	list := toolByName(t, tools, schedule.ToolScheduleList)
	const workers = 32
	var waitGroup sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			result, err := create.Invoke(context.Background(), rig.ToolCall{
				Arguments: fmt.Sprintf(`{"name":"task-%d","prompt":"prompt-%d"}`, index, index),
			})
			if err != nil {
				errs <- err
				return
			}
			if result.IsError {
				errs <- fmt.Errorf("create %d: %s", index, result.Content)
			}
		}(i)
	}
	waitGroup.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	tasks, err := scheduler.List(context.Background())
	if err != nil || len(tasks) != workers {
		t.Fatalf("List = %d/%v, want %d tasks", len(tasks), err, workers)
	}
	ids := make(map[string]bool, workers)
	for _, task := range tasks {
		if ids[task.ID] {
			t.Fatalf("duplicate task ID %q", task.ID)
		}
		ids[task.ID] = true
	}
	requireContains(t, invoke(t, list, `{}`).Content, strconv.Itoa(workers-1))
}

// TestScheduleToolMetadata verifies names, descriptions and schemas.
func TestScheduleToolMetadata(t *testing.T) {
	tools := schedule.Tools(schedule.NewScheduler())
	wantNames := []string{schedule.ToolScheduleCreate, schedule.ToolScheduleList, schedule.ToolScheduleUpdate, schedule.ToolScheduleDelete}
	for _, want := range wantNames {
		tool := toolByName(t, tools, want)
		if strings.TrimSpace(tool.Description()) == "" {
			t.Fatalf("%s has an empty description", want)
		}
		schema := tool.Schema()
		if schema["type"] != "object" {
			t.Fatalf("%s schema = %v", want, schema)
		}
	}
	create := toolByName(t, tools, schedule.ToolScheduleCreate)
	required, ok := create.Schema()["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "prompt" {
		t.Fatalf("create required = %v", create.Schema()["required"])
	}
	properties, ok := create.Schema()["properties"].(map[string]any)
	if !ok {
		t.Fatalf("create properties = %v", create.Schema()["properties"])
	}
	for _, name := range []string{"name", "title", "prompt", "schedule", "enabled", "after_seconds", "at", "every_seconds", "daily", "weekly", "cron"} {
		if _, ok := properties[name]; !ok {
			t.Fatalf("create schema is missing %q: %v", name, properties)
		}
	}
}
