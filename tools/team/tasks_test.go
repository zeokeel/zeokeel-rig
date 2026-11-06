package team

// Tests for the shared task board: the in-memory rig.TeamTaskManager and the
// team_task_create/list/get/update tools.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	rigtools "github.com/zeokeel/zeokeel-rig/tools"
)

// newTaskRegistry returns a registry containing only the shared task tools.
func newTaskRegistry(t *testing.T, manager rig.TeamTaskManager) *rigtools.Registry {
	t.Helper()
	return rigtools.NewRegistry(TaskTools(manager)...)
}

// taskDetail fetches one task through the get tool and returns its text.
func taskDetail(t *testing.T, registry *rigtools.Registry, id string) string {
	t.Helper()
	return invokeOK(t, registry, ToolTeamTaskGet, `{"id":"`+id+`"}`)
}

// TestTaskToolNamesAndSchemas verifies metadata and JSON schemas.
func TestTaskToolNamesAndSchemas(t *testing.T) {
	manager := NewTaskManager()
	names := TaskToolNames()
	tools := TaskTools(manager)
	if len(names) != len(tools) {
		t.Fatalf("TaskToolNames() = %d names, TaskTools() = %d tools", len(names), len(tools))
	}
	for index, name := range names {
		if tools[index].Name() != name {
			t.Fatalf("name[%d] = %q, tool name = %q", index, name, tools[index].Name())
		}
		if strings.TrimSpace(tools[index].Description()) == "" {
			t.Fatalf("%s has an empty description", name)
		}
		if tools[index].Schema()["type"] != "object" {
			t.Fatalf("%s schema = %v", name, tools[index].Schema())
		}
	}
	if aliasNames := toolNamesOf(Tools(manager)); strings.Join(aliasNames, ",") != strings.Join(names, ",") {
		t.Fatalf("Tools() names = %v, want %v", aliasNames, names)
	}
	if len(TaskTools(nil)) != len(names) {
		t.Fatalf("TaskTools(nil) returned %d tools", len(TaskTools(nil)))
	}

	required := func(name string) []string {
		values, _ := taskToolByName(tools, name).Schema()["required"].([]string)
		return values
	}
	if got := required(ToolTeamTaskCreate); len(got) != 1 || got[0] != "subject" {
		t.Fatalf("create required = %v", got)
	}
	if got := required(ToolTeamTaskGet); len(got) != 1 || got[0] != "id" {
		t.Fatalf("get required = %v", got)
	}
	if got := required(ToolTeamTaskUpdate); len(got) != 2 || got[0] != "id" || got[1] != "action" {
		t.Fatalf("update required = %v", got)
	}
	updateSchema := taskToolByName(tools, ToolTeamTaskUpdate).Schema()
	properties, _ := updateSchema["properties"].(map[string]any)
	action, _ := properties["action"].(map[string]any)
	enum, _ := action["enum"].([]string)
	if len(enum) != 7 {
		t.Fatalf("update action enum = %v, want seven actions", action["enum"])
	}
}

// toolNamesOf returns the names of tools in order.
func toolNamesOf(tools []rig.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	return names
}

// toolByName finds one tool by name.
func taskToolByName(tools []rig.Tool, name string) rig.Tool {
	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}
	return nil
}

