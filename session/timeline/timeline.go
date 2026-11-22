// Package timeline 把会话日志里的耐久工具事件回放到恢复后的消息历史上。
//
// Rig 的历史只记录"某个助手轮次发起了哪些工具调用"，不记录每个调用出现在
// 回复正文的哪个位置；耐久 tool_call 事件携带调用到达时的助手正文长度
// （text_offset），tool_result 事件携带结果状态。恢复时回放这两类事件，
// 让"工具卡片在正文之前"的轮次在重启后仍按原顺序渲染，而不是退回旧版的
// "整段正文然后卡片"顺序。
//
// 只应在历史确实带工具调用的会话上调用 Hydrate：事件读取的代价与工具记录
// 成正比；旧版本事件没有 text_offset 时，对应调用保持"位置未知"，由宿主
// 沿用旧的渲染回退。
package timeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
)

// 耐久会话事件类型。
const (
	// EventToolCall 是模型发起一次工具调用的事件。
	EventToolCall = "tool_call"
	// EventToolResult 是一次工具调用结束的事件。
	EventToolResult = "tool_result"
)

// EventLog 是回放所需的最小会话日志视图；rig.SessionLog、session.MemoryLog
// 与 session/sqlite 的事件视图都满足它。
type EventLog interface {
	// Events 返回 from <= seq <= to 的事件；to <= 0 表示直到末尾。
	Events(ctx context.Context, sessionID string, from, to int64) ([]rig.SessionEvent, error)
}

// File 是一个多文件调用（apply_patch 等）产生的文件条目。字段与耐久事件里的
// generated 载荷一致。
type File struct {
	// Path 是调用触碰的工作区相对路径。
	Path string `json:"path"`
	// Added 表示新建文件；Deleted 表示删除文件；两者都为 false 表示修改或移动。
	Added   bool `json:"added,omitempty"`
	Deleted bool `json:"deleted,omitempty"`
}

// Placement 是一个工具调用从耐久事件重建出来的时间线信息。
type Placement struct {
	// CallID 是事件的调用 id（已去掉首尾空白）。
	CallID string
	// TextOffset 是调用到达时助手的正文长度。指针有意为之：旧事件没有该
	// 字段，nil 表示"位置未知"，不能被误读成 0（0 是合法的"先调工具"）。
	TextOffset *int
	// Status 是 tool_result 事件记录的结果状态（"ok"/"error" 等）；没有结果
	// 事件时为空。
	Status string
	// Summary 是 tool_result 事件记录的（有界）结果正文。
	Summary string
	// ErrorKind 是事件里持久化的失败分类。旧事件没有该字段时保持为空；
	// 本包不做内容推断（宿主的分类器不在 rig 层）。
	ErrorKind string
	// Path 是文件类调用记录的工作区相对路径；非文件调用或旧事件为空。
	Path string
	// Generated 是多文件调用记录的文件列表；单文件调用与旧事件为空。
	Generated []File
	// StartedAt 是 tool_call 事件的时间；事件没有时间时为零值（未知）。
	StartedAt time.Time
	// EndedAt 是 tool_result 事件的时间；事件没有时间时为零值（未知）。
	EndedAt time.Time
}

// ToolPlacement 是一个已定位的工具调用：Placement 携带耐久事件里的元数据，
// Message/Tool 指向它在恢复后消息历史里的位置。
type ToolPlacement struct {
	Message int
	Tool    int
	Placement
}

// MessageTimeline 是一条恢复消息的工具时间线。Tools 与消息的 ToolCalls 按
// 相同下标一一对应；Known 为 true 表示该消息的每个调用都有显式 text_offset，
// 宿主可以按位置渲染，否则应沿用旧的整段回退顺序。
type MessageTimeline struct {
	// Message 是恢复历史（[]rig.Message）里的消息下标。
	Message int
	// Tools 按 ToolCalls 下标顺序排列。
	Tools []ToolPlacement
	// Known 表示该消息的工具时间线是否完整可知。
	Known bool
}

// toolCallEvent 是耐久 tool_call 事件的载荷。
type toolCallEvent struct {
	ID string `json:"id"`
	// Path 是文件类调用记录的工作区相对路径（来自未截断的参数）。
	Path string `json:"path"`
	// Generated 是多文件调用的文件列表（来自未截断的参数）。
	Generated []File `json:"generated,omitempty"`
	// TextOffset 是调用到达时助手的正文长度。指针：旧事件没有该字段，不能
	// 被当成记录下来的 0。
	TextOffset *int `json:"text_offset"`
}

// toolResultEvent 是耐久 tool_result 事件的载荷。
type toolResultEvent struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Content   string `json:"content"`
	ErrorKind string `json:"error_kind"`
}

// Hydrate 读取 sessionID 的耐久事件并折叠成按时间顺序排列的调用列表。
// 返回的 Placement 每个 tool_call 事件一项（调用 id 为空的除外），tool_result
// 事件按调用 id 合并到它前面的那次调用上。
//
// 日志读取失败时返回包装后的错误；其余坏数据（畸形载荷、孤儿结果、空 id）
// 被跳过，不影响同一份日志里的其他调用。sessionID 为空或 log 为 nil 时返回
// (nil, nil)。
func Hydrate(ctx context.Context, log EventLog, sessionID string) ([]Placement, error) {
	if log == nil {
		return nil, nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}
	events, err := log.Events(ctx, sessionID, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("timeline: 读取会话 %q 的事件: %w", sessionID, err)
	}
	return Fold(events), nil
}

