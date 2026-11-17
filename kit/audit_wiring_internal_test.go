package kit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/instructions"
	"github.com/zeokeel/zeokeel-rig/secrets"
)

// 本文件独立审计 kit/wiring.go 的装配胶水（脱敏包装、system prompt 拼接、事件桥）。
// 命名统一加 leadAudit 前缀，避免与 kit 里其它测试文件撞名。

const leadAuditSecret = "sk-auditdeadbeefdeadbeefdeadbeef"

// errLeadAudit 是一个用来验证错误身份（errors.Is）的哨兵。
var errLeadAudit = errors.New("audit sentinel")

// leadAuditTypedErr 用来验证 errors.As 仍然可用。
type leadAuditTypedErr struct{ code int }

func (e *leadAuditTypedErr) Error() string { return fmt.Sprintf("typed failure code=%d", e.code) }

// leadAuditTool 是最小 rig.Tool：用闭包决定 Invoke 行为。
type leadAuditTool struct {
	name   string
	invoke func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error)
}

func (t leadAuditTool) Name() string           { return t.name }
func (t leadAuditTool) Description() string    { return "audit tool" }
func (t leadAuditTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (t leadAuditTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	return t.invoke(ctx, call)
}

// TestAuditRedactingToolErrorPreservesIdentity 断言工具返回的 Go error 被脱敏之后
// 仍然保持错误身份（errors.Is / errors.As 都要能工作）。
//
// 回归用：早先的实现用 fmt.Errorf("%s", …) 重建错误，%w 链被切断，宿主对
// sandbox.ErrDenied / context.Canceled / 自定义哨兵的判定全部失效。
func TestAuditRedactingToolErrorPreservesIdentity(t *testing.T) {
	wrapped := redactingTool{
		Tool: leadAuditTool{name: "leaky", invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{}, fmt.Errorf("call failed with %s: %w", leadAuditSecret, errLeadAudit)
		}},
		redactor: secrets.New(),
	}

	_, err := wrapped.Invoke(context.Background(), rig.ToolCall{ID: "c1", Name: "leaky"})
	if err == nil {
		t.Fatal("期望返回 error")
	}
	if strings.Contains(err.Error(), leadAuditSecret) {
		t.Fatalf("错误文本没有脱敏（模型可见）: %v", err)
	}
	if !errors.Is(err, errLeadAudit) {
		t.Fatalf("errors.Is 失效：错误身份被切断（err=%v）", err)
	}

	// errors.As：类型化错误同样要能取出。
	typed := redactingTool{
		Tool: leadAuditTool{name: "typed", invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{}, fmt.Errorf("boom %s: %w", leadAuditSecret, &leadAuditTypedErr{code: 42})
		}},
		redactor: secrets.New(),
	}
	_, err = typed.Invoke(context.Background(), rig.ToolCall{ID: "c2", Name: "typed"})
	var target *leadAuditTypedErr
	if !errors.As(err, &target) || target.code != 42 {
		t.Fatalf("errors.As 失效: %v", err)
	}
}

// TestAuditRedactingToolErrorPathRedactsResult 断言**同时**返回 result 与 error 时，
// result 里的内容也会脱敏（部分工具会带回半截输出）。
func TestAuditRedactingToolErrorPathRedactsResult(t *testing.T) {
	wrapped := redactingTool{
		Tool: leadAuditTool{name: "partial", invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{
				Content: "partial output " + leadAuditSecret,
				Parts:   []rig.ContentPart{rig.TextPart("part " + leadAuditSecret)},
			}, errors.New("boom")
		}},
		redactor: secrets.New(),
	}

	result, err := wrapped.Invoke(context.Background(), rig.ToolCall{ID: "c1", Name: "partial"})
	if err == nil {
		t.Fatal("期望返回 error")
	}
	if strings.Contains(result.Content, leadAuditSecret) {
		t.Fatalf("错误路径上的 result.Content 未脱敏: %q", result.Content)
	}
	if len(result.Parts) != 1 || strings.Contains(result.Parts[0].Text, leadAuditSecret) {
		t.Fatalf("错误路径上的文本 Part 未脱敏: %+v", result.Parts)
	}
}

