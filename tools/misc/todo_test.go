package misc_test

// Tests for todo_write and its in-memory TodoStore: replacement semantics,
// rendering, status normalization, validation and concurrency.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/tools/misc"
)

// todoArguments builds a todo_write argument object.
func todoArguments(t *testing.T, todos []misc.Todo) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"todos": todos})
	if err != nil {
		t.Fatalf("marshal todos: %v", err)
	}
	return string(payload)
}

func TestTodoWriteRendersChecklist(t *testing.T) {
	store := misc.NewTodoStore()
	tool := misc.NewTodoTool(store)
	if tool.Name() != "todo_write" {
		t.Fatalf("Name = %q, want todo_write", tool.Name())
	}
	result := invoke(t, tool, todoArguments(t, []misc.Todo{
		{ID: "1", Content: "write tests", Status: "pending"},
		{ID: "2", Content: "fix bug", Status: "in_progress"},
		{ID: "3", Content: "ship", Status: "completed"},
	}))
	if result.IsError {
		t.Fatalf("todo_write = %q", result.Content)
	}
	want := "[ ] write tests\n[~] fix bug\n[x] ship"
	if result.Content != want {
		t.Fatalf("rendered list = %q, want %q", result.Content, want)
	}
	snapshot := store.Todos()
	if len(snapshot) != 3 || snapshot[0].Status != misc.TodoPending ||
		snapshot[1].Status != misc.TodoInProgress || snapshot[2].Status != misc.TodoCompleted {
		t.Fatalf("store snapshot = %+v, want normalized statuses", snapshot)
	}
	// The snapshot is a copy: mutating it must not change the store.
	snapshot[0].Content = "mutated"
	if store.Todos()[0].Content != "write tests" {
		t.Fatal("Todos() returned the store's backing slice")
	}
}

func TestTodoWriteReplacesWholeList(t *testing.T) {
	store := misc.NewTodoStore()
	tool := misc.NewTodoTool(store)
	invoke(t, tool, todoArguments(t, []misc.Todo{
		{ID: "a", Content: "first", Status: "pending"},
		{ID: "b", Content: "second", Status: "pending"},
	}))
	// The second call replaces the list entirely: "second" is gone.
	result := invoke(t, tool, todoArguments(t, []misc.Todo{
		{ID: "c", Content: "only", Status: "completed"},
	}))
	if result.Content != "[x] only" || store.Len() != 1 {
		t.Fatalf("replace = %q (Len=%d), want one completed item", result.Content, store.Len())
	}
	// An explicit empty array clears the list.
	result = invoke(t, tool, `{"todos":[]}`)
	if result.IsError || result.Content != "0 todos" {
		t.Fatalf("clear = %q (IsError=%v), want 0 todos", result.Content, result.IsError)
	}
	if store.Len() != 0 {
		t.Fatalf("store Len after clear = %d, want 0", store.Len())
	}
}

func TestTodoStatusNormalization(t *testing.T) {
	store := misc.NewTodoStore()
	tool := misc.NewTodoTool(store)
	invoke(t, tool, todoArguments(t, []misc.Todo{
		{ID: "1", Content: "blank", Status: ""},
		{ID: "2", Content: "alias", Status: "ACTIVE"},
		{ID: "3", Content: "done alias", Status: "done"},
	}))
	snapshot := store.Todos()
	if snapshot[0].Status != misc.TodoPending || snapshot[1].Status != misc.TodoInProgress || snapshot[2].Status != misc.TodoCompleted {
		t.Fatalf("normalized statuses = %+v", snapshot)
	}
	result := invoke(t, tool, todoArguments(t, snapshot))
	want := "[ ] blank\n[~] alias\n[x] done alias"
	if result.Content != want {
		t.Fatalf("render = %q, want %q", result.Content, want)
	}
}

func TestTodoWriteValidationErrors(t *testing.T) {
	tool := misc.NewTodoTool(nil)
	cases := []struct {
		name      string
		arguments string
		want      string
	}{
		{"missing todos", `{}`, "required"},
		{"null todos", `{"todos":null}`, "required"},
		{"empty id", `{"todos":[{"id":"","content":"x"}]}`, "empty id"},
		{"duplicate id", `{"todos":[{"id":"a","content":"x"},{"id":"a","content":"y"}]}`, "duplicate"},
		{"empty content", `{"todos":[{"id":"a","content":""}]}`, "empty content"},
		{"invalid status", `{"todos":[{"id":"a","content":"x","status":"weird"}]}`, "invalid status"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			text := invokeError(t, tool, testCase.arguments)
			requireContains(t, text, testCase.want)
		})
	}
	// Too many items is rejected before anything is stored.
	many := make([]misc.Todo, 0, misc.MaxTodos+1)
	for index := 0; index <= misc.MaxTodos; index++ {
		many = append(many, misc.Todo{ID: fmt.Sprintf("t%d", index), Content: "x", Status: "pending"})
	}
	requireContains(t, invokeError(t, tool, todoArguments(t, many)), "at most")

	// Malformed JSON is an error the host sees, not a model tool result.
	if _, err := tool.Invoke(context.Background(), v1.ToolCall{Arguments: "{"}); err == nil {
		t.Fatal("malformed arguments returned nil error")
	}
}

func TestTodoToolWithDefaultStore(t *testing.T) {
	tool := misc.NewTodoTool(nil)
	result := invoke(t, tool, todoArguments(t, []misc.Todo{{ID: "1", Content: "one", Status: "pending"}}))
	if result.IsError || result.Content != "[ ] one" {
		t.Fatalf("default store = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestTodoSchema(t *testing.T) {
	tool := misc.NewTodoTool(misc.NewTodoStore())
	if tool.Description() == "" {
		t.Fatal("todo_write has an empty description")
	}
	schema := tool.Schema()
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "todos" {
		t.Fatalf("schema required = %v, want [todos]", required)
	}
}

func TestTodoWriteIsConcurrent(t *testing.T) {
	store := misc.NewTodoStore()
	tool := misc.NewTodoTool(store)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for call := 0; call < 10; call++ {
				todos := []misc.Todo{
					{ID: fmt.Sprintf("w%d-a", worker), Content: "a", Status: "pending"},
					{ID: fmt.Sprintf("w%d-b", worker), Content: "b", Status: "in_progress"},
				}
				result, err := tool.Invoke(context.Background(), v1.ToolCall{
					Arguments: todoArguments(t, todos),
				})
				if err != nil || result.IsError {
					t.Errorf("concurrent todo_write: result=%q err=%v", result.Content, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if store.Len() != 2 {
		t.Fatalf("store Len after concurrent writes = %d, want 2", store.Len())
	}
	snapshot := store.Todos()
	if len(snapshot) != 2 || !strings.HasPrefix(snapshot[0].ID, "w") {
		t.Fatalf("store snapshot after concurrent writes = %+v", snapshot)
	}
}
