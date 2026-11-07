package rig

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CheckpointPhase 标识检查点被写入时回合所处的阶段。agent 在三个 barrier 上
// 写入检查点：模型请求发出前、有副作用的工具执行前、回合结束。前两个阶段是
// 非终结阶段；进程在回合中途退出时，它们就是恢复中断回合的依据。
type CheckpointPhase string

const (
	// CheckpointPhaseCompleted 表示一个已正常结束（含达到轮数上限等已记录终态）
	// 的回合。completed 检查点不会再触发中断恢复。
	CheckpointPhaseCompleted CheckpointPhase = "completed"
	// CheckpointPhaseRequested 表示模型请求即将/已经交给 provider，但助手响应
	// 尚未完成。此时崩溃会丢失模型响应，检查点提供恢复所需的对话尾部。
	CheckpointPhaseRequested CheckpointPhase = "requested"
	// CheckpointPhaseToolPending 表示助手已请求工具调用、但顶层有副作用工具的
	// 结果尚未记录。恢复时必须为该调用补一条“结果未知、未重放”的合成结果，
	// 绝不能重放副作用。
	CheckpointPhaseToolPending CheckpointPhase = "tool_pending"
)

// Valid 报告 p 是否是 Rig 已定义的阶段值。
func (p CheckpointPhase) Valid() bool {
	switch p {
	case CheckpointPhaseCompleted, CheckpointPhaseRequested, CheckpointPhaseToolPending:
		return true
	default:
		return false
	}
}

// Interrupted 报告 p 是否代表一个尚未终结、需要恢复的回合阶段。只有
// requested 与 tool_pending 是非终结阶段；completed、空值与未知值都不算中断。
func (p CheckpointPhase) Interrupted() bool {
	return p == CheckpointPhaseRequested || p == CheckpointPhaseToolPending
}

// 检查点契约的哨兵错误。调用方可以用 errors.Is 判断；实现方应包装而不是替换
// 它们（与 rig 其他哨兵错误一致）。
var (
	// ErrCheckpointInvalid 表示检查点自身不完整或取值越界（例如 PriorMessages
	// 超出消息条数范围）。
	ErrCheckpointInvalid = errors.New("rig: invalid checkpoint")
	// ErrCheckpointSessionRequired 表示检查点缺少会话 ID。
	ErrCheckpointSessionRequired = errors.New("rig: checkpoint session id is required")
	// ErrCheckpointPhaseInvalid 表示检查点的 Phase 不是已定义阶段。
	ErrCheckpointPhaseInvalid = errors.New("rig: checkpoint phase is invalid")
	// ErrCheckpointTurnRequired 表示非终结阶段缺少 TurnID；没有 TurnID 就无法
	// 把中断修复归属到具体回合。
	ErrCheckpointTurnRequired = errors.New("rig: checkpoint turn id is required for an interrupted phase")
)

