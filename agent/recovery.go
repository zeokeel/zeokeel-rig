package agent

// 上下文溢出恢复：模型后端返回"超出上下文"时，自动压缩一次历史并重试同一回合。
//
// 这是宿主最容易写错的一段编排：压缩后必须替换历史（否则下一次请求仍然超限）、
// 必须保留当前用户消息（否则重试回答的是更早的问题）、必须只在真正的上下文错误上
// 触发（限流/鉴权失败不该被当成超限反复重试）、且压缩是破坏性的——替换前要给宿主
// 一个归档机会（fail-closed：归档失败就不替换）。
//
// 关闭时（AgentSpec.ContextRecovery == nil）行为与之前完全一致。

import (
	"context"

	"github.com/zeokeel/zeokeel-rig"
)

// recoveryEnabled reports whether one automatic recovery is configured.
func (a *Agent) recoveryEnabled() bool {
	return a != nil && a.contextRecovery != nil && a.history != nil && a.compactor != nil
}

// recoverContext compacts the session history and retries the same turn once.
// It returns retried=false when the recovery could not even attempt (no
// replacer, unreadable history, failed archive, failed compaction); in that case
// the caller keeps the original error. retried=true means the returned error is
// the retry's outcome.
func (a *Agent) recoverContext(ctx context.Context, req rig.RunRequest, first rig.RunResult) (rig.RunResult, error, bool) {
	if !a.recoveryEnabled() {
		return first, nil, false
	}
	replacer, ok := a.history.(rig.HistoryReplacer)
	if !ok {
		return first, nil, false
	}
	report := func(before, after int, err error) {
		if a.contextRecovery.OnRecovery != nil {
			a.contextRecovery.OnRecovery(ctx, req.SessionID, before, after, err)
		}
	}
	messages, err := a.history.Load(ctx, req.SessionID)
	if err != nil || len(messages) == 0 {
		report(0, 0, err)
		return first, nil, false
	}
	before := rig.EstimateTokens(messages)
	budget := a.contextBudget - rig.EstimateTextTokens(a.effectiveSystemPrompt(ctx, req))
	if budget <= 0 {
		// No configured budget (or the prompt alone exceeds it): compact to half
		// the current size, with a floor so a tiny history is still summarized.
		budget = before / 2
		if budget < 1024 {
			budget = 1024
		}
	}
	compacted, compactErr := a.compactor.Compact(ctx, req.SessionID, cloneMessages(messages), budget)
	if compactErr != nil || len(compacted) == 0 {
		report(before, before, compactErr)
		return first, nil, false
	}
	// 当前用户消息必须留在重试的历史里：压缩窗口可能刚好把它挤出去。
	if last, ok := lastUserMessageOf(messages); ok && !containsMessage(compacted, last) {
		compacted = append(compacted, last)
	}
	// 破坏性替换之前给宿主归档机会；归档失败就保留原始历史（fail-closed）。
	if hook := a.contextRecovery.BeforeReplace; hook != nil {
		if hookErr := hook(ctx, req.SessionID, cloneMessages(messages), cloneMessages(compacted)); hookErr != nil {
			report(before, before, hookErr)
			return first, nil, false
		}
	}
	if err := replacer.Replace(ctx, req.SessionID, compacted); err != nil {
		report(before, before, err)
		return first, nil, false
	}
	after := rig.EstimateTokens(compacted)
	// 同一回合重试：输入已经写进历史，不能再追加一次。
	retryReq := req
	retryReq.Input = nil
	result, retryErr := a.runTurn(ctx, retryReq)
	// 失败的那次尝试已经被计费：保留它的用量与轮数。
	if retryErr == nil {
		result.Usage.Add(first.Usage)
		result.Iterations += first.Iterations
	}
	report(before, after, retryErr)
	return result, retryErr, true
}

// lastUserMessageOf returns the most recent user message.
func lastUserMessageOf(messages []rig.Message) (rig.Message, bool) {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == rig.RoleUser {
			return messages[index], true
		}
	}
	return rig.Message{}, false
}

// containsMessage reports whether the same message (role + text + parts) is in the
// list; used to keep the current user message after compaction.
func containsMessage(messages []rig.Message, want rig.Message) bool {
	for _, message := range messages {
		if message.Role != want.Role || message.Text() != want.Text() || len(message.Content) != len(want.Content) {
			continue
		}
		same := true
		for index := range message.Content {
			if message.Content[index] != want.Content[index] {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}
