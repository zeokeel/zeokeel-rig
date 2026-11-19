package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/memory"
	"github.com/zeokeel/zeokeel-rig/storage"
)

// DefaultSessionID 是 DirStorage.History() 绑定的默认会话 ID。
//
// 契约 storage.Storage 的 History() 没有参数，而 rig.History 的每个方法都接受
// sessionID 且"非空参数优先"。所以：宿主想用别的会话，照常给 Load/Append 传
// sessionID；想用默认会话，传 "" 或 DefaultSessionID 都可以。
const DefaultSessionID = "default"

// ErrUnsupported 表示**目录后端不支持这项能力**（不是故障，也不是数据问题）。
//
// 目录后端的额外面只有"会话消息"：没有检查点、没有记忆 wiki、没有溢出结果。
// 相应访问器返回一个每个方法都报 ErrUnsupported 的诚实桩。宿主用
//
//	errors.Is(err, store.ErrUnsupported)
//
// 判断"这项能力得换后端（session/sqlite）才有"，而不是把它当成存储故障重试。
var ErrUnsupported = errors.New("session store: 目录后端不支持该能力")

// errUnboundDirStorage 表示 DirStorage 没有绑定 *Dir（零值或 NewDirStorage(nil)）。
//
// 单独定义而不是让 nil 指针自己崩：适配器是宿主直接接触的面，零值应该得到一条
// 能读懂的配置错误，而不是一次段错误。
var errUnboundDirStorage = errors.New("session store: DirStorage 未绑定 Dir（请用 NewDirStorage 构造）")

// DirStorage 把目录后端（*Dir，一个会话一个 NDJSON 文件）适配成聚合契约
// storage.Storage。
//
// # 为什么适配器在 session/store，而不是 storage 包
//
// 分层：storage 是第 1 层契约包（只 import 根 rig 与
// memory），session/store 是第 2 层实现。把适配器放进 storage 会让契约层反向
// 依赖一个具体实现，所以方向只能是**单向的**：
//
//	session/store ──import──▶ storage ──import──▶ rig / memory
//
// 这也意味着 session/store 绝不能反向被 storage 依赖（`Dir` 上不能有
// `Storage() Storage` 方法）——否则立刻成环。
//
// # 能力：一项有，三项没有
//
//	History()      ✅ 会话消息（NDJSON），实现 rig.History + rig.HistoryReplacer
//	Checkpoints()  ❌ 目录后端不支持 → 返回"不支持"桩，方法一律报 ErrUnsupported
//	Memory()       ❌ 目录后端不支持 → 同上
//	Spill()        ❌ 目录后端不支持 → 同上
//	Close()        ✅ 转发 Dir.Close（幂等）
//
// 三个"不支持"为什么不返回 nil：storage.Storage 的接口文档要求访问器**不返回
// nil**（nil 会让调用方在离原因很远的地方 panic）。桩也不伪造能力——它不会假装
// 存下任何东西，每个方法都返回包装了 ErrUnsupported 的错误，宿主一眼就能判断
// "这项能力得换后端"。目录后端要真正支持检查点/记忆/溢出，只能换成
// session/sqlite 这类"一个库装全部"的实现，而不是在这里补一张隐式索引。
//
// # 零值
//
// 零值 DirStorage（或 NewDirStorage(nil)）不是 panic 陷阱：History() 返回一个
// 每个方法都报 errUnboundDirStorage 的句柄，三个"不支持"桩照常返回，
// Close() 直接返回 nil（没有资源可关）。
type DirStorage struct{ dir *Dir }

// 编译期断言：DirStorage 满足聚合契约（值类型，宿主可以按值拷贝/传递）。
var _ storage.Storage = DirStorage{}

// NewDirStorage 把已打开的 *Dir 适配成 storage.Storage。
//
// 适配器不改变 d 的生命周期语义：Close 就是 d.Close，且**不**重复打开或扫描目录。
func NewDirStorage(d *Dir) DirStorage { return DirStorage{dir: d} }

// History 返回绑定到 DefaultSessionID 的 rig.History。
//
// 等价于 d.History(context.Background(), DefaultSessionID)：契约方法没有 ctx 与
// 默认会话 ID，而 Dir.History 两者都要。context.Background() 在这里是安全的——
// Dir.History 的 ctx 只为保持调用风格，构造本身不做 I/O，真正的取消与关闭判断
// 发生在每次 Load/Append/Replace 上。
//
// 返回的是**未导出**包装（dirHistory），因此宿主无法把它断言回 *store.History；
// 包装同时转发 Replace（rig.HistoryReplacer），上下文压缩落盘不会因为换成聚合
// 契约而丢失。
func (s DirStorage) History() rig.History {
	if s.dir == nil {
		return dirHistory{}
	}
	return dirHistory{history: s.dir.History(context.Background(), DefaultSessionID)}
}

