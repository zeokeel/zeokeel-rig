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
	// GC 只回收**与 ctx 同作用域**的条目，保留其中最新的 keep 条，返回删掉的条数。
	//
	// 作用域由 WithSpillScope 打在 ctx 上，通常是会话 ID。agent 在**每条溢出结果
	// 写入之后**按 SpillPolicy.MaxFiles 调一次 GC；全局裁剪会让会话 A 的回收删掉
	// 会话 B 的句柄——句柄失效就是模型的工具输出永久丢失。文件后端
	// （SpillPolicy.Dir）本来就按文件名里的作用域前缀分区，数据库后端必须给出
	// 同样的行为。宿主也可以按自己的节奏调（例如关闭会话时、启动清理时）。
	//
	// ctx 上没有作用域时（SpillScope 返回空串）**只**回收作用域为空串的行，不是
	// "空作用域就回收全库"：老数据（作用域这一列还不存在的年代写下的行）与不经过
	// agent 的调用方都落在这一桶里，把空作用域当成通配符就会退回上面那个 bug。
	GC(ctx context.Context, keep int) (int, error)
}

// spillScopeKey 是溢出作用域在 context 里的键。用私有空结构体做键：外部包无法
// 用同名字符串塞值进来，作用域只能由 WithSpillScope 写。
type spillScopeKey struct{}

// WithSpillScope 给 ctx 打上溢出条目的作用域（通常是会话 ID）。
//
// 作用域决定 SpillStore.GC 回收哪一批条目。传空串表示"显式属于空作用域"（只回收
// 空作用域的行），它**不会**被当成"继承外层作用域"或"全库"。
func WithSpillScope(ctx context.Context, scope string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, spillScopeKey{}, scope)
}

// SpillScope 返回 ctx 上的作用域；没有时返回空串（nil ctx 同样返回空串）。
func SpillScope(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	scope, _ := ctx.Value(spillScopeKey{}).(string)
	return scope
}

// ErrSpillNotFound 表示句柄背后没有存着的结果。
var ErrSpillNotFound = errSpillNotFound{}

type errSpillNotFound struct{}

func (errSpillNotFound) Error() string { return "rig: spilled result not found" }
