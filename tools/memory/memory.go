// Package memory provides the `memory` tool and an in-memory rig.MemoryStore
// implementation.
//
// A host may supply its own durable rig.MemoryStore (database, files, remote
// service) through Tools; when the store is nil the tools fall back to
// NewMemoryStore, a concurrency-safe process-local implementation that is lost
// on restart.
//
// The single `memory` tool takes an action and operates on the store:
//
//	{"action":"add","text":"The release branch is release/2026.","scope":"workspace"}
//	{"action":"list","scope":"workspace"}
//	{"action":"search","query":"release"}
//	{"action":"remove","id":"<memory id>"}
//	{"action":"clear","scope":"workspace"}
//
// list and search return readable lines ("- <id> [<scope>] <text>"); add,
// remove and clear report what changed. clear is implemented on top of the
// MemoryStore interface (List then Remove per entry), so custom stores do not
// need an extra method.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
)

const (
	// ToolName is the model-facing tool name.
	ToolName = "memory"
	// DefaultScope is the scope stored when the call omits one.
	DefaultScope = "workspace"
	// DefaultMaxOutputBytes caps one tool response.
	DefaultMaxOutputBytes = 32 << 10
)

// MemoryStore is a concurrency-safe in-memory rig.MemoryStore. Entries keep their
// insertion order for List; IDs are generated as "mem-<n>" when Add receives an
// empty ID, and Created defaults to the current UTC time. Calls are safe for
// concurrent use.
type MemoryStore struct {
	mu    sync.Mutex
	seq   uint64
	items []rig.Memory
}

var _ rig.MemoryStore = (*MemoryStore)(nil)

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

// List implements rig.MemoryStore. It returns a copy in insertion order.
func (s *MemoryStore) List(context.Context) ([]rig.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rig.Memory(nil), s.items...), nil
}

// Add implements rig.MemoryStore. A missing ID is generated, a missing Created
// is set to now (UTC), and the text is stored verbatim.
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

// Remove implements rig.MemoryStore.
func (s *MemoryStore) Remove(_ context.Context, id string) bool {
	id = strings.TrimSpace(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, memory := range s.items {
		if memory.ID == id {
			s.items = append(s.items[:index], s.items[index+1:]...)
			return true
		}
	}
	return false
}

// Search implements rig.MemoryStore. Query matching is a case-insensitive
// substring match on the memory text; an empty query returns every entry.
func (s *MemoryStore) Search(_ context.Context, query string) ([]rig.Memory, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	s.mu.Lock()
	defer s.mu.Unlock()
	if query == "" {
		return append([]rig.Memory(nil), s.items...), nil
	}
	matches := make([]rig.Memory, 0, len(s.items))
	for _, memory := range s.items {
		if strings.Contains(strings.ToLower(memory.Text), query) {
			matches = append(matches, memory)
		}
	}
	return matches, nil
}

// Tools returns the memory tool bound to store. A nil store uses a fresh
// in-memory store.
func Tools(store rig.MemoryStore) []rig.Tool {
	if store == nil {
		store = NewMemoryStore()
	}
	return []rig.Tool{&tool{store: store}}
}

// tool is the `memory` tool implementation.
type tool struct {
	store rig.MemoryStore
}

var _ rig.Tool = (*tool)(nil)

// Name implements rig.Tool.
func (t *tool) Name() string { return ToolName }

// Description implements rig.Tool.
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

// Schema implements rig.Tool.
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

// args is the decoded memory tool argument object.
type args struct {
	Action string `json:"action"`
	Text   string `json:"text"`
	Scope  string `json:"scope"`
	ID     string `json:"id"`
	Query  string `json:"query"`
}

// Invoke implements rig.Tool.
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	var a args
	if err := decodeArgs(call, &a); err != nil {
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
		return errorResult("memory: action is required (add, list, search, remove, clear)"), nil
	default:
		return errorResult("memory: unknown action %q (add, list, search, remove, clear)", a.Action), nil
	}
}

