package agent_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
)

// fakeCheckpointStore 是同时实现 rig.CheckpointStore 与 agent.CheckpointLoader
// 的内存检查点仓库，用于恢复逻辑的单元测试。它记录写入次数，以便断言恢复过程
// 是只读的（不会自己持久化修复结果）。
type fakeCheckpointStore struct {
	mu       sync.Mutex
	data     map[string]*rig.Checkpoint
	loadErr  error
	saveErr  error
	clearErr error
	saves    int
	clears   int
}

var _ rig.CheckpointStore = (*fakeCheckpointStore)(nil)
var _ agent.CheckpointLoader = (*fakeCheckpointStore)(nil)

func newFakeCheckpointStore() *fakeCheckpointStore {
	return &fakeCheckpointStore{data: make(map[string]*rig.Checkpoint)}
}

func (s *fakeCheckpointStore) Load(ctx context.Context, sessionID string) (*rig.Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return s.data[sessionID].Clone(), nil
}

func (s *fakeCheckpointStore) Save(ctx context.Context, sessionID string, checkpoint *rig.Checkpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.saves++
	if checkpoint == nil {
		delete(s.data, sessionID)
		return nil
	}
	stored := checkpoint.Clone()
	if strings.TrimSpace(stored.SessionID) == "" {
		stored.SessionID = sessionID
	}
	s.data[sessionID] = stored
	return nil
}

func (s *fakeCheckpointStore) Clear(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clearErr != nil {
		return s.clearErr
	}
	s.clears++
	delete(s.data, sessionID)
	return nil
}

func (s *fakeCheckpointStore) stored(sessionID string) *rig.Checkpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[sessionID].Clone()
}

func (s *fakeCheckpointStore) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

// --- message helpers -------------------------------------------------------

func checkpointUserMessage(text string) rig.Message {
	return rig.NewMessage(rig.RoleUser, text)
}

func checkpointAssistantToolCalls(calls ...rig.ToolCall) rig.Message {
	return rig.Message{Role: rig.RoleAssistant, ToolCalls: calls}
}

func checkpointToolResult(callID, name, text string) rig.Message {
	return rig.Message{
		Role:       rig.RoleTool,
		Content:    []rig.ContentPart{rig.TextPart(text)},
		ToolCallID: callID,
		Name:       name,
	}
}

