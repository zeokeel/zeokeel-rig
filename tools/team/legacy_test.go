package team

// Tests for the legacy tool-name adapters (spawn_team, list_teams,
// team_status, close_team, wait_team, send_message, interrupt_agent,
// list_subagent_models). They reuse the manager fixtures.

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

func legacyToolMap(t *testing.T, manager *Manager, models []string) map[string]v1.Tool {
	t.Helper()
	tools := LegacyTools(manager, models)
	byName := make(map[string]v1.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	return byName
}

func invokeLegacy(t *testing.T, tools map[string]v1.Tool, name, arguments string) v1.ToolResult {
	t.Helper()
	tool, ok := tools[name]
	if !ok {
		t.Fatalf("legacy tool %q not found", name)
	}
	result, err := tool.Invoke(context.Background(), v1.ToolCall{ID: "call-1", Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", name, err)
	}
	return result
}

func invokeLegacyError(t *testing.T, tools map[string]v1.Tool, name, arguments string) string {
	t.Helper()
	result := invokeLegacy(t, tools, name, arguments)
	if !result.IsError {
		t.Fatalf("%s: result = %q, want IsError", name, result.Content)
	}
	return result.Content
}

func TestLegacyToolCatalog(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner, WithMaxMembers(4))
	t.Cleanup(func() { _ = manager.CloseAll() })
	tools := LegacyTools(manager, []string{"model-a"})
	names := LegacyToolNames()
	if len(tools) != len(names) {
		t.Fatalf("LegacyTools = %d tools, want %d", len(tools), len(names))
	}
	for index, tool := range tools {
		if tool.Name() != names[index] {
			t.Fatalf("tool[%d] = %q, want %q", index, tool.Name(), names[index])
		}
		if tool.Description() == "" {
			t.Fatalf("%s has an empty description", tool.Name())
		}
		if schema := tool.Schema(); schema["type"] != "object" {
			t.Fatalf("%s schema type = %v", tool.Name(), schema["type"])
		}
	}
	if LegacyTools(nil, nil) != nil {
		t.Fatal("LegacyTools(nil) should return nil")
	}
}

func TestLegacySpawnListStatusWait(t *testing.T) {
	manager := NewManager(newFakeRunner(), WithMaxMembers(4))
	t.Cleanup(func() { _ = manager.CloseAll() })
	tools := legacyToolMap(t, manager, nil)

	// spawn_team accepts the legacy task alias for prompt.
	spawned := invokeLegacy(t, tools, ToolSpawnTeam, `{"name":"helper","task":"do work"}`)
	if spawned.IsError {
		t.Fatalf("spawn_team: %q", spawned.Content)
	}
	if !strings.Contains(spawned.Content, "spawned team member helper (member-1)") {
		t.Fatalf("spawn_team = %q", spawned.Content)
	}

	listed := invokeLegacy(t, tools, ToolListTeams, `{}`)
	if listed.IsError {
		t.Fatalf("list_teams: %q", listed.Content)
	}
	for _, want := range []string{"member-1", "helper", "active"} {
		if !strings.Contains(listed.Content, want) {
			t.Fatalf("list_teams = %q, want %q", listed.Content, want)
		}
	}

	status := invokeLegacy(t, tools, ToolTeamStatus, `{}`)
	if status.IsError {
		t.Fatalf("team_status: %q", status.Content)
	}
	for _, want := range []string{"id: member-1", "name: helper", "status: active", "last: (no completed run yet)"} {
		if !strings.Contains(status.Content, want) {
			t.Fatalf("team_status = %q, want %q", status.Content, want)
		}
	}

	waited := invokeLegacy(t, tools, ToolWaitTeam, `{"target":"helper"}`)
	if waited.IsError || !strings.Contains(waited.Content, "first:do work") {
		t.Fatalf("wait_team = %q (IsError=%v)", waited.Content, waited.IsError)
	}

	// The status now includes the newest completed run output.
	status = invokeLegacy(t, tools, ToolTeamStatus, `{"target":"member-1"}`)
	if status.IsError || !strings.Contains(status.Content, "last: first:do work") {
		t.Fatalf("team_status after wait = %q (IsError=%v)", status.Content, status.IsError)
	}
	if !strings.Contains(status.Content, "usage: iterations=1") {
		t.Fatalf("team_status missing usage: %q", status.Content)
	}
}

func TestLegacySendMessage(t *testing.T) {
	manager := NewManager(newFakeRunner(), WithMaxMembers(4))
	t.Cleanup(func() { _ = manager.CloseAll() })
	tools := legacyToolMap(t, manager, nil)
	if got := invokeLegacy(t, tools, ToolSpawnTeam, `{"name":"helper","prompt":"start"}`); got.IsError {
		t.Fatalf("spawn_team: %q", got.Content)
	}
	// Wait for the first run so the member is quiescent, then send by name.
	if got := invokeLegacy(t, tools, ToolWaitTeam, `{"target":"helper"}`); got.IsError {
		t.Fatalf("wait_team: %q", got.Content)
	}
	replied := invokeLegacy(t, tools, ToolSendMessage, `{"target":"helper","message":"next step"}`)
	if replied.IsError || !strings.Contains(replied.Content, "reply:next step") {
		t.Fatalf("send_message = %q (IsError=%v)", replied.Content, replied.IsError)
	}
	text := invokeLegacyError(t, tools, ToolSendMessage, `{"target":"ghost","message":"x"}`)
	if !strings.Contains(text, "unknown team member") {
		t.Fatalf("unknown target = %q, want unknown-member error", text)
	}
}

func TestLegacyWaitTimeout(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner, WithMaxMembers(4))
	t.Cleanup(func() { _ = manager.CloseAll() })
	if _, err := manager.Spawn(context.Background(), v1.SubagentSpec{Name: "slow", Prompt: "work"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	runner.subagents()[0].waitBlock = make(chan struct{})
	tools := legacyToolMap(t, manager, nil)
	started := time.Now()
	text := invokeLegacyError(t, tools, ToolWaitTeam, `{"target":"slow","timeout_ms":50}`)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("wait_team timeout took %s", elapsed)
	}
	if !strings.Contains(text, "no result after") {
		t.Fatalf("wait_team timeout = %q", text)
	}
}

func TestLegacyInterruptAndClose(t *testing.T) {
	manager := NewManager(newFakeRunner(), WithMaxMembers(4))
	t.Cleanup(func() { _ = manager.CloseAll() })
	tools := legacyToolMap(t, manager, nil)
	if got := invokeLegacy(t, tools, ToolSpawnTeam, `{"name":"helper","prompt":"start"}`); got.IsError {
		t.Fatalf("spawn_team: %q", got.Content)
	}
	interrupted := invokeLegacy(t, tools, ToolInterruptAgent, `{"target":"helper"}`)
	if interrupted.IsError || !strings.Contains(interrupted.Content, "interrupted team member") {
		t.Fatalf("interrupt_agent = %q (IsError=%v)", interrupted.Content, interrupted.IsError)
	}
	listed := invokeLegacy(t, tools, ToolListTeams, `{}`)
	if !strings.Contains(listed.Content, "interrupted") {
		t.Fatalf("list_teams after interrupt = %q", listed.Content)
	}
	closed := invokeLegacy(t, tools, ToolCloseTeam, `{"target":"member-1"}`)
	if closed.IsError || !strings.Contains(closed.Content, "closed team member") {
		t.Fatalf("close_team = %q (IsError=%v)", closed.Content, closed.IsError)
	}
	listed = invokeLegacy(t, tools, ToolListTeams, `{}`)
	if listed.IsError || listed.Content != "no team members" {
		t.Fatalf("list_teams after close = %q (IsError=%v)", listed.Content, listed.IsError)
	}
}

func TestLegacyValidationErrors(t *testing.T) {
	manager := NewManager(newFakeRunner(), WithMaxMembers(4))
	t.Cleanup(func() { _ = manager.CloseAll() })
	tools := legacyToolMap(t, manager, nil)
	cases := []struct {
		name      string
		tool      string
		arguments string
		want      string
	}{
		{"spawn missing name", ToolSpawnTeam, `{"prompt":"x"}`, "name is required"},
		{"spawn missing prompt", ToolSpawnTeam, `{"name":"a"}`, "prompt (or task) is required"},
		{"send missing target", ToolSendMessage, `{"message":"x"}`, "id or name is required"},
		{"send missing message", ToolSendMessage, `{"target":"a"}`, "message is required"},
		{"wait unknown", ToolWaitTeam, `{"target":"ghost"}`, "unknown team member"},
		{"wait missing target", ToolWaitTeam, `{}`, "id or name is required"},
		{"interrupt missing target", ToolInterruptAgent, `{}`, "id or name is required"},
		{"close missing target", ToolCloseTeam, `{}`, "id or name is required"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			text := invokeLegacyError(t, tools, testCase.tool, testCase.arguments)
			if !strings.Contains(text, testCase.want) {
				t.Fatalf("%s = %q, want %q", testCase.tool, text, testCase.want)
			}
		})
	}
	// Malformed JSON is a Go error.
	if _, err := tools[ToolSpawnTeam].Invoke(context.Background(), v1.ToolCall{Arguments: "{"}); err == nil {
		t.Fatal("malformed arguments returned nil error")
	}
}

func TestLegacyListSubagentModels(t *testing.T) {
	manager := NewManager(newFakeRunner())
	t.Cleanup(func() { _ = manager.CloseAll() })
	withModels := legacyToolMap(t, manager, []string{"model-a", "model-b"})
	result := invokeLegacy(t, withModels, ToolListSubagentModels, `{}`)
	if result.IsError {
		t.Fatalf("list_subagent_models: %q", result.Content)
	}
	for _, want := range []string{"available subagent models:", "- model-a", "- model-b"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("list_subagent_models = %q, want %q", result.Content, want)
		}
	}
	withoutModels := legacyToolMap(t, manager, nil)
	result = invokeLegacy(t, withoutModels, ToolListSubagentModels, `{}`)
	if result.IsError || !strings.Contains(result.Content, "inherit the parent's model") {
		t.Fatalf("list_subagent_models(empty) = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestLegacyCancelledContext(t *testing.T) {
	manager := NewManager(newFakeRunner())
	t.Cleanup(func() { _ = manager.CloseAll() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := manager.LegacyTools(nil)[0].Invoke(ctx, v1.ToolCall{Arguments: `{"name":"a","prompt":"x"}`})
	if err == nil {
		t.Fatal("cancelled spawn_team returned nil error")
	}
}
