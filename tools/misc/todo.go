package misc

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// Todo statuses as stored in a TodoStore.
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoCompleted  = "completed"
)

// MaxTodos bounds one todo_write list.
const MaxTodos = 200

// Todo is one todo item. Status is normalized to pending, in_progress or
// completed; Priority is optional and kept for hosts that sort the list.
type Todo struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority int    `json:"priority,omitempty"`
}

// TodoStore is a concurrency-safe in-memory todo list. One store belongs to one
// agent/session; NewTodoStore creates it.
type TodoStore struct {
	mu    sync.RWMutex
	todos []Todo
}

// NewTodoStore creates an empty store.
func NewTodoStore() *TodoStore { return &TodoStore{} }

// Replace atomically replaces the whole list with a copy of todos.
func (s *TodoStore) Replace(todos []Todo) {
	copied := slices.Clone(todos)
	s.mu.Lock()
	s.todos = copied
	s.mu.Unlock()
}

// Todos returns a snapshot of the current list.
func (s *TodoStore) Todos() []Todo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.todos)
}

// Len returns the number of todos.
func (s *TodoStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.todos)
}

// todoWriteArgs is the todo_write argument object. A pointer distinguishes a
// missing "todos" field (an error) from an explicit empty array (clears the
// list).
type todoWriteArgs struct {
	Todos *[]Todo `json:"todos"`
}

// todoTool is the rig.Tool implementation of todo_write.
type todoTool struct {
	store *TodoStore
}

// NewTodoTool returns the todo_write tool backed by store. A nil store creates
// a fresh one, so `NewTodoTool(nil)` is usable for a single-session agent.
func NewTodoTool(store *TodoStore) rig.Tool {
	if store == nil {
		store = NewTodoStore()
	}
	return &todoTool{store: store}
}

// TodoTool is the named constructor of todo_write; it is NewTodoTool.
func TodoTool(store *TodoStore) rig.Tool { return NewTodoTool(store) }

// Name implements rig.Tool.
func (t *todoTool) Name() string { return "todo_write" }

// Description implements rig.Tool.
func (t *todoTool) Description() string {
	return "Replace the session's todo list with a new list and return it rendered. " +
		"Statuses: pending, in_progress, completed. Send an empty array to clear the list."
}

// Schema implements rig.Tool.
func (t *todoTool) Schema() map[string]any {
	item := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":       toolutil.StringProp("Stable item ID, unique in the list."),
			"content":  toolutil.StringProp("What to do."),
			"status":   map[string]any{"type": "string", "enum": []any{TodoPending, TodoInProgress, TodoCompleted}, "description": "Defaults to pending."},
			"priority": map[string]any{"type": "integer", "description": "Optional integer priority (higher = more important); kept for hosts."},
		},
		"required":             []string{"id", "content"},
		"additionalProperties": false,
	}
	return toolutil.ObjectSchema(map[string]any{
		"todos": map[string]any{
			"type":        "array",
			"description": "The complete new todo list (replaces the previous one; [] clears it).",
			"items":       item,
			"maxItems":    MaxTodos,
		},
	}, "todos")
}

// Invoke implements rig.Tool.
func (t *todoTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args todoWriteArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	if args.Todos == nil {
		return toolutil.ErrorResultf("todo_write: todos is required (send an array; [] clears the list)"), nil
	}
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	todos, err := normalizeTodos(*args.Todos)
	if err != nil {
		return toolutil.ErrorResultf("todo_write: %v", err), nil
	}
	t.store.Replace(todos)
	return toolutil.TextResult(renderTodos(todos)), nil
}

// normalizeTodos validates and normalizes one incoming list.
func normalizeTodos(todos []Todo) ([]Todo, error) {
	if len(todos) > MaxTodos {
		return nil, fmt.Errorf("at most %d todos are allowed, got %d", MaxTodos, len(todos))
	}
	normalized := make([]Todo, 0, len(todos))
	seen := make(map[string]bool, len(todos))
	for index, todo := range todos {
		todo.ID = strings.TrimSpace(todo.ID)
		todo.Content = strings.TrimSpace(todo.Content)
		if todo.ID == "" {
			return nil, fmt.Errorf("todo #%d has an empty id", index+1)
		}
		if seen[todo.ID] {
			return nil, fmt.Errorf("duplicate todo id %q", todo.ID)
		}
		seen[todo.ID] = true
		if todo.Content == "" {
			return nil, fmt.Errorf("todo %q has empty content", todo.ID)
		}
		status, ok := normalizeStatus(todo.Status)
		if !ok {
			return nil, fmt.Errorf("todo %q has invalid status %q (want pending, in_progress or completed)",
				todo.ID, todo.Status)
		}
		todo.Status = status
		normalized = append(normalized, todo)
	}
	return normalized, nil
}

// normalizeStatus maps the accepted status spelling to the stored value.
func normalizeStatus(status string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "pending", "todo", "open":
		return TodoPending, true
	case "in_progress", "in-progress", "inprogress", "active", "doing", "started":
		return TodoInProgress, true
	case "completed", "complete", "done", "finished", "closed":
		return TodoCompleted, true
	default:
		return "", false
	}
}

// renderTodos renders the model-facing checklist: "[ ]" pending, "[~]" in
// progress, "[x]" completed.
func renderTodos(todos []Todo) string {
	if len(todos) == 0 {
		return "0 todos"
	}
	lines := make([]string, 0, len(todos))
	for _, todo := range todos {
		marker := "[ ]"
		switch todo.Status {
		case TodoInProgress:
			marker = "[~]"
		case TodoCompleted:
			marker = "[x]"
		}
		lines = append(lines, marker+" "+todo.Content)
	}
	return strings.Join(lines, "\n")
}