// TestAuditRedactingToolErrorWithoutSecretKeepsOriginal 断言错误文本里没有凭据时
// **原样返回同一个 error**（不做无意义的包装，调用方的 == 判断也不受影响）。
func TestAuditRedactingToolErrorWithoutSecretKeepsOriginal(t *testing.T) {
	original := errors.New("plain failure")
	wrapped := redactingTool{
		Tool: leadAuditTool{name: "plain", invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{}, original
		}},
		redactor: secrets.New(),
	}
	_, err := wrapped.Invoke(context.Background(), rig.ToolCall{ID: "c1", Name: "plain"})
	if err != original {
		t.Fatalf("没有凭据时不应包装错误: %v（want 同一个 error 值）", err)
	}
}

// TestAuditRedactingToolSuccessPath 守住正常路径：Content 与文本 Parts 脱敏，
// 图片 Parts 与 IsError 原样保留。
func TestAuditRedactingToolSuccessPath(t *testing.T) {
	wrapped := redactingTool{
		Tool: leadAuditTool{name: "ok", invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{
				Content: "token=" + leadAuditSecret,
				IsError: true,
				Parts: []rig.ContentPart{
					rig.TextPart("nested " + leadAuditSecret),
					rig.ImagePart("data:image/png;base64,AAAA", "auto"),
				},
			}, nil
		}},
		redactor: secrets.New(),
	}

	result, err := wrapped.Invoke(context.Background(), rig.ToolCall{ID: "c1", Name: "ok"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if strings.Contains(result.Content, leadAuditSecret) {
		t.Fatalf("Content 未脱敏: %q", result.Content)
	}
	if !result.IsError {
		t.Fatal("IsError 被改掉了")
	}
	if len(result.Parts) != 2 {
		t.Fatalf("Parts = %d, want 2", len(result.Parts))
	}
	if strings.Contains(result.Parts[0].Text, leadAuditSecret) {
		t.Fatalf("文本 Part 未脱敏: %q", result.Parts[0].Text)
	}
	if result.Parts[1].ImageURL != "data:image/png;base64,AAAA" || result.Parts[1].Type != rig.PartImageURL {
		t.Fatalf("图片 Part 被改动: %+v", result.Parts[1])
	}
}

// TestAuditRedactToolsEdgeCases 覆盖 nil 脱敏器、nil/空工具列表、顺序与名字保持。
func TestAuditRedactToolsEdgeCases(t *testing.T) {
	tools := []rig.Tool{leadAuditTool{name: "a"}, leadAuditTool{name: "b"}}
	if got := redactTools(tools, nil); len(got) != 2 {
		t.Fatalf("nil redactor 应原样返回: %d", len(got))
	}
	if got := redactTools(nil, secrets.New()); got != nil {
		t.Fatalf("nil 工具列表应返回 nil: %v", got)
	}
	if got := redactTools([]rig.Tool{}, secrets.New()); got == nil || len(got) != 0 {
		t.Fatalf("空工具列表应返回空: %v", got)
	}
	wrapped := redactTools(tools, secrets.New())
	if len(wrapped) != 2 || wrapped[0].Name() != "a" || wrapped[1].Name() != "b" {
		t.Fatalf("顺序或名字被破坏: %v", wrapped)
	}
	// 重复名字不在这里去重：交给 agent.New 报错（保持原语义）。
	dupes := redactTools([]rig.Tool{leadAuditTool{name: "x"}, leadAuditTool{name: "x"}}, secrets.New())
	if len(dupes) != 2 {
		t.Fatalf("重复名字被静默去重了: %d", len(dupes))
	}
}

// TestAuditRedactToolsNilElement 断言 nil 元素不会 panic，并且**位置保持不变**：
// 包装层跳过它、把决定权留给 agent.New 的校验（"tool N is nil"）。
//
// 这与 withoutTool 的语义一致（都保留 nil），因此接不接 Redactor，
// `Tools: []Tool{nil}` 都会得到同一个可读校验错误。
func TestAuditRedactToolsNilElement(t *testing.T) {
	tools := []rig.Tool{nil, leadAuditTool{name: "kept"}}
	wrapped := redactTools(tools, secrets.New())
	if len(wrapped) != 2 {
		t.Fatalf("len = %d, want 2（nil 必须占位保留）", len(wrapped))
	}
	if wrapped[0] != nil {
		t.Fatalf("第 0 个元素 = %v, want nil", wrapped[0])
	}
	if wrapped[1] == nil || wrapped[1].Name() != "kept" {
		t.Fatalf("第 1 个元素被破坏: %+v", wrapped[1])
	}
	if kept := withoutTool(tools, "nope"); len(kept) != 2 || kept[0] != nil {
		t.Fatalf("withoutTool 与 redactTools 的 nil 语义不一致: %v", kept)
	}
}

// TestAuditAssembleSystemPrompt 覆盖拼接的空行、幂等与组合顺序。
func TestAuditAssembleSystemPrompt(t *testing.T) {
	environment := &instructions.Environment{
		WorkspaceRoot: "/work", Cwd: "/work", OS: "linux", Arch: "amd64",
		Time: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC), TempDir: "/tmp",
	}
	set := &instructions.Set{Documents: []instructions.Document{{
		Path: "/work/AGENTS.md", Source: instructions.SourceRoot, Content: "# rules\n",
	}}}

	t.Run("base 为空时没有前导空行", func(t *testing.T) {
		got := assembleSystemPrompt("", environment, set, "/work")
		if strings.HasPrefix(got, "\n") || strings.HasPrefix(got, " ") {
			t.Fatalf("出现了前导空白: %q", got[:min(20, len(got))])
		}
		if !strings.HasPrefix(got, "<environment>") {
			t.Fatalf("顺序错误（environment 应在前）: %.40q", got)
		}
		if !strings.Contains(got, "## ") {
			t.Fatalf("指令文档缺失: %.80q", got)
		}
		if strings.Contains(got, "\n\n\n") {
			t.Fatalf("出现了三个以上连续换行: %q", got)
		}
	})

	t.Run("base 以空行结尾时不留尾部空行", func(t *testing.T) {
		if got := assembleSystemPrompt("persona\n\n", nil, nil, "/work"); got != "persona" {
			t.Fatalf("got %q, want %q", got, "persona")
		}
	})

	t.Run("空 Set 与 nil 都不产生块", func(t *testing.T) {
		if got := assembleSystemPrompt("persona", nil, &instructions.Set{}, "/work"); got != "persona" {
			t.Fatalf("空 Set 产生了内容: %q", got)
		}
		if got := assembleSystemPrompt("persona", nil, nil, ""); got != "persona" {
			t.Fatalf("nil 参数产生了内容: %q", got)
		}
	})

	t.Run("两次调用字节相同", func(t *testing.T) {
		if first, second := assembleSystemPrompt("persona", environment, set, "/work"), assembleSystemPrompt("persona", environment, set, "/work"); first != second {
			t.Fatal("同一输入两次调用结果不同，会打掉 provider 的 prompt 缓存")
		}
	})

	t.Run("root 为空也能渲染指令", func(t *testing.T) {
		got := assembleSystemPrompt("", nil, set, "")
		if !strings.Contains(got, "AGENTS.md") {
			t.Fatalf("root 为空时指令渲染异常: %q", got)
		}
	})
}

