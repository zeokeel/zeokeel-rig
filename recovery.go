package rig

import (
	"context"
	"strings"
)

// ContextRecovery 配置 agent 的"一次自动压缩 + 重试同一回合"上下文溢出恢复。
//
// 压缩是破坏性的：替换历史前宿主必须有机会归档（BeforeReplace，fail-closed），
// 恢复的成败通过 OnRecovery 通知。关闭时把 AgentSpec.ContextRecovery 留 nil。
type ContextRecovery struct {
	// BeforeReplace 在压缩结果替换历史之前调用，参数是压缩前的完整历史与压缩后
	// 将要写入的历史；返回错误则放弃恢复（保留原始历史与原始错误）。宿主在这里做
	// fail-closed 归档/审计。
	BeforeReplace func(ctx context.Context, sessionID string, messages, compacted []Message) error
	// OnRecovery 报告一次恢复尝试的结果；before/after 是压缩前后的估算 token 数，
	// err 非 nil 表示恢复没有完成。
	OnRecovery func(ctx context.Context, sessionID string, before, after int, err error)
}

// IsContextOverflowError 报告一个模型错误是否表示"请求超出模型上下文"。
//
// 判定只看错误文本里的稳定短语（各家 provider 措辞不同），宁可漏判也不要误判：
// 把限流/鉴权错误当成超限去压缩历史，会破坏用户会话。
func IsContextOverflowError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, pattern := range []string{
		"context_length_exceeded",
		"context length",
		"maximum context",
		"context window",
		"too many tokens",
		"prompt is too long",
		"reduce the length of the messages",
	} {
		if strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}
