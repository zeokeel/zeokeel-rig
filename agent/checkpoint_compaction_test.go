package agent_test

// 回归测试：上下文压缩与检查点叠加时的 PriorMessages 分界（审计 P1）。
//
// 背景：prepareContext 会把 r.conversation 整体替换成压缩器的输出，压缩前算好的
// "本回合起点"在压缩后既不指向同一条消息、还可能超过对话长度，于是检查点写成
// PriorMessages > len(Messages)，任何按 rig.Checkpoint.Validate 契约校验的
// CheckpointStore 都会拒收 —— saveCheckpoint 失败，run 硬终止（kit 默认开压缩，
// 宿主一旦配置 CheckpointStore 就必现）。这里用真实 WindowCompactor 把它钉住。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/compact"
)

// savedCheckpoint 记录一次 Save 的检查点快照，以及保存时 History 里属于本回合的
// 消息（用于断言 PriorMessages 的分界）。
type savedCheckpoint struct {
	checkpoint *rig.Checkpoint
	turn       []rig.Message
}

// validatingCheckpointStore 是一个按契约做校验的 CheckpointStore：Save 时调用
// Checkpoint.Validate()，非法检查点直接拒绝（这正是历史上让 Run 硬失败的路径）。
type validatingCheckpointStore struct {
	mu      sync.Mutex
	current *rig.Checkpoint
	saves   []savedCheckpoint
	// history 返回当前会话已持久化的消息；historyBefore 是本次 Run 开始时 History
	// 的条数，两者相减即"本回合追加的消息"。
	history       func() []rig.Message
	historyBefore int
}

func (s *validatingCheckpointStore) Load(context.Context, string) (*rig.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current.Clone(), nil
}

func (s *validatingCheckpointStore) Save(_ context.Context, _ string, checkpoint *rig.Checkpoint) error {
	if err := checkpoint.Validate(); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	snapshot := s.current
	if checkpoint.Phase == rig.CheckpointPhaseCompleted {
		snapshot = checkpoint.Clone()
	}
	var turn []rig.Message
	if s.history != nil {
		persisted := s.history()
		if s.historyBefore <= len(persisted) {
			turn = persisted[s.historyBefore:]
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves = append(s.saves, savedCheckpoint{checkpoint: checkpoint.Clone(), turn: append([]rig.Message(nil), turn...)})
	s.current = snapshot
	return nil
}

func (s *validatingCheckpointStore) Clear(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = nil
	return nil
}

func (s *validatingCheckpointStore) saved() []savedCheckpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]savedCheckpoint(nil), s.saves...)
}

// seedConversation 造一段"上一回合留下"的历史：6 条消息，够触发压缩。
func seedConversation() []rig.Message {
	messages := make([]rig.Message, 0, 6)
	for index := 0; index < 3; index++ {
		messages = append(messages,
			rig.NewMessage(rig.RoleUser, fmt.Sprintf("earlier question %d about the workspace layout", index)),
			rig.NewMessage(rig.RoleAssistant, fmt.Sprintf("earlier answer %d with a little detail to spend tokens", index)),
		)
	}
	return messages
}

// assertCheckpointBoundary 校验一次检查点的 PriorMessages 分界：
//   - 在合法范围内（Validate 在 Save 里已经强制过，这里再显式断言一次）；
//   - prior 侧不含本回合追加的消息（逐条比对）；
//   - turn 侧正好是本回合追加消息的尾部（逐条比对）。
func assertCheckpointBoundary(t *testing.T, save savedCheckpoint, runLabel string) {
	t.Helper()
	prior := save.checkpoint.PriorMessages
	total := len(save.checkpoint.Messages)
	if prior < 0 || prior > total {
		t.Fatalf("%s: PriorMessages = %d outside [0,%d]", runLabel, prior, total)
	}
	turnMessages := save.turn
	if turnPart := save.checkpoint.Messages[prior:]; len(turnPart) > len(turnMessages) {
		t.Fatalf("%s: turn part has %d messages but the run only appended %d",
			runLabel, len(turnPart), len(turnMessages))
	}
	// 逐条比对：prior 侧不能混进本回合的消息。
	for index, message := range save.checkpoint.Messages[:prior] {
		for turnIndex, turnMessage := range turnMessages {
			if sameMessageForTest(message, turnMessage) {
				t.Fatalf("%s: messages[:%d][%d] is this turn's message #%d (prior boundary too small): %q",
					runLabel, prior, index, turnIndex, message.Text())
			}
		}
	}
	// 逐条比对：turn 侧必须是本回合消息的尾部。
	turnPart := save.checkpoint.Messages[prior:]
	offset := len(turnMessages) - len(turnPart)
	for index, message := range turnPart {
		if !sameMessageForTest(message, turnMessages[offset+index]) {
			t.Fatalf("%s: turn part[%d] = %q, want this turn's message[%d] = %q",
				runLabel, index, message.Text(), offset+index, turnMessages[offset+index].Text())
		}
	}
}