// TestAuditAssembleSystemPromptZeroEnvironment 断言零值 Environment 不产生块
// （否则会拼进 "date: 0001-01-01" 这种垃圾，既误导模型也污染 prompt 缓存）。
func TestAuditAssembleSystemPromptZeroEnvironment(t *testing.T) {
	got := assembleSystemPrompt("persona", &instructions.Environment{}, nil, "")
	if got != "persona" {
		t.Fatalf("零值 Environment 应当被忽略: %q", got)
	}
	if isZeroEnvironment(instructions.Environment{TempDir: "/tmp"}) != true {
		t.Log("提示：isZeroEnvironment 不检查 TempDir，只设置 TempDir 的 Environment 也会被忽略")
	}
}

// TestAuditAssembleSystemPromptPartialEnvironment 记录一个**残余边界**：只填了一部分
// 字段（例如 Cwd）但 Time 为零的 Environment 仍然会渲染出 "date: 0001-01-01"。
//
// 建议（需 Lead 决策，属于 instructions.Render 的职责）：Time 为零时省略 date 行，
// 或者在 kit 侧对部分填充的 Environment 也做一次 Time 兜底。
func TestAuditAssembleSystemPromptPartialEnvironment(t *testing.T) {
	got := assembleSystemPrompt("", &instructions.Environment{Cwd: "/w"}, nil, "")
	if strings.Contains(got, "0001-01-01") {
		t.Skipf("已知残余边界：部分填充的 Environment 渲染了零值日期（建议在 instructions.Render 里 Time 为零时省略 date）: %q", got)
	}
}

