package agent

// A host that keeps spilled results in its own storage: the model still sees the
// summary prefix and the preview, and the handle replaces the path.

import (
	"context"
	"strings"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
)

// fakeSpill records what the agent stored.
type fakeSpill struct {
	saved  []string
	loaded string
}

func (f *fakeSpill) Save(_ context.Context, call rig.ToolCall, content string) (rig.SpillRef, error) {
	f.saved = append(f.saved, call.Name+":"+content)
	return rig.SpillRef{ID: "ref-1", Bytes: len(content)}, nil
}

func (f *fakeSpill) Load(context.Context, string) (string, error) { return f.loaded, nil }
func (f *fakeSpill) GC(context.Context, int) (int, error)         { return 0, nil }

// TestSpillUsesTheHostStore covers the branch a database-backed host takes.
func TestSpillUsesTheHostStore(t *testing.T) {
	store := &fakeSpill{}
	policy := rig.SpillPolicy{ThresholdBytes: 8, Store: store}
	summary, err := spillToolResult(policy, rig.ToolCall{Name: "bash", ID: "c1"}, strings.Repeat("x", 32))
	if err != nil {
		t.Fatalf("spillToolResult: %v", err)
	}
	if len(store.saved) != 1 || !strings.HasPrefix(store.saved[0], "bash:") {
		t.Fatalf("store saw %+v", store.saved)
	}
	if !rig.IsSpilledToolResult(summary) {
		t.Fatalf("summary lost its prefix: %q", summary)
	}
	if !strings.Contains(summary, "ref: ref-1") {
		t.Fatalf("summary does not name the handle: %q", summary)
	}
	if strings.Contains(summary, "path:") {
		t.Fatalf("summary still speaks of a path: %q", summary)
	}
}

// TestSpillKeepsTheFileBehaviourWithoutAStore guards the default: no store means
// a path, as before.
func TestSpillKeepsTheFileBehaviourWithoutAStore(t *testing.T) {
	dir := t.TempDir()
	policy := rig.SpillPolicy{Dir: dir, ThresholdBytes: 8}
	summary, err := spillToolResult(policy, rig.ToolCall{Name: "bash", ID: "c2"}, strings.Repeat("y", 32))
	if err != nil {
		t.Fatalf("spillToolResult: %v", err)
	}
	if !strings.Contains(summary, "path: ") {
		t.Fatalf("file summary does not name a path: %q", summary)
	}
}
