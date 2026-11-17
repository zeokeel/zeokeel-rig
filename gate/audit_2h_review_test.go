package gate_test

// 第二轮审计（2H）：自动审查门禁（ReviewGate）在"审查者不可靠"时的行为。
//
// 契约要求：审查者返回非法输入 / 超时 / 出错时必须 fail closed（或者回退到人工），
// 且**不能中断 run**（Approve 不返回 Go error，除非调用方 ctx 结束）。

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
)

// TestAudit2HReviewInvalidArgsFailsClosed 参数不是合法 JSON 时审查者无法判断：
// 必须按失败处理（默认为拒绝，且不是 Go error）。
func TestAudit2HReviewInvalidArgsFailsClosed(t *testing.T) {
	called := false
	g := gate.Review(gate.ToolReviewerFunc(func(context.Context, gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
		called = true
		return gate.ToolReviewDecision{Allow: true}, nil
	}))
	decision, err := g.Approve(context.Background(), auditTool{name: "write_file"}, rig.ToolCall{
		Name: "write_file", Arguments: `{"path": `, // 坏的 JSON
	})
	if err != nil {
		t.Fatalf("Approve 不该返回 Go error（会中止 run）: %v", err)
	}
	if decision != rig.DecisionDeny {
		t.Fatalf("decision = %q, want deny", decision)
	}
	if called {
		t.Fatal("参数非法时不该调用审查者")
	}
}

