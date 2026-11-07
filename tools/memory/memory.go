package memory

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

const (
	// ToolName 是模型可见的工具名。
	ToolName = "memory"
	// DefaultScope 是调用未提供 scope 时存储的作用域。
	DefaultScope = "workspace"
	// DefaultMaxOutputBytes 限制单次工具响应的字节数。
	DefaultMaxOutputBytes = 32 << 10
)

// MemoryStore 是并发安全的进程内 rig.MemoryStore 实现。List 按插入顺序返回条目；
// Add 收到空 ID 时生成 "mem-<n>"，Created 默认取当前 UTC 时间。可并发调用。
type MemoryStore struct {
	mu    sync.Mutex
	seq   uint64
	items []rig.Memory
}

var _ rig.MemoryStore = (*MemoryStore)(nil)

// NewMemoryStore 返回一个空的进程内存储。
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

// List 实现 rig.MemoryStore，按插入顺序返回一份副本。
func (s *MemoryStore) List(context.Context) ([]rig.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.items), nil
}

// Add 实现 rig.MemoryStore：缺失的 ID 会自动生成，缺失的 Created 设为当前时间（UTC），
// 文本按原样存储。
func (s *MemoryStore) Add(_ context.Context, memory rig.Memory) (rig.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(memory.ID) == "" {
		s.seq++
		memory.ID = fmt.Sprintf("mem-%d", s.seq)
	}
	if memory.Created.IsZero() {
		memory.Created = time.Now().UTC()
	}
	s.items = append(s.items, memory)
	return memory, nil
}

// Remove 实现 rig.MemoryStore。
func (s *MemoryStore) Remove(_ context.Context, id string) bool {
	id = strings.TrimSpace(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, memory := range s.items {
		if memory.ID == id {
			s.items = slices.Delete(s.items, index, index+1)
			return true
		}
	}
	return false
}

// Search 实现 rig.MemoryStore：查询对记忆文本做大小写不敏感的子串匹配；空查询返回
// 全部条目。
func (s *MemoryStore) Search(_ context.Context, query string) ([]rig.Memory, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	s.mu.Lock()
	defer s.mu.Unlock()
	if query == "" {
		return slices.Clone(s.items), nil
	}
	matches := make([]rig.Memory, 0, len(s.items))
	for _, memory := range s.items {
		if strings.Contains(strings.ToLower(memory.Text), query) {
			matches = append(matches, memory)
		}
	}
	return matches, nil
}

// Tools 返回绑定到 store 的 memory 工具。store 为 nil 时使用一个新的进程内存储。
func Tools(store rig.MemoryStore) []rig.Tool {
	if store == nil {
		store = NewMemoryStore()
	}
	return []rig.Tool{&tool{store: store}}
}

// tool 是 `memory` 工具的实现。
type tool struct {
	store rig.MemoryStore
}

var _ rig.Tool = (*tool)(nil)

// Name 实现 rig.Tool。
func (t *tool) Name() string { return ToolName }

// Description 实现 rig.Tool。
func (t *tool) Description() string {
	return `Manage the agent's memory entries through a host-provided store.

Actions:
- add: store a new memory. "text" is required; optional "scope" (default workspace) groups entries (for example workspace or global).
- list: show memories as "- <id> [<scope>] <text>". Optional "scope" filters by exact scope.
- search: case-insensitive substring search over memory text. "query" is required; optional "scope" filters by exact scope.
- remove: delete one memory by "id". Unknown IDs are an error.
- clear: remove every memory in the optional "scope" (all memories when the scope is omitted) and report the count.

The stored entry keeps its ID, text, scope and creation time. Whether the store
is durable depends on the host; the default implementation is process-local.`
}

// Schema 实现 rig.Tool。
func (t *tool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "Operation to perform.",
				"enum":        []string{"add", "list", "search", "remove", "clear"},
			},
			"text": map[string]any{
				"type":        "string",
				"description": "Memory text (required for add).",
			},
			"scope": map[string]any{
				"type":        "string",
				"description": "Optional scope for add/list/search/clear (default workspace for add).",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "Memory ID (required for remove).",
			},
			"query": map[string]any{
				"type":        "string",
				"description": "Search query (required for search).",
			},
		},
		"required":             []string{"action"},
		"additionalProperties": false,
	}
}

// args 是解码后的 memory 工具参数对象。
type args struct {
	Action string `json:"action"`
	Text   string `json:"text"`
	Scope  string `json:"scope"`
	ID     string `json:"id"`
	Query  string `json:"query"`
}

