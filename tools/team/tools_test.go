package team

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	rigtools "github.com/zeokeel/zeokeel-rig/tools"
)

// newToolRegistry returns a registry containing only the team tools.
func newToolRegistry(t *testing.T, manager *Manager) *rigtools.Registry {
	t.Helper()
	return rigtools.NewRegistry(manager.Tools()...)
}

func invokeTool(t *testing.T, registry *rigtools.Registry, name, arguments string) (v1.ToolResult, error) {
	t.Helper()
	return registry.Invoke(context.Background(), v1.ToolCall{ID: "call-1", Name: name, Arguments: arguments})
}

func invokeOK(t *testing.T, registry *rigtools.Registry, name, arguments string) string {
	t.Helper()
	result, err := invokeTool(t, registry, name, arguments)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if result.IsError {
		t.Fatalf("%s returned an error result: %s", name, result.Content)
	}
	return result.Content
}

func invokeError(t *testing.T, registry *rigtools.Registry, name, arguments string) string {
	t.Helper()
	result, err := invokeTool(t, registry, name, arguments)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if !result.IsError {
		t.Fatalf("%s did not return an error result: %q", name, result.Content)
	}
	return result.Content
}

func TestToolNamesMatchTools(t *testing.T) {
	names := ToolNames()
	tools := NewManager(nil).Tools()
	if len(names) != len(tools) {
		t.Fatalf("ToolNames() = %d names, Tools() = %d tools", len(names), len(tools))
	}
	for index, name := range names {
		if tools[index].Name() != name {
			t.Fatalf("name[%d] = %q, tool = %q", index, name, tools[index].Name())
		}
	}
}

func TestToolCatalog(t *testing.T) {
	manager := NewManager(newFakeRunner())
	tools := manager.Tools()
	wantNames := []string{
		ToolSpawn, ToolList, ToolSend, ToolWait, ToolLast, ToolClose, ToolInterrupt,
	}
	if len(tools) != len(wantNames) {
		t.Fatalf("Tools() = %d tools, want %d", len(tools), len(wantNames))
	}
	for index, tool := range tools {
		if tool.Name() != wantNames[index] {
			t.Fatalf("tool[%d] = %q, want %q", index, tool.Name(), wantNames[index])
		}
		if strings.TrimSpace(tool.Description()) == "" {
			t.Fatalf("%s has an empty description", tool.Name())
		}
		schema := tool.Schema()
		if schema["type"] != "object" {
			t.Fatalf("%s schema type = %v, want object", tool.Name(), schema["type"])
		}
		if _, ok := schema["properties"].(map[string]any); !ok {
			t.Fatalf("%s schema has no properties object", tool.Name())
		}
	}
	spawnSchema := toolByName(t, tools, ToolSpawn).Schema()
	required, _ := spawnSchema["required"].([]string)
	if !strings.Contains(strings.Join(required, ","), "name") || !strings.Contains(strings.Join(required, ","), "prompt") {
		t.Fatalf("team_spawn required = %v, want name and prompt", required)
	}
	sendSchema := toolByName(t, tools, ToolSend).Schema()
	required, _ = sendSchema["required"].([]string)
	if len(required) != 1 || required[0] != "message" {
		t.Fatalf("team_send required = %v, want message", required)
	}
	listSchema := toolByName(t, tools, ToolList).Schema()
	if required, _ := listSchema["required"].([]string); len(required) != 0 {
		t.Fatalf("team_list required = %v, want none", required)
	}
}

func toolByName(t *testing.T, tools []v1.Tool, name string) v1.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func TestToolSpawnAndList(t *testing.T) {
	runner := newFakeRunner()
	registry := newToolRegistry(t, NewManager(runner))

	content := invokeOK(t, registry, ToolSpawn,
		`{"name":"research","prompt":"investigate the repo","model":"small","system_prompt":"be terse"}`)
	if !strings.Contains(content, "member-1") || !strings.Contains(content, "research") {
		t.Fatalf("team_spawn content = %q", content)
	}
	started := runner.started()
	if len(started) != 1 || started[0].Model != "small" || started[0].SystemPrompt != "be terse" {
		t.Fatalf("runner specs = %+v", started)
	}
	content = invokeOK(t, registry, ToolList, `{}`)
	for _, want := range []string{"member-1", "research", StatusActive, "0s"} {
		if !strings.Contains(content, want) {
			t.Fatalf("team_list content = %q, want %q", content, want)
		}
	}
	if content := invokeError(t, registry, ToolSpawn, `{"name":"x"}`); !strings.Contains(content, "prompt is required") {
		t.Fatalf("team_spawn without prompt = %q", content)
	}
	if content := invokeOK(t, registry, ToolList, `{}`); strings.Contains(content, "x") {
		t.Fatalf("team_list shows a member whose spawn failed: %q", content)
	}
}

