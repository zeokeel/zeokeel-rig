package memoryvfs

import (
	"context"
	"io"
)

// FS 是虚拟文件系统能力。行为对齐 OpenViking 的文件语义：
//   - List 只返回直接子项（目录项带 L0 摘要，不含正文）；
//   - Tree 递归返回，depth<=0 表示不限；
//   - Read 按 level 返回该层内容（L0 只给 Summary，L1 给 Summary+Overview）；
//   - Write 是覆盖写（不存在则创建）；Append 只追加到正文；
//   - Move 保留版本号并递增；Delete 是软删前的正式删除（实现会写 trash 快照）。
type FS interface {
	Stat(ctx context.Context, uri URI) (Entry, error)
	List(ctx context.Context, uri URI) ([]Entry, error)
	Tree(ctx context.Context, uri URI, depth int) ([]Entry, error)
	Read(ctx context.Context, uri URI, level Level) (Entry, error)
	Write(ctx context.Context, entry Entry) (Entry, error)
	Append(ctx context.Context, uri URI, text string) (Entry, error)
	Move(ctx context.Context, from, to URI) error
	Delete(ctx context.Context, uri URI) error
	Mkdir(ctx context.Context, uri URI, description string) (Entry, error)
}

// Retriever 是检索能力。Find 是单查询；Search 在 Query.Mode 为 thinking/context
// 时允许意图分析与多查询融合。两者都只返回元数据（Entry.Content 为空，
// 除非 Query.IncludeBody 为 true）。
type Retriever interface {
	Find(ctx context.Context, query Query) (Result, error)
	Search(ctx context.Context, query Query) (Result, error)
}

// Memory 是面向记忆的高层写入。Remember 会做类型校验、相似度合并与去重；
// Forget 删除记忆条目（实现写 trash 快照）。
type Memory interface {
	Remember(ctx context.Context, entry Entry) (Entry, error)
	Forget(ctx context.Context, uri URI) error
}

// Sessions 管理会话捕获与提交。
type Sessions interface {
	// Capture 幂等追加消息到该会话的 pending 队列（同一条消息重复 Capture 只存一次）。
	Capture(ctx context.Context, sessionID string, messages ...Message) error
	// Pending 返回待提交状态。
	Pending(ctx context.Context, sessionID string) (SessionState, error)
	// Commit 归档 pending 消息，并按 opts 抽取长期记忆。
	Commit(ctx context.Context, sessionID string, opts CommitOptions) (CommitResult, error)
}

// Rules 编译并检索行为规则。
type Rules interface {
	// CompileRules 把一段指令（或若干来源 URI）编译成规则条目；instruction 与
	// sources 不能同时为空。
	CompileRules(ctx context.Context, instruction string, sources []URI) (Entry, error)
	// SearchRules 按任务上下文返回适用规则，threshold 是 0..1 的适用概率阈值。
	SearchRules(ctx context.Context, taskContext string, threshold float64) ([]Rule, error)
}

// Versions 提供快照、差异与恢复。
type Versions interface {
	Snapshot(ctx context.Context, label string) (Snapshot, error)
	Snapshots(ctx context.Context) ([]Snapshot, error)
	Diff(ctx context.Context, from, to string) (string, error)
	Restore(ctx context.Context, id string) error
}

// Maintenance 提供统计与回收。
type Maintenance interface {
	Stats(ctx context.Context) (Stats, error)
	GC(ctx context.Context, opts GCOptions) (GCResult, error)
	Rebuild(ctx context.Context) error
}

// Packer 提供导入/导出。
type Packer interface {
	Export(ctx context.Context, scope URI) (Pack, error)
	Import(ctx context.Context, pack Pack, opts ImportOptions) (ImportResult, error)
}

// Store 是完整后端。实现必须并发安全，Close 之后所有方法返回 ErrClosed。
//
// 宿主可以只依赖自己需要的子接口（例如只需要 FS + Retriever），这样替换后端
// 时不必实现会话/规则/版本。
type Store interface {
	FS
	Retriever
	Memory
	Sessions
	Rules
	Versions
	Maintenance
	Packer
	io.Closer
}
