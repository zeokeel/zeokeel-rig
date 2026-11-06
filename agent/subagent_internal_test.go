package agent

// Internal tests for the runner's depth accounting: a nested runner (depth > 0)
// cannot start further children when MaxDepth is reached. Hosts normally never
// create nested runners; child agents get no orchestration tools.

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

type depthStream struct{ sent bool }

func (s *depthStream) Recv() (rig.Chunk, error) {
	if s.sent {
		return rig.Chunk{}, io.EOF
	}
	s.sent = true
	return rig.Chunk{Delta: "ok", FinishReason: "stop"}, nil
}

type depthProvider struct{}

func (depthProvider) Name() string { return "depth" }
func (depthProvider) Stream(context.Context, rig.Request) (rig.Stream, error) {
	return &depthStream{}, nil
}

func TestRunnerDepthAccounting(t *testing.T) {
	parent := rig.AgentSpec{Model: "m", Provider: depthProvider{}}

	// Depth 0 with MaxDepth 2: one level of children is allowed.
	root, err := newRunnerAtDepth(parent, RunnerOptions{MaxDepth: 2, MaxConcurrent: 1}, 0)
	if err != nil {
		t.Fatalf("newRunnerAtDepth: %v", err)
	}
	sub, err := root.Start(context.Background(), rig.SubagentSpec{Prompt: "x"})
	if err != nil {
		t.Fatalf("depth 0 Start: %v", err)
	}
	_ = sub.Stop()

	// A runner already at MaxDepth refuses to start children.
	nested, err := newRunnerAtDepth(parent, RunnerOptions{MaxDepth: 1, MaxConcurrent: 1}, 1)
	if err != nil {
		t.Fatalf("newRunnerAtDepth: %v", err)
	}
	if _, err := nested.Start(context.Background(), rig.SubagentSpec{Prompt: "x"}); err == nil ||
		!strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("depth 1 Start error = %v, want depth limit", err)
	}

	// A MaxDepth below the runner's own depth also refuses.
	deep, err := newRunnerAtDepth(parent, RunnerOptions{MaxDepth: 2, MaxConcurrent: 1}, 3)
	if err != nil {
		t.Fatalf("newRunnerAtDepth: %v", err)
	}
	if _, err := deep.Start(context.Background(), rig.SubagentSpec{Prompt: "x"}); err == nil {
		t.Fatal("runner above MaxDepth accepted a child")
	}
}
