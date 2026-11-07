package agent_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
)

type recordingCompactor struct {
	calls int
	out   []rig.Message
	err   error
}

func (c *recordingCompactor) Compact(_ context.Context, _ string, _ []rig.Message, _ int) ([]rig.Message, error) {
	c.calls++
	return c.out, c.err
}

func TestAgentCompactsOverBudgetContext(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("ok")}}
	compactor := &recordingCompactor{out: []rig.Message{rig.NewMessage(rig.RoleUser, "compacted")}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:         "m",
		Provider:      provider,
		ContextBudget: 1,
		Compactor:     compactor,
	})
	result, err := a.Run(context.Background(), rig.RunRequest{
		Input: []rig.Message{
			rig.NewMessage(rig.RoleUser, strings.Repeat("long input ", 100)),
			rig.NewMessage(rig.RoleAssistant, strings.Repeat("long answer ", 100)),
		},
	})
	requireNoError(t, err)
	if compactor.calls != 1 {
		t.Fatalf("Compactor calls = %d, want 1", compactor.calls)
	}
	if len(provider.calls) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(provider.calls))
	}
	request := provider.calls[0]
	if len(request.Messages) != 1 || request.Messages[0].Text() != "compacted" {
		t.Fatalf("provider request was not compacted: %#v", request.Messages)
	}
	if result.Messages[len(result.Messages)-1].Text() != "ok" {
		t.Fatalf("result = %#v", result.Messages)
	}
}

func TestAgentSpillsLargeToolResult(t *testing.T) {
	dir := t.TempDir()
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", "big", `{}`),
		textScript("done"),
	}}
	big := &fakeTool{name: "big", fn: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
		return rig.ToolResult{Content: strings.Repeat("payload ", 200)}, nil
	}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:    "m",
		Provider: provider,
		Tools:    []rig.Tool{big},
		Spill:    &rig.SpillPolicy{Dir: dir, ThresholdBytes: 100},
	})
	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{rig.NewMessage(rig.RoleUser, "go")}})
	requireNoError(t, err)
	var toolText string
	for _, message := range result.Messages {
		if message.Role == rig.RoleTool {
			toolText = message.Text()
		}
	}
	if !strings.HasPrefix(toolText, rig.SpillSummaryPrefix) {
		t.Fatalf("tool result was not spilled: %q", toolText)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("spill dir entries = %v, err = %v", entries, err)
	}
}

func TestAgentDoesNotSpillSmallToolResult(t *testing.T) {
	dir := t.TempDir()
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", "small", `{}`),
		textScript("done"),
	}}
	small := &fakeTool{name: "small", fn: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
		return rig.ToolResult{Content: "small output"}, nil
	}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:    "m",
		Provider: provider,
		Tools:    []rig.Tool{small},
		Spill:    &rig.SpillPolicy{Dir: dir, ThresholdBytes: 100},
	})
	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{rig.NewMessage(rig.RoleUser, "go")}})
	requireNoError(t, err)
	for _, message := range result.Messages {
		if message.Role == rig.RoleTool && message.Text() != "small output" {
			t.Fatalf("small result changed: %q", message.Text())
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("spill dir = %v, want empty", entries)
	}
}

var _ = agent.DefaultMaxIterations
