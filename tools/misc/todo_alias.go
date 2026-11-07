package misc

import (
	"context"

	"github.com/zeokeel/zeokeel-rig"
)

// NewTodoAliasTool 用旧工具名 "todo"（上一代 agent 的名字）暴露同一个待办列表，而
// NewTodoTool 保留较新的 "todo_write" 拼写。用同一个 store 构造时，两个工具共享同一
// 份列表。
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
