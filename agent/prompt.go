package agent

// 系统提示词的"每轮取值"逻辑（契约见根包 agent.go 的 SystemPromptFunc）。

import (
	"context"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// effectiveSystemPrompt 返回本次运行生效的系统提示词。
//
// SystemPromptFunc 非 nil 时以它的结果为准（返回空串 = 这次没有系统提示词，
// 不回退到静态值：宿主用空串表达"这一轮不要环境块"是合理的意图，静默回退会让
// 配置看起来没生效）。函数 panic 不在保护范围内——那是宿主的实现错误。
func (a *Agent) effectiveSystemPrompt(ctx context.Context, req rig.RunRequest) string {
	if a.systemPromptFunc != nil {
		return strings.TrimSpace(a.systemPromptFunc(ctx, req))
	}
	return a.systemPrompt
}