// sameMessageForTest 是测试侧的"同一条消息"判定（角色 + 文本 + 工具调用标识）。
func sameMessageForTest(left, right rig.Message) bool {
	if left.Role != right.Role || left.ToolCallID != right.ToolCallID || left.Name != right.Name {
		return false
	}
	if len(left.ToolCalls) != len(right.ToolCalls) || left.Reasoning != right.Reasoning {
		return false
	}
	return left.Text() == right.Text()
}

// TestCheckpointPriorMessagesSurviveCompaction 用真实 WindowCompactor{KeepLast:2}
// 在小预算下连跑 3 轮 Run：检查点必须始终合法，且分界（PriorMessages）指向
// "本回合追加消息的起点"。
func TestCheckpointPriorMessagesSurviveCompaction(t *testing.T) {
	const sessionID = "s-compact"
	history := newCaptureHistory(map[string][]rig.Message{sessionID: seedConversation()})
	store := &validatingCheckpointStore{history: func() []rig.Message { return history.stored(sessionID) }}

	provider := &scriptProvider{scripts: [][]rig.Chunk{
		textScript("answer 1"),
		textScript("answer 2"),
		textScript("answer 3"),
	}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:           "m",
		Provider:        provider,
		History:         history,
		ContextBudget:   20, // 远小于种子历史，强制每轮都压缩
		Compactor:       compact.WindowCompactor{KeepLast: 2},
		CheckpointStore: store,
	})

	compactionSeen := 0
	for round := 1; round <= 3; round++ {
		store.historyBefore = len(history.stored(sessionID))
		rec := &recorder{}
		result, err := a.Run(context.Background(), rig.RunRequest{
			SessionID: sessionID,
			Input:     []rig.Message{userMessage(fmt.Sprintf("q%d: please continue the work", round))},
			Emit:      rec.emit,
		})
		if err != nil {
			t.Fatalf("round %d: run failed: %v", round, err)
		}
		if len(rec.ofType(rig.EventContextCompacted)) == 0 {
			t.Fatalf("round %d: expected the tiny budget to trigger compaction", round)
		}
		compactionSeen += len(rec.ofType(rig.EventContextCompacted))
		if result.FinishReason != "stop" {
			t.Fatalf("round %d: FinishReason = %q", round, result.FinishReason)
		}
	}

	saves := store.saved()
	if len(saves) < 6 {
		t.Fatalf("expected at least 6 checkpoint saves (2 per round), got %d", len(saves))
	}
	compactedPrior := 0
	for index, save := range saves {
		label := fmt.Sprintf("save[%d] phase=%s", index, save.checkpoint.Phase)
		assertCheckpointBoundary(t, save, label)
		// 每一步都要有合法的分界；压缩后 prior 必须 > 0（摘要/旧消息在 prior 一侧），
		// 这样才能证明不是靠"钳到 0"蒙过去的。
		if save.checkpoint.PriorMessages > 0 {
			compactedPrior++
		}
		t.Logf("%s: prior=%d len=%d turn=%d", label, save.checkpoint.PriorMessages,
			len(save.checkpoint.Messages), len(save.checkpoint.Messages)-save.checkpoint.PriorMessages)
	}
	if compactionSeen == 0 || compactedPrior == 0 {
		t.Fatalf("prior was never > 0; the boundary is not tracking the compacted history")
	}
	// 压缩后 conversation 的头部必须是压缩摘要，且它落在 prior 一侧。
	last := saves[len(saves)-1].checkpoint
	head := last.Messages[0].Text()
	if !strings.HasPrefix(head, "Earlier conversation compacted (") {
		t.Fatalf("expected a compaction summary at the head after compaction, got %q", head)
	}
	if last.PriorMessages < 1 {
		t.Fatalf("compaction summary must be on the prior side, PriorMessages = %d", last.PriorMessages)
	}
}