func TestToolSendWaitAndLast(t *testing.T) {
	runner := newFakeRunner()
	registry := newToolRegistry(t, NewManager(runner))
	invokeOK(t, registry, ToolSpawn, `{"name":"research","prompt":"go"}`)

	content := invokeOK(t, registry, ToolSend, `{"name":"research","message":"status?"}`)
	if !strings.Contains(content, "reply:status?") {
		t.Fatalf("team_send content = %q", content)
	}
	content = invokeOK(t, registry, ToolLast, `{"name":"research"}`)
	if !strings.Contains(content, "reply:status?") {
		t.Fatalf("team_last content = %q", content)
	}

	// Wait on the first run: the fake already resolved it.
	content = invokeOK(t, registry, ToolWait, `{"id":"member-1","timeout_ms":1000}`)
	if !strings.Contains(content, "first:go") {
		t.Fatalf("team_wait content = %q", content)
	}

	// A blocked wait times out with a readable result, and stays waitable.
	sub := runner.subagents()[0]
	sub.mu.Lock()
	sub.waitBlock = make(chan struct{})
	sub.mu.Unlock()
	content = invokeError(t, registry, ToolWait, `{"id":"member-1","timeout_ms":50}`)
	if !strings.Contains(content, "no result after") {
		t.Fatalf("team_wait timeout = %q", content)
	}
	sub.mu.Lock()
	close(sub.waitBlock)
	sub.waitBlock = nil
	sub.mu.Unlock()
	content = invokeOK(t, registry, ToolWait, `{"id":"member-1"}`)
	if !strings.Contains(content, "first:go") {
		t.Fatalf("team_wait after timeout = %q", content)
	}
}

func TestToolInterruptAndClose(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)
	registry := newToolRegistry(t, manager)
	invokeOK(t, registry, ToolSpawn, `{"name":"alpha","prompt":"p"}`)
	invokeOK(t, registry, ToolSpawn, `{"name":"beta","prompt":"p"}`)

	content := invokeOK(t, registry, ToolInterrupt, `{"name":"alpha"}`)
	if !strings.Contains(content, "interrupted") {
		t.Fatalf("team_interrupt content = %q", content)
	}
	content = invokeOK(t, registry, ToolList, `{}`)
	if !strings.Contains(content, StatusInterrupted) {
		t.Fatalf("team_list after interrupt = %q", content)
	}
	content = invokeError(t, registry, ToolSend, `{"name":"alpha","message":"still there?"}`)
	if !strings.Contains(content, "not active") {
		t.Fatalf("team_send to interrupted member = %q", content)
	}
	invokeOK(t, registry, ToolClose, `{"id":"beta"}`)
	content = invokeOK(t, registry, ToolList, `{}`)
	if strings.Contains(content, "beta") || !strings.Contains(content, "alpha") {
		t.Fatalf("team_list after close = %q", content)
	}
	invokeOK(t, registry, ToolClose, `{"name":"alpha"}`)
	if content := invokeOK(t, registry, ToolList, `{}`); content != "no team members" {
		t.Fatalf("team_list after closing everything = %q", content)
	}
}

