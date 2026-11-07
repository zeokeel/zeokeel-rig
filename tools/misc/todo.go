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

// TodoStore 中保存的待办状态。
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoCompleted  = "completed"
)

// MaxTodos 限制单个 todo_write 列表的条目数。
const MaxTodos = 200

// Todo 是一项待办。status 会被规范化为 pending、in_progress 或 completed；Priority
// 可选，保留给需要排序的宿主。
type Todo struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority int    `json:"priority,omitempty"`
}

// TodoStore 是并发安全的进程内待办列表。一个 store 属于一个 agent/会话；由
// NewTodoStore 创建。
type TodoStore struct {
	mu    sync.RWMutex
	todos []Todo
}

// NewTodoStore 创建一个空 store。
func NewTodoStore() *TodoStore { return &TodoStore{} }

// Replace 用 todos 的副本原子地整体替换列表。
func (s *TodoStore) Replace(todos []Todo) {
	copied := slices.Clone(todos)
	s.mu.Lock()
	s.todos = copied
	s.mu.Unlock()
}

// Todos 返回当前列表的快照。
func (s *TodoStore) Todos() []Todo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.todos)
}

// Len 返回待办数量。
func (s *TodoStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.todos)
}

// todoWriteArgs 是 todo_write 的参数对象。使用指针区分“缺失 todos 字段”（错误）与
// “显式空数组”（清空列表）。
type todoWriteArgs struct {
	Todos *[]Todo `json:"todos"`
}

// todoTool 是 todo_write 的 rig.Tool 实现。
type todoTool struct {
	store *TodoStore
}

// NewTodoTool 返回由 store 支撑的 todo_write 工具。store 为 nil 时会新建一个，因此
// `NewTodoTool(nil)` 可直接用于单会话 agent。
func NewTodoTool(store *TodoStore) rig.Tool {
	if store == nil {
		store = NewTodoStore()
	}
	return &todoTool{store: store}
}

// TodoTool 是 todo_write 的具名构造器，等同于 NewTodoTool。
func TodoTool(store *TodoStore) rig.Tool { return NewTodoTool(store) }

// Name 实现 rig.Tool。
func (t *todoTool) Name() string { return "todo_write" }

// Description 实现 rig.Tool。
func (t *todoTool) Description() string {
	return "Replace the session's todo list with a new list and return it rendered. " +
		"Statuses: pending, in_progress, completed. Send an empty array to clear the list."
}

// Schema 实现 rig.Tool。
func (t *todoTool) Schema() map[string]any {
	item := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":       toolutil.StringProp("Optional stable item ID (generated when omitted; unique in the list)."),
			"content":  toolutil.StringProp("What to do."),
			"status":   map[string]any{"type": "string", "enum": []any{TodoPending, TodoInProgress, TodoCompleted}, "description": "Defaults to pending."},
			"priority": map[string]any{"type": "integer", "description": "Optional integer priority (higher = more important); kept for hosts."},
		},
		"required":             []string{"content"},
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

// Invoke 实现 rig.Tool。
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

// normalizeTodos 校验并规范化传入的列表。
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
			// DSH 的条目只有 {content,status}；这里为宿主侧 store 生成稳定 id，并避免与同一列表
			// 中显式给出的 id 冲突。
			base := fmt.Sprintf("todo-%d", index+1)
			todo.ID = base
			for seen[todo.ID] {
				todo.ID += "-x"
			}
		} else if seen[todo.ID] {
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

// normalizeStatus 把可接受的状态拼写映射为存储值。
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

// renderTodos 渲染模型可见的检查清单："[ ]" 待办、"[~]" 进行中、"[x]" 已完成。
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
