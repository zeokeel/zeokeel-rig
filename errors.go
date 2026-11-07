package rig

import "errors"

// 哨兵错误：调用方可以用 errors.Is 判断。Provider 与工具可以用上下文信息包装
// 它们，但不得替换成别的错误。
var (
	// ErrProviderRequired：agent 未配置 provider 时返回。
	ErrProviderRequired = errors.New("rig: provider is required")
	// ErrModelRequired：没有任何可用的模型名时返回。
	ErrModelRequired = errors.New("rig: model is required")
	// ErrToolNotFound：工具注册表被要求调用一个未注册的工具时返回。agent 会把
	// 未知工具调用作为工具错误回传给模型，而不是让整次运行失败。
	ErrToolNotFound = errors.New("rig: tool not found")
	// ErrMaxIterations：模型↔工具循环达到轮数上限、但模型仍在请求工具时，由 Run
	// 包装返回。
	ErrMaxIterations = errors.New("rig: max iterations reached")
)
