// Package observability 把上下文可观测性的持久事件折叠成一份有界的会话读模型。
//
// 宿主在跑一次 agent 时记录四类持久事件（payload 是 JSON，无需 schema 迁移）：
//
//	context_snapshot 每次 run 一条，在第一次 provider 调用前：模型/窗口、system
//	                 prompt 大小、工具目录、历史消息计数、可选的注入项。
//	model_call       每次 provider 调用一条：迭代序号、模型、prompt/cached/
//	                 completion/total，以及这次调用真实看到的上下文 token。
//	compact          手动或自动压缩的结果（before/after/replaced/saved），
//	                 inferred 标记启发式推断出的自动压缩。
//	spill            工具结果被落盘摘要替换（path/ref/bytes/preview）。
//
// 折叠结果是 Timeline：最近快照、有界的调用列表、压缩与 spill 审计列表、token
// 汇总、当前上下文占用（Used）与窗口。本包只依赖根包 rig 与标准库，不 import
// 任何产品/后端模块，因此宿主既可以拿 payload 结构体写事件，也可以独立读回并
// 折叠同一份日志，不必把 UI 或产品模型带进 SDK。
//
// 折叠严格按事件在日志中的顺序（旧→新）进行，是纯函数：同样的输入永远得到同样
// 的输出（事件时间原样携带，不用 time.Now() 兜底，便于测试与对比）。未知事件
// 类型、空或 null payload、无法解码或字段类型不匹配的 payload 一律跳过并计入
// Timeline.Skipped，不返回错误，也不影响其余事件的折叠。
//
// # 与产品侧折叠的差异
//
// 本包不查模型目录：不计算 CostUSD，也不能用目录信息覆盖上下文窗口（窗口只来自
// 快照 payload）。产品侧的 compact_archive / 恢复事件不在这四类里，会被当成未知
// 事件跳过；agent 内部的自动压缩不可直接观测，宿主可在两次调用的 prompt token
// 骤降时用 AutoCompactDrop 判定并补记一条 Inferred 的 compact 事件。工具分组由
// payload 的 Group 字段提供，本包不按工具名推断分组。
package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
)

// 持久事件类型。字符串与事件日志里的类型名一一对应，宿主写事件时直接引用，
// 读回折叠时也按它分派。
const (
	// EventContextSnapshot 是每次 run 的请求级快照事件。
	EventContextSnapshot = "context_snapshot"
	// EventModelCall 是每次 provider 调用的用量事件。
	EventModelCall = "model_call"
	// EventCompact 是一次压缩（手动或自动）的结果事件。
	EventCompact = "compact"
	// EventSpill 是一条工具结果被落盘摘要替换的事件。
	EventSpill = "spill"
)

const (
	// DefaultWindow 是 Fold 在 window<=0 时读取的事件条数上限。
	DefaultWindow = 4000
	// MaxCalls 是折叠保留的 model_call 条数上限（保留最新的）。
	MaxCalls = 200
	// MaxEvents 是压缩与 spill 审计列表各自的条数上限（保留最新的）。
	MaxEvents = 200
	// MaxPreviewRunes 是折叠后保留的 spill 预览 rune 上限。
	MaxPreviewRunes = 240
	// AutoCompactMinTokens 是能触发自动压缩推断的最小上下文：更小的上下文上
	// 的 token 波动是噪声，不算压缩。
	AutoCompactMinTokens = 4096
	// AutoCompactDropPercent 是判定自动压缩的 prompt token 降幅（相对上一次的
	// 百分比）。
	AutoCompactDropPercent = 20
)

// EventLog 是折叠所需的会话日志切片；session/store、session/sqlstore 等实现都
// 满足它。宿主也可以只传一个薄包装，不需要依赖完整的 rig.SessionLog。
type EventLog interface {
	// Last 返回某个会话最近的若干事件（旧→新）。
	Last(ctx context.Context, sessionID string, limit int) ([]rig.SessionEvent, error)
}

// Fold 从会话日志读回最近 window 条事件并折叠成 Timeline。window<=0 时用
// DefaultWindow。log 为 nil 或 sessionID 为空时返回零值 Timeline 且不报错；
// 日志读取失败时错误原样返回。
func Fold(ctx context.Context, log EventLog, sessionID string, window int) (Timeline, error) {
	if log == nil {
		return Timeline{}, nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Timeline{}, nil
	}
	if window <= 0 {
		window = DefaultWindow
	}
	events, err := log.Last(ctx, sessionID, window)
	if err != nil {
		return Timeline{}, err
	}
	return FoldEvents(events, sessionID), nil
}