// add implements the add action.
func (t *tool) add(ctx context.Context, a args) rig.ToolResult {
	text := strings.TrimSpace(a.Text)
	if text == "" {
		return errorResult("memory: text is required for action: add")
	}
	scope := strings.TrimSpace(a.Scope)
	if scope == "" {
		scope = DefaultScope
	}
	added, err := t.store.Add(ctx, rig.Memory{Text: text, Scope: scope})
	if err != nil {
		return errorResult("memory: add: %v", err)
	}
	return textResult(fmt.Sprintf("added memory %s (scope=%s)", added.ID, displayScope(added.Scope)))
}

// list implements the list action.
func (t *tool) list(ctx context.Context, a args) rig.ToolResult {
	memories, err := t.store.List(ctx)
	if err != nil {
		return errorResult("memory: list: %v", err)
	}
	return textResult(truncateOutput(formatMemories(filterScope(memories, a.Scope)), DefaultMaxOutputBytes))
}

// search implements the search action.
func (t *tool) search(ctx context.Context, a args) rig.ToolResult {
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return errorResult("memory: query is required for action: search")
	}
	memories, err := t.store.Search(ctx, query)
	if err != nil {
		return errorResult("memory: search: %v", err)
	}
	memories = filterScope(memories, a.Scope)
	return textResult(truncateOutput(formatMemories(memories), DefaultMaxOutputBytes))
}

// remove implements the remove action.
func (t *tool) remove(ctx context.Context, a args) rig.ToolResult {
	id := strings.TrimSpace(a.ID)
	if id == "" {
		return errorResult("memory: id is required for action: remove")
	}
	if !t.store.Remove(ctx, id) {
		return errorResult("memory: no memory with id %q", id)
	}
	return textResult(fmt.Sprintf("removed memory %s", id))
}

// clear implements the clear action using the MemoryStore interface (List then
// Remove per entry). It removes only the requested scope when one is given.
func (t *tool) clear(ctx context.Context, a args) rig.ToolResult {
	memories, err := t.store.List(ctx)
	if err != nil {
		return errorResult("memory: clear: %v", err)
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
		return textResult(fmt.Sprintf("cleared %d memor(ies)", removed))
	}
	return textResult(fmt.Sprintf("cleared %d memor(ies) in scope %s", removed, scope))
}

// filterScope keeps memories whose scope matches scope (case-insensitive exact
// match). An empty scope keeps everything.
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

// formatMemories renders memories as readable lines, deterministically ordered
// by creation time and then ID.
func formatMemories(memories []rig.Memory) string {
	if len(memories) == 0 {
		return "no memories"
	}
	ordered := append([]rig.Memory(nil), memories...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].Created.Equal(ordered[j].Created) {
			return ordered[i].Created.Before(ordered[j].Created)
		}
		return ordered[i].ID < ordered[j].ID
	})
	var builder strings.Builder
	fmt.Fprintf(&builder, "%d memor(ies):\n", len(ordered))
	for _, memory := range ordered {
		fmt.Fprintf(&builder, "- %s [%s] %s\n", memory.ID, displayScope(memory.Scope), oneLine(memory.Text))
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

// displayScope renders an empty scope as the default scope.
func displayScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return DefaultScope
	}
	return scope
}

// oneLine collapses newlines so one memory stays on one output line.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// truncateOutput clamps model-facing output to max bytes without splitting a
// UTF-8 rune, appending a truncation notice.
func truncateOutput(content string, max int) string {
	if max <= 0 || len(content) <= max {
		return content
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	return content[:cut] + "\n[truncated: output exceeded the memory tool limit]"
}

// decodeArgs decodes one call's argument JSON. Missing arguments decode as an
// empty object.
func decodeArgs(call rig.ToolCall, target any) error {
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
func textResult(content string) rig.ToolResult { return rig.ToolResult{Content: content} }

// errorResult builds a model-facing tool failure.
func errorResult(format string, args ...any) rig.ToolResult {
	return rig.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}
