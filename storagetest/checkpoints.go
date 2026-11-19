package storagetest

import (
	"context"
	"reflect"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// runCheckpointCases 守检查点契约（可选能力：不支持的后端 skip 并打印原因）。
//
// 检查点不是"会话事实的来源"，而是崩溃恢复的提示：恢复方只依赖 SessionID /
// Phase / TurnID / Messages / PriorMessages 这五项。时间戳由实现补齐，套件不
// 断言具体值——断言"实现必须填"只会惩罚实现选择。
func runCheckpointCases(t *testing.T, caps Capability, newStorage Factory) {
	t.Run("CheckpointUnknownSessionIsEmpty", func(t *testing.T) {
		s := newStore(t, newStorage)
		checkpoints := requireCheckpoints(t, s, caps)
		checkpoint, err := checkpoints.Load(context.Background(), unknown)
		if err != nil {
			t.Fatalf("Load(未知会话) = %v，want nil error", err)
		}
		if checkpoint != nil {
			t.Fatalf("Load(未知会话) = %+v，want nil：没有检查点不是错误", checkpoint)
		}
	})

	t.Run("CheckpointRoundTrip", func(t *testing.T) {
		s := newStore(t, newStorage)
		checkpoints := requireCheckpoints(t, s, caps)
		ctx := context.Background()
		want := &rig.Checkpoint{
			SessionID:     sessionA,
			TurnID:        "turn-1",
			Phase:         rig.CheckpointPhaseRequested,
			Messages:      conversation(),
			PriorMessages: 2,
			Metadata:      map[string]string{"trace": "trace-1"},
		}
		if err := checkpoints.Save(ctx, sessionA, want); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := checkpoints.Load(ctx, sessionA)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got == nil {
			t.Fatalf("Load = nil，刚保存的检查点必须能读回")
		}
		// 逐字段比较恢复真正依赖的五个字段；时间戳不参与。
		if got.SessionID != want.SessionID {
			t.Errorf("SessionID = %q，want %q", got.SessionID, want.SessionID)
		}
		if got.TurnID != want.TurnID {
			t.Errorf("TurnID = %q，want %q", got.TurnID, want.TurnID)
		}
		if got.Phase != want.Phase {
			t.Errorf("Phase = %q，want %q", got.Phase, want.Phase)
		}
		if got.PriorMessages != want.PriorMessages {
			t.Errorf("PriorMessages = %d，want %d", got.PriorMessages, want.PriorMessages)
		}
		if !reflect.DeepEqual(got.Messages, want.Messages) {
			t.Errorf("Messages 不一致：\n got = %+v\nwant = %+v", got.Messages, want.Messages)
		}
		if !reflect.DeepEqual(got.Metadata, want.Metadata) {
			t.Errorf("Metadata = %v，want %v", got.Metadata, want.Metadata)
		}
	})

	t.Run("CheckpointOverwriteKeepsLatest", func(t *testing.T) {
		s := newStore(t, newStorage)
		checkpoints := requireCheckpoints(t, s, caps)
		ctx := context.Background()
		// 同一会话的每个回合用新 TurnID；存储层按会话分键覆盖，读到的必须是最后一个。
		if err := checkpoints.Save(ctx, sessionA, &rig.Checkpoint{
			SessionID: sessionA,
			TurnID:    "turn-1",
			Phase:     rig.CheckpointPhaseRequested,
		}); err != nil {
			t.Fatalf("Save turn-1: %v", err)
		}
		if err := checkpoints.Save(ctx, sessionA, &rig.Checkpoint{
			SessionID:     sessionA,
			TurnID:        "turn-2",
			Phase:         rig.CheckpointPhaseCompleted,
			Messages:      conversation(),
			PriorMessages: len(conversation()),
		}); err != nil {
			t.Fatalf("Save turn-2: %v", err)
		}
		got, err := checkpoints.Load(ctx, sessionA)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got == nil || got.TurnID != "turn-2" || got.Phase != rig.CheckpointPhaseCompleted {
			t.Fatalf("Load = %+v，want turn-2/completed（按会话只保留最新一个检查点）", got)
		}
	})

	t.Run("CheckpointClearIsIdempotent", func(t *testing.T) {
		s := newStore(t, newStorage)
		checkpoints := requireCheckpoints(t, s, caps)
		ctx := context.Background()
		if err := checkpoints.Save(ctx, sessionA, &rig.Checkpoint{
			SessionID: sessionA,
			TurnID:    "turn-1",
			Phase:     rig.CheckpointPhaseCompleted,
		}); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := checkpoints.Clear(ctx, sessionA); err != nil {
			t.Fatalf("Clear: %v", err)
		}
		if checkpoint, err := checkpoints.Load(ctx, sessionA); err != nil || checkpoint != nil {
			t.Fatalf("Clear 后 Load = (%+v, %v)，want (nil, nil)", checkpoint, err)
		}
		// 幂等：清一个本来就不存在的检查点不是错误（会话删除/重置路径会这么调）。
		if err := checkpoints.Clear(ctx, sessionA); err != nil {
			t.Fatalf("第二次 Clear = %v，want nil（Clear 必须幂等）", err)
		}
		if err := checkpoints.Clear(ctx, unknown); err != nil {
			t.Fatalf("Clear(未知会话) = %v，want nil", err)
		}
	})
}
