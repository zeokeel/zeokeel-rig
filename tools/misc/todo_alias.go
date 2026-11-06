package misc

import (
	"context"

	"github.com/zeokeel/zeokeel-rig"
)

// NewTodoAliasTool exposes the todo list under the legacy tool name "todo"
// (the previous agent's name) while NewTodoTool keeps the newer "todo_write"
// spelling. Both tools share the same store when constructed with one.
func NewTodoAliasTool(store *TodoStore) rig.Tool {
	return &todoAlias{inner: NewTodoTool(store)}
}

type todoAlias struct {
	inner rig.Tool
}

func (t *todoAlias) Name() string           { return "todo" }
func (t *todoAlias) Description() string    { return t.inner.Description() }
func (t *todoAlias) Schema() map[string]any { return t.inner.Schema() }
func (t *todoAlias) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	return t.inner.Invoke(ctx, call)
}