// FoldEvents 折叠一个会话的事件（按日志顺序，旧→新）。SessionID 为空的匿名
// 事件也会被折叠；其它会话的事件被忽略，且不计入 Skipped。
func FoldEvents(events []rig.SessionEvent, sessionID string) Timeline {
	var timeline Timeline
	for _, event := range events {
		if event.SessionID != "" && event.SessionID != sessionID {
			continue
		}
		if !timeline.foldEvent(event) {
			timeline.Skipped++
		}
	}
	return timeline
}

// ObservedEvent 报告一个事件类型是否参与折叠。宿主回放日志时可先用它过滤，
// 结果与 FoldEvents 的分派一致。
func ObservedEvent(eventType string) bool {
	switch eventType {
	case EventContextSnapshot, EventModelCall, EventCompact, EventSpill:
		return true
	default:
		return false
	}
}

// foldEvent 把一条事件折进 timeline；返回 false 表示事件被跳过（未知类型或
// payload 不可用）。被跳过的事件不增加 Revision。
func (t *Timeline) foldEvent(event rig.SessionEvent) bool {
	switch event.Type {
	case EventContextSnapshot:
		var payload ContextSnapshotPayload
		if !decodePayload(event.Payload, &payload) {
			return false
		}
		snapshot := Snapshot{
			Seq:                event.Seq,
			At:                 event.Time,
			Model:              strings.TrimSpace(payload.Model),
			Window:             nonNegative(payload.Window),
			SystemPromptBytes:  nonNegative(payload.SystemPromptBytes),
			SystemPromptTokens: nonNegative(payload.SystemPromptTokens),
			Sections:           sanitizeSections(payload.Sections),
			Tools:              sanitizeTools(payload.Tools),
			ToolCount:          nonNegative(payload.ToolCount),
			History:            sanitizeHistory(payload.History),
			Injected:           payload.Injected,
			EstimatedPrompt:    nonNegative(payload.EstimatedPrompt),
		}
		if count := len(snapshot.Tools); count > snapshot.ToolCount {
			snapshot.ToolCount = count
		}
		t.Snapshot = snapshot
		t.HasSnapshot = true
		if snapshot.Window > 0 {
			t.Window = snapshot.Window
		}
		// 快照的估算只在该会话还没有真实调用记录时代表"当前占用"；之后的
		// run 再发快照也不覆盖真实测量值。
		if t.CallsSeen == 0 && snapshot.EstimatedPrompt > 0 {
			t.Used = snapshot.EstimatedPrompt
		}
		t.ToolCalls = snapshot.History.Tool
	case EventModelCall:
		var payload ModelCallPayload
		if !decodePayload(event.Payload, &payload) {
			return false
		}
		call := Call{
			Seq:           event.Seq,
			At:            event.Time,
			Iteration:     nonNegative(payload.Iteration),
			Model:         strings.TrimSpace(payload.Model),
			Prompt:        nonNegative(payload.PromptTokens),
			Cached:        nonNegative(payload.CachedTokens),
			Completion:    nonNegative(payload.CompletionTokens),
			Total:         nonNegative(payload.TotalTokens),
			ContextTokens: nonNegative(payload.ContextTokens),
		}
		t.Tokens.Prompt += call.Prompt
		t.Tokens.Cached += call.Cached
		t.Tokens.Completion += call.Completion
		t.Tokens.Total += call.Total
		t.Calls = append(t.Calls, call)
		t.CallsSeen++
		if len(t.Calls) > MaxCalls {
			t.Calls = append([]Call(nil), t.Calls[len(t.Calls)-MaxCalls:]...)
		}
		if call.Iteration > t.Iterations {
			t.Iterations = call.Iteration
		}
		if call.ContextTokens > 0 {
			t.Used = call.ContextTokens
		}
	case EventCompact:
		var payload CompactPayload
		if !decodePayload(event.Payload, &payload) {
			return false
		}
		kind := strings.TrimSpace(payload.Kind)
		if kind == "" {
			kind = "manual"
		}
		compaction := Compaction{
			Seq:      event.Seq,
			At:       event.Time,
			Kind:     kind,
			Before:   nonNegative(payload.Before),
			After:    nonNegative(payload.After),
			Replaced: nonNegative(payload.Replaced),
			Saved:    nonNegative(payload.Saved),
			Inferred: payload.Inferred,
			Outcome:  strings.TrimSpace(payload.Outcome),
		}
		t.Compactions = append(t.Compactions, compaction)
		t.CompactionsSeen++
		if len(t.Compactions) > MaxEvents {
			t.Compactions = append([]Compaction(nil), t.Compactions[len(t.Compactions)-MaxEvents:]...)
		}
		if compaction.After > 0 {
			t.Used = compaction.After
		}
	case EventSpill:
		var payload SpillPayload
		if !decodePayload(event.Payload, &payload) {
			return false
		}
		spill := Spill{
			Seq:     event.Seq,
			At:      event.Time,
			CallID:  strings.TrimSpace(payload.CallID),
			Tool:    strings.TrimSpace(payload.Tool),
			Path:    strings.TrimSpace(payload.Path),
			Ref:     strings.TrimSpace(payload.Ref),
			Bytes:   nonNegative(payload.Bytes),
			Preview: truncateRunes(payload.Preview, MaxPreviewRunes),
		}
		t.Spills = append(t.Spills, spill)
		t.SpillsSeen++
		if len(t.Spills) > MaxEvents {
			t.Spills = append([]Spill(nil), t.Spills[len(t.Spills)-MaxEvents:]...)
		}
	default:
		return false
	}
	t.Revision++
	return true
}