// Checkpoints 返回**不支持检查点**的桩：目录后端没有这项能力。
//
// 检查点是"回合边界上的恢复状态"：按会话分键、覆盖写、可 Clear。目录后端只有
// "一个会话一个 NDJSON 追加日志"，硬塞进来等于在 append-only 日志之外再造第二份
// 索引，而两份索引一旦不一致，崩溃恢复就会读到"从没发生过的回合"。宿主需要
// 检查点时请换 session/sqlite（或其它实现了 rig.CheckpointStore 的后端）。
func (s DirStorage) Checkpoints() rig.CheckpointStore { return unsupportedCheckpoints{} }

// Memory 返回**不支持记忆 wiki** 的桩：目录后端没有这项能力。
//
// 记忆的落盘实现是 internal/memwiki/file.go（按工作区分目录），它不归会话存储管，
// 把两者拼进同一个 DirStorage 只会让"一份状态放在两个地方"变成隐式约定。宿主
// 需要记忆时直接注入 memory.Wiki（文件 wiki 或 sqlite 的实现）。
func (s DirStorage) Memory() memory.Wiki { return unsupportedWiki{} }

// Spill 返回**不支持溢出结果**的桩：目录后端没有这项能力。
//
// 溢出结果今天走 rig.SpillPolicy 的目录实现（工具层自己管句柄与 GC），不在会话
// 存储里。宿主需要"句柄从文件路径变成一行记录"时再换实现了 rig.SpillStore 的
// 后端（例如 session/sqlite）。
func (s DirStorage) Spill() rig.SpillStore { return unsupportedSpill{} }

// Close 转发到 Dir.Close：之后的读写一律返回 ErrClosed，且可重复调用。
//
// 目录后端 Close 只置关闭标志（没有 WAL、没有需要 flush 的缓冲），所以这里没有
// 额外的收尾顺序；第二次调用仍返回 nil，正是契约要求的幂等语义。
func (s DirStorage) Close() error {
	if s.dir == nil {
		return nil
	}
	return s.dir.Close()
}

// ListSessions 按"最近更新在前"返回全部会话 ID。
//
// 它**不属于** storage.Storage：会话列表的过滤参数（store.Filter）与返回视图
// （store.Meta）是后端自有类型，收进契约会让 storage 与 session/store 互相
// import 成环。这里把它作为适配器的
// **扩展方法**暴露，宿主按需类型断言——storagetest 的 SessionAdmin 用的就是
// 结构化匹配，双方都不需要 import 对方。
func (s DirStorage) ListSessions(ctx context.Context) ([]string, error) {
	if s.dir == nil {
		return nil, errUnboundDirStorage
	}
	metas, err := s.dir.List(ctx, Filter{})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(metas))
	for _, meta := range metas {
		ids = append(ids, meta.ID)
	}
	return ids, nil
}

// DeleteSession 删除会话（数据文件 + 内存索引条目）。
//
// 同样**不属于** storage.Storage，理由见 ListSessions。会话不存在时返回包装了
// ErrNotFound 的错误（调用方用 errors.Is 判断）。
func (s DirStorage) DeleteSession(ctx context.Context, sessionID string) error {
	if s.dir == nil {
		return errUnboundDirStorage
	}
	if err := s.dir.Delete(ctx, sessionID); err != nil {
		return fmt.Errorf("session store: delete session %q: %w", sessionID, err)
	}
	return nil
}

// unsupportedError 构造统一的"目录后端不支持 X"错误（包装 ErrUnsupported）。
func unsupportedError(capability string) error {
	return fmt.Errorf("%w: %s（需要检查点/记忆/溢出请换 session/sqlite 一类后端）", ErrUnsupported, capability)
}

// unsupportedCheckpoints 是"目录后端没有检查点"的诚实桩。
//
// 三个方法的返回值都是同一个 ErrUnsupported：Load 不返回 (nil, nil)（那会被读成
// "这个会话没有检查点"），Save 不静默成功（那会让 agent 以为恢复点已经落盘）。
// 说谎的代价在崩溃恢复路径上最高——一次静默成功的 Save 让人以为能续，实际续不上。
type unsupportedCheckpoints struct{}

var _ rig.CheckpointStore = unsupportedCheckpoints{}

