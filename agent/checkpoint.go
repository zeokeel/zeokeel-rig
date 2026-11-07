package agent

import (
	"context"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// 中断回合恢复（checkpoint recovery）。
//
// 进程在回合中途退出时，checkpoint 契约里最新的非终结检查点（requested /
// tool_pending）记录了被中断回合的对话尾部。RecoverInterruptedTurn 是恢复的
// 纯函数部分：读取检查点、确定性地补齐缺失的工具结果、追加一条 assistant
// 中断标记，并返回修复后的完整会话与可持久化的 completed 检查点。它只构造
// 消息，不执行 / 不重放任何工具副作用。
//
// # 集成契约（agent 循环 / 宿主需要调用的点）
//
// 1. 回合开始、seed 历史之前：调用
//
//	repair, err := agent.RecoverInterruptedTurn(ctx, checkpointStore, sessionID)
//
// 返回非 nil repair 时，用 repair.Messages 作为本回合的初始会话（SystemPrompt
// 仍由 agent 在每次请求前单独前置），并在追加本次用户输入之前调用
// store.Save(ctx, sessionID, repair.RepairedCheckpoint)：保存后检查点变为
// completed，下一次恢复即为 no-op，这是恢复幂等性的来源。宿主可以选择把
// repair.Marker（assistant 角色）作为 EventMessage 发给 UI。
//
// 2. 每个回合使用新的 TurnID，不得复用被打断回合的 TurnID。检查点按
// (SessionID, TurnID, Phase) 区分保存，复用 TurnID 会让多次保存无法区分。
//
// 3. 三个 barrier 的写入点（Lead 集成 agent 循环时实现）：
//
//   - requested：把模型请求交给 Provider 之前，此时 History 已追加本回合用户
//     输入、尚未收到助手消息。Save 一个 Phase=CheckpointPhaseRequested 的检查点，
//     Messages 为 seeded 历史 + 本回合已记录消息，PriorMessages 为 seeded 历史
//     条数。
//   - tool_pending：顶层“有副作用”工具（写文件、执行命令、网络、启动子 agent
//     等）真正执行之前，此时助手消息及其 tool_calls 已记录。Save 一个
//     Phase=CheckpointPhaseToolPending 的检查点；只读工具与纯内存工具不需要
//     barrier，保持屏障开销低。
//   - completed：回合正常结束（或达到轮数上限等已有明确终态）后，Save 一个
//     Phase=CheckpointPhaseCompleted 的检查点，Messages 为完整会话快照。
//
// 4. Save 失败时当前回合应按错误结束（fail closed）：不要在缺少屏障记录时继续
// 执行副作用工具，也不要把失败静默当作成功。
//
// 5. 用户 Stop / context 取消：不要在回合真正结束前写 completed。保留
// requested / tool_pending 屏障，下一次 Run 开始前的 RecoverInterruptedTurn 会
// 把它当作中断回合关闭（补齐“结果未知”的工具结果）；产品层若另有“终态会话”
// 概念（例如已记录 Stop 的会话状态），可以在调用恢复前用该状态短路，避免把
// 主动停止误判成崩溃。RecoverInterruptedTurn 本身只依据 Phase 判断，不读取
// 外部会话状态。
//
// 6. 未配置 CheckpointStore 时，上述所有写入与 RecoverInterruptedTurn（store
// 为 nil）都是 no-op，Run 的行为与没有检查点功能时完全一致。
//
// 7. CheckpointStore 的 Load 错误被视为“没有可用检查点”（返回 nil, nil）：
// 损坏的恢复提示不得阻止会话继续；context 取消仍会作为错误上报，避免被停止的
// 调用方误以为恢复成功。History 与 checkpoint 同时存在时，History 是已完成
// 回合的事实来源，checkpoint 只用于中断恢复与冷启动提示，不得用旧检查点覆盖
// 更新的 History。
const (
	// InterruptedToolResult 是写给“已派发但结果未知”的工具调用的合成结果。
	// 措辞刻意保守：调用没有被重放，外部副作用状态未知，模型必须先核实状态。
	InterruptedToolResult = "Interrupted before the tool call finished; its outcome is unknown. It was not replayed. Retry only if the operation is read-only or idempotent; verify external state first if it may have side effects."

	// InterruptedTurnMarker 是关闭被中断回合的 assistant 消息内容。它给下一次
	// 模型请求一个位于被放弃尾部之后的 assistant 锚点，并提示模型不要假定中断
	// 的工作已经完成。
	InterruptedTurnMarker = "The previous turn was interrupted: ZeoKeel exited before it finished. Nothing was replayed. Treat the pending work as not done and verify external state before retrying."
)

// CheckpointLoader 是恢复逻辑所需的只读检查点接口。它故意比
// rig.CheckpointStore 窄：恢复只读取最新的检查点，把修复结果持久化的时机由
// 调用方掌握（见 InterruptedTurnRepair.RepairedCheckpoint）。
type CheckpointLoader interface {
	// Load 返回会话最新的检查点；没有检查点时返回 (nil, nil)。
	Load(ctx context.Context, sessionID string) (*rig.Checkpoint, error)
}

// rig.CheckpointStore 实现了 CheckpointLoader（Load 方法签名一致），因此
// 宿主可以直接把完整仓库传给 RecoverInterruptedTurn。
var _ CheckpointLoader = (rig.CheckpointStore)(nil)

// InterruptedTurnRepair 是对一个被进程退出中断的回合的确定性修复结果。
// 它同时提供修复后的完整会话（Messages）、确定性的追加尾部（Appended，按顺序
// 为合成工具结果与 assistant 标记）、以及可直接持久化的 completed 检查点
// （RepairedCheckpoint）。
type InterruptedTurnRepair struct {
	// SessionID 是被修复的会话。
	SessionID string
	// TurnID 是被中断回合的 ID（来自检查点）。
	TurnID string
	// Phase 是触发修复的非终结阶段（requested 或 tool_pending）。
	Phase rig.CheckpointPhase
	// Messages 是修复后的完整会话：检查点快照 + 缺失工具调用的合成结果 +
	// 一条 assistant 中断标记。可以直接作为下一次模型请求的历史 seed。
	Messages []rig.Message
	// Appended 是按顺序追加到检查点快照尾部的确定性消息（先合成工具结果，
	// 后 assistant 标记）。它永远不包含 user 消息：恢复不会重复用户提示。
	Appended []rig.Message
	// Marker 是唯一的 assistant 中断标记消息（即 Appended 的最后一条）。
	Marker rig.Message
	// InterruptedToolResults 是本次为“已派发但结果未知”的工具调用补写的合成
	// 工具结果，按调用顺序排列；没有需要补写的调用时为空。
	InterruptedToolResults []rig.Message
	// RepairedCheckpoint 是 phase=completed、Messages/ TurnID 与修复结果一致的
	// 检查点。调用方保存它之后，同一会话的下一次恢复会是 no-op（恢复幂等）。
	RepairedCheckpoint *rig.Checkpoint
}

// RecoverInterruptedTurn 读取会话最新的检查点，并在它记录了一个非终结回合
// （requested / tool_pending）时构造中断修复。函数是只读且确定性的：同一个
// 检查点产生同一份修复；缺失、completed 或不可读的检查点返回 (nil, nil)。
//
// store 为 nil、sessionID 为空或仅空白时直接返回 (nil, nil)。ctx 取消时返回
// ctx.Err()：被停止的调用方不应把恢复当成成功。ctx 为 nil 时按
// context.Background() 处理（仅为防御性兼容；调用方应传真实 context）。
//
// 恢复不会执行或重放工具：对每个“已记录 tool_calls 但没有对应 tool 结果”的
// 调用，只在会话尾部补一条内容为 InterruptedToolResult 的 role=tool 消息
// （ToolCallID/Name 与调用一致），然后追加一条 InterruptedTurnMarker 的
// assistant 消息。已经记录结果的调用不会被重复补结果。
func RecoverInterruptedTurn(ctx context.Context, store CheckpointLoader, sessionID string) (*InterruptedTurnRepair, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if store == nil || sessionID == "" {
		return nil, nil
	}
	checkpoint, err := store.Load(ctx, sessionID)
	if err != nil {
		// 检查点只是恢复提示：损坏或不可读的检查点不得阻止会话继续。
		// 取消是例外：被停止的调用方必须看到 ctx.Err()。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, nil
	}
	if checkpoint == nil {
		return nil, nil
	}
	phase := rig.CheckpointPhase(strings.TrimSpace(string(checkpoint.Phase)))
	if !isInterruptedPhase(phase) {
		return nil, nil
	}
	if len(checkpoint.Messages) == 0 {
		// 没有对话尾部可修复；不能凭空造一个回合。
		return nil, nil
	}
	return buildInterruptedTurnRepair(sessionID, checkpoint, phase), nil
}

// isInterruptedPhase 报告 phase 是否需要中断修复（requested / tool_pending）。
func isInterruptedPhase(phase rig.CheckpointPhase) bool {
	return phase.Interrupted()
}

// buildInterruptedTurnRepair 由检查点快照确定性地构造修复。输入检查点不会被
// 修改：消息先做深拷贝，合成结果与新标记都是新分配的消息。
func buildInterruptedTurnRepair(sessionID string, checkpoint *rig.Checkpoint, phase rig.CheckpointPhase) *InterruptedTurnRepair {
	history := cloneCheckpointMessages(checkpoint.Messages)
	repair := &InterruptedTurnRepair{
		SessionID: sessionID,
		TurnID:    checkpoint.TurnID,
		Phase:     phase,
	}

	// 任一 role=tool 消息都算对相应 tool_call id 的回答。扫描整个快照而不是只看
	// 尾部，避免畸形检查点在同一调用上重复补结果。
	answered := make(map[string]bool, len(history))
	for _, message := range history {
		if message.Role == rig.RoleTool && message.ToolCallID != "" {
			answered[message.ToolCallID] = true
		}
	}

	repaired := make([]rig.Message, 0, len(history)+1)
	for index := 0; index < len(history); index++ {
		message := history[index]
		repaired = append(repaired, message)
		if message.Role != rig.RoleAssistant || len(message.ToolCalls) == 0 {
			continue
		}
		// 先原样保留紧跟在 assistant 消息之后的工具结果（协议要求它们紧邻
		// assistant tool_calls），并为这次崩溃丢失结果的调用补占位结果。
		next := index + 1
		for next < len(history) && history[next].Role == rig.RoleTool {
			repaired = append(repaired, history[next])
			next++
		}
		for _, call := range message.ToolCalls {
			if strings.TrimSpace(call.ID) == "" || answered[call.ID] {
				continue
			}
			result := interruptedToolResult(call)
			repaired = append(repaired, result)
			repair.InterruptedToolResults = append(repair.InterruptedToolResults, result)
			repair.Appended = append(repair.Appended, result)
		}
		index = next - 1
	}

	// assistant 锚点关闭被放弃的尾部：requested 阶段的检查点以用户提示结尾，
	// 下一次请求不能紧接它再追加一条 user 消息。
	marker := interruptedTurnMarker()
	repaired = append(repaired, marker)
	repair.Marker = marker
	repair.Appended = append(repair.Appended, marker)
	repair.Messages = repaired
	repair.RepairedCheckpoint = repairedCheckpoint(sessionID, checkpoint.TurnID, repaired)
	return repair
}

// interruptedToolResult 构造一条“结果未知、未重放”的 role=tool 合成结果。
func interruptedToolResult(call rig.ToolCall) rig.Message {
	return rig.Message{
		Role:       rig.RoleTool,
		Content:    []rig.ContentPart{rig.TextPart(InterruptedToolResult)},
		ToolCallID: call.ID,
		Name:       call.Name,
	}
}

// interruptedTurnMarker 构造关闭被中断回合的 assistant 标记消息。
func interruptedTurnMarker() rig.Message {
	return rig.Message{
		Role:    rig.RoleAssistant,
		Content: []rig.ContentPart{rig.TextPart(InterruptedTurnMarker)},
	}
}

// repairedCheckpoint 构造恢复后应持久化的 completed 检查点：消息是修复后的完整
// 会话（对下一次回合而言全部属于 seeded prior），TurnID 保持不变，使修复归属
// 于被中断的回合。时间戳与 Metadata 留空由存储实现补齐 / 调用方按需补充。
func repairedCheckpoint(sessionID, turnID string, messages []rig.Message) *rig.Checkpoint {
	return &rig.Checkpoint{
		SessionID:     sessionID,
		TurnID:        turnID,
		Phase:         rig.CheckpointPhaseCompleted,
		Messages:      cloneCheckpointMessages(messages),
		PriorMessages: len(messages),
	}
}

// cloneCheckpointMessages 深拷贝检查点快照，避免修复结果与调用方共享切片。
func cloneCheckpointMessages(messages []rig.Message) []rig.Message {
	if messages == nil {
		return nil
	}
	out := make([]rig.Message, len(messages))
	for index, message := range messages {
		out[index] = cloneCheckpointMessage(message)
	}
	return out
}

// cloneCheckpointMessage 深拷贝一条消息的内容分片与工具调用切片。
func cloneCheckpointMessage(message rig.Message) rig.Message {
	out := message
	if message.Content != nil {
		out.Content = append([]rig.ContentPart(nil), message.Content...)
	}
	if message.ToolCalls != nil {
		out.ToolCalls = append([]rig.ToolCall(nil), message.ToolCalls...)
	}
	return out
}