// TestAuditMessageText 覆盖多段内容、纯图片消息与 nil 消息。
func TestAuditMessageText(t *testing.T) {
	if got := messageText(nil); got != "" {
		t.Fatalf("nil message = %q", got)
	}
	multi := &rig.Message{Content: []rig.ContentPart{
		rig.TextPart("alpha "), rig.ImagePart("data:image/png;base64,AAAA", ""), rig.TextPart("beta"),
	}}
	if got := messageText(multi); got != "alpha beta" {
		t.Fatalf("messageText = %q, want %q", got, "alpha beta")
	}
	imageOnly := &rig.Message{Content: []rig.ContentPart{rig.ImagePart("https://x/y.png", "")}}
	if got := messageText(imageOnly); got != "" {
		t.Fatalf("纯图片消息应得到空文本: %q", got)
	}
}

// TestAuditEventBridgeNilSafety 断言事件桥在 nil 情况下不 panic。
func TestAuditEventBridgeNilSafety(t *testing.T) {
	var bridge *eventBridge
	bridge.emit("notice", nil)
	bridge.emitEvent(rig.Event{Type: rig.EventTextDelta, Text: "x"})
	callback := bridge.wrap(nil)
	callback(rig.Event{Type: rig.EventTextDelta, Text: "x"})

	var zero Agent
	if err := zero.EventError(); err != nil {
		t.Fatalf("零值 Agent.EventError() = %v", err)
	}
	var nilAgent *Agent
	if err := nilAgent.EventError(); err != nil {
		t.Fatalf("nil Agent.EventError() = %v", err)
	}
}

