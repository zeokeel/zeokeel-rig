package rig

import "context"

// SubagentSpec 描述一个由编排工具（subagent / workflow / team）或希望做进程内
// 委派的宿主启动的子 agent。
type SubagentSpec struct {
	// Name 是子 agent 的展示名（在同一个团队管理器内唯一）。
	Name string
	// Prompt 是发给子 agent 的第一条用户消息。
	Prompt string
	// SystemPrompt 非空时覆盖父 agent 的系统提示词。
	SystemPrompt string
	// Model 非空时覆盖父 agent 使用的模型。
	Model string
	// MaxIterations 限制子 agent 的模型↔工具轮数（0 表示使用 agent 默认值）。
	MaxIterations int
	// Fork 让子 agent 以父会话已完成历史的一份拷贝作为起点，而不是全新的上下文。
	// runner 必须配置了 RunnerOptions.ParentSessionID（以及父 History）；否则 Start
	// 会返回配置错误。
	Fork bool
	// OutputSchema 要求子 agent 产出结构化输出：runner 会暴露一个 structured_output
	// 工具，其参数必须匹配这份（JSON Schema 子集），捕获到的对象通过
	// SubagentResult.Structured 返回。
	OutputSchema map[string]any
}

// SubagentResult 是一次子 agent 运行的产物。
type SubagentResult struct {
	// Name 回显 spec 中的名字。
	Name string
	// Output 是该次运行的最终助手文本。
	Output string
	// Usage 是子 agent 在该次运行中的 token 统计。
	Usage Usage
	// Iterations 是实际执行的模型轮数。
	Iterations int
	// Structured 保存子 agent 的结构化输出；spec 未携带 OutputSchema 时为 nil。
	Structured map[string]any
}

// Subagent 是一个存活的进程内子 agent。实现必须保证 Send/Wait/Stop 的并发调用
// 安全；同一个 Subagent 上的多次运行会被串行化。
type Subagent interface {
	// Name 返回 spec 中的名字。
	Name() string
	// Send 投递一条后续用户消息，并阻塞到该轮运行结束。它会先等待正在进行的运行
	// 结束（同一子 agent 上的运行是串行的）。
	Send(ctx context.Context, message string) (SubagentResult, error)
	// Wait 阻塞等待当前（或首次）运行并返回其结果。运行已结束后再调用会返回同一
	// 结果。
	Wait(ctx context.Context) (SubagentResult, error)
	// Stop 取消正在进行的运行并释放该子 agent。可重复调用（幂等）。
	Stop() error
}

// SubagentRunner 启动用于编排的子 agent。agent 包提供默认实现
// （agent.NewRunner）：它复用父 agent 的 provider/模型/工具配置，并施加深度与
// 并发上限。第三方可以实现该接口接入不同的运行时。
type SubagentRunner interface {
	Start(ctx context.Context, spec SubagentSpec) (Subagent, error)
}