// Checkpoint 是一个会话在回合边界上的持久化恢复状态。检查点用 encoding/json
// 可直接序列化，实现 CheckpointStore 的宿主可以把 Messages 原样落盘。
//
// Messages 是截至该 barrier 的完整会话快照：先是本回合开始前的 seeded 消息
// （条数由 PriorMessages 给出），随后是本回合已经记录的消息。恢复一个中断回合
// 只需要这个快照、Phase 与 TurnID；恢复过程不依赖任何外部副作用状态。
//
// 检查点只是恢复提示，不是会话事实的来源。已完成的会话历史应以 History 为准；
// Load 失败或数据损坏不得阻止新回合开始（见 agent.RecoverInterruptedTurn）。
type Checkpoint struct {
	// SessionID 是检查点所属会话；存储层以此为键，不能为空。
	SessionID string `json:"session_id"`
	// TurnID 标识写入检查点的回合。同一会话的每个回合必须使用新的 TurnID，
	// 不得复用被打断回合的 TurnID，否则无法区分“同一回合的多次保存”。
	TurnID string `json:"turn_id,omitempty"`
	// Phase 是写入时的回合阶段。
	Phase CheckpointPhase `json:"phase"`
	// Messages 是截至该 barrier 的完整会话快照（seeded 消息 + 本回合已记录消息）。
	Messages []Message `json:"messages,omitempty"`
	// PriorMessages 是 Messages 中属于本回合之前的 seeded 消息条数；其余消息
	// 属于本回合。非终结阶段据此区分“回合前历史”与“本回合已产生的记录”。
	PriorMessages int `json:"prior_messages,omitempty"`
	// CreatedAt 是检查点首次创建时间；存储实现可以在首次 Save 时补齐。
	CreatedAt time.Time `json:"created_at,omitempty"`
	// UpdatedAt 是最近一次 Save 的时间；存储实现应在每次 Save 时更新它。
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	// Metadata 是调用方自定义的附加信息（键值语义由调用方定义，Rig 不解释）；
	// 它必须可 JSON 序列化，用于关联外部追踪 ID、产品特性开关等。
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Split 按 PriorMessages 边界把 Messages 切成 prior（本回合之前）与 turn
// （本回合已记录）两个只读切片。边界越界时会被收敛到 [0, len(Messages)]；
// 返回的切片与 Messages 共享底层数组，调用方不得修改。
func (c *Checkpoint) Split() (prior, turn []Message) {
	if c == nil {
		return nil, nil
	}
	boundary := c.PriorMessages
	if boundary < 0 {
		boundary = 0
	}
	if boundary > len(c.Messages) {
		boundary = len(c.Messages)
	}
	return c.Messages[:boundary], c.Messages[boundary:]
}

// Clone 返回检查点的深拷贝：Messages 的内容分片、工具调用与 Metadata 都会被
// 复制，调用方修改返回值不会影响原检查点。nil 接收者返回 nil。
func (c *Checkpoint) Clone() *Checkpoint {
	if c == nil {
		return nil
	}
	out := *c
	if c.Messages != nil {
		out.Messages = make([]Message, len(c.Messages))
		for index, message := range c.Messages {
			out.Messages[index] = cloneCheckpointMessage(message)
		}
	}
	if c.Metadata != nil {
		out.Metadata = make(map[string]string, len(c.Metadata))
		for key, value := range c.Metadata {
			out.Metadata[key] = value
		}
	}
	return &out
}

// Validate 校验检查点的必填字段与取值范围。它不要求时间戳非零：CreatedAt 与
// UpdatedAt 由存储实现负责补齐。非终结阶段必须携带 TurnID，否则中断修复无法
// 归属回合。校验错误都包装了对应的哨兵错误，可用 errors.Is 判断。
func (c *Checkpoint) Validate() error {
	if c == nil {
		return fmt.Errorf("%w: nil checkpoint", ErrCheckpointInvalid)
	}
	if strings.TrimSpace(c.SessionID) == "" {
		return ErrCheckpointSessionRequired
	}
	if !c.Phase.Valid() {
		return fmt.Errorf("%w: %q", ErrCheckpointPhaseInvalid, string(c.Phase))
	}
	if c.Phase.Interrupted() && strings.TrimSpace(c.TurnID) == "" {
		return ErrCheckpointTurnRequired
	}
	if c.PriorMessages < 0 || c.PriorMessages > len(c.Messages) {
		return fmt.Errorf("%w: prior_messages %d outside [0,%d]", ErrCheckpointInvalid, c.PriorMessages, len(c.Messages))
	}
	return nil
}

// CheckpointStore 持久化回合边界的检查点，用于崩溃/冷启动恢复。实现必须并发
// 安全（不同会话可能并发读写），并按 sessionID 分键。
//
// Load 在会话没有检查点时应返回 (nil, nil)，而不是“未找到”错误。Save 负责补齐
// 时间戳与 SessionID（若 checkpoint.SessionID 为空可用参数 sessionID 兜底），
// 并用 sessionID 作为路由键。调用方（agent 循环或宿主）在 Save 失败时应把当前
// 回合视为失败（fail closed），不要在没有屏障记录的情况下继续执行副作用工具。
//
// Clear 用于会话删除/重置；恢复逻辑从不清除检查点，它通过保存 completed 修复
// 检查点使后续恢复幂等。
type CheckpointStore interface {
	// Load 返回会话最新的检查点；没有检查点时返回 (nil, nil)。损坏或不可读的
	// 检查点可以返回错误，恢复方会把它当作“没有可用检查点”处理。
	Load(ctx context.Context, sessionID string) (*Checkpoint, error)
	// Save 写入（覆盖）一个检查点；sessionID 是路由键，checkpoint 是快照。
	Save(ctx context.Context, sessionID string, checkpoint *Checkpoint) error
	// Clear 删除会话的检查点；不存在时不报错（幂等）。
	Clear(ctx context.Context, sessionID string) error
}

// cloneCheckpointMessage 深拷贝一条消息，避免检查点快照与调用方共享内容分片或
// 工具调用切片。
func cloneCheckpointMessage(message Message) Message {
	out := message
	if message.Content != nil {
		out.Content = make([]ContentPart, len(message.Content))
		copy(out.Content, message.Content)
	}
	if message.ToolCalls != nil {
		out.ToolCalls = make([]ToolCall, len(message.ToolCalls))
		copy(out.ToolCalls, message.ToolCalls)
	}
	return out
}
