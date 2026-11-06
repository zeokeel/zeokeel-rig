package memory_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/tools/memory"
)

// invoke runs one memory tool call and fails the test on a Go error.
func invoke(t *testing.T, tool v1.Tool, arguments string) v1.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), v1.ToolCall{Name: tool.Name(), Arguments: arguments})
	if err != nil {
		t.Fatalf("Invoke(%s): %v", arguments, err)
	}
	return result
}

func requireContains(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("text %q does not contain %q", text, want)
		}
	}
}

func requireError(t *testing.T, result v1.ToolResult, wants ...string) {
	t.Helper()
	if !result.IsError {
		t.Fatalf("expected a tool error, got %q", result.Content)
	}
	requireContains(t, result.Content, wants...)
}

// TestMemoryToolCRUD covers add/list/search/remove/clear and scope filtering.
func TestMemoryToolCRUD(t *testing.T) {
	store := memory.NewMemoryStore()
	tools := memory.Tools(store)
	if len(tools) != 1 {
		t.Fatalf("Tools returned %d tools, want 1", len(tools))
	}
	tool := tools[0]
	if tool.Name() != memory.ToolName {
		t.Fatalf("tool name = %q", tool.Name())
	}

	added := invoke(t, tool, `{"action":"add","text":"release branch is release/2026","scope":"workspace"}`)
	requireContains(t, added.Content, "added memory mem-1", "scope=workspace")
	invoke(t, tool, `{"action":"add","text":"prefer concise answers","scope":"global"}`)

	list := invoke(t, tool, `{"action":"list"}`)
	requireContains(t, list.Content, "2 memor(ies)", "mem-1", "release/2026", "mem-2", "prefer concise answers")

	scoped := invoke(t, tool, `{"action":"list","scope":"global"}`)
	requireContains(t, scoped.Content, "1 memor(ies)", "prefer concise answers")
	if strings.Contains(scoped.Content, "release/2026") {
		t.Fatalf("global scope leaked a workspace memory: %q", scoped.Content)
	}

	search := invoke(t, tool, `{"action":"search","query":"RELEASE"}`)
	requireContains(t, search.Content, "mem-1", "release/2026")
	if strings.Contains(search.Content, "prefer concise answers") {
		t.Fatalf("case-insensitive search matched too much: %q", search.Content)
	}
	scopedSearch := invoke(t, tool, `{"action":"search","query":"release","scope":"global"}`)
	requireContains(t, scopedSearch.Content, "no memories")

	removed := invoke(t, tool, `{"action":"remove","id":"mem-1"}`)
	requireContains(t, removed.Content, "removed memory mem-1")
	requireError(t, invoke(t, tool, `{"action":"remove","id":"mem-1"}`), "no memory with id")

	invoke(t, tool, `{"action":"add","text":"second workspace note"}`)
	cleared := invoke(t, tool, `{"action":"clear","scope":"workspace"}`)
	requireContains(t, cleared.Content, "cleared 1 memor(ies) in scope workspace")
	requireContains(t, invoke(t, tool, `{"action":"clear","scope":"workspace"}`).Content, "cleared 0 memor(ies)")
	all := invoke(t, tool, `{"action":"clear"}`)
	requireContains(t, all.Content, "cleared 1 memor(ies)")
	list = invoke(t, tool, `{"action":"list"}`)
	requireContains(t, list.Content, "no memories")
}

// TestMemoryToolDefaultScopeAndErrors covers defaults and validation.
func TestMemoryToolDefaultScopeAndErrors(t *testing.T) {
	tool := memory.Tools(nil)[0] // nil store uses the in-memory default
	added := invoke(t, tool, `{"action":"add","text":"default scope note"}`)
	requireContains(t, added.Content, "scope=workspace")
	list := invoke(t, tool, `{"action":"list"}`)
	requireContains(t, list.Content, "[workspace]", "default scope note")

	requireError(t, invoke(t, tool, `{"action":"nope"}`), "unknown action")
	requireError(t, invoke(t, tool, `{}`), "action is required")
	requireError(t, invoke(t, tool, `{"action":"add"}`), "text is required")
	requireError(t, invoke(t, tool, `{"action":"search"}`), "query is required")
	requireError(t, invoke(t, tool, `{"action":"remove"}`), "id is required")

	if result, err := tool.Invoke(context.Background(), v1.ToolCall{Arguments: `{invalid`}); err == nil {
		t.Fatalf("invalid JSON returned no Go error: %+v", result)
	}
}

