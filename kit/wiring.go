package kit

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/costmeter"
	"github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/instructions"
)

// 本文件是 kit 的"装配层胶水"：系统提示词组装与事件桥。
//
// 工具边界**不做**脱敏：个人客户端默认不"防自己人"（单用户、数据都是用户自己
// 的）。事件桥原样写出事件：SDK 不做输出脱敏。

// assembleSystemPrompt 按"base persona → <environment> → 指令文档"的顺序拼装。
//
// 顺序是有理由的：模型对更靠后的具体约束权重更高，而 AGENTS.md 属于**不可信
// 输入**（仓库作者可写），所以宿主自己的 system prompt 放在最前，块与文档在
// 其后、由宿主决定是否追加更严格的安全规则。
// PromptSection 是宿主追加的一段系统提示词。Name 只用于诊断（不渲染进输出），
// Body 是正文。
type PromptSection struct {
	Name string
	Body string
}

// assembleSystemPrompt 按"base persona → 宿主自定义区段（按序）→ <environment> →
// 指令文档"拼装。空区段被忽略，区段之间恰好一个空行。
func assembleSystemPrompt(base string, environment *instructions.Environment, set *instructions.Set, root string, sections ...PromptSection) string {
	var builder strings.Builder
	builder.WriteString(strings.TrimRight(base, "\n"))
	appendBlock := func(block string) {
		block = strings.TrimSpace(block)
		if block == "" {
			return
		}
		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(block)
	}
	for _, section := range sections {
		appendBlock(section.Body)
	}
	// 零值 Environment 会渲染出空 workspace / 零值日期 / "platform: /"，那既会
	// 误导模型也会污染 prompt 缓存：只有真正探测过的环境才渲染。
	if environment != nil && !isZeroEnvironment(*environment) {
		appendBlock(environment.Render())
	}
	if set != nil {
		appendBlock(set.Render(root))
	}
	return builder.String()
}

// noticePayload 是事件流 notice 的 payload（events 包不定义具体类型，保持自由）。
type noticePayload struct {
	Message string `json:"message"`
}

// isZeroEnvironment 判断环境块是否"没被探测过"。
func isZeroEnvironment(env instructions.Environment) bool {
	return env.WorkspaceRoot == "" && env.Cwd == "" && env.OS == "" && env.TempDir == "" && env.Time.IsZero()
}

// eventBridge 把 agent 事件转成 events.Envelope（NDJSON）。
//
// 它是**尽力而为**的旁路：写失败不打断 agent（事件日志不该拖垮主循环），
// 但错误会被记住，宿主可通过 Agent.EventError() 读取。
type eventBridge struct {
	emitter *events.Emitter
	// meter 非空时按 EventUsage 累计会话成本。注意成本必须归因到"本次运行真正
	// 使用的模型"——调用方每次 Run 前用 setMeterModel 更新。
	meter *costmeter.Meter
	model string
}

// newEventBridgeFor 在**有事件写入器或有成本计量器**时构造桥：只开 Meter 的宿主
// 也要走事件路径，否则用量永远进不了计量器。
func newEventBridgeFor(w io.Writer, meter *costmeter.Meter, model string) *eventBridge {
	if w == nil && meter == nil {
		return nil
	}
	b := newEventBridge(w)
	if b == nil {
		b = &eventBridge{}
	}
	b.meter = meter
	b.model = model
	return b
}

func newEventBridge(w io.Writer) *eventBridge {
	if w == nil {
		return nil
	}
	return &eventBridge{emitter: events.NewEmitter(w)}
}

// setMeter 挂上成本计量器，并指定本次运行的模型名（成本按模型计价）。
func (b *eventBridge) setMeter(m *costmeter.Meter, model string) {
	if b == nil {
		return
	}
	b.meter = m
	b.model = model
}

// EventSink 是事件桥的**导出形态**：把 rig.Event 映射成 events.Envelope 并写成
// NDJSON。给不经过 kit 装配的宿主（自己 agent.New）用同一份映射，不必抄一份。
//
// 语义与 kit 内部的那条旁路一致：尽力而为——写失败不打断 Run，错误累积到 Err()。
// 它原样写出事件（SDK 不做输出脱敏）。
type EventSink struct {
	bridge *eventBridge
}

// NewEventSink 构造事件桥。w 为 nil 时返回一个惰性 sink（Emit 是 no-op，
// Err 永远 nil），这样宿主可以无条件挂到 AgentSpec.Emit 上。
func NewEventSink(w io.Writer) *EventSink {
	sink := &EventSink{}
	if w == nil {
		return sink
	}
	sink.bridge = newEventBridge(w)
	return sink
}

// Emit 写一条事件。nil sink 上是 no-op（方便"事件流没开"的宿主直接挂）。
func (s *EventSink) Emit(event rig.Event) {
	if s == nil || s.bridge == nil {
		return
	}
	s.bridge.emitEvent(event)
}

// Err 返回第一次写失败的错误。
func (s *EventSink) Err() error {
	if s == nil || s.bridge == nil || s.bridge.emitter == nil {
		return nil
	}
	return s.bridge.emitter.Err()
}

