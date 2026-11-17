package gate_test

// 审计 2C / 2F：**「需要审批但没有审批通道」必须是 deny + 理由，不能是 Go error**。
//
// 原因：agent 看到 gate 返回的 Go error 会**中止整个 run**，模型没有下一轮、用户看到
// "运行失败"。对个人客户端这是最差的失败模式——一次配置疏漏（忘了传 inner/Asker）
// 让整个会话死掉，而不是让模型知道"这条命令没被允许，换个办法"。

import (
	"context"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
)

// noChannelReasonFragment 是"没有审批通道"类理由的共同片段。
const noChannelReasonFragment = "没有配置审批通道"

// TestCommandRiskGateNilInnerDeniesWithReason 断言 CommandRiskGate(nil) 在需要 inner
// 时返回 deny + 理由，而不是 Go error。
func TestCommandRiskGateNilInnerDeniesWithReason(t *testing.T) {
	g := gate.CommandRiskGate(nil)
	tool, call := bashInvocation("rm -rf ./build") // 可变命令：需要人工批准

	decision, err := approve(t, g, tool, call)
	if err != nil {
		t.Fatalf("必须是 DecisionDeny 而不是 error（error 会中止整个 run）: %v", err)
	}
	if decision != rig.DecisionDeny {
		t.Fatalf("decision = %q, want deny", decision)
	}
	reason := explainDenial(g, tool, call)
	if !strings.Contains(reason, noChannelReasonFragment) {
		t.Fatalf("理由应说明没有审批通道: %q", reason)
	}
	if strings.Contains(reason, "no inner gate is configured") {
		t.Fatalf("理由不应该还是英文配置错误原文: %q", reason)
	}

	// 只读命令仍然免问（不需要 inner）。
	readTool, readCall := bashInvocation("ls -la")
	if decision, err := approve(t, g, readTool, readCall); err != nil || decision != rig.DecisionAllow {
		t.Fatalf("只读命令: decision=%q err=%v, want allow", decision, err)
	}
	// 危险命令仍然拒绝，理由用规则 id（比"没有通道"更有用）。
	dangerTool, dangerCall := bashInvocation("rm -rf /")
	if decision, err := approve(t, g, dangerTool, dangerCall); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("危险命令: decision=%q err=%v, want deny", decision, err)
	}
	if reason := explainDenial(g, dangerTool, dangerCall); !strings.Contains(reason, "rm.recursive-root") {
		t.Fatalf("危险命令理由应含规则 id: %q", reason)
	}
	// 非命令工具（read_file 这种不走命令判定的）在 inner=nil 下也要 deny + 理由。
	fileTool := stubTool{name: "apply_patch"}
	fileCall := rig.ToolCall{Name: "apply_patch", Arguments: `{"patch":"x"}`}
	decision, err = approve(t, g, fileTool, fileCall)
	if err != nil || decision != rig.DecisionDeny {
		t.Fatalf("非命令工具: decision=%q err=%v, want deny", decision, err)
	}
	if reason := explainDenial(g, fileTool, fileCall); !strings.Contains(reason, noChannelReasonFragment) {
		t.Fatalf("非命令工具的理由应说明没有审批通道: %q", reason)
	}
}

// TestAllowThenAskNilInnerDeniesWithReason 断言组合门禁同样 fail closed 但不抛错。
func TestAllowThenAskNilInnerDeniesWithReason(t *testing.T) {
	g := gate.AllowThenAsk(gate.SandboxReadOnlyTools, nil)

	// 白名单工具免问。
	readTool := stubTool{name: "read_file"}
	if decision, err := approve(t, g, readTool, rig.ToolCall{Name: "read_file"}); err != nil || decision != rig.DecisionAllow {
		t.Fatalf("白名单工具: decision=%q err=%v, want allow", decision, err)
	}
	// 非白名单：deny + 理由，而不是 error。
	writeTool := stubTool{name: "write_file"}
	writeCall := rig.ToolCall{Name: "write_file", Arguments: `{"path":"a.md","content":"x"}`}
	decision, err := approve(t, g, writeTool, writeCall)
	if err != nil {
		t.Fatalf("必须是 deny 而不是 error: %v", err)
	}
	if decision != rig.DecisionDeny {
		t.Fatalf("decision = %q, want deny", decision)
	}
	if reason := explainDenial(g, writeTool, writeCall); !strings.Contains(reason, noChannelReasonFragment) {
		t.Fatalf("理由应说明没有审批通道: %q", reason)
	}
}

