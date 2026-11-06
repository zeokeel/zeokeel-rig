package misc

import (
	"context"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// NewTodoAliasTool exposes the todo list under the legacy tool name "todo"
// (the previous agent's name) while NewTodoTool keeps the newer "todo_write"
// spelling. Both tools share the same store when constructed with one.
func NewTodoAliasTool(store *TodoStore) v1.Tool {
	return &todoAlias{inner: NewTodoTool(store)}
}

type todoAlias struct {
	inner v1.Tool
}

func (t *todoAlias) Name() string           { return "todo" }
func (t *todoAlias) Description() string    { return t.inner.Description() }
func (t *todoAlias) Schema() map[string]any { return t.inner.Schema() }
func (t *todoAlias) Invoke(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	return t.inner.Invoke(ctx, call)
}