// Invoke 实现 rig.Tool。
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	var a args
	if err := toolutil.DecodeArgs(call, &a); err != nil {
		return rig.ToolResult{}, err
	}
	switch strings.ToLower(strings.TrimSpace(a.Action)) {
	case "add":
		return t.add(ctx, a), nil
	case "list":
		return t.list(ctx, a), nil
	case "search":
		return t.search(ctx, a), nil
	case "remove":
		return t.remove(ctx, a), nil
	case "clear":
		return t.clear(ctx, a), nil
	case "":
		return toolutil.ErrorResultf("memory: action is required (add, list, search, remove, clear)"), nil
	default:
		return toolutil.ErrorResultf("memory: unknown action %q (add, list, search, remove, clear)", a.Action), nil
	}
}

// add 实现 add 动作。
func (t *tool) add(ctx context.Context, a args) rig.ToolResult {
	text := strings.TrimSpace(a.Text)
	if text == "" {
		return toolutil.ErrorResultf("memory: text is required for action: add")
	}
	scope := strings.TrimSpace(a.Scope)
	if scope == "" {
		scope = DefaultScope
	}
	added, err := t.store.Add(ctx, rig.Memory{Text: text, Scope: scope})
	if err != nil {
		return toolutil.ErrorResultf("memory: add: %v", err)
	}
	return toolutil.TextResult(fmt.Sprintf("added memory %s (scope=%s)", added.ID, displayScope(added.Scope)))
}

// list 实现 list 动作。
func (t *tool) list(ctx context.Context, a args) rig.ToolResult {
	memories, err := t.store.List(ctx)
	if err != nil {
		return toolutil.ErrorResultf("memory: list: %v", err)
	}
	return toolutil.TextResult(toolutil.TruncateBytes(formatMemories(filterScope(memories, a.Scope)), DefaultMaxOutputBytes, "\n[truncated: output exceeded the memory tool limit]"))
}

// search 实现 search 动作。
func (t *tool) search(ctx context.Context, a args) rig.ToolResult {
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return toolutil.ErrorResultf("memory: query is required for action: search")
	}
	memories, err := t.store.Search(ctx, query)
	if err != nil {
		return toolutil.ErrorResultf("memory: search: %v", err)
	}
	memories = filterScope(memories, a.Scope)
	return toolutil.TextResult(toolutil.TruncateBytes(formatMemories(memories), DefaultMaxOutputBytes, "\n[truncated: output exceeded the memory tool limit]"))
}

// remove 实现 remove 动作。
func (t *tool) remove(ctx context.Context, a args) rig.ToolResult {
	id := strings.TrimSpace(a.ID)
	if id == "" {
		return toolutil.ErrorResultf("memory: id is required for action: remove")
	}
	if !t.store.Remove(ctx, id) {
		return toolutil.ErrorResultf("memory: no memory with id %q", id)
	}
	return toolutil.TextResult(fmt.Sprintf("removed memory %s", id))
}

// clear 基于 MemoryStore 接口（逐条 List 再 Remove）实现 clear 动作；指定 scope 时
// 只删除该作用域的条目。
func (t *tool) clear(ctx context.Context, a args) rig.ToolResult {
	memories, err := t.store.List(ctx)
	if err != nil {
		return toolutil.ErrorResultf("memory: clear: %v", err)
	}
	memories = filterScope(memories, a.Scope)
	removed := 0
	for _, memory := range memories {
		if t.store.Remove(ctx, memory.ID) {
			removed++
		}
	}
	scope := strings.TrimSpace(a.Scope)
	if scope == "" {
		return toolutil.TextResult(fmt.Sprintf("cleared %d memor(ies)", removed))
	}
	return toolutil.TextResult(fmt.Sprintf("cleared %d memor(ies) in scope %s", removed, scope))
}

// filterScope 保留 scope 匹配（大小写不敏感的精确匹配）的记忆；空 scope 表示全部保留。
func filterScope(memories []rig.Memory, scope string) []rig.Memory {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return memories
	}
	filtered := make([]rig.Memory, 0, len(memories))
	for _, memory := range memories {
		if strings.EqualFold(strings.TrimSpace(memory.Scope), scope) {
			filtered = append(filtered, memory)
		}
	}
	return filtered
}

// formatMemories 把记忆渲染为可读行，排序规则确定：先按创建时间，再按 ID。
func formatMemories(memories []rig.Memory) string {
	if len(memories) == 0 {
		return "no memories"
	}
	ordered := slices.Clone(memories)
	slices.SortStableFunc(ordered, func(a, b rig.Memory) int {
		return cmp.Or(a.Created.Compare(b.Created), strings.Compare(a.ID, b.ID))
	})
	var builder strings.Builder
	fmt.Fprintf(&builder, "%d memor(ies):\n", len(ordered))
	for _, memory := range ordered {
		fmt.Fprintf(&builder, "- %s [%s] %s\n", memory.ID, displayScope(memory.Scope), oneLine(memory.Text))
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

// displayScope 把空 scope 渲染为默认作用域。
func displayScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return DefaultScope
	}
	return scope
}

// oneLine 折叠换行，确保一条记忆在输出中只占一行。
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