// TestCheckpointPriorMessagesWithoutCompaction：不压缩时 PriorMessages 必须等于
// seeded 历史条数，且 Messages[:PriorMessages] 与种子历史逐条一致。
func TestCheckpointPriorMessagesWithoutCompaction(t *testing.T) {
	const sessionID = "s-plain"
	seed := seedConversation()
	history := newCaptureHistory(map[string][]rig.Message{sessionID: seed})
	store := &validatingCheckpointStore{history: func() []rig.Message { return history.stored(sessionID) }}
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("done")}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:           "m",
		Provider:        provider,
		History:         history,
		ContextBudget:   1_000_000, // 不触发压缩
		Compactor:       compact.WindowCompactor{KeepLast: 2},
		CheckpointStore: store,
	})
	rec := &recorder{}
	store.historyBefore = len(history.stored(sessionID))
	_, err := a.Run(context.Background(), rig.RunRequest{
		SessionID: sessionID,
		Input:     []rig.Message{userMessage("only question")},
		Emit:      rec.emit,
	})
	requireNoError(t, err)
	if len(rec.ofType(rig.EventContextCompacted)) != 0 {
		t.Fatal("compaction must not happen with a huge budget")
	}
	saves := store.saved()
	if len(saves) == 0 {
		t.Fatal("no checkpoints saved")
	}
	for index, save := range saves {
		if save.checkpoint.PriorMessages != len(seed) {
			t.Fatalf("save[%d]: PriorMessages = %d, want %d (seeded history)",
				index, save.checkpoint.PriorMessages, len(seed))
		}
		for messageIndex, message := range save.checkpoint.Messages[:save.checkpoint.PriorMessages] {
			if !sameMessageForTest(message, seed[messageIndex]) {
				t.Fatalf("save[%d]: messages[:prior][%d] = %q, want seeded %q",
					index, messageIndex, message.Text(), seed[messageIndex].Text())
			}
		}
		assertCheckpointBoundary(t, save, fmt.Sprintf("save[%d]", index))
	}
}

// TestRecoveredTurnIsPersistedToHistory：冷启动（History 为空、检查点记录了被中断
// 回合）时，修复出来的消息必须写回 History，而不是只出现在 RunResult 里。
func TestRecoveredTurnIsPersistedToHistory(t *testing.T) {
	const sessionID = "s-cold"
	history := newCaptureHistory(nil) // 会话没有历史
	store := &validatingCheckpointStore{
		current: &rig.Checkpoint{
			SessionID: sessionID,
			TurnID:    "turn-crashed",
			Phase:     rig.CheckpointPhaseRequested,
			Messages: []rig.Message{
				rig.NewMessage(rig.RoleUser, "please do it"),
				{
					Role:      rig.RoleAssistant,
					ToolCalls: []rig.ToolCall{{ID: "call_x", Name: "echo", Arguments: `{}`}},
				},
			},
			PriorMessages: 1,
		},
		history: func() []rig.Message { return history.stored(sessionID) },
	}
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("done")}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:           "m",
		Provider:        provider,
		History:         history,
		CheckpointStore: store,
	})
	result, err := a.Run(context.Background(), rig.RunRequest{
		SessionID: sessionID,
		Input:     []rig.Message{userMessage("are you there?")},
	})
	requireNoError(t, err)

	persisted := history.stored(sessionID)
	recoveredToolResult := false
	recoveredMarker := false
	for index, message := range persisted {
		if message.Role == rig.RoleTool && message.ToolCallID == "call_x" {
			recoveredToolResult = true
			if !strings.Contains(message.Text(), "Interrupted before the tool call finished") {
				t.Fatalf("synthetic tool result has unexpected content: %q", message.Text())
			}
		}
		if message.Role == rig.RoleAssistant && strings.Contains(message.Text(), "previous turn was interrupted") {
			recoveredMarker = true
			// 修复出来的消息必须排在本次用户输入之前。
			if index > len(persisted)-2 {
				t.Fatalf("interruption marker is not ahead of this run's input: index %d of %d", index, len(persisted))
			}
		}
	}
	if !recoveredToolResult || !recoveredMarker {
		t.Fatalf("recovered turn not persisted to History (tool result=%v marker=%v, history=%d messages, RunResult=%d)",
			recoveredToolResult, recoveredMarker, len(persisted), len(result.Messages))
	}

	// 修复后的检查点应被保存为 completed，使下一次恢复成为 no-op。
	store.mu.Lock()
	current := store.current.Clone()
	store.mu.Unlock()
	if current == nil || current.Phase != rig.CheckpointPhaseCompleted {
		t.Fatalf("repaired checkpoint phase = %v, want completed", current)
	}
	// 恢复出来的消息属于上一个回合：检查点里它们必须在 prior 一侧。
	for _, save := range store.saved() {
		turnPart := save.checkpoint.Messages[save.checkpoint.PriorMessages:]
		for _, message := range turnPart {
			if message.Role == rig.RoleTool && message.ToolCallID == "call_x" {
				t.Fatalf("recovered tool result must be on the prior side (prior=%d)", save.checkpoint.PriorMessages)
			}
		}
	}
}
