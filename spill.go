package rig

// 溢出（spill）的工具结果：大到放不进模型上下文的结果会被存起来，模型看到一段
// 摘要加一个句柄，需要时再把内容读回来。
//
// 默认存储是 SpillPolicy.Dir 下的文件。宿主如果本来就把状态放在数据库里，可以改为
// 传一个 SpillStore：句柄从文件路径变成一行记录，模型可见的那句话形状不变。
//
// 读回：句柄必须能读回来，所以 `tools/full.All` 在拿到 Store 时会注册 `spill_read`
// 工具（调本接口的 Load）。没有读回路径的"已存起来"等于"永久丢失"。

import "context"

// SpillRef 标识一条已存下的工具结果。
type SpillRef struct {
	// ID 是模型看到、也能传回读回工具的句柄。
	ID string
	// Bytes 是存下来的字节数。
	Bytes int
}

// SpillStore 存放过大的工具结果。
//
// 实现必须并发安全：多个工具可能同时结束。
type SpillStore interface {
	// Save 存下内容并返回句柄。
	Save(ctx context.Context, call ToolCall, content string) (SpillRef, error)
	// Load 返回句柄背后的正文；句柄不存在时返回 ErrSpillNotFound。
	Load(ctx context.Context, id string) (string, error)
	// GC 只保留最新的 keep 条，返回删掉的条数。
	GC(ctx context.Context, keep int) (int, error)
}

// ErrSpillNotFound 表示句柄背后没有存着的结果。
var ErrSpillNotFound = errSpillNotFound{}

type errSpillNotFound struct{}

func (errSpillNotFound) Error() string { return "rig: spilled result not found" }
