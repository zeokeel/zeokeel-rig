// Package session 提供 ZeoKeel-Rig 的内存会话状态。
//
// Memory 是 rig.History 的参考实现：一个并发安全、按会话隔离的消息存储，适合
// 测试、无头运行以及不需要持久化的单进程宿主。持久化实现（SQLite/JSONL 等）位于
// SDK 之外，只需实现同一套 rig.History 契约即可替换。
//
// # 主要类型
//
//   - Memory：内存会话历史，按 sessionID 保存 rig.Message 列表。
//   - rig.History：会话历史契约，Load 读取、Append 追加；所有实现都必须并发安全。
//   - rig.Message：会话消息（system/user/assistant/tool），agent 循环每轮都会向
//     History 追加助手消息与工具结果。
//
// # 典型用法
//
// 把 Memory 作为 agent 的 History，即可让同一个 sessionID 在多轮 Run 之间保留上下文：
//
//	import (
//		"context"
//
//		"github.com/zeokeel/zeokeel-rig"
//		"github.com/zeokeel/zeokeel-rig/agent"
//		"github.com/zeokeel/zeokeel-rig/session"
//	)
//
//	history := session.NewMemory()
//	a, err := agent.New(rig.AgentSpec{
//		Provider: provider, // 已构造好的 rig.Provider，例如 openai.Provider
//		Model:    "gpt-4o-mini",
//		History:  history,
//	})
//	if err != nil {
//		return err
//	}
//
//	// 相同 SessionID 的两次 Run 共享历史；不同 SessionID 相互隔离。
//	_, err = a.Run(ctx, rig.RunRequest{
//		SessionID: "s1",
//		Input:     []rig.Message{rig.NewMessage(rig.RoleUser, "第一条消息")},
//	})
//	msgs, err := history.Load(context.Background(), "s1")
//
// # 不变量与语义
//
//   - Memory 的零值不可用，必须用 NewMemory 构造；所有方法都并发安全。
//   - Load 返回副本：外层切片以及每条消息的 Content/ToolCalls 切片都是深拷贝，
//     调用方修改返回值不会影响已存储的历史。
//   - Append 以一次调用为原子单位：并发读者只会看到“全部消息”或“一条都没有”；
//     传入的消息在存储前同样会被深拷贝。
//   - 未知会话的 Load 返回 (nil, nil)，符合 rig.History 契约；Len 对未知会话返回 0，
//     与空会话不可区分，仅供测试与诊断使用，不要用于协议判断。
//   - 传入已取消/已过期的 context 时，Load/Append 返回 ctx.Err()；同时实现会容忍
//     nil context（按未取消处理），避免宿主误传 nil 导致 panic。
//   - 空 sessionID 是合法的，会被当作一个普通会话键保存；是否允许空会话由上层
//     （例如 rig.RunRequest 的约定）决定。
//
// 与 agent 循环的关系：agent 在 Run 开始时 Load 历史、把输入与每轮助手消息、工具
// 结果 Append 回去；Memory 只负责存储，不参与消息裁剪或截断。
package session

import (
	"context"
	"slices"
	"sync"

	"github.com/zeokeel/zeokeel-rig"
)

// Memory 是内存版 rig.History。零值不可用，请用 NewMemory 构造。
// 所有方法都并发安全。
type Memory struct {
	mu       sync.RWMutex
	sessions map[string][]rig.Message
}

// NewMemory 返回一个空的内存历史。
func NewMemory() *Memory {
	return &Memory{sessions: make(map[string][]rig.Message)}
}

// Load 返回 sessionID 已存储消息的副本。未知会话返回 (nil, nil)，符合
// rig.History 契约；返回的切片以及其中消息的 Content/ToolCalls 切片都是副本，
// 修改它们不会影响已存储的历史。
func (m *Memory) Load(ctx context.Context, sessionID string) ([]rig.Message, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	stored := m.sessions[sessionID]
	if len(stored) == 0 {
		return nil, nil
	}
	return cloneMessages(stored), nil
}

// Append 以原子方式向 sessionID 追加消息：并发读者要么看不到、要么看到一次调用
// 的全部消息。消息在存储前会被复制，因此调用方之后修改自己的切片不会改变历史。
func (m *Memory) Append(ctx context.Context, sessionID string, messages ...rig.Message) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if len(messages) == 0 {
		return nil
	}
	copied := cloneMessages(messages)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[sessionID] = append(m.sessions[sessionID], copied...)
	return nil
}

// Replace 原子替换 sessionID 的全部消息（实现 rig.HistoryReplacer）。空 messages
// 等价于清空该会话；消息在存储前会被复制。
func (m *Memory) Replace(ctx context.Context, sessionID string, messages []rig.Message) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	copied := cloneMessages(messages)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(copied) == 0 {
		delete(m.sessions, sessionID)
		return nil
	}
	m.sessions[sessionID] = copied
	return nil
}

// Len 报告 sessionID 存储的消息条数。未知会话的 0 与空会话不可区分；它面向
// 测试与诊断，不应用于协议决策。
func (m *Memory) Len(sessionID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions[sessionID])
}

// contextErr 返回已取消或已过期的 context 错误，同时不要求调用方一定传入非 nil
// context（nil context 调用 Err 会 panic，这里做防御：rig.History 契约总会传入真实
// context，但防御代码可避免测试里的零值调用 panic）。
func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// cloneMessages 深拷贝消息切片以及每条消息的 Content/ToolCalls 切片
// （ContentPart/ToolCall 内部的字符串是不可变值，无需单独拷贝）。
func cloneMessages(messages []rig.Message) []rig.Message {
	out := make([]rig.Message, len(messages))
	for i, msg := range messages {
		out[i] = msg
		if msg.Content != nil {
			out[i].Content = slices.Clone(msg.Content)
		}
		if msg.ToolCalls != nil {
			out[i].ToolCalls = slices.Clone(msg.ToolCalls)
		}
	}
	return out
}
