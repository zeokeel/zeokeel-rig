package team

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

func dshToolMap(t *testing.T, manager *Manager) map[string]rig.Tool {
	t.Helper()
	tools := DSHTools(manager)
	byName := make(map[string]rig.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	return byName
}

func invokeDSH(t *testing.T, tools map[string]rig.Tool, name, arguments string) rig.ToolResult {
	t.Helper()
	tool, ok := tools[name]
	if !ok {
		t.Fatalf("missing DSH tool %q", name)
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return result
}

func TestDSHToolSet(t *testing.T) {
	manager := NewManager(newFakeRunner())
	t.Cleanup(func() { _ = manager.CloseAll() })
	tools := dshToolMap(t, manager)
	for _, name := range []string{ToolSpawnTeammate, ToolListAgents, ToolWaitAgent, ToolSubagent, ToolSubagentFork} {
		tool, ok := tools[name]
		if !ok {
			t.Fatalf("missing %q", name)
		}
		if tool.Description() == "" {
			t.Fatalf("%q has no description", name)
		}
	}
}

func TestDSHSpawnTeammateAndListAgents(t *testing.T) {
	manager := NewManager(newFakeRunner())
	t.Cleanup(func() { _ = manager.CloseAll() })
	tools := dshToolMap(t, manager)
	result := invokeDSH(t, tools, ToolSpawnTeammate, `{"name":"helper","description":"researcher","prompt":"find things"}`)
	if result.IsError || !strings.Contains(result.Content, "spawned teammate helper") {
		t.Fatalf("spawn_teammate = %q (IsError=%v)", result.Content, result.IsError)
	}
	listed := invokeDSH(t, tools, ToolListAgents, `{}`)
	if listed.IsError || !strings.Contains(listed.Content, "target:") || !strings.Contains(listed.Content, "name: helper") {
		t.Fatalf("list_agents = %q", listed.Content)
	}
}

func TestDSHSubagentAsyncCompletionNotice(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)
	t.Cleanup(func() { _ = manager.CloseAll() })
	tools := dshToolMap(t, manager)
	result := invokeDSH(t, tools, ToolSubagent, `{"description":"research","prompt":"do work"}`)
	if result.IsError || !strings.Contains(result.Content, "started subagent") {
		t.Fatalf("subagent = %q (IsError=%v)", result.Content, result.IsError)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if notices := manager.PendingNotices(); len(notices) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("async subagent did not queue a completion notice")
}

func TestDSHSubagentFork(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)
	t.Cleanup(func() { _ = manager.CloseAll() })
	tools := dshToolMap(t, manager)
	result := invokeDSH(t, tools, ToolSubagentFork, `{"description":"forked","prompt":"continue"}`)
	if result.IsError {
		t.Fatalf("subagent_fork = %q", result.Content)
	}
	started := runner.started()
	if len(started) != 1 || !started[0].Fork {
		t.Fatalf("fork spec = %+v", started)
	}
}

func TestDSHWaitAgentNoProgress(t *testing.T) {
	manager := NewManager(newFakeRunner())
	t.Cleanup(func() { _ = manager.CloseAll() })
	tools := dshToolMap(t, manager)
	result := invokeDSH(t, tools, ToolWaitAgent, `{"timeout_ms":50}`)
	if result.IsError || result.Content != "noProgress" {
		t.Fatalf("wait_agent with no agents = %q", result.Content)
	}
}
