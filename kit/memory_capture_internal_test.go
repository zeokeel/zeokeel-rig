package kit

import (
	"context"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/memory"
)

func TestCaptureSkipsToolResultsWhenDisabled(t *testing.T) {
	store := memory.NewMemoryStore()
	agent := &Agent{
		memoryStore:              store,
		memoryCapture:            true,
		memoryCaptureToolResults: false,
		memoryCommitTokens:       1 << 20,
	}
	result := rig.RunResult{Messages: []rig.Message{
		rig.NewMessage(rig.RoleUser, "hello"),
		rig.NewMessage(rig.RoleTool, "tool output"),
	}}
	agent.captureMemory(context.Background(), "s", result, false)
	state, err := store.Pending(context.Background(), "s")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if state.Pending != 1 {
		t.Fatalf("关闭 tool-result 捕获后 pending=%d, want 1", state.Pending)
	}
}
