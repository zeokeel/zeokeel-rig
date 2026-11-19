package sqlite

// 本包实现目录存储（session/store）的契约：宿主只是换掉它调用的构造函数，其余
// 代码不用改。这些别名让调用方用哪个包的名字都能指到同一个类型。

import (
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// Meta 是一个会话的持久化列表项视图（各字段的权重与语义见 store.Meta）。
type Meta = store.Meta

// Filter 收窄一次 List 的范围。
type Filter = store.Filter

// 宿主用来分支判断的错误值，与目录存储**同一个对象**，所以换实现之后
// `errors.Is` 照常成立。
var (
	// ErrNotFound 表示库里没有这个会话。
	ErrNotFound = store.ErrNotFound
	// ErrClosed 表示在 Close 之后继续使用这个 store。
	ErrClosed = store.ErrClosed
	// ErrCorrupt 表示某条记录无法解码。数据库在插入时就会拒绝格式非法的写入，所以
	// 这个错误只由**导入器**返回：源目录里有一条读不出来的记录。
	ErrCorrupt = store.ErrCorrupt
	// ErrExists 表示要新建的会话 ID 已经存在（与目录存储同义）。
	ErrExists = store.ErrExists
	// ErrMessageTooLarge 表示单条消息超过存储允许的上限。
	ErrMessageTooLarge = store.ErrMessageTooLarge
)

// MaxTitleRunes 是 Append 从第一条消息派生标题时的最长符文数。
const MaxTitleRunes = store.MaxTitleRunes

// 编译期证明：数据库实现满足目录存储的契约。
var _ store.Store = (*Store)(nil)