// TestAskNilAskerDeniesWithReason 断言 gate.Ask(nil) 也是 deny + 理由（对话继续）。
func TestAskNilAskerDeniesWithReason(t *testing.T) {
	for name, g := range map[string]rig.Gate{
		"Ask(nil)":       gate.Ask(nil),
		"Ask(typed nil)": gate.Ask((*stubAsker)(nil)),
	} {
		t.Run(name, func(t *testing.T) {
			tool, call := bashInvocation("ls -la")
			decision, err := approve(t, g, tool, call)
			if err != nil {
				t.Fatalf("必须是 deny 而不是 error: %v", err)
			}
			if decision != rig.DecisionDeny {
				t.Fatalf("decision = %q, want deny", decision)
			}
			reason := explainDenial(g, tool, call)
			if !strings.Contains(reason, "没有可用的审批通道") {
				t.Fatalf("理由应说明没有 Asker: %q", reason)
			}
			if !strings.Contains(reason, "未执行") {
				t.Fatalf("理由应说明本次调用未执行: %q", reason)
			}
		})
	}
}

// TestPersonalDefaultNilDeniesAndKeepsRunAlive 断言 PersonalDefault(nil) 不再抛错。
func TestPersonalDefaultNilDeniesAndKeepsRunAlive(t *testing.T) {
	g := gate.PersonalDefault(nil)

	// 只读免问。
	readTool := stubTool{name: "read_file"}
	if decision, err := approve(t, g, readTool, rig.ToolCall{Name: "read_file"}); err != nil || decision != rig.DecisionAllow {
		t.Fatalf("只读工具: decision=%q err=%v, want allow", decision, err)
	}
	readCmdTool, readCmdCall := bashInvocation("ls")
	if decision, err := approve(t, g, readCmdTool, readCmdCall); err != nil || decision != rig.DecisionAllow {
		t.Fatalf("只读命令: decision=%q err=%v, want allow", decision, err)
	}
	// 危险命令仍然拒绝。
	dangerTool, dangerCall := bashInvocation("rm -rf /")
	if decision, err := approve(t, g, dangerTool, dangerCall); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("危险命令: decision=%q err=%v, want deny", decision, err)
	}
	// 需要审批的：deny + 理由（**不是** error）。
	tool, call := bashInvocation("rm -rf ./build")
	decision, err := approve(t, g, tool, call)
	if err != nil {
		t.Fatalf("PersonalDefault(nil) 必须 deny 而不是 error（否则一次配置疏漏会让会话死掉）: %v", err)
	}
	if decision != rig.DecisionDeny {
		t.Fatalf("decision = %q, want deny", decision)
	}
	if reason := explainDenial(g, tool, call); !strings.Contains(reason, noChannelReasonFragment) {
		t.Fatalf("理由应说明没有审批通道: %q", reason)
	}
}

// TestUnrecoverableGateErrorsStillFail 是**对照**：真正的配置错误仍然必须返回 error，
// 不能把"所有错误都吞成 deny"。
func TestUnrecoverableGateErrorsStillFail(t *testing.T) {
	tests := []struct {
		name string
		gate rig.Gate
	}{
		// 构造函数用错（fn 为 nil）：重试也不会好，必须让宿主看见。
		{"Func(nil)", gate.Func(nil)},
		// 门禁值本身是 nil 指针：调用方违约。
		{"nil *AskGate", (*gate.AskGate)(nil)},
		// 没有 reviewer 且没有 fallback：门禁整体不可用。
		{"Review(nil)", gate.Review(nil)},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			tool, call := bashInvocation("ls")
			if _, err := testCase.gate.Approve(context.Background(), tool, call); err == nil {
				t.Fatalf("%s 应当返回 error（不可恢复的配置错误，不能吞成 deny）", testCase.name)
			}
		})
	}
}

// stubAsker 用于构造 typed-nil Asker。
type stubAsker struct{}

func (s *stubAsker) Ask(context.Context, string, []string) (string, error) { return gate.AskAllow, nil }
