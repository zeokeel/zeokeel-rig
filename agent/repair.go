package agent

// 历史修复：把"不合法的工具消息序列"在进入请求之前修好。
//
// 为什么必须修：OpenAI 兼容端点对 chat-completions 的消息序列有一硬约束——
// role=tool 的消息**必须**紧跟在带对应 tool_calls 的 assistant 消息之后。历史里
// 一旦出现孤立 tool 消息（进程崩在工具执行中途、跨端同步、人工编辑、旧版本写出
// 的坏数据），该会话的**每一次**请求都会 400：
//
//	messages with role 'tool' must be a response to a preceding message with 'tool_calls'
//
// 用户看到一条看不懂的英文错误，而且这个会话从此打不开。checkpoint 恢复路径本来
// 就会合成缺失的工具结果，但普通 loadHistory 不会——这里补上。

import (
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// RepairInterruptedToolResult 是 repairConversation 为"没人回复的调用"合成的
// 结果内容（模型可见，告诉它当时发生了什么）。
const RepairInterruptedToolResult = "[tool result unavailable: the session was interrupted before this call finished]"

// repairConversation 返回一份"可以直接发给 provider"的历史：
//
//   - 丢弃没有对应 assistant tool_calls 的 tool 消息（孤立结果）；
//   - 丢弃重复的工具结果（同一 ToolCallID 只保留第一条）；
//   - 为没人回复的 tool_calls 合成一条失败结果，使 assistant 消息不再悬空；
//   - 丢弃空 ID 的工具调用（无法配对，provider 会拒绝）。
//
// 它不修改入参，也不改变"正常消息"的相对顺序与内容；只有原本就非法的部分被
// 丢弃或补齐。返回的切片是新的。
func repairConversation(messages []rig.Message) []rig.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]rig.Message, 0, len(messages)+4)
	// pending 是"已经发出、还没拿到结果"的工具调用（按出现顺序）。
	pending := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)

	flush := func() {
		for _, id := range pending {
			if strings.TrimSpace(id) == "" {
				continue
			}
			out = append(out, rig.Message{
				Role:       rig.RoleTool,
				ToolCallID: id,
				Content:    []rig.ContentPart{rig.TextPart(RepairInterruptedToolResult)},
			})
		}
		pending = pending[:0]
	}

	for _, message := range messages {
		clone := cloneMessage(message)
		switch clone.Role {
		case rig.RoleTool:
			id := strings.TrimSpace(clone.ToolCallID)
			if id == "" {
				continue // 无法配对的结果只能丢
			}
			if _, ok := seen[id]; ok {
				continue // 同一次调用的重复结果
			}
			if !containsCall(pending, id) {
				continue // 孤立工具结果：没有发起过这次调用
			}
			delete(seen, id)
			pending = removeCall(pending, id)
			out = append(out, clone)
		case rig.RoleAssistant:
			// 悬空的上一批调用先补齐，否则这条 assistant 消息会把它们挤到后面。
			flush()
			calls := clone.ToolCalls[:0:0]
			for _, call := range clone.ToolCalls {
				id := strings.TrimSpace(call.ID)
				if id == "" {
					continue // 没有 ID 的调用无法配对，直接丢
				}
				call.ID = id
				calls = append(calls, call)
				pending = append(pending, id)
				seen[id] = struct{}{}
			}
			clone.ToolCalls = calls
			out = append(out, clone)
		default:
			flush()
			out = append(out, clone)
		}
	}
	flush()
	return out
}

// containsCall 报告 pending 里是否有 id。
func containsCall(pending []string, id string) bool {
	for _, candidate := range pending {
		if candidate == id {
			return true
		}
	}
	return false
}

// removeCall 返回去掉了 id 的新切片（保持顺序）。
func removeCall(pending []string, id string) []string {
	for index, candidate := range pending {
		if candidate != id {
			continue
		}
		out := make([]string, 0, len(pending)-1)
		out = append(out, pending[:index]...)
		return append(out, pending[index+1:]...)
	}
	return pending
}

// repairSummary 返回便于日志的一行摘要（诊断用）。
func repairSummary(before, after int) string {
	if before == after {
		return ""
	}
	return fmt.Sprintf("repaired conversation: %d -> %d message(s)", before, after)
}
