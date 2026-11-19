package gate_test

// spill_read 的"注册了但不可用"回归。
//
// spill_read 只在宿主接了 SpillStore 时才注册（见 tools/builtin.NewSpillRead），
// 它读的是**本进程自己**为超长工具结果写下的 `ref:` 句柄：不碰用户文件、不起进程、
// 不走网络。它却不在 gate.SandboxReadOnlyTools 里 → 默认 ask 档下每次调用都要
// 弹窗（没有 Asker 时直接被拒），于是"溢出结果读回来"这条唯一的补救路径等于没有。

import (
	"context"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

// spillReadStore 是只实现"读回"的最小 SpillStore；本用例只关心审批结论。
type spillReadStore struct{}

func (spillReadStore) Save(context.Context, rig.ToolCall, string) (rig.SpillRef, error) {
	return rig.SpillRef{}, nil
}

func (spillReadStore) Load(context.Context, string) (string, error) { return "payload", nil }

func (spillReadStore) GC(context.Context, int) (int, error) { return 0, nil }

// TestSpillReadIsAllowedInReadOnlyTier 断言默认 ask 档下 spill_read 不触发审批。
func TestSpillReadIsAllowedInReadOnlyTier(t *testing.T) {
	tool := builtin.NewSpillRead(spillReadStore{})
	if tool == nil {
		t.Fatal("NewSpillRead 在 store 非 nil 时返回了 nil")
	}
	if tool.Name() != "spill_read" {
		t.Fatalf("工具名 = %q, want spill_read", tool.Name())
	}

	asker := &auditAsker{answer: "allow"}
	row := decide(t, gate.PersonalAsk(gate.Ask(asker)), asker, tool.Name(), `{"id":"spill-1"}`)
	if row.err != nil {
		t.Fatalf("Approve: %v", row.err)
	}
	if row.asked != 0 {
		t.Fatalf("spill_read 在 ask 档下打扰了审批人（asked=%d）", row.asked)
	}
	if row.decision != rig.DecisionAllow {
		t.Fatalf("decision = %q, want allow", row.decision)
	}

	// 没有 Asker 的宿主同样必须能读回（否则溢出结果等于永久丢失）。
	noAskerRow := decide(t, gate.PersonalAskWithoutAsker(), nil, tool.Name(), `{"id":"spill-1"}`)
	if noAskerRow.decision != rig.DecisionAllow {
		t.Fatalf("无 Asker 的 ask 档 decision = %q, want allow（理由: %s）",
			noAskerRow.decision, noAskerRow.reason)
	}
}