// TestTaskCreateListGet covers creation, filtering and detail rendering.
func TestTaskCreateListGet(t *testing.T) {
	registry := newTaskRegistry(t, NewTaskManager())

	first := invokeOK(t, registry, ToolTeamTaskCreate,
		`{"subject":"Fix bug","description":"details here","owner":"alice","write_scopes":["tools/a","tools/b","tools/a"]}`)
	requireTaskContains(t, first, "task-1", "subject: Fix bug", "status: pending", "owner: alice",
		"write_scopes: tools/a, tools/b", "description:", "details here")

	second := invokeOK(t, registry, ToolTeamTaskCreate, `{"subject":"Write docs","blocked_by":["task-1"]}`)
	requireTaskContains(t, second, "task-2", "blocked_by: task-1", "owner: -")

	listed := invokeOK(t, registry, ToolTeamTaskList, `{}`)
	requireTaskContains(t, listed, "ID", "STATUS", "OWNER", "SUBJECT", "BLOCKED BY",
		"task-1", "Fix bug", "task-2", "Write docs")
	if strings.Index(listed, "task-1") > strings.Index(listed, "task-2") {
		t.Fatalf("list is not ordered by created time: %q", listed)
	}

	owned := invokeOK(t, registry, ToolTeamTaskList, `{"owner":"alice"}`)
	requireTaskContains(t, owned, "task-1")
	if strings.Contains(owned, "task-2") {
		t.Fatalf("owner filter leaked another task: %q", owned)
	}

	detail := taskDetail(t, registry, "task-2")
	requireTaskContains(t, detail, "id: task-2", "subject: Write docs", "status: pending",
		"blocked_by: task-1", "created:", "updated:")

	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskGet, `{"id":"task-99"}`), "not found")
	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskGet, `{}`), "id is required")
	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskCreate, `{"subject":"   "}`), "subject is required")
	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskCreate,
		`{"subject":"bad dep","blocked_by":["task-99"]}`), "unknown task")
}