func TestToolReferenceValidation(t *testing.T) {
	runner := newFakeRunner()
	registry := newToolRegistry(t, NewManager(runner))
	invokeOK(t, registry, ToolSpawn, `{"name":"alpha","prompt":"p"}`)

	if content := invokeError(t, registry, ToolSend, `{"id":"member-1","name":"alpha","message":"m"}`); !strings.Contains(content, "either id or name") {
		t.Fatalf("send with both references = %q", content)
	}
	if content := invokeError(t, registry, ToolSend, `{"message":"m"}`); !strings.Contains(content, "id or name is required") {
		t.Fatalf("send without reference = %q", content)
	}
	if content := invokeError(t, registry, ToolWait, `{"name":"ghost"}`); !strings.Contains(content, "unknown team member") {
		t.Fatalf("wait with unknown reference = %q", content)
	}

	// Two members with the same name: the name is ambiguous, ids are not.
	invokeOK(t, registry, ToolSpawn, `{"name":"worker","prompt":"p"}`)
	invokeOK(t, registry, ToolSpawn, `{"name":"worker","prompt":"p"}`)
	content := invokeError(t, registry, ToolSend, `{"name":"worker","message":"m"}`)
	if !strings.Contains(content, "ambiguous") {
		t.Fatalf("send with ambiguous name = %q", content)
	}
	for _, id := range []string{"member-2", "member-3"} {
		if !strings.Contains(content, id) {
			t.Fatalf("ambiguity error %q does not list %q", content, id)
		}
	}
	if content := invokeOK(t, registry, ToolSend, `{"id":"member-2","message":"hi"}`); !strings.Contains(content, "reply:hi") {
		t.Fatalf("send by id = %q", content)
	}
}

func TestToolErrors(t *testing.T) {
	// No runner configured.
	noRunner := newToolRegistry(t, NewManager(nil))
	if content := invokeError(t, noRunner, ToolSpawn, `{"name":"a","prompt":"p"}`); !strings.Contains(content, "no subagent runner") {
		t.Fatalf("spawn without runner = %q", content)
	}

	// Member limit.
	limited := newToolRegistry(t, NewManager(newFakeRunner(), WithMaxMembers(1)))
	invokeOK(t, limited, ToolSpawn, `{"name":"a","prompt":"p"}`)
	if content := invokeError(t, limited, ToolSpawn, `{"name":"b","prompt":"p"}`); !strings.Contains(content, "team is full") {
		t.Fatalf("spawn over limit = %q", content)
	}

	// Runner start failure.
	runner := newFakeRunner()
	runner.startErr = errors.New("provider exploded")
	broken := newToolRegistry(t, NewManager(runner))
	if content := invokeError(t, broken, ToolSpawn, `{"name":"a","prompt":"p"}`); !strings.Contains(content, "provider exploded") {
		t.Fatalf("spawn with failing runner = %q", content)
	}
}

func TestToolAfterCloseAll(t *testing.T) {
	manager := NewManager(newFakeRunner())
	registry := newToolRegistry(t, manager)
	invokeOK(t, registry, ToolSpawn, `{"name":"alpha","prompt":"p"}`)
	if err := manager.CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if content := invokeError(t, registry, ToolSpawn, `{"name":"late","prompt":"p"}`); !strings.Contains(content, "team manager is closed") {
		t.Fatalf("spawn after CloseAll = %q", content)
	}
	if content := invokeError(t, registry, ToolSend, `{"id":"member-1","message":"m"}`); !strings.Contains(content, "team manager is closed") {
		t.Fatalf("send after CloseAll = %q", content)
	}
	if content := invokeOK(t, registry, ToolList, `{}`); content != "no team members" {
		t.Fatalf("list after CloseAll = %q", content)
	}
}

func TestToolContextCancellation(t *testing.T) {
	registry := newToolRegistry(t, NewManager(newFakeRunner()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, name := range []string{ToolSpawn, ToolList, ToolSend, ToolWait, ToolLast, ToolClose, ToolInterrupt} {
		result, err := registry.Invoke(ctx, v1.ToolCall{Name: name, Arguments: `{"name":"a","prompt":"p","message":"m","id":"member-1"}`})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s(cancelled ctx) = %v (result %+v), want context.Canceled", name, err, result)
		}
	}
}

func TestToolMalformedArguments(t *testing.T) {
	registry := newToolRegistry(t, NewManager(newFakeRunner()))
	if _, err := invokeTool(t, registry, ToolSpawn, "not-json"); err == nil {
		t.Fatal("team_spawn accepted invalid arguments JSON")
	}
}
