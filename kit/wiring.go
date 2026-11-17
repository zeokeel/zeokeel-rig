package kit

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/instructions"
	"github.com/zeokeel/zeokeel-rig/secrets"
)

// 本文件是 kit 的"装配层胶水"：脱敏包装、系统提示词组装、事件桥。
// 三者都只依赖公开契约，不改变任何包的行为语义。

// redactTools 把每个工具的返回内容过一遍 Redactor。
//
// 脱敏放在**工具边界**而不是 agent 内部：一次包装覆盖所有工具（含自定义与
// MCP），且模型、日志、后续压缩看到的都是已脱敏的文本。ToolResult.Parts 里的
// 文本块同样处理；图片等非文本块原样保留。
func redactTools(tools []rig.Tool, redactor *secrets.Redactor) []rig.Tool {
	if redactor == nil || len(tools) == 0 {
		return tools
	}
	out := make([]rig.Tool, len(tools))
	for index, tool := range tools {
		if tool == nil {
			continue // 保持位置不变（nil 交由 agent 的校验报错），只是不加包装
		}
		out[index] = redactingTool{Tool: tool, redactor: redactor}
	}
	return out
}

type redactingTool struct {
	rig.Tool
	redactor *secrets.Redactor
}

func (t redactingTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	result, err := t.Tool.Invoke(ctx, call)
	// 错误路径同样要脱敏：部分工具在返回 error 的同时也带回了部分输出。
	result.Content = t.redactor.Redact(result.Content)
	if len(result.Parts) > 0 {
		result.Parts = redactParts(t.redactor, result.Parts)
	}
	if err != nil {
		// Go error 的文本也会成为模型可见内容（agent 把它写进 tool 消息），所以要脱敏；
		// 但**不能**用 fmt.Errorf 切断错误身份（errors.Is/As 会失效），因此包一层
		// 保留 Unwrap。注意：Unwrap 拿到的是原始 error，宿主的错误处理若需要原文
		// 可以自行解包（模型不可见）。
		redacted := t.redactor.Redact(err.Error())
		if redacted == err.Error() {
			return result, err
		}
		return result, &redactedError{original: err, message: redacted}
	}
	return result, nil
}

// redactParts 只脱敏文本块，图片等非文本块原样保留（改写会破坏内容）。
func redactParts(redactor *secrets.Redactor, parts []rig.ContentPart) []rig.ContentPart {
	out := make([]rig.ContentPart, len(parts))
	copy(out, parts)
	for index := range out {
		if out[index].Type == rig.PartText {
			out[index].Text = redactor.Redact(out[index].Text)
		}
	}
	return out
}

// redactedError 保留原始错误以便 errors.Is/As 继续工作，同时对外暴露脱敏后的文本。
type redactedError struct {
	original error
	message  string
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.original }

// assembleSystemPrompt 按"base persona → <environment> → 指令文档"的顺序拼装。
//
// 顺序是有理由的：模型对更靠后的具体约束权重更高，而 AGENTS.md 属于**不可信
// 输入**（仓库作者可写），所以宿主自己的 system prompt 放在最前，块与文档在
// 其后、由宿主决定是否追加更严格的安全规则。
func assembleSystemPrompt(base string, environment *instructions.Environment, set *instructions.Set, root string) string {
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
	emitter  *events.Emitter
	redactor *secrets.Redactor
}

func newEventBridge(w io.Writer, redactor *secrets.Redactor) *eventBridge {
	if w == nil {
		return nil
	}
	return &eventBridge{emitter: events.NewEmitter(w), redactor: redactor}
}

// redact 对事件里的自由文本做脱敏。工具**入参**在 agent 层刻意不脱敏（模型可能
// 正当地把凭据交给工具），但事件流是日志/UI 通道，明文凭据不该出现在那里。
func (b *eventBridge) redact(text string) string {
	if b == nil || b.redactor == nil {
		return text
	}
	return b.redactor.Redact(text)
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
	if b == nil || b.emitter == nil {
		return
	}
	switch event.Type {
	case rig.EventTurnStart:
		b.emit(events.KindNotice, noticePayload{Message: fmt.Sprintf("turn %d started", event.Iteration)})
	case rig.EventTextDelta:
		b.emit(events.KindMessageDelta, events.MsgDelta{Role: "assistant", Text: b.redact(event.Text)})
	case rig.EventReasoningDelta:
		b.emit(events.KindReasoning, events.MsgDelta{Role: "assistant", Text: b.redact(event.Reasoning)})
	case rig.EventToolCall:
		if event.Call != nil {
			b.emit(events.KindToolCall, events.ToolCall{
				ID:        event.Call.ID,
				Name:      event.Call.Name,
				Arguments: b.redact(event.Call.Arguments),
			})
		}
	case rig.EventToolResult:
		if event.Result != nil {
			// rig.ToolResult 不带 call id/name，它们来自同一事件携带的调用。
			payload := events.ToolResult{Content: b.redact(event.Result.Content), IsError: event.Result.IsError}
			if event.Call != nil {
				payload.ID = event.Call.ID
				payload.Name = event.Call.Name
			}
			b.emit(events.KindToolResult, payload)
		}
	case rig.EventMessage:
		if event.Message != nil {
			b.emit(events.KindMessage, events.Message{
				Role:      string(event.Message.Role),
				Text:      b.redact(messageText(event.Message)),
				ToolCalls: len(event.Message.ToolCalls),
			})
		}
	case rig.EventUsage:
		if event.Usage != nil {
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
		b.emit(events.KindError, events.Error{Stage: "agent", Message: b.redact(message)})
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