// TestCheckpointRecovery 覆盖中断回合恢复的完整契约。子测试名以 Recover 开头，
// 因此 `go test ./agent -run Checkpoint/Recover` 与
// `go test ./agent -run 'Checkpoint|Recover'` 都会执行全部用例。
func TestCheckpointRecovery(t *testing.T) {
	t.Run("RecoverNoOpStates", func(t *testing.T) {
		tests := []struct {
			name      string
			phase     rig.CheckpointPhase
			hasStore  bool
			hasCP     bool
			messages  []rig.Message
			sessionID string
		}{
			{name: "completed", phase: rig.CheckpointPhaseCompleted, hasStore: true, hasCP: true, sessionID: "s-noop", messages: []rig.Message{checkpointUserMessage("done")}},
			{name: "empty phase", phase: "", hasStore: true, hasCP: true, sessionID: "s-noop", messages: []rig.Message{checkpointUserMessage("x")}},
			{name: "unknown phase", phase: "weird", hasStore: true, hasCP: true, sessionID: "s-noop", messages: []rig.Message{checkpointUserMessage("x")}},
			{name: "missing checkpoint", phase: rig.CheckpointPhaseRequested, hasStore: true, hasCP: false, sessionID: "s-noop"},
			{name: "empty messages", phase: rig.CheckpointPhaseRequested, hasStore: true, hasCP: true, sessionID: "s-noop"},
			{name: "nil store", hasStore: false, sessionID: "s-noop"},
			{name: "empty session id", phase: rig.CheckpointPhaseRequested, hasStore: true, hasCP: true, messages: []rig.Message{checkpointUserMessage("x")}},
			{name: "blank session id", phase: rig.CheckpointPhaseRequested, hasStore: true, hasCP: true, messages: []rig.Message{checkpointUserMessage("x")}},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				var loader agent.CheckpointLoader
				if test.hasStore {
					store := newFakeCheckpointStore()
					if test.hasCP {
						cp := &rig.Checkpoint{
							SessionID: "s-noop",
							TurnID:    "turn-noop",
							Phase:     test.phase,
							Messages:  test.messages,
						}
						if err := store.Save(context.Background(), "s-noop", cp); err != nil {
							t.Fatalf("Save: %v", err)
						}
					}
					loader = store
				}
				sessionID := test.sessionID
				if test.name == "blank session id" {
					sessionID = "   "
				}
				repair, err := agent.RecoverInterruptedTurn(context.Background(), loader, sessionID)
				if err != nil || repair != nil {
					t.Fatalf("RecoverInterruptedTurn = (%+v, %v), want (nil, nil)", repair, err)
				}
			})
		}
	})

	t.Run("RecoverRequestedPhaseClosesWithAssistantMarker", func(t *testing.T) {
		store := newFakeCheckpointStore()
		original := &rig.Checkpoint{
			SessionID:     "s-requested",
			TurnID:        "turn-requested",
			Phase:         rig.CheckpointPhaseRequested,
			Messages:      []rig.Message{checkpointUserMessage("fix the failing test")},
			PriorMessages: 0,
		}
		if err := store.Save(context.Background(), "s-requested", original); err != nil {
			t.Fatalf("Save: %v", err)
		}
		before := original.Clone()

		repair, err := agent.RecoverInterruptedTurn(context.Background(), store, "s-requested")
		if err != nil {
			t.Fatalf("RecoverInterruptedTurn: %v", err)
		}
		if repair == nil {
			t.Fatal("requested checkpoint was not repaired")
		}
		if repair.SessionID != "s-requested" || repair.TurnID != "turn-requested" || repair.Phase != rig.CheckpointPhaseRequested {
			t.Fatalf("repair identity = %+v", repair)
		}
		// 恰好追加一条 assistant 标记；恢复绝不重复用户提示。
		if len(repair.Appended) != 1 {
			t.Fatalf("Appended = %+v, want exactly the assistant marker", repair.Appended)
		}
		if len(repair.InterruptedToolResults) != 0 {
			t.Fatalf("InterruptedToolResults = %+v, want none", repair.InterruptedToolResults)
		}
		marker := repair.Appended[0]
		if marker.Role != rig.RoleAssistant || marker.Text() != agent.InterruptedTurnMarker {
			t.Fatalf("marker = %+v, want the InterruptedTurnMarker assistant message", marker)
		}
		if !reflect.DeepEqual(repair.Marker, marker) {
			t.Fatalf("Marker = %+v, want %+v", repair.Marker, marker)
		}
		wantRoles := []rig.Role{rig.RoleUser, rig.RoleAssistant}
		if got := checkpointRoles(repair.Messages); !reflect.DeepEqual(got, wantRoles) {
			t.Fatalf("repaired roles = %v, want %v", got, wantRoles)
		}
		userMessages := 0
		for _, message := range repair.Messages {
			if message.Role == rig.RoleUser {
				userMessages++
			}
		}
		if userMessages != 1 {
			t.Fatalf("repaired history has %d user messages, want exactly 1", userMessages)
		}
		// RepairedCheckpoint 可以直接持久化，且字段与修复结果一致。
		if repair.RepairedCheckpoint == nil {
			t.Fatal("RepairedCheckpoint is nil")
		}
		if err := repair.RepairedCheckpoint.Validate(); err != nil {
			t.Fatalf("RepairedCheckpoint.Validate: %v", err)
		}
		if repair.RepairedCheckpoint.Phase != rig.CheckpointPhaseCompleted ||
			repair.RepairedCheckpoint.SessionID != "s-requested" ||
			repair.RepairedCheckpoint.TurnID != "turn-requested" {
			t.Fatalf("RepairedCheckpoint = %+v", repair.RepairedCheckpoint)
		}
		if !reflect.DeepEqual(repair.RepairedCheckpoint.Messages, repair.Messages) {
			t.Fatalf("RepairedCheckpoint.Messages does not match repair.Messages")
		}
		if repair.RepairedCheckpoint.PriorMessages != len(repair.Messages) {
			t.Fatalf("PriorMessages = %d, want %d", repair.RepairedCheckpoint.PriorMessages, len(repair.Messages))
		}
		// 恢复是只读的：不写 store，也不修改输入检查点。
		if store.saveCount() != 1 {
			t.Fatalf("Save calls = %d, want only the test's initial Save (recovery must not write)", store.saveCount())
		}
		if !reflect.DeepEqual(original, before) {
			t.Fatalf("recovery mutated the input checkpoint")
		}
		// 同一检查点的两次恢复结果完全一致。
		again, err := agent.RecoverInterruptedTurn(context.Background(), store, "s-requested")
		if err != nil {
			t.Fatalf("second RecoverInterruptedTurn: %v", err)
		}
		if !reflect.DeepEqual(repair, again) {
			t.Fatalf("recovery is not deterministic:\nfirst = %+v\nsecond = %+v", repair, again)
		}
	})

	t.Run("RecoverKeepsAnsweredToolResults", func(t *testing.T) {
		store := newFakeCheckpointStore()
		cp := &rig.Checkpoint{
			SessionID: "s-answered",
			TurnID:    "turn-answered",
			Phase:     rig.CheckpointPhaseRequested,
			Messages: []rig.Message{
				checkpointUserMessage("read the file"),
				checkpointAssistantToolCalls(rig.ToolCall{ID: "call-1", Name: "read_file", Arguments: `{"path":"a.txt"}`}),
				checkpointToolResult("call-1", "read_file", "file body"),
			},
		}
		if err := store.Save(context.Background(), "s-answered", cp); err != nil {
			t.Fatalf("Save: %v", err)
		}
		repair, err := agent.RecoverInterruptedTurn(context.Background(), store, "s-answered")
		if err != nil {
			t.Fatalf("RecoverInterruptedTurn: %v", err)
		}
		if repair == nil {
			t.Fatal("requested checkpoint was not repaired")
		}
		if len(repair.Appended) != 1 || repair.Appended[0].Role != rig.RoleAssistant {
			t.Fatalf("Appended = %+v, want only the assistant marker", repair.Appended)
		}
		if len(repair.InterruptedToolResults) != 0 {
			t.Fatalf("InterruptedToolResults = %+v, want none for an answered call", repair.InterruptedToolResults)
		}
		wantRoles := []rig.Role{rig.RoleUser, rig.RoleAssistant, rig.RoleTool, rig.RoleAssistant}
		if got := checkpointRoles(repair.Messages); !reflect.DeepEqual(got, wantRoles) {
			t.Fatalf("repaired roles = %v, want %v", got, wantRoles)
		}
		if repair.Messages[2].ToolCallID != "call-1" || repair.Messages[2].Text() != "file body" {
			t.Fatalf("answered tool result was not preserved: %+v", repair.Messages[2])
		}
	})

	t.Run("RecoverToolPendingFillsMissingToolResults", func(t *testing.T) {
		store := newFakeCheckpointStore()
		cp := &rig.Checkpoint{
			SessionID: "s-tool",
			TurnID:    "turn-tool",
			Phase:     rig.CheckpointPhaseToolPending,
			Messages: []rig.Message{
				checkpointUserMessage("delete the temp file and write a note"),
				checkpointAssistantToolCalls(
					rig.ToolCall{ID: "call-1", Name: "bash", Arguments: `{"command":"rm -rf /tmp/zeokeel-test"}`},
					rig.ToolCall{ID: "call-2", Name: "write_file", Arguments: `{"path":"note.txt"}`},
					rig.ToolCall{ID: "call-3", Name: "web_fetch", Arguments: `{"url":"https://example.com"}`},
				),
				checkpointToolResult("call-1", "bash", "removed"),
			},
		}
		if err := store.Save(context.Background(), "s-tool", cp); err != nil {
			t.Fatalf("Save: %v", err)
		}
		repair, err := agent.RecoverInterruptedTurn(context.Background(), store, "s-tool")
		if err != nil {
			t.Fatalf("RecoverInterruptedTurn: %v", err)
		}
		if repair == nil {
			t.Fatal("tool_pending checkpoint was not repaired")
		}
		// 已记录的 call-1 结果保留；call-2、call-3 各补一条“结果未知”工具结果，
		// 最后一条是 assistant 标记。
		wantRoles := []rig.Role{rig.RoleUser, rig.RoleAssistant, rig.RoleTool, rig.RoleTool, rig.RoleTool, rig.RoleAssistant}
		if got := checkpointRoles(repair.Messages); !reflect.DeepEqual(got, wantRoles) {
			t.Fatalf("repaired roles = %v, want %v", got, wantRoles)
		}
		if len(repair.InterruptedToolResults) != 2 {
			t.Fatalf("InterruptedToolResults = %+v, want 2 synthetic results", repair.InterruptedToolResults)
		}
		var ids []string
		for _, result := range repair.InterruptedToolResults {
			ids = append(ids, result.ToolCallID)
			if result.Role != rig.RoleTool || result.Text() != agent.InterruptedToolResult {
				t.Fatalf("synthetic tool result = %+v, want role=tool with InterruptedToolResult", result)
			}
			if strings.TrimSpace(result.Text()) == "" {
				t.Fatal("synthetic tool result is empty; the next provider request would reject a dangling call")
			}
		}
		if !reflect.DeepEqual(ids, []string{"call-2", "call-3"}) {
			t.Fatalf("synthetic tool result ids = %v, want [call-2 call-3] in call order", ids)
		}
		if repair.InterruptedToolResults[0].Name != "write_file" || repair.InterruptedToolResults[1].Name != "web_fetch" {
			t.Fatalf("synthetic tool result names = %+v", repair.InterruptedToolResults)
		}
		// Appended 顺序：[合成结果..., assistant 标记]
		if len(repair.Appended) != 3 {
			t.Fatalf("Appended = %+v, want [tool tool marker]", repair.Appended)
		}
		if repair.Appended[2].Role != rig.RoleAssistant || !reflect.DeepEqual(repair.Appended[2], repair.Marker) {
			t.Fatalf("last appended message is not the marker: %+v", repair.Appended[2])
		}
		for _, message := range repair.Appended {
			if message.Role == rig.RoleUser {
				t.Fatalf("recovery appended a user message: %+v", message)
			}
		}
		if repair.TurnID != "turn-tool" || repair.Phase != rig.CheckpointPhaseToolPending {
			t.Fatalf("repair identity = %+v", repair)
		}
	})

	t.Run("RecoverParallelToolCallsInOrder", func(t *testing.T) {
		store := newFakeCheckpointStore()
		cp := &rig.Checkpoint{
			SessionID: "s-parallel",
			TurnID:    "turn-parallel",
			Phase:     rig.CheckpointPhaseToolPending,
			Messages: []rig.Message{
				checkpointUserMessage("run both"),
				checkpointAssistantToolCalls(
					rig.ToolCall{ID: "call-a", Name: "bash", Arguments: `{}`},
					rig.ToolCall{ID: "call-b", Name: "write_file", Arguments: `{}`},
				),
			},
		}
		if err := store.Save(context.Background(), "s-parallel", cp); err != nil {
			t.Fatalf("Save: %v", err)
		}
		repair, err := agent.RecoverInterruptedTurn(context.Background(), store, "s-parallel")
		if err != nil || repair == nil {
			t.Fatalf("RecoverInterruptedTurn = (%+v, %v)", repair, err)
		}
		var ids []string
		for _, message := range repair.InterruptedToolResults {
			ids = append(ids, message.ToolCallID)
		}
		if !reflect.DeepEqual(ids, []string{"call-a", "call-b"}) {
			t.Fatalf("synthetic tool results = %v, want [call-a call-b] in call order", ids)
		}
	})

	t.Run("RecoverSkipsEmptyToolCallID", func(t *testing.T) {
		store := newFakeCheckpointStore()
		cp := &rig.Checkpoint{
			SessionID: "s-empty-id",
			TurnID:    "turn-empty-id",
			Phase:     rig.CheckpointPhaseToolPending,
			Messages: []rig.Message{
				checkpointUserMessage("go"),
				checkpointAssistantToolCalls(
					rig.ToolCall{ID: "", Name: "bash"},
					rig.ToolCall{ID: "call-ok", Name: "write_file"},
				),
			},
		}
		if err := store.Save(context.Background(), "s-empty-id", cp); err != nil {
			t.Fatalf("Save: %v", err)
		}
		repair, err := agent.RecoverInterruptedTurn(context.Background(), store, "s-empty-id")
		if err != nil || repair == nil {
			t.Fatalf("RecoverInterruptedTurn = (%+v, %v)", repair, err)
		}
		// 没有 id 的调用无法被协议匹配，不能补结果；只补 call-ok。
		if len(repair.InterruptedToolResults) != 1 || repair.InterruptedToolResults[0].ToolCallID != "call-ok" {
			t.Fatalf("InterruptedToolResults = %+v, want only call-ok", repair.InterruptedToolResults)
		}
	})

	t.Run("RecoverToleratesStoreErrors", func(t *testing.T) {
		store := newFakeCheckpointStore()
		store.loadErr = errors.New("corrupted checkpoint payload")
		repair, err := agent.RecoverInterruptedTurn(context.Background(), store, "s-corrupt")
		if err != nil || repair != nil {
			t.Fatalf("unreadable checkpoint = (%+v, %v), want (nil, nil): a recovery hint must not block the session", repair, err)
		}
	})

	t.Run("RecoverReportsCancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := agent.RecoverInterruptedTurn(ctx, newFakeCheckpointStore(), "s-canceled")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled recovery error = %v, want context.Canceled", err)
		}
		// 即使 store 同时报错，取消原因优先上报。
		store := newFakeCheckpointStore()
		store.loadErr = errors.New("also broken")
		if _, err := agent.RecoverInterruptedTurn(ctx, store, "s-canceled"); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled recovery with broken store error = %v, want context.Canceled", err)
		}
	})

	t.Run("RecoverPersistedRepairIsIdempotent", func(t *testing.T) {
		store := newFakeCheckpointStore()
		cp := &rig.Checkpoint{
			SessionID: "s-idem",
			TurnID:    "turn-idem",
			Phase:     rig.CheckpointPhaseToolPending,
			Messages: []rig.Message{
				checkpointUserMessage("do it"),
				checkpointAssistantToolCalls(rig.ToolCall{ID: "call-1", Name: "bash", Arguments: `{}`}),
			},
		}
		if err := store.Save(context.Background(), "s-idem", cp); err != nil {
			t.Fatalf("Save: %v", err)
		}
		repair, err := agent.RecoverInterruptedTurn(context.Background(), store, "s-idem")
		if err != nil || repair == nil {
			t.Fatalf("first recovery = (%+v, %v)", repair, err)
		}
		if err := store.Save(context.Background(), "s-idem", repair.RepairedCheckpoint); err != nil {
			t.Fatalf("save repaired checkpoint: %v", err)
		}
		saved := store.stored("s-idem")
		if saved == nil || saved.Phase != rig.CheckpointPhaseCompleted {
			t.Fatalf("stored checkpoint = %+v, want completed", saved)
		}
		if !reflect.DeepEqual(saved.Messages, repair.Messages) {
			t.Fatalf("stored repaired messages differ from repair.Messages")
		}
		second, err := agent.RecoverInterruptedTurn(context.Background(), store, "s-idem")
		if err != nil {
			t.Fatalf("recovery after repair: %v", err)
		}
		if second != nil {
			t.Fatalf("second recovery = %+v, want no-op after a completed checkpoint was persisted", second)
		}
	})

	t.Run("RecoverCloneIsolation", func(t *testing.T) {
		store := newFakeCheckpointStore()
		cp := &rig.Checkpoint{
			SessionID: "s-clone",
			TurnID:    "turn-clone",
			Phase:     rig.CheckpointPhaseToolPending,
			Messages: []rig.Message{
				checkpointUserMessage("go"),
				checkpointAssistantToolCalls(rig.ToolCall{ID: "call-1", Name: "bash", Arguments: `{}`}),
			},
		}
		if err := store.Save(context.Background(), "s-clone", cp); err != nil {
			t.Fatalf("Save: %v", err)
		}
		repair, err := agent.RecoverInterruptedTurn(context.Background(), store, "s-clone")
		if err != nil || repair == nil {
			t.Fatalf("RecoverInterruptedTurn = (%+v, %v)", repair, err)
		}
		// 修改修复结果不能影响 store 里的检查点。
		repair.Messages[0].Content[0].Text = "mutated"
		repair.RepairedCheckpoint.Messages[0].Content[0].Text = "mutated too"
		stored := store.stored("s-clone")
		if stored.Messages[0].Text() != "go" {
			t.Fatalf("store checkpoint was mutated through the repair result: %q", stored.Messages[0].Text())
		}
	})
}

// checkpointRoles 返回消息角色序列，便于断言对话结构。
func checkpointRoles(messages []rig.Message) []rig.Role {
	roles := make([]rig.Role, len(messages))
	for index, message := range messages {
		roles[index] = message.Role
	}
	return roles
}