// Err 返回事件写入的第一个错误（没有则 nil）。
func (a *Agent) EventError() error {
	if a == nil || a.events == nil || a.events.emitter == nil {
		return nil
	}
	return a.events.emitter.Err()
}

// wrap 在保留调用方回调的前提下，把同一批事件也写进事件流。
func (b *eventBridge) wrap(next func(rig.Event)) func(rig.Event) {
	return func(event rig.Event) {
		b.emitEvent(event)
		if next != nil {
			next(event)
		}
	}
}

func (b *eventBridge) emit(kind string, payload any) {
	if b == nil || b.emitter == nil {
		return
	}
	_ = b.emitter.Emit(kind, payload)
}

func (b *eventBridge) emitEvent(event rig.Event) {
	if b == nil {
		return
	}
	if b.emitter == nil && b.meter == nil {
		return
	}
	switch event.Type {
	case rig.EventContextCompacted:
		// 个人客户端最怕"历史被删了"：压缩必须让用户看见。
		if event.Compaction != nil {
			b.emit(events.KindNotice, noticePayload{Message: fmt.Sprintf(
				"compacted %d message(s) into a summary", event.Compaction.Before)})
			break
		}
		b.emit(events.KindNotice, noticePayload{Message: "context compacted"})
	case rig.EventTurnStart:
		b.emit(events.KindNotice, noticePayload{Message: fmt.Sprintf("turn %d started", event.Iteration)})
	case rig.EventTextDelta:
		b.emit(events.KindMessageDelta, events.MsgDelta{Role: "assistant", Text: event.Text})
	case rig.EventReasoningDelta:
		b.emit(events.KindReasoning, events.MsgDelta{Role: "assistant", Text: event.Reasoning})
	case rig.EventToolCall:
		if event.Call != nil {
			b.emit(events.KindToolCall, events.ToolCall{
				ID:        event.Call.ID,
				Name:      event.Call.Name,
				Arguments: event.Call.Arguments,
			})
		}
	case rig.EventToolResult:
		if event.Result != nil {
			// rig.ToolResult 不带 call id/name，它们来自同一事件携带的调用。
			payload := events.ToolResult{Content: event.Result.Content, IsError: event.Result.IsError}
			if event.Call != nil {
				payload.ID = event.Call.ID
				payload.Name = event.Call.Name
			}
			if event.Result.Data != nil {
				// 宿主的结构化结果原样进事件流（工具结果在事件层是无损的）。
				// marshal 失败不该让整条 tool.result 或主循环崩掉：留空 Data，
				// 补一条 notice 说明原因，工具结果文本照发。
				if encoded, err := json.Marshal(event.Result.Data); err != nil {
					b.emit(events.KindNotice, noticePayload{
						Message: "tool result data was not serializable: " + err.Error(),
					})
				} else {
					payload.Data = encoded
				}
			}
			b.emit(events.KindToolResult, payload)
		}
	case rig.EventMessage:
		if event.Message != nil {
			b.emit(events.KindMessage, events.Message{
				Role:      string(event.Message.Role),
				Text:      messageText(event.Message),
				ToolCalls: len(event.Message.ToolCalls),
			})
		}
	case rig.EventUsage:
		if event.Usage != nil {
			if b.meter != nil {
				// 归因到本次运行真正使用的模型（RunRequest.Model 优先于 Spec.Model）。
				b.meter.Add(b.model, *event.Usage)
			}
			b.emit(events.KindNotice, noticePayload{
				Message: fmt.Sprintf("usage: prompt=%d completion=%d total=%d",
					event.Usage.PromptTokens, event.Usage.CompletionTokens, event.Usage.TotalTokens),
			})
		}
	case rig.EventError:
		message := "unknown error"
		if event.Err != nil {
			message = event.Err.Error()
		}
		b.emit(events.KindError, events.Error{Stage: "agent", Message: message})
	case rig.EventTurnEnd:
		b.emit(events.KindNotice, noticePayload{Message: fmt.Sprintf("turn %d finished", event.Iteration)})
	}
}

// messageText 把多段 content 拼成纯文本（事件流只承载可读文本）。
func messageText(message *rig.Message) string {
	if message == nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range message.Content {
		if part.Type == rig.PartText {
			builder.WriteString(part.Text)
		}
	}
	return builder.String()
}

// emitApproval 把一次审批观察点写成 approval.request / approval.response 事件。
//
// 个人客户端的 UI 靠它表达"正在等你确认"：先收到 approval.request（可以弹窗、
// 可以显示等待态），决定出来后再收到 approval.response（关掉弹窗、显示结论与
// 理由）。两个 payload 都是 events 包里定的线格式。
func (b *eventBridge) emitApproval(event gate.AskEvent) {
	if b == nil {
		return
	}
	switch event.Stage {
	case gate.AskStageRequested:
		b.emit(events.KindApprovalRequest, events.ApprovalRequest{
			Tool: event.Tool,
			Args: event.Args,
		})
	case gate.AskStageDecided:
		payload := events.ApprovalResponse{
			Tool:     event.Tool,
			Decision: string(event.Decision),
			Reason:   event.Reason,
		}
		if event.Err != nil {
			payload.Decision = "error"
			payload.Reason = event.Err.Error()
		}
		b.emit(events.KindApprovalResponse, payload)
	}
}