// TestMemoryStoreDirect covers the default store implementation.
func TestMemoryStoreDirect(t *testing.T) {
	store := memory.NewMemoryStore()
	ctx := context.Background()

	added, err := store.Add(ctx, v1.Memory{Text: "first", Scope: "workspace"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if added.ID != "mem-1" || added.Created.IsZero() {
		t.Fatalf("Add = %+v, want generated id and created time", added)
	}
	if _, err := store.Add(ctx, v1.Memory{Text: "second global", Scope: "global"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	list, err := store.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %v/%v", list, err)
	}
	// The returned slice is a copy: mutating it must not change the store.
	list[0].Text = "mutated"
	list, _ = store.List(ctx)
	if list[0].Text != "first" {
		t.Fatalf("List returned aliased data: %+v", list[0])
	}

	matches, err := store.Search(ctx, "GLOBAL")
	if err != nil || len(matches) != 1 || matches[0].Text != "second global" {
		t.Fatalf("Search = %+v/%v", matches, err)
	}
	all, err := store.Search(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("empty Search = %+v/%v", all, err)
	}
	if !store.Remove(ctx, "mem-1") {
		t.Fatal("Remove(mem-1) = false")
	}
	if store.Remove(ctx, "mem-1") {
		t.Fatal("second Remove(mem-1) = true")
	}
}

// TestMemoryToolSchema verifies the public metadata and schema.
func TestMemoryToolSchema(t *testing.T) {
	tool := memory.Tools(memory.NewMemoryStore())[0]
	if !strings.Contains(tool.Description(), "add") || !strings.Contains(tool.Description(), "search") {
		t.Fatalf("description looks incomplete: %q", tool.Description())
	}
	schema := tool.Schema()
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %v", schema["properties"])
	}
	action, ok := properties["action"].(map[string]any)
	if !ok {
		t.Fatalf("action schema = %v", properties["action"])
	}
	enum, ok := action["enum"].([]string)
	if !ok || len(enum) != 5 {
		t.Fatalf("action enum = %v, want five actions", action["enum"])
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "action" {
		t.Fatalf("schema required = %v", schema["required"])
	}
}

// TestMemoryToolConcurrentAdds verifies the default store is concurrency-safe.
func TestMemoryToolConcurrentAdds(t *testing.T) {
	store := memory.NewMemoryStore()
	tool := memory.Tools(store)[0]
	const workers = 32
	var waitGroup sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			result, err := tool.Invoke(context.Background(), v1.ToolCall{
				Arguments: fmt.Sprintf(`{"action":"add","text":"note-%d"}`, index),
			})
			if err != nil {
				errs <- err
				return
			}
			if result.IsError {
				errs <- fmt.Errorf("add %d: %s", index, result.Content)
			}
		}(i)
	}
	waitGroup.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	memories, err := store.List(context.Background())
	if err != nil || len(memories) != workers {
		t.Fatalf("List after concurrent adds = %d/%v, want %d", len(memories), err, workers)
	}
	seen := make(map[string]bool, workers)
	for _, memory := range memories {
		if seen[memory.ID] {
			t.Fatalf("duplicate generated id %q", memory.ID)
		}
		seen[memory.ID] = true
	}

	// Concurrent removals and searches must stay safe too.
	var second sync.WaitGroup
	for index := 0; index < workers; index += 2 {
		second.Add(1)
		go func(memoryID string) {
			defer second.Done()
			store.Remove(context.Background(), memoryID)
			_, _ = store.Search(context.Background(), "note")
		}(memories[index].ID)
	}
	second.Wait()
}

// TestMemoryToolArgumentDecoding verifies nested/unknown field tolerance (the
// tool decodes a flat object and ignores unknown keys).
func TestMemoryToolArgumentDecoding(t *testing.T) {
	tool := memory.Tools(nil)[0]
	result := invoke(t, tool, `{"action":"add","text":"with extras","unknown":true}`)
	if result.IsError {
		t.Fatalf("extra argument caused an error: %s", result.Content)
	}
	if _, err := json.Marshal(result); err != nil {
		t.Fatal(err)
	}
}
