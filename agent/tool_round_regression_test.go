package agent_test

// Regression tests for tool-round robustness fixes in v0.9.1:
//
//   - cancel/gate-error abort paths must leave a replayable history (every
//     assistant tool_calls message answered by a role=tool message);
//   - the approval gate and allow-always cache must see normalized tool names;
//   - one model round must not spawn unbounded tool goroutines.

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// TestCancelMidToolKeepsHistoryReplayable reproduces the old bug where a run
// cancelled while a tool was executing persisted an assistant tool_calls
// message with no tool results (the next request would be rejected by an
// OpenAI-compatible endpoint with HTTP 400).
func TestCancelMidToolKeepsHistoryReplayable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tool := &fakeTool{name: "block", fn: func(ctx context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
		cancel()
		<-ctx.Done()
		return rig.ToolResult{}, ctx.Err()
	}}
	history := newCaptureHistory(nil)
	provider := &scriptProvider{scripts: [][]rig.Chunk{toolCallScript("call-1", "block")}}
	a := mustNewAgent(t, rig.AgentSpec{
		Provider: provider,
		Model:    "m",
		Tools:    []rig.Tool{tool},
		History:  history,
	})
	if _, err := a.Run(ctx, rig.RunRequest{
		SessionID: "cancel-mid-tool",
		Input:     []rig.Message{userMessage("run the tool")},
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	assertReplayableHistory(t, history.stored("cancel-mid-tool"), "call-1")
}

// TestGateErrorKeepsHistoryReplayable: an approval-gate error aborts the run,
// but the already-persisted assistant tool_calls must still be answered.
func TestGateErrorKeepsHistoryReplayable(t *testing.T) {
	tool := &fakeTool{name: "probe"}
	gate := &fakeGate{fn: func(rig.ToolCall) (rig.Decision, error) {
		return "", errors.New("approval service unavailable")
	}}
	history := newCaptureHistory(nil)
	a := mustNewAgent(t, rig.AgentSpec{
		Provider: &scriptProvider{scripts: [][]rig.Chunk{toolCallScript("call-2", "probe")}},
		Model:    "m",
		Tools:    []rig.Tool{tool},
		History:  history,
		Gate:     gate,
	})
	_, err := a.Run(context.Background(), rig.RunRequest{
		SessionID: "gate-error",
		Input:     []rig.Message{userMessage("run the tool")},
	})
	if err == nil || !contains(err.Error(), "approval gate") {
		t.Fatalf("Run error = %v, want an approval-gate error", err)
	}
	if tool.callCount() != 0 {
		t.Fatalf("gate error ran the tool %d time(s)", tool.callCount())
	}
	assertReplayableHistory(t, history.stored("gate-error"), "call-2")
}

// TestGateSeesNormalizedToolName: a leading space in the model-provided name
// must not slip past a gate that matches call.Name.
func TestGateSeesNormalizedToolName(t *testing.T) {
	tool := &fakeTool{name: "marker"}
	gate := &fakeGate{fn: func(call rig.ToolCall) (rig.Decision, error) {
		if call.Name == "marker" {
			return rig.DecisionDeny, nil
		}
		return rig.DecisionAllow, nil
	}}
	a := mustNewAgent(t, rig.AgentSpec{
		Provider: &scriptProvider{scripts: [][]rig.Chunk{
			toolCallScript("call-3", " marker "),
			textScript("done"),
		}},
		Model: "m",
		Tools: []rig.Tool{tool},
		Gate:  gate,
	})
	result, err := a.Run(context.Background(), rig.RunRequest{
		SessionID: "normalized",
		Input:     []rig.Message{userMessage("call the marker")},
	})
	requireNoError(t, err)
	if tool.callCount() != 0 {
		t.Fatalf("denied tool ran %d time(s)", tool.callCount())
	}
	if gate.callCount() != 1 || gate.calls[0].Name != "marker" {
		t.Fatalf("gate saw calls %+v, want one with the normalized name %q", gate.calls, "marker")
	}
	if result.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want stop", result.FinishReason)
	}
}

// TestToolRoundBoundsConcurrency: one model round with many tool calls must
// execute at most maxParallelToolCalls bodies at the same time.
func TestToolRoundBoundsConcurrency(t *testing.T) {
	const calls = 64
	var current, peak int64
	probe := &fakeTool{name: "probe", fn: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
		n := atomic.AddInt64(&current, 1)
		for {
			p := atomic.LoadInt64(&peak)
			if n <= p || atomic.CompareAndSwapInt64(&peak, p, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		atomic.AddInt64(&current, -1)
		return rig.ToolResult{Content: "ok"}, nil
	}}
	chunks := make([]rig.Chunk, 0, calls+1)
	for i := 0; i < calls; i++ {
		chunks = append(chunks, rig.Chunk{ToolCalls: []rig.ToolCallDelta{{
			Index: i, ID: "call-" + strconv.Itoa(i), Name: "probe", Arguments: "{}",
		}}})
	}
	chunks = append(chunks, finishChunk("tool_calls"))
	history := newCaptureHistory(nil)
	a := mustNewAgent(t, rig.AgentSpec{
		Provider: &scriptProvider{scripts: [][]rig.Chunk{chunks, textScript("done")}},
		Model:    "m",
		Tools:    []rig.Tool{probe},
		History:  history,
	})
	result, err := a.Run(context.Background(), rig.RunRequest{
		SessionID: "bounded",
		Input:     []rig.Message{userMessage("run many tools")},
	})
	requireNoError(t, err)
	if got := probe.callCount(); got != calls {
		t.Fatalf("tool ran %d time(s), want %d", got, calls)
	}
	if peak > 8 {
		t.Fatalf("peak concurrency = %d, want <= 8", peak)
	}
	if peak < 2 {
		t.Fatalf("peak concurrency = %d, want the worker pool to run calls concurrently", peak)
	}
	if len(result.Messages) < calls {
		t.Fatalf("result messages = %d, want at least one tool message per call", len(result.Messages))
	}
}

// assertReplayableHistory checks that messages ending in an assistant tool_calls
// message are each answered by a role=tool message for the same ToolCallID.
func assertReplayableHistory(t *testing.T, messages []rig.Message, wantToolCallID string) {
	t.Helper()
	for i, msg := range messages {
		if msg.Role != rig.RoleAssistant || len(msg.ToolCalls) == 0 {
			continue
		}
		for _, call := range msg.ToolCalls {
			answered := false
			for _, later := range messages[i+1:] {
				if later.Role == rig.RoleTool && later.ToolCallID == call.ID {
					answered = true
					break
				}
			}
			if !answered {
				t.Fatalf("assistant tool_calls %q is not answered by a role=tool message; history=%+v", call.ID, messages)
			}
		}
	}
	found := false
	for _, msg := range messages {
		if msg.Role == rig.RoleTool && msg.ToolCallID == wantToolCallID {
			found = true
		}
	}
	if !found {
		t.Fatalf("history has no tool message for %q: %+v", wantToolCallID, messages)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