// Load 实现 rig.CheckpointStore：目录后端不支持检查点。
func (unsupportedCheckpoints) Load(ctx context.Context, sessionID string) (*rig.Checkpoint, error) {
	return nil, unsupportedError("检查点")
}

// Save 实现 rig.CheckpointStore：目录后端不支持检查点。
func (unsupportedCheckpoints) Save(ctx context.Context, sessionID string, checkpoint *rig.Checkpoint) error {
	return unsupportedError("检查点")
}

// Clear 实现 rig.CheckpointStore：目录后端不支持检查点。
func (unsupportedCheckpoints) Clear(ctx context.Context, sessionID string) error {
	return unsupportedError("检查点")
}

// unsupportedWiki 是"目录后端没有记忆 wiki"的诚实桩。
//
// 六个方法全部报 ErrUnsupported：尤其 Write/Append 不能静默成功——模型会以为
// 自己记住了，下一回合却什么都读不到。
type unsupportedWiki struct{}

var _ memory.Wiki = unsupportedWiki{}

// Index 实现 memory.Wiki：目录后端不支持记忆。
func (unsupportedWiki) Index(ctx context.Context) ([]memory.PageMeta, error) {
	return nil, unsupportedError("记忆 wiki")
}

// Read 实现 memory.Wiki：目录后端不支持记忆。
func (unsupportedWiki) Read(ctx context.Context, name string) (memory.Page, error) {
	return memory.Page{}, unsupportedError("记忆 wiki")
}

// Write 实现 memory.Wiki：目录后端不支持记忆。
func (unsupportedWiki) Write(ctx context.Context, page memory.Page) error {
	return unsupportedError("记忆 wiki")
}

// Append 实现 memory.Wiki：目录后端不支持记忆。
func (unsupportedWiki) Append(ctx context.Context, name, text string) error {
	return unsupportedError("记忆 wiki")
}

// Delete 实现 memory.Wiki：目录后端不支持记忆。
func (unsupportedWiki) Delete(ctx context.Context, name string) error {
	return unsupportedError("记忆 wiki")
}

// Search 实现 memory.Wiki：目录后端不支持记忆。
func (unsupportedWiki) Search(ctx context.Context, query string) ([]memory.PageMeta, error) {
	return nil, unsupportedError("记忆 wiki")
}

// unsupportedSpill 是"目录后端没有溢出结果存储"的诚实桩。
type unsupportedSpill struct{}

var _ rig.SpillStore = unsupportedSpill{}

// Save 实现 rig.SpillStore：目录后端不支持溢出结果。
func (unsupportedSpill) Save(ctx context.Context, call rig.ToolCall, content string) (rig.SpillRef, error) {
	return rig.SpillRef{}, unsupportedError("溢出结果")
}

// Load 实现 rig.SpillStore：目录后端不支持溢出结果。
func (unsupportedSpill) Load(ctx context.Context, id string) (string, error) {
	return "", unsupportedError("溢出结果")
}

// GC 实现 rig.SpillStore：目录后端不支持溢出结果。
func (unsupportedSpill) GC(ctx context.Context, keep int) (int, error) {
	return 0, unsupportedError("溢出结果")
}

// dirHistory 是 DirStorage.History() 的薄包装：只转发，不加语义。
//
// 存在的理由有两个：
//
//  1. 不把 *store.History 这个具体类型暴露到契约面上——否则宿主会写下
//     `history.(*store.History)` 这样的断言，换后端时立刻崩；
//  2. 零值 DirStorage 能有"每个方法都报错"的稳定行为，而不是 nil 解引用。
//
// 它同时实现 rig.HistoryReplacer：包装不该让能力变少（Replace 是上下文压缩的
// 持久化路径）。
type dirHistory struct{ history *History }

var (
	_ rig.History         = dirHistory{}
	_ rig.HistoryReplacer = dirHistory{}
)

// Load 实现 rig.History。
func (h dirHistory) Load(ctx context.Context, sessionID string) ([]rig.Message, error) {
	if h.history == nil {
		return nil, errUnboundDirStorage
	}
	return h.history.Load(ctx, sessionID)
}

// Append 实现 rig.History。
func (h dirHistory) Append(ctx context.Context, sessionID string, messages ...rig.Message) error {
	if h.history == nil {
		return errUnboundDirStorage
	}
	return h.history.Append(ctx, sessionID, messages...)
}

// Replace 实现 rig.HistoryReplacer（上下文压缩用）。
func (h dirHistory) Replace(ctx context.Context, sessionID string, messages []rig.Message) error {
	if h.history == nil {
		return errUnboundDirStorage
	}
	return h.history.Replace(ctx, sessionID, messages)
}