// Fold 把一份按时间顺序排列的单会话事件折叠成调用列表。它不读日志，便于
// 已经持有事件的宿主复用；跨会话或乱序的事件不属于本函数的约定。
func Fold(events []rig.SessionEvent) []Placement {
	placements := make([]Placement, 0, 4)
	// pending 记录每个调用 id 尚未等到结果的 Placement 下标，队列保证
	// 重复 id（provider 按轮编号 "call_0"/"call_1"）按事件顺序配对。
	pending := make(map[string][]int)
	for _, event := range events {
		switch event.Type {
		case EventToolCall:
			var payload toolCallEvent
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				continue
			}
			callID := strings.TrimSpace(payload.ID)
			if callID == "" {
				continue
			}
			placement := Placement{
				CallID:    callID,
				Path:      strings.TrimSpace(payload.Path),
				Generated: append([]File(nil), payload.Generated...),
			}
			if payload.TextOffset != nil {
				offset := *payload.TextOffset
				placement.TextOffset = &offset
			}
			if !event.Time.IsZero() {
				placement.StartedAt = event.Time
			}
			placements = append(placements, placement)
			pending[callID] = append(pending[callID], len(placements)-1)
		case EventToolResult:
			var payload toolResultEvent
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				continue
			}
			callID := strings.TrimSpace(payload.ID)
			queue := pending[callID]
			if len(queue) == 0 {
				// 孤儿结果：没有可配对的前置调用，跳过。
				continue
			}
			index := queue[0]
			pending[callID] = queue[1:]
			placement := &placements[index]
			if status := strings.TrimSpace(payload.Status); status != "" {
				placement.Status = status
			}
			placement.Summary = payload.Content
			if kind := strings.TrimSpace(payload.ErrorKind); kind != "" {
				placement.ErrorKind = kind
			}
			if !event.Time.IsZero() {
				placement.EndedAt = event.Time
			}
		}
	}
	return placements
}

// Apply 把 Hydrate 的结果落回恢复后的消息历史。
//
// 只有事件条数与恢复历史条数一致的调用 id 才会回放：条数不一致说明日志里
// 还有当前历史不再包含的轮次（重新生成、回退或压缩掉的轮次），按 id 硬配
// 会把另一次运行的偏移贴到这次调用上，因此这些 id 整体保持未知。这同时
// 支持按轮复用 id 的 provider：两边都按时间顺序排队，逐条配对。
//
// 返回仅包含带工具调用的消息，按消息下标升序；每条消息的 Tools 与
// ToolCalls 下标一一对应。没有可回放事件的调用返回零值 Placement（CallID
// 已填），Known 为 false。
func Apply(messages []rig.Message, placements []Placement) []MessageTimeline {
	type restoredRef struct {
		message int
		tool    int
	}
	refs := make([]restoredRef, 0)
	queues := make(map[string][]int)
	for messageIndex := range messages {
		for toolIndex := range messages[messageIndex].ToolCalls {
			callID := strings.TrimSpace(messages[messageIndex].ToolCalls[toolIndex].ID)
			if callID == "" {
				continue
			}
			refs = append(refs, restoredRef{message: messageIndex, tool: toolIndex})
			queues[callID] = append(queues[callID], len(refs)-1)
		}
	}
	if len(refs) == 0 {
		return nil
	}

	placementQueues := make(map[string][]int, len(placements))
	for index := range placements {
		callID := strings.TrimSpace(placements[index].CallID)
		if callID == "" {
			continue
		}
		placementQueues[callID] = append(placementQueues[callID], index)
	}
	eligible := make(map[string]bool, len(queues))
	for callID, queue := range queues {
		if len(placementQueues[callID]) == len(queue) {
			eligible[callID] = true
		}
	}

	hydrated := make(map[restoredRef]Placement, len(refs))
	cursor := make(map[string]int, len(queues))
	for _, ref := range refs {
		callID := strings.TrimSpace(messages[ref.message].ToolCalls[ref.tool].ID)
		if !eligible[callID] {
			continue
		}
		queue := placementQueues[callID]
		next := cursor[callID]
		if next >= len(queue) {
			continue
		}
		cursor[callID] = next + 1
		hydrated[ref] = placements[queue[next]]
	}

	// timed 统计每条消息有显式偏移的调用数；全部都有时时间线才"已知"。
	timed := make([]int, len(messages))
	for _, ref := range refs {
		if placement, ok := hydrated[ref]; ok && placement.TextOffset != nil {
			timed[ref.message]++
		}
	}

	timelines := make([]MessageTimeline, 0, len(messages))
	for messageIndex := range messages {
		calls := messages[messageIndex].ToolCalls
		if len(calls) == 0 {
			continue
		}
		tools := make([]ToolPlacement, 0, len(calls))
		for toolIndex := range calls {
			ref := restoredRef{message: messageIndex, tool: toolIndex}
			placement := hydrated[ref]
			// 事件 id 已由 Hydrate 归一化；手写 placements 的宿主同样得到
			// 与恢复历史一致的 id。
			placement.CallID = strings.TrimSpace(placement.CallID)
			if placement.CallID == "" {
				placement.CallID = strings.TrimSpace(calls[toolIndex].ID)
			}
			tools = append(tools, ToolPlacement{
				Message:   messageIndex,
				Tool:      toolIndex,
				Placement: placement,
			})
		}
		timelines = append(timelines, MessageTimeline{
			Message: messageIndex,
			Tools:   tools,
			Known:   timed[messageIndex] == len(calls),
		})
	}
	return timelines
}
