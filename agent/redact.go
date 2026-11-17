package agent

// 本文件是 agent 层的脱敏收口（对应 secrets 包审计报告里的"接线层还缺什么"第一条）。
//
// 脱敏点选在"输出离开 agent"的位置，而不是"输入进入 agent"的位置，理由：
//
//   - 工具结果：在写入对话历史、落盘（History.Append）、spill 到文件、发事件之前
//     就脱敏，因此模型、历史、上下文压缩、session store 与 UI 都看不到明文。
//   - 助手文本与推理：模型可能复述它读到的凭据，因此返回前脱敏。
//   - 事件：Emit 是 UI/日志通道，所有事件在交给宿主之前脱敏（ToolResult 的内容、
//     完整消息、错误文本）。
//   - 错误：Run 返回的错误文本同样会进 UI/日志，因此统一包装成脱敏文本，同时保留
//     原始错误的包装链（errors.Is/As 仍然可用）。
//
// 刻意**不**脱敏的地方（见 RedactMessages 的文档）：工具调用入参 ToolCall.Arguments、
// 图片分片（ImageURL / data URL）、以及用户自己输入的 RunRequest.Input。
//
// 已知边界：EventTextDelta/EventReasoningDelta 是**逐 chunk** 脱敏的。若 provider
// 把一个凭据拆到两个增量里（"sk-abc" + "def..."），单个增量内的片段不会命中模式，
// 流式 UI 可能短暂看到它；但累积后的助手消息、历史、EventMessage 与 RunResult 仍然
// 是完整脱敏的。要做到增量通道零泄露，需要像 secrets.Writer 那样按行扣留——那会推迟
// 最后一个未完成行的显示，属于 UX 取舍，由宿主自行决定（可以用 secrets.Writer 包
// 自己的 UI 通道）。

import (
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/secrets"
)

// NewWithRedactor 与 New 完全相同，只是额外把流经 agent 的文本交给 redactor 脱敏。
//
// 为什么不是 AgentSpec 上的字段：rig.AgentSpec 在模块根包，而根包的契约是"仅标准库
// 且是 import 图的叶子"（README 的依赖策略），不能导入本仓库的 secrets 子包。因此
// 脱敏器通过这个构造函数注入；redactor 为 nil 时与 New 逐字节等价（不增加任何开销）。
//
// 子 agent 的继承：NewRunner 的子 agent 默认不继承脱敏器，需要显式配置
// RunnerOptions.Redactor（见 subagent.go）。
func NewWithRedactor(spec rig.AgentSpec, redactor *secrets.Redactor) (rig.Agent, error) {
	return newAgent(spec, redactor)
}

// RedactMessages 对一组消息做原地脱敏：改写每个文本分片（Type == PartText）的
// Text 与 Reasoning，其余字段原样保留。redactor 为 nil 时是 no-op（零开销）。
//
// **刻意不脱敏 ToolCall.Arguments**：模型可能正当地把凭据"交给"工具——写 .env、
// 带 Authorization 的 web_fetch、给 MCP server 的 key。把入参抹掉会直接破坏这些
// 功能，而且入参本来就要发给工具而不是回显给用户。同理，图片分片（ImageURL，
// 含 data URL）不脱敏，否则图片会被改坏。
//
// 幂等：已经是占位符的文本再次脱敏不会变化（secrets.Redactor 的保证），因此对
// 已脱敏的历史重复调用是安全的。改写只在确有变化时发生：没有秘密的文本不会复制
// 分片数组（调用方持有的旧切片不会被就地改写）。
func RedactMessages(redactor *secrets.Redactor, messages []rig.Message) {
	if redactor == nil {
		return
	}
	for i := range messages {
		redactMessageInPlace(redactor, &messages[i])
	}
}