// TestAudit2HReviewTimeoutFailsClosed 审查超时必须按失败处理，且**不能**把 run 卡住。
func TestAudit2HReviewTimeoutFailsClosed(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	g := gate.Review(
		gate.ToolReviewerFunc(func(ctx context.Context, _ gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
			<-block // 无视 ctx 的坏审查者
			return gate.ToolReviewDecision{Allow: true}, nil
		}),
		gate.WithReviewTimeout(20*time.Millisecond),
	)
	start := time.Now()
	decision, err := g.Approve(context.Background(), auditTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{"command":"ls"}`})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Approve 不该返回 Go error: %v", err)
	}
	if decision != rig.DecisionDeny {
		t.Fatalf("超时后 decision = %q, want deny（fail closed）", decision)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("超时没有生效，耗时 %s", elapsed)
	}
}

// TestAudit2HReviewerErrorUsesFallback 审查失败时应回退到人工门禁（配置了的话）。
func TestAudit2HReviewerErrorUsesFallback(t *testing.T) {
	var fallbackCalls atomic.Int32
	base := gate.AllowThenAsk(nil, gate.Ask(&auditAsker{answer: "allow"}))
	g := gate.Review(
		gate.ToolReviewerFunc(func(context.Context, gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
			return gate.ToolReviewDecision{}, errors.New("reviewer model down")
		}),
		gate.WithReviewFallback(audit2hCountingGate{inner: base, calls: &fallbackCalls}),
	)
	decision, err := g.Approve(context.Background(), auditTool{name: "write_file"}, rig.ToolCall{Name: "write_file", Arguments: `{"path":"a"}`})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if decision != rig.DecisionAllow {
		t.Fatalf("decision = %q, want allow（回退门禁放行）", decision)
	}
	if fallbackCalls.Load() != 1 {
		t.Fatalf("回退门禁调用次数 = %d, want 1", fallbackCalls.Load())
	}
}

// audit2hCountingGate 记录调用次数后转发给内层门禁。
type audit2hCountingGate struct {
	inner rig.Gate
	calls *atomic.Int32
}

func (g audit2hCountingGate) Approve(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
	g.calls.Add(1)
	return g.inner.Approve(ctx, tool, call)
}

// TestAudit2HReviewDenyReasonIsExposed 审查拒绝的理由必须能通过 ExplainDenial 拿到
// （否则模型只会看到"被拒绝"，然后原样重试）。
func TestAudit2HReviewDenyReasonIsExposed(t *testing.T) {
	g := gate.Review(gate.ToolReviewerFunc(func(context.Context, gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
		return gate.ToolReviewDecision{Allow: false, Reason: "写系统目录需要人工确认"}, nil
	}))
	tool := auditTool{name: "write_file"}
	call := rig.ToolCall{Name: "write_file", Arguments: `{"path":"/etc/passwd"}`}
	decision, err := g.Approve(context.Background(), tool, call)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if decision != rig.DecisionDeny {
		t.Fatalf("decision = %q", decision)
	}
	reason := g.ExplainDenial(tool, call)
	if !strings.Contains(reason, "写系统目录需要人工确认") {
		t.Fatalf("ExplainDenial = %q", reason)
	}
}

// TestAudit2HObserverPanicIsIsolated 覆盖 2H-P4 修复：观察者 panic 被隔离，
// 审批决定照常返回，run 不被中断。
func TestAudit2HObserverPanicIsIsolated(t *testing.T) {
	g := gate.Review(
		gate.ToolReviewerFunc(func(context.Context, gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
			return gate.ToolReviewDecision{Allow: true}, nil
		}),
		gate.WithReviewObserver(func(gate.ReviewOutcome) { panic("observer exploded") }),
	)
	decision, err := g.Approve(context.Background(), auditTool{name: "read_file"}, rig.ToolCall{Name: "read_file", Arguments: `{}`})
	if err != nil {
		t.Fatalf("观察者 panic 不该让 Approve 返回 Go error: %v", err)
	}
	if decision != rig.DecisionAllow {
		t.Fatalf("decision = %q, want allow（决定已经成立）", decision)
	}
}

// TestAudit2HReviewerPanicFailsClosed 覆盖 2H-P4 修复：审查者 panic 被转成"审查
// 失败"，走既有通道（无 fallback 时 fail-closed 拒绝），进程不崩。
func TestAudit2HReviewerPanicFailsClosed(t *testing.T) {
	g := gate.Review(gate.ToolReviewerFunc(func(context.Context, gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
		panic("reviewer exploded")
	}))
	decision, err := g.Approve(context.Background(), auditTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{"command":"ls"}`})
	if err != nil {
		t.Fatalf("审查者 panic 必须按「审查失败」处理（不返回 Go error）: %v", err)
	}
	if decision != rig.DecisionDeny {
		t.Fatalf("decision = %q, want deny（fail closed）", decision)
	}
	if reason := g.ExplainDenial(auditTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{"command":"ls"}`}); !strings.Contains(reason, "panic") {
		t.Fatalf("ExplainDenial 应说明审查者 panic 过: %q", reason)
	}
}

// TestAudit2HReviewerPanicUsesFallback：审查者 panic 且配置了回退时，交给人审。
func TestAudit2HReviewerPanicUsesFallback(t *testing.T) {
	var fallbackCalls atomic.Int32
	base := gate.AllowThenAsk(nil, gate.Ask(&auditAsker{answer: "allow"}))
	g := gate.Review(
		gate.ToolReviewerFunc(func(context.Context, gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
			panic("reviewer exploded")
		}),
		gate.WithReviewFallback(audit2hCountingGate{inner: base, calls: &fallbackCalls}),
	)
	decision, err := g.Approve(context.Background(), auditTool{name: "write_file"}, rig.ToolCall{Name: "write_file", Arguments: `{"path":"a"}`})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if decision != rig.DecisionAllow || fallbackCalls.Load() != 1 {
		t.Fatalf("decision=%q fallbackCalls=%d（应回退人工并放行）", decision, fallbackCalls.Load())
	}
}

// TestAudit2HAskerPanicIsIsolated：人工审批通道（宿主 Asker）panic 同样不能崩。
func TestAudit2HAskerPanicIsIsolated(t *testing.T) {
	g := gate.Ask(panicAsker{})
	decision, err := g.Approve(context.Background(), auditTool{name: "write_file"}, rig.ToolCall{Name: "write_file", Arguments: `{"path":"a"}`})
	if err == nil {
		t.Fatalf("Asker panic 应按「审批设施故障」返回错误，实际 decision=%q", decision)
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("错误应说明 panic: %v", err)
	}
}

// panicAsker 是一个必定 panic 的宿主审批通道。
type panicAsker struct{}

func (panicAsker) Ask(context.Context, string, []string) (string, error) { panic("asker exploded") }

// TestAudit2HAskObserverStages：AskGate 的观察者先收到 requested、再收到 decided，
// 供 kit 发 approval.request / approval.response。
func TestAudit2HAskObserverStages(t *testing.T) {
	var stages []gate.AskStage
	var decisions []rig.Decision
	g := gate.Ask(&auditAsker{answer: "allow"}, gate.WithAskObserver(func(event gate.AskEvent) {
		stages = append(stages, event.Stage)
		if event.Stage == gate.AskStageDecided {
			decisions = append(decisions, event.Decision)
		}
		if event.Tool != "write_file" {
			t.Errorf("event.Tool = %q", event.Tool)
		}
	}))
	decision, err := g.Approve(context.Background(), auditTool{name: "write_file"}, rig.ToolCall{Name: "write_file", Arguments: `{"path":"a"}`})
	if err != nil || decision != rig.DecisionAllow {
		t.Fatalf("Approve = (%q,%v)", decision, err)
	}
	if len(stages) != 2 || stages[0] != gate.AskStageRequested || stages[1] != gate.AskStageDecided {
		t.Fatalf("stages = %v", stages)
	}
	if len(decisions) != 1 || decisions[0] != rig.DecisionAllow {
		t.Fatalf("decisions = %v", decisions)
	}
}

// TestAudit2HReviewNotifierFires：审查开始前的通知先于结果观察者。
func TestAudit2HReviewNotifierFires(t *testing.T) {
	var order []string
	g := gate.Review(
		gate.ToolReviewerFunc(func(context.Context, gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
			order = append(order, "review")
			return gate.ToolReviewDecision{Allow: true}, nil
		}),
		gate.WithReviewNotifier(func(request gate.ToolReviewRequest) {
			order = append(order, "notify:"+request.Tool)
		}),
		gate.WithReviewObserver(func(gate.ReviewOutcome) { order = append(order, "observed") }),
	)
	if _, err := g.Approve(context.Background(), auditTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{}`}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	want := []string{"notify:bash", "review", "observed"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", order, want)
	}
}