// TestTaskUpdateActions covers claim/complete/reopen/release/edit/
// set_dependencies/reassign.
func TestTaskUpdateActions(t *testing.T) {
	registry := newTaskRegistry(t, NewTaskManager())
	invokeOK(t, registry, ToolTeamTaskCreate, `{"subject":"Task one"}`)
	invokeOK(t, registry, ToolTeamTaskCreate, `{"subject":"Task two"}`)

	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-1","action":"claim"}`), "owner is required")

	claimed := invokeOK(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-1","action":"claim","owner":"alice"}`)
	requireTaskContains(t, claimed, "status=in_progress", "owner=alice")
	requireTaskContains(t, taskDetail(t, registry, "task-1"), "status: in_progress", "owner: alice")

	requireTaskContains(t, invokeOK(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-1","action":"complete"}`), "status=completed")
	requireTaskContains(t, taskDetail(t, registry, "task-1"), "status: completed")
	requireTaskContains(t, invokeOK(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-1","action":"reopen"}`), "status=pending")

	invokeOK(t, registry, ToolTeamTaskUpdate, `{"id":"task-1","action":"claim","owner":"bob"}`)
	released := invokeOK(t, registry, ToolTeamTaskUpdate, `{"id":"task-1","action":"release"}`)
	requireTaskContains(t, released, "status=pending", "owner=-")
	requireTaskContains(t, taskDetail(t, registry, "task-1"), "status: pending", "owner: -")

	edited := invokeOK(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-2","action":"edit","subject":"Task two edited","description":"new details","write_scopes":["tools/x"]}`)
	requireTaskContains(t, edited, "task-2")
	requireTaskContains(t, taskDetail(t, registry, "task-2"),
		"subject: Task two edited", "description:", "new details", "write_scopes: tools/x")

	reassigned := invokeOK(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-2","action":"reassign","owner":"carol"}`)
	requireTaskContains(t, reassigned, "owner=carol")
	requireTaskContains(t, taskDetail(t, registry, "task-2"), "owner: carol")

	added := invokeOK(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-2","action":"set_dependencies","add_blocked_by":["task-1"]}`)
	requireTaskContains(t, added, "blocked_by=task-1")
	requireTaskContains(t, invokeOK(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-2","action":"set_dependencies","remove_blocked_by":["task-1"]}`), "blocked_by=-")

	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-1","action":"nope"}`), "unknown action")
	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-99","action":"complete"}`), "not found")
	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-1","action":"edit","status":"weird"}`), "invalid")
	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-1","action":"edit"}`), "nothing to edit")
	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-1","action":"set_dependencies"}`), "add_blocked_by")
	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-1"}`), "action is required")
	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskUpdate,
		`{"action":"complete"}`), "id is required")

	if result, err := taskToolByName(TaskTools(nil), ToolTeamTaskUpdate).Invoke(context.Background(),
		rig.ToolCall{Arguments: `{not json`}); err == nil {
		t.Fatalf("invalid JSON returned no Go error: %+v", result)
	}
}

// TestTaskDependencyCycleValidation covers direct, transitive and unknown
// dependencies; failures must leave the stored graph unchanged.
func TestTaskDependencyCycleValidation(t *testing.T) {
	registry := newTaskRegistry(t, NewTaskManager())
	invokeOK(t, registry, ToolTeamTaskCreate, `{"subject":"A"}`)
	invokeOK(t, registry, ToolTeamTaskCreate, `{"subject":"B"}`)
	invokeOK(t, registry, ToolTeamTaskCreate, `{"subject":"C"}`)

	invokeOK(t, registry, ToolTeamTaskUpdate, `{"id":"task-2","action":"set_dependencies","add_blocked_by":["task-1"]}`)
	invokeOK(t, registry, ToolTeamTaskUpdate, `{"id":"task-3","action":"set_dependencies","add_blocked_by":["task-2"]}`)

	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-1","action":"set_dependencies","add_blocked_by":["task-1"]}`), "cycle")
	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-1","action":"set_dependencies","add_blocked_by":["task-3"]}`), "cycle")
	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-1","action":"set_dependencies","add_blocked_by":["task-99"]}`), "unknown task")

	requireTaskContains(t, taskDetail(t, registry, "task-1"), "blocked_by: -")
	requireTaskContains(t, taskDetail(t, registry, "task-2"), "blocked_by: task-1")
	requireTaskContains(t, taskDetail(t, registry, "task-3"), "blocked_by: task-2")
}

// TestTaskStatusValidation covers status validation in the manager and tools.
func TestTaskStatusValidation(t *testing.T) {
	manager := NewTaskManager()
	ctx := testContext(t)
	if _, err := manager.Create(ctx, rig.TeamTask{Subject: "x", Status: "weird"}); !errors.Is(err, ErrTaskInvalidStatus) {
		t.Fatalf("Create(weird status) = %v, want ErrTaskInvalidStatus", err)
	}
	if _, err := manager.Create(ctx, rig.TeamTask{Subject: "x", Status: TaskStatusInProgress}); !errors.Is(err, ErrTaskOwnerRequired) {
		t.Fatalf("Create(in_progress, no owner) = %v, want ErrTaskOwnerRequired", err)
	}
	if _, err := manager.Create(ctx, rig.TeamTask{Subject: "  "}); !errors.Is(err, ErrTaskSubjectRequired) {
		t.Fatalf("Create(blank subject) = %v, want ErrTaskSubjectRequired", err)
	}
	if _, err := manager.List(ctx, rig.TeamTaskFilter{Status: "weird"}); !errors.Is(err, ErrTaskInvalidStatus) {
		t.Fatalf("List(weird status) = %v, want ErrTaskInvalidStatus", err)
	}

	registry := newTaskRegistry(t, manager)
	created := invokeOK(t, registry, ToolTeamTaskCreate, `{"subject":"x"}`)
	requireTaskContains(t, created, "task-1")
	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskList, `{"status":"weird"}`), "invalid")
	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskUpdate,
		`{"id":"task-1","action":"edit","status":"weird"}`), "invalid")
}

// TestTaskManagerDirect covers the manager API directly: id/timestamps,
// filtering, ordering, copies and unknown ids.
func TestTaskManagerDirect(t *testing.T) {
	manager := NewTaskManager()
	ctx := testContext(t)

	first, err := manager.Create(ctx, rig.TeamTask{Subject: "First", WriteScopes: []string{" a ", "a", ""}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if first.ID != "task-1" || first.Status != TaskStatusPending || first.Created.IsZero() ||
		first.Updated.IsZero() || len(first.WriteScopes) != 1 || first.WriteScopes[0] != "a" {
		t.Fatalf("created task = %+v", first)
	}
	second, err := manager.Create(ctx, rig.TeamTask{Subject: "Second", BlockedBy: []string{"task-1"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if second.BlockedBy[0] != "task-1" {
		t.Fatalf("second dependencies = %v", second.BlockedBy)
	}
	if _, err := manager.Create(ctx, rig.TeamTask{ID: "task-1", Subject: "dup"}); !errors.Is(err, ErrTaskExists) {
		t.Fatalf("duplicate Create = %v, want ErrTaskExists", err)
	}
	if _, err := manager.Get(ctx, "missing"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrTaskNotFound", err)
	}
	if _, err := manager.Update(ctx, "missing", rig.TeamTaskUpdate{}); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Update(missing) = %v, want ErrTaskNotFound", err)
	}
	if _, err := manager.Update(ctx, "task-1", rig.TeamTaskUpdate{}); !errors.Is(err, ErrTaskNoChanges) {
		t.Fatalf("empty Update = %v, want ErrTaskNoChanges", err)
	}

	// Returned values are copies: mutating them must not change the board.
	copied, err := manager.Get(ctx, "task-2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	copied.BlockedBy[0] = "mutated"
	copied.Subject = "mutated"
	refetched, err := manager.Get(ctx, "task-2")
	if err != nil || refetched.BlockedBy[0] != "task-1" || refetched.Subject != "Second" {
		t.Fatalf("Get returned aliased data: %+v/%v", refetched, err)
	}

	subject := "First edited"
	updated, err := manager.Update(ctx, "task-1", rig.TeamTaskUpdate{Subject: &subject})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Subject != "First edited" || updated.Updated.Before(updated.Created) {
		t.Fatalf("updated task = %+v", updated)
	}

	all, err := manager.List(ctx, rig.TeamTaskFilter{})
	if err != nil || len(all) != 2 || all[0].ID != "task-1" || all[1].ID != "task-2" {
		t.Fatalf("List = %+v/%v", all, err)
	}
	pending, err := manager.List(ctx, rig.TeamTaskFilter{Status: TaskStatusPending})
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending List = %d/%v", len(pending), err)
	}
	inProgress, err := manager.List(ctx, rig.TeamTaskFilter{Status: TaskStatusInProgress})
	if err != nil || len(inProgress) != 0 {
		t.Fatalf("in_progress List = %d/%v", len(inProgress), err)
	}
}

// TestTaskFiltering covers status/owner filtering through the tools.
func TestTaskFiltering(t *testing.T) {
	manager := NewTaskManager()
	ctx := testContext(t)
	pending, err := manager.Create(ctx, rig.TeamTask{Subject: "Pending", Owner: "alice"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := manager.Create(ctx, rig.TeamTask{Subject: "Active", Status: TaskStatusInProgress, Owner: "bob"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := manager.Update(ctx, pending.ID, rig.TeamTaskUpdate{Status: strPtr(TaskStatusCompleted)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	registry := newTaskRegistry(t, manager)

	completed := invokeOK(t, registry, ToolTeamTaskList, `{"status":"completed"}`)
	requireTaskContains(t, completed, "Pending")
	if strings.Contains(completed, "Active") {
		t.Fatalf("status filter leaked another task: %q", completed)
	}
	active := invokeOK(t, registry, ToolTeamTaskList, `{"owner":"bob"}`)
	requireTaskContains(t, active, "Active")
	if strings.Contains(active, "Pending") {
		t.Fatalf("owner filter leaked another task: %q", active)
	}
	combined := invokeOK(t, registry, ToolTeamTaskList, `{"status":"in_progress","owner":"bob"}`)
	requireTaskContains(t, combined, "Active")
	empty := invokeOK(t, registry, ToolTeamTaskList, `{"status":"completed","owner":"bob"}`)
	requireTaskContains(t, empty, "no shared team tasks")
}

// strPtr returns a pointer to value.
func strPtr(value string) *string { return &value }

// TestTaskLimit covers the configurable task-count limit.
func TestTaskLimit(t *testing.T) {
	manager := NewTaskManager(WithMaxTasks(2))
	ctx := testContext(t)
	if _, err := manager.Create(ctx, rig.TeamTask{Subject: "one"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := manager.Create(ctx, rig.TeamTask{Subject: "two"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := manager.Create(ctx, rig.TeamTask{Subject: "three"}); !errors.Is(err, ErrTaskBoardFull) {
		t.Fatalf("third Create = %v, want ErrTaskBoardFull", err)
	}
	registry := newTaskRegistry(t, manager)
	requireTaskContains(t, invokeError(t, registry, ToolTeamTaskCreate, `{"subject":"three"}`), "full")
	if got := NewTaskManager().maxTasks; got != DefaultMaxTasks {
		t.Fatalf("default maxTasks = %d, want %d", got, DefaultMaxTasks)
	}
}

// TestTaskConcurrency verifies concurrent creates, claims and lists.
func TestTaskConcurrency(t *testing.T) {
	manager := NewTaskManager()
	const workers = 32
	var waitGroup sync.WaitGroup
	createErrs := make(chan error, workers)
	for index := 0; index < workers; index++ {
		waitGroup.Add(1)
		go func(number int) {
			defer waitGroup.Done()
			if _, err := manager.Create(context.Background(), rig.TeamTask{
				Subject: fmt.Sprintf("task-%d", number),
			}); err != nil {
				createErrs <- err
			}
		}(index)
	}
	waitGroup.Wait()
	close(createErrs)
	for err := range createErrs {
		t.Fatal(err)
	}
	tasks, err := manager.List(context.Background(), rig.TeamTaskFilter{})
	if err != nil || len(tasks) != workers {
		t.Fatalf("List after concurrent creates = %d/%v, want %d", len(tasks), err, workers)
	}
	ids := make(map[string]bool, workers)
	for _, task := range tasks {
		if ids[task.ID] {
			t.Fatalf("duplicate task id %q", task.ID)
		}
		ids[task.ID] = true
	}

	// Concurrent claims on distinct tasks and reads must stay race-free.
	var second sync.WaitGroup
	claimErrs := make(chan error, workers)
	for _, task := range tasks {
		second.Add(1)
		go func(id string) {
			defer second.Done()
			owner := "owner-" + id
			if _, err := manager.Update(context.Background(), id, rig.TeamTaskUpdate{
				Status: strPtr(TaskStatusInProgress),
				Owner:  &owner,
			}); err != nil {
				claimErrs <- fmt.Errorf("claim %s: %w", id, err)
			}
			_, _ = manager.List(context.Background(), rig.TeamTaskFilter{Status: TaskStatusInProgress})
		}(task.ID)
	}
	second.Wait()
	close(claimErrs)
	for err := range claimErrs {
		t.Fatal(err)
	}
	inProgress, err := manager.List(context.Background(), rig.TeamTaskFilter{Status: TaskStatusInProgress})
	if err != nil || len(inProgress) != workers {
		t.Fatalf("in_progress tasks = %d/%v, want %d", len(inProgress), err, workers)
	}
}

// TestTaskNilManager verifies tools constructed without a manager fail
// readably on every invocation.
func TestTaskNilManager(t *testing.T) {
	registry := newTaskRegistry(t, nil)
	cases := []struct {
		name string
		args string
	}{
		{ToolTeamTaskCreate, `{"subject":"x"}`},
		{ToolTeamTaskList, `{}`},
		{ToolTeamTaskGet, `{"id":"task-1"}`},
		{ToolTeamTaskUpdate, `{"id":"task-1","action":"claim","owner":"a"}`},
	}
	for _, testCase := range cases {
		result, err := invokeTool(t, registry, testCase.name, testCase.args)
		if err != nil {
			t.Fatalf("%s returned a Go error: %v", testCase.name, err)
		}
		if !result.IsError || !strings.Contains(result.Content, "not configured") {
			t.Fatalf("%s with nil manager = %+v, want a configuration error", testCase.name, result)
		}
	}
}

// requireTaskContains fails when text lacks any wanted fragment.
func requireTaskContains(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("text %q does not contain %q", text, want)
		}
	}
}