// redactMessageInPlace 脱敏单条消息的文本分片与推理（写时复制分片数组）。
func redactMessageInPlace(redactor *secrets.Redactor, msg *rig.Message) {
	if redactor == nil || msg == nil {
		return
	}
	var cloned bool
	for i := range msg.Content {
		part := &msg.Content[i]
		if part.Type != rig.PartText || part.Text == "" {
			continue
		}
		redacted := redactor.Redact(part.Text)
		if redacted == part.Text {
			continue
		}
		if !cloned {
			msg.Content = append([]rig.ContentPart(nil), msg.Content...)
			cloned = true
			part = &msg.Content[i]
		}
		part.Text = redacted
	}
	if msg.Reasoning != "" {
		if redacted := redactor.Redact(msg.Reasoning); redacted != msg.Reasoning {
			msg.Reasoning = redacted
		}
	}
	// msg.ToolCalls 的 Arguments 刻意保留原文（理由见 RedactMessages）。
}

// redactResultInPlace 脱敏一次工具结果的 Content 与文本分片。IsError 与分片结构
// （数量、顺序、图片分片）保持不变。
func redactResultInPlace(redactor *secrets.Redactor, result *rig.ToolResult) {
	if redactor == nil || result == nil {
		return
	}
	if result.Content != "" {
		if redacted := redactor.Redact(result.Content); redacted != result.Content {
			result.Content = redacted
		}
	}
	var cloned bool
	for i := range result.Parts {
		part := &result.Parts[i]
		if part.Type != rig.PartText || part.Text == "" {
			continue
		}
		redacted := redactor.Redact(part.Text)
		if redacted == part.Text {
			continue
		}
		if !cloned {
			result.Parts = append([]rig.ContentPart(nil), result.Parts...)
			cloned = true
			part = &result.Parts[i]
		}
		part.Text = redacted
	}
}

// redactText 是 Redact 的 nil 安全包装（redactor 为 nil 时原样返回）。
func redactText(redactor *secrets.Redactor, text string) string {
	if redactor == nil || text == "" {
		return text
	}
	return redactor.Redact(text)
}

// redactEvent 返回脱敏后的事件副本：不修改入参指针指向的对象（事件可能被宿主缓存
// 或复用），因此这里先复制 ToolResult/Message 再改写。
func redactEvent(redactor *secrets.Redactor, event rig.Event) rig.Event {
	if redactor == nil {
		return event
	}
	if event.Text != "" {
		event.Text = redactText(redactor, event.Text)
	}
	if event.Reasoning != "" {
		event.Reasoning = redactText(redactor, event.Reasoning)
	}
	if event.Result != nil {
		result := *event.Result
		redactResultInPlace(redactor, &result)
		event.Result = &result
	}
	if event.Message != nil {
		message := *event.Message
		redactMessageInPlace(redactor, &message)
		event.Message = &message
	}
	// event.Call 不脱敏：工具入参要原样交给工具（见 RedactMessages 的说明）。
	if event.Err != nil {
		event.Err = redactError(redactor, event.Err)
	}
	return event
}

// redactedError 只改写错误的展示文本，保留原始错误的包装链：errors.Is / errors.As
// 仍然能定位到哨兵错误（例如 context.Canceled、provider 的自定义错误）。
type redactedError struct {
	message string
	err     error
}

func (e *redactedError) Error() string { return e.message }

func (e *redactedError) Unwrap() error { return e.err }

// redactError 返回脱敏后的错误；没有变化时返回原错误（保持错误身份与零开销）。
func redactError(redactor *secrets.Redactor, err error) error {
	if redactor == nil || err == nil {
		return err
	}
	original := err.Error()
	redacted := redactor.Redact(original)
	if redacted == original {
		return err
	}
	return &redactedError{message: redacted, err: err}
}

// 下面是 run 上的薄包装，让调用点读起来短一些。

func (r *run) redactMessage(msg *rig.Message) { redactMessageInPlace(r.a.redactor, msg) }

func (r *run) redactResult(result *rig.ToolResult) { redactResultInPlace(r.a.redactor, result) }

// redactNotices 脱敏一组完成通知（它们会作为一条 user 消息进入历史）。
func (r *run) redactNotices(notices []string) []string {
	if r.a.redactor == nil || len(notices) == 0 {
		return notices
	}
	out := make([]string, len(notices))
	for i, notice := range notices {
		out[i] = strings.TrimSpace(notice)
		redacted := r.a.redactor.Redact(out[i])
		if redacted != out[i] {
			out[i] = redacted
		}
	}
	return out
}