// ParseSpillSummary 从一条已被落盘摘要替换的工具结果里解析出结构化字段：
//
//	[tool result spilled to file]
//	path: <path>      （文件后端）  或  ref: <handle>  （store 后端）
//	bytes: <n>
//	preview: <一行>
//
// 返回值只填 Path/Ref/Bytes/Preview（CallID、Tool、时间由宿主补）。内容不是
// spill 摘要，或摘要里没有任何可用字段时返回 false。
func ParseSpillSummary(content string) (Spill, bool) {
	if !rig.IsSpilledToolResult(content) {
		return Spill{}, false
	}
	var parsed Spill
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "path:"):
			parsed.Path = strings.TrimSpace(strings.TrimPrefix(line, "path:"))
		case strings.HasPrefix(line, "ref:"):
			parsed.Ref = strings.TrimSpace(strings.TrimPrefix(line, "ref:"))
		case strings.HasPrefix(line, "bytes:"):
			if value, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "bytes:"))); err == nil {
				parsed.Bytes = nonNegative(value)
			}
		case strings.HasPrefix(line, "preview:"):
			parsed.Preview = strings.TrimSpace(strings.TrimPrefix(line, "preview:"))
		}
	}
	if parsed.Path == "" && parsed.Ref == "" && parsed.Bytes == 0 {
		return Spill{}, false
	}
	return parsed, true
}

// AutoCompactDrop 报告两次相邻 provider 调用的 prompt token 数是否像一次自动
// 压缩：上下文足够大（≥AutoCompactMinTokens）且降幅至少 AutoCompactDropPercent。
// agent 循环内部的自动压缩不可直接观测，宿主只能在观察到骤降时记一条
// Inferred 的 compact 事件；本函数就是那个判定。
func AutoCompactDrop(previous, current int) bool {
	if previous < AutoCompactMinTokens || current <= 0 || current >= previous {
		return false
	}
	return (previous-current)*100 >= previous*AutoCompactDropPercent
}

// decodePayload 解码一条事件 payload。空 payload 与 JSON null 都视为不可用
// （null 会把结构体解成零值，不能当成有效事件），类型不匹配同样返回 false。
func decodePayload(payload json.RawMessage, into any) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	return json.Unmarshal(trimmed, into) == nil
}

// nonNegative 把荒谬的负数（payload 被写坏）压回 0，避免污染求和与占用计算。
func nonNegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

// truncateRunes 把文本截到 limit 个 rune（limit<=0 表示不截）。
func truncateRunes(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit])
}

// sanitizeSections 复制快照分段并压掉负数字段，同时让折叠结果不共享 payload
// 的底层数组。
func sanitizeSections(sections []Section) []Section {
	if len(sections) == 0 {
		return nil
	}
	cleaned := make([]Section, len(sections))
	for index, section := range sections {
		section.Bytes = nonNegative(section.Bytes)
		section.Tokens = nonNegative(section.Tokens)
		cleaned[index] = section
	}
	return cleaned
}

// sanitizeTools 复制工具目录并压掉负数的 token 估算。
func sanitizeTools(tools []Tool) []Tool {
	if len(tools) == 0 {
		return nil
	}
	cleaned := make([]Tool, len(tools))
	for index, tool := range tools {
		tool.Tokens = nonNegative(tool.Tokens)
		cleaned[index] = tool
	}
	return cleaned
}

// sanitizeHistory 压掉历史计数里的负数。
func sanitizeHistory(history History) History {
	history.Messages = nonNegative(history.Messages)
	history.User = nonNegative(history.User)
	history.Assistant = nonNegative(history.Assistant)
	history.Tool = nonNegative(history.Tool)
	return history
}