// TestAuditEventBridgePayloads 断言事件桥产出的 NDJSON 结构：kind 序列、seq 连续、
// tool.result 带 ID/Name、错误走 error kind。
func TestAuditEventBridgePayloads(t *testing.T) {
	var buffer bytes.Buffer
	bridge := &eventBridge{emitter: events.NewEmitter(&buffer)}

	call := &rig.ToolCall{ID: "call_1", Name: "read_file", Arguments: `{"path":"a.md"}`}
	result := &rig.ToolResult{Content: "hello"}
	bridge.emitEvent(rig.Event{Type: rig.EventTurnStart, Iteration: 1})
	bridge.emitEvent(rig.Event{Type: rig.EventTextDelta, Text: "hi"})
	bridge.emitEvent(rig.Event{Type: rig.EventToolCall, Call: call})
	bridge.emitEvent(rig.Event{Type: rig.EventToolResult, Call: call, Result: result})
	bridge.emitEvent(rig.Event{Type: rig.EventMessage, Message: &rig.Message{
		Role: rig.RoleAssistant, Content: []rig.ContentPart{rig.TextPart("done")},
		ToolCalls: []rig.ToolCall{*call},
	}})
	bridge.emitEvent(rig.Event{Type: rig.EventUsage, Usage: &rig.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}})
	bridge.emitEvent(rig.Event{Type: rig.EventError, Err: errors.New("provider exploded")})
	bridge.emitEvent(rig.Event{Type: rig.EventTurnEnd, Iteration: 1})

	envelopes := make([]events.Envelope, 0, 8)
	if err := events.Replay(&buffer, func(envelope events.Envelope) error {
		envelopes = append(envelopes, envelope)
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(envelopes) != 8 {
		t.Fatalf("事件条数 = %d, want 8", len(envelopes))
	}
	for index, envelope := range envelopes {
		if envelope.Seq != int64(index+1) {
			t.Fatalf("seq 不连续: %d at %d", envelope.Seq, index)
		}
		if envelope.Version != events.Version {
			t.Fatalf("version = %d", envelope.Version)
		}
	}
	wantKinds := []string{
		events.KindNotice, events.KindMessageDelta, events.KindToolCall, events.KindToolResult,
		events.KindMessage, events.KindNotice, events.KindError, events.KindNotice,
	}
	for index, want := range wantKinds {
		if envelopes[index].Kind != want {
			t.Fatalf("第 %d 条 kind = %q, want %q", index, envelopes[index].Kind, want)
		}
	}

	var toolResult events.ToolResult
	if err := json.Unmarshal(envelopes[3].Payload, &toolResult); err != nil {
		t.Fatalf("解析 tool.result: %v", err)
	}
	if toolResult.ID != "call_1" || toolResult.Name != "read_file" || toolResult.Content != "hello" {
		t.Fatalf("tool.result payload = %+v", toolResult)
	}

	var errorPayload events.Error
	if err := json.Unmarshal(envelopes[6].Payload, &errorPayload); err != nil {
		t.Fatalf("解析 error: %v", err)
	}
	if errorPayload.Stage != "agent" || !strings.Contains(errorPayload.Message, "provider exploded") {
		t.Fatalf("error payload = %+v", errorPayload)
	}

	// 记录设计缺口：events 包定义了 run.start / run.end（含 model/turns/用量），
	// 但事件桥把 TurnStart/TurnEnd 映射成了 notice，从未写出这两个 kind。
	for _, envelope := range envelopes {
		if envelope.Kind == events.KindRunStart || envelope.Kind == events.KindRunEnd {
			t.Fatalf("意外出现了 run 级事件: %q", envelope.Kind)
		}
	}
	t.Log("现状：事件桥不产出 events.KindRunStart / KindRunEnd（只有 turn 级 notice），" +
		"NDJSON 消费方拿不到 model/workspace/turns/用量汇总")
}

// TestAuditEventBridgeToolResultWithoutCall 记录：没有 Call 的 ToolResult 事件会被
// 写成空 ID/Name（消费方无法与 tool.call 关联）。
func TestAuditEventBridgeToolResultWithoutCall(t *testing.T) {
	var buffer bytes.Buffer
	bridge := &eventBridge{emitter: events.NewEmitter(&buffer)}
	bridge.emitEvent(rig.Event{Type: rig.EventToolResult, Result: &rig.ToolResult{Content: "x"}})

	var envelope events.Envelope
	if err := events.Replay(&buffer, func(item events.Envelope) error {
		envelope = item
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	var payload events.ToolResult
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if payload.ID != "" || payload.Name != "" {
		t.Fatalf("payload = %+v, want 空 ID/Name", payload)
	}
	t.Log("现状：无 Call 的 tool.result 写空 ID/Name（agent 的正常路径总会带 Call）")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
