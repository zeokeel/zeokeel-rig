// Package store 提供会话历史的内置落盘实现（NDJSON + 索引），用于把
// rig.History 从内存（session.Memory）升级成"进程重启后仍然存在"。
//
// 对标 Codex 的 state crate（SQLite threads/logs + session_index + history），
// 但这里刻意只用标准库、只用普通文件：
//
//	<dir>/index.json            —— 会话索引（Meta 列表，原子替换：tmp + rename + fsync）
//	<dir>/sessions/<id>.ndjson  —— 一行一条 rig.Message（追加 + fsync）
//
// # 为什么是 NDJSON 而不是 SQLite
//
// 本仓库的硬约束是"纯 Go、零新增依赖、CGO 关闭"，SQLite 需要 cgo 或第三方纯 Go
// 实现。NDJSON 的代价是索引与数据分离（需要自行保证一致性），换来的是：可读、
// 可用 grep/jq 排查、单条追加只写一行、崩溃时最多丢最后一条半行。
//
// # 崩溃安全（写入顺序即协议）
//
//	Append：① 追加一行到 .ndjson 并 Sync → ② 更新 index.json（tmp + rename + fsync 目录）
//
// 崩溃点分析：
//
//   - ①之前崩溃：什么都没发生。
//   - ①之后、②之前崩溃：数据行在盘上，索引里的条数偏小。下次 Load/List 读到
//     真实条数后会**校正并回写索引**（自愈），不会丢数据。
//   - ②之后崩溃：一致。
//
// 因此"半写入不破坏已有数据"：最坏情况是索引暂时偏小，Load 永远能读出完整记录。
// 文件尾部若出现"没有换行结尾的半行"（写一行时的撕裂），读取时丢弃该行并把
// 真实条数写回索引；后续 Append 会先把半行裁掉再追加，避免把新记录粘在坏行后面。
//
// # 损坏容忍
//
// 中间某行不是合法 rig.Message JSON（例如磁盘位翻转、被编辑器改坏）时，跳过该行
// 继续加载后面的行，并通过 WithNotice 回调上报；只有"整份文件的完整行全部无法
// 解析"才返回 ErrCorrupt（让调用方做显式决策，而不是静默丢光会话）。
//
// # 并发
//
// 同目录、同会话的并发追加在进程内被串行化（按 ID 分片的 sync.Mutex）；不同
// 会话可以并行，索引更新由一把 RWMutex 保护。**不提供跨进程互斥**（那需要
// 平台相关的文件锁），多进程同时写同一目录属于已知限制，见包内 README 注释。
//
// # 典型用法
//
//	st, err := store.NewDir("/var/lib/myapp/sessions")
//	if err != nil {
//		return err
//	}
//	defer st.Close()
//
//	// 直接用 rig.History 契约接 agent：同一个实例按 SessionID 分流。
//	history := st.History(context.Background(), "default-session")
//	a, err := agent.New(rig.AgentSpec{Provider: provider, History: history})
//
//	// 或者显式管理会话元数据：
//	meta, err := st.Create(ctx, store.Meta{Title: "排查构建失败", Workspace: "/repo"})
//	meta, err = st.Append(ctx, meta.ID, rig.NewMessage(rig.RoleUser, "为什么构建挂了？"))
//	metas, err := st.List(ctx, store.Filter{Workspace: "/repo", Limit: 20})
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// MaxMessageBytes 是单条消息序列化后的上限（4 MiB）。超过即拒绝写盘并返回
// ErrMessageTooLarge：静默截断会让历史在回放时被模型看到"缺了一半"的内容，
// 那比直接报错危险得多。
const MaxMessageBytes = 4 << 20

// MaxTitleRunes 是 Meta.Title 的最大字符数（按 rune 计，中文标题不会被砍半）。
const MaxTitleRunes = 120

const (
	// indexFileName 是索引文件名。
	indexFileName = "index.json"
	// sessionsDirName 是会话数据目录名。
	sessionsDirName = "sessions"
	// sessionExt 是会话文件名后缀。
	sessionExt = ".ndjson"
	// indexVersion 是索引格式版本（不认识的版本会从数据文件重建）。
	indexVersion = 1
	// shardCount 是按 ID 分片的互斥锁数量。
	shardCount = 64
	// maxIDLength 是外部传入 ID 的长度上限。
	maxIDLength = 128
	// indexTmpPrefix 是索引原子替换时临时文件的前缀。
	indexTmpPrefix = ".index-"
	// reloadInterval 是**同一触发键**两次自愈重载之间的最小间隔。
	//
	// 目的：一个反复查询不存在会话的调用方（重试循环 / 模型反复试探）不应该每次
	// 都 stat+读盘。第一次未命中允许重载；同一个键在 200ms 内的后续未命中直接被
	// 内存索引短路。不同键各有一次机会，互不饿死。
	reloadInterval = 200 * time.Millisecond
	// maxReloadKeys 限制节流表的规模（海量随机 ID 不能让它无限增长）。
	maxReloadKeys = 256
)

// 错误哨兵。调用方用 errors.Is 判断。
var (
	// ErrNotFound 表示会话不存在。
	ErrNotFound = errors.New("session store: session not found")
	// ErrClosed 表示 Store 已关闭，不再接受任何操作。
	ErrClosed = errors.New("session store: closed")
	// ErrCorrupt 表示记录无法解析（同一文件里连一条合法记录都没有）。
	ErrCorrupt = errors.New("session store: corrupt record")
	// ErrMessageTooLarge 表示单条消息超过 MaxMessageBytes。
	ErrMessageTooLarge = errors.New("session store: message too large")
	// ErrExists 表示 Create 用了已经存在的 ID。
	ErrExists = errors.New("session store: session already exists")
	// ErrInvalidID 表示 ID 不能安全地用作文件名。
	ErrInvalidID = errors.New("session store: invalid session id")
	// ErrInvalidMessage 表示待写入的消息不满足落盘契约（例如缺少 role）。
	//
	// 写侧必须与读侧同样严格：读侧把"没有 role 的记录"当作损坏跳过，因此写侧若
	// 接受它，Append 会返回成功而 Load 读不回来——静默丢数据比直接报错危险得多。
	ErrInvalidMessage = errors.New("session store: invalid message")
)

// Meta 是会话索引条目。它是操作索引的**唯一**入口：List 只读索引，不读数据文件
// （损坏的会话会被按需核对并校正）。
type Meta struct {
	// ID 是会话 ID。为空时由 Create 生成（时间序 + 随机后缀）。
	ID string `json:"id"`
	// Title 是首条用户消息的摘要（≤ MaxTitleRunes 个 rune）。短标题由 Append
	// 在标题为空时自动填充，调用方也可以显式给出。
	Title string `json:"title"`
	// Workspace 是会话所属工作区（精确匹配的过滤维度）。
	Workspace string `json:"workspace"`
	// Model 是会话使用的模型名。
	Model string `json:"model"`
	// CreatedAt 是创建时间（UTC）。
	CreatedAt time.Time `json:"createdAt"`
	// UpdatedAt 是最后一次写入时间（UTC）。List 按它倒序。
	UpdatedAt time.Time `json:"updatedAt"`
	// Messages 是有效消息条数。缺失/半行/损坏行不计入。
	Messages int `json:"messages"`
	// Bytes 是会话文件的物理字节数（含被跳过的损坏字节）；正常路径下等于
	// 有效记录的字节数。它用于 List 用一次 stat 就判断"是否需要核对"。
	Bytes int64 `json:"bytes"`
}

// Filter 是 List 的过滤条件。零值表示"全部会话，不限数量"。
type Filter struct {
	// Workspace 精确匹配 Meta.Workspace；空表示不限。
	Workspace string
	// Since 只保留 UpdatedAt >= Since 的会话；零值不限。
	Since time.Time
	// Limit 是最多返回条数；0 表示不限。
	Limit int
}

// Store 是会话持久化接口。
//
// 实现必须并发安全，并且崩溃安全：任何一次调用中途被强杀，都不得破坏已经写入
// 的数据，也不得让 Store 无法再次打开。
type Store interface {
	// Create 新建一个会话并返回落盘后的 Meta。ID 为空时生成。
	Create(ctx context.Context, meta Meta) (Meta, error)
	// Append 追加一条消息，返回更新后的 Meta。
	Append(ctx context.Context, id string, msg rig.Message) (Meta, error)
	// AppendBatch 以一次写入追加多条消息（全部成功或全部不写）。
	AppendBatch(ctx context.Context, id string, msgs ...rig.Message) (Meta, error)
	// Load 返回会话元数据与全部消息。会话不存在时返回 ErrNotFound。
	Load(ctx context.Context, id string) (Meta, []rig.Message, error)
	// List 按 UpdatedAt 倒序返回匹配的会话索引。
	List(ctx context.Context, filter Filter) ([]Meta, error)
	// Delete 删除会话（索引条目与数据文件）。
	Delete(ctx context.Context, id string) error
	// Close 关闭 Store。之后的调用都返回 ErrClosed；可重复调用。
	Close() error
}

// Option 配置 Dir。
type Option func(*Dir)

// WithNotice 注册诊断回调：损坏行被跳过、索引被校正、会话文件缺失等**不致命**
// 的异常都会通过它上报（format 是 printf 风格）。
//
// 约定：
//   - 回调可能在 Store 内部并发调用，实现必须自己保证安全；
//   - 回调在 Store 的锁**之外**被调用，因此可以在回调里安全地调用 Store；
//   - 回调里的错误不改变返回值：它只是可观测性通道，不参与失败判定。
func WithNotice(notice func(format string, args ...any)) Option {
	return func(d *Dir) {
		if notice != nil {
			d.notice = notice
		}
	}
}

// WithClock 覆盖时间源（默认 time.Now）。它面向确定性测试与需要注入时钟的宿主；
// Meta 的时间戳与 ID 的时间前缀都来自它。
func WithClock(clock func() time.Time) Option {
	return func(d *Dir) {
		if clock != nil {
			d.clock = clock
		}
	}
}

// Dir 是基于目录的 NDJSON 实现。零值不可用，请用 NewDir 构造。
//
// 它同时实现 Store 与（通过 History）rig.History。
type Dir struct {
	// root 是数据根目录（绝对路径）。
	root string
	// sessionsDir 是 <root>/sessions。
	sessionsDir string
	// indexPath 是 <root>/index.json。
	indexPath string

	// indexMu 保护 index（内存索引）与 index.json 的读写。
	// 锁顺序：分片锁 → indexMu。任何路径都不得反向获取，避免死锁。
	indexMu sync.RWMutex
	index   map[string]Meta

	// shards 把"同一会话的写入"串行化。
	shards [shardCount]sync.Mutex

	// closedMu 保护 closed。
	closedMu sync.RWMutex
	closed   bool

	// noticeMu 串行化诊断回调的调用。
	noticeMu sync.Mutex
	notice   func(format string, args ...any)

	// clock 是时间源，构造后不再变化。
	clock func() time.Time

	// processMu 是**同一目录**在本进程内共享的互斥锁（见 processLocks）。
	//
	// 同一进程里可能有两个 Dir 指向同一目录（两个 kit、两个测试实例）。它们的
	// 分片锁互不相识，因此需要这把按目录共享的锁来串行化写入，避免 O_APPEND 写入
	// 交错、索引互相覆盖。它不提供任何跨进程语义。
	//
	// 锁顺序（全局唯一）：processMu → 分片锁 → indexMu。
	processMu *sync.Mutex

	// indexStamp 记录最近一次读到/写出的 index.json 的 (mtime, size)，用于让
	// List 用一次 stat 判断"别的实例是否改过索引"，避免每次 List 都重读。
	indexStamp indexStamp

	// reloadMu 保护 reloadAt（按触发键的重载节流表）与 reloads（自愈重载次数）。
	reloadMu sync.Mutex
	reloadAt map[string]time.Time
	reloads  int
}

// indexStamp 是 index.json 的一个轻量指纹。
type indexStamp struct {
	at   time.Time
	size int64
}

// processLocks 按目录路径共享进程内互斥锁（键是绝对路径）。
var processLocks sync.Map

// processLockFor 返回某个目录的进程内互斥锁（同一目录的多个 Dir 实例共享）。
func processLockFor(dir string) *sync.Mutex {
	actual, _ := processLocks.LoadOrStore(dir, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

// ReloadCountForTest 返回该实例执行过多少次"从磁盘自愈重载"。
//
// 只为测试与诊断存在：正常单实例运行时应恒为 0（证明未命中路径没有偷偷读盘）。
func ReloadCountForTest(d *Dir) int {
	if d == nil {
		return 0
	}
	d.reloadMu.Lock()
	defer d.reloadMu.Unlock()
	return d.reloads
}

// 编译期断言：Dir 实现 Store。
var _ Store = (*Dir)(nil)

// NewDir 打开（必要时创建）一个目录型 Store。
//
// 行为：
//   - 目录不存在时按 0700 创建 <dir>/ 与 <dir>/sessions/；
//   - index.json 存在且可解析时直接加载；
//   - index.json 缺失、无法解析或版本不认识时，**从 sessions/*.ndjson 重建**
//     （条数与标题都能恢复；CreatedAt/UpdatedAt 退化为文件修改时间，Workspace/
//     Model 无法恢复，留空）；
//   - 索引里指向不存在文件的条目会在打开时被丢弃（崩溃在 Delete 中途的残留）。
func NewDir(dir string, opts ...Option) (*Dir, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("session store: empty directory")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("session store: resolve %q: %w", dir, err)
	}
	store := &Dir{
		root:        absolute,
		sessionsDir: filepath.Join(absolute, sessionsDirName),
		indexPath:   filepath.Join(absolute, indexFileName),
		index:       make(map[string]Meta),
		processMu:   processLockFor(absolute),
		clock:       time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(store)
		}
	}
	if err := os.MkdirAll(store.sessionsDir, 0o700); err != nil {
		return nil, fmt.Errorf("session store: create %q: %w", store.sessionsDir, err)
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

// Dir 返回数据根目录（便于宿主做备份/诊断）。
func (d *Dir) Dir() string { return d.root }

// Create 实现 Store。
//
// 计数器字段（Messages/Bytes）由 Store 拥有：调用方传入的值会被忽略，避免索引与
// 文件内容不一致。
func (d *Dir) Create(ctx context.Context, meta Meta) (Meta, error) {
	if err := d.precheck(ctx); err != nil {
		return Meta{}, err
	}
	now := d.clock().UTC()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	} else {
		meta.CreatedAt = meta.CreatedAt.UTC()
	}
	if meta.UpdatedAt.IsZero() {
		meta.UpdatedAt = meta.CreatedAt
	} else {
		meta.UpdatedAt = meta.UpdatedAt.UTC()
	}
	meta.Title = clampTitle(meta.Title)
	meta.Messages = 0
	meta.Bytes = 0

	d.processMu.Lock()
	defer d.processMu.Unlock()

	// 先并进磁盘上已有、而本地没有的条目：Create 不能因为自己的索引视图陈旧
	// 就把别的实例刚建的会话从 index.json 里抹掉。
	d.mergeDiskIndex()

	d.indexMu.Lock()
	defer d.indexMu.Unlock()

	if meta.ID != "" {
		if err := checkID(meta.ID); err != nil {
			return Meta{}, err
		}
		if _, exists := d.index[meta.ID]; exists {
			return Meta{}, fmt.Errorf("%w: %q", ErrExists, meta.ID)
		}
	} else {
		id, err := d.generateIDLocked(meta.CreatedAt)
		if err != nil {
			return Meta{}, err
		}
		meta.ID = id
	}
	// 文件名冲突（同一个 stem 被不同 ID 占用）在这里暴露，而不是静默覆盖别人的文件。
	if owner, ok := d.ownerOfStemLocked(fileStem(meta.ID)); ok && owner != meta.ID {
		return Meta{}, fmt.Errorf("%w: %q 与已有会话 %q 的文件名冲突", ErrInvalidID, meta.ID, owner)
	}
	path := d.sessionPath(meta.ID)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return Meta{}, fmt.Errorf("%w: %q", ErrExists, meta.ID)
		}
		return Meta{}, fmt.Errorf("session store: create session file %q: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return Meta{}, fmt.Errorf("session store: sync session file %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return Meta{}, fmt.Errorf("session store: close session file %q: %w", path, err)
	}
	if err := syncDir(d.sessionsDir); err != nil {
		_ = os.Remove(path)
		return Meta{}, fmt.Errorf("session store: sync sessions dir: %w", err)
	}

	d.index[meta.ID] = meta
	if err := d.saveIndexLocked(); err != nil {
		delete(d.index, meta.ID)
		_ = os.Remove(path)
		return Meta{}, err
	}
	return meta, nil
}

// Append 实现 Store。
func (d *Dir) Append(ctx context.Context, id string, msg rig.Message) (Meta, error) {
	return d.AppendBatch(ctx, id, msg)
}

// AppendBatch 实现 Store：整批消息一次写入、一次 Sync，索引在文件之后更新。
func (d *Dir) AppendBatch(ctx context.Context, id string, msgs ...rig.Message) (Meta, error) {
	if err := d.precheck(ctx); err != nil {
		return Meta{}, err
	}
	if err := checkID(id); err != nil {
		return Meta{}, err
	}
	if len(msgs) == 0 {
		// 空追加是已存在的会话上的合法 no-op（History 在压缩后可能这么调用）。
		meta, ok := d.lookupWithRefresh(ctx, id)
		if !ok {
			return Meta{}, fmt.Errorf("%w: %q", ErrNotFound, id)
		}
		return meta, nil
	}
	// 序列化与上限校验放在锁外：失败时连一行都不写。
	payload, err := encodeMessages(msgs)
	if err != nil {
		return Meta{}, err
	}

	// 别的实例可能改过索引（它自己的 Append/Create）：先按需刷新，
	// 保证下面的整份索引保存在磁盘条目的超集上。
	d.refreshBeforeWrite(ctx, id)
	// 未命中时再尝试一次从磁盘自愈（另一实例刚 Create / 索引被覆盖）。
	// 必须在拿 processMu 之前做：重载自身不持该锁，但拿锁后再重载会自锁。
	if _, ok := d.lookupWithRefresh(ctx, id); !ok {
		return Meta{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}

	d.processMu.Lock()
	defer d.processMu.Unlock()

	lock := d.shardLock(id)
	lock.Lock()
	defer lock.Unlock()

	if _, ok := d.lookup(id); !ok {
		return Meta{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	path := d.sessionPath(id)
	if err := appendRecords(path, payload); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 索引里有、文件没了（被外部删除 / Delete 中途崩溃）：与 Load/List 采用
			// 同一种自愈策略——丢弃条目并报 ErrNotFound，而不是重建一个只剩新数据的
			// 文件、让索引计数停在"旧 + 新"的谎言上。
			d.dropSession(id, "会话文件缺失")
			return Meta{}, fmt.Errorf("%w: %q", ErrNotFound, id)
		}
		return Meta{}, err
	}
	size, err := fileSize(path)
	if err != nil {
		return Meta{}, fmt.Errorf("session store: stat session file %q: %w", path, err)
	}
	return d.commitAppend(id, len(msgs), size, firstUserTitle(msgs))
}

// Load 实现 Store。
//
// 尾部半行被丢弃；中间损坏的行被跳过（通过 Notice 上报）；条数与索引不一致时
// 校正索引（崩溃自愈）。
func (d *Dir) Load(ctx context.Context, id string) (Meta, []rig.Message, error) {
	if err := d.precheck(ctx); err != nil {
		return Meta{}, nil, err
	}
	if err := checkID(id); err != nil {
		return Meta{}, nil, fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	// 同一会话的"读满 + 校正索引"必须与 Append 互斥：否则会读到追加之前的
	// 文件，却在追加提交之后把索引改回旧条数（逻辑竞争，-race 抓不到）。
	lock := d.shardLock(id)
	lock.Lock()
	defer lock.Unlock()

	meta, ok := d.lookupWithRefresh(ctx, id)
	if !ok {
		return Meta{}, nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	read, err := readRecords(d.sessionPath(id))
	if err != nil {
		if errors.Is(err, ErrCorrupt) {
			d.emitAll(read.notices)
			return Meta{}, nil, err
		}
		if errors.Is(err, os.ErrNotExist) {
			// 索引里有、文件没了（Delete 中途崩溃 / 被外部删除）：丢掉条目自愈。
			d.dropSession(id, "会话文件缺失")
			return Meta{}, nil, fmt.Errorf("%w: %q", ErrNotFound, id)
		}
		d.emitAll(read.notices)
		return Meta{}, nil, err
	}
	if read.records != meta.Messages || read.size != meta.Bytes {
		meta = d.correctMeta(id, read.records, read.size, read.notices)
	} else {
		d.emitAll(read.notices)
	}
	return meta, read.messages, nil
}

// List 实现 Store：按 UpdatedAt 倒序（相同则 ID 倒序，保证结果确定）。
//
// 索引里 Bytes 与会话文件实际大小不一致的会话会被**按需核对**（这是崩溃/半行
// 留下的痕迹）；正常会话只读索引，不读数据文件。
func (d *Dir) List(ctx context.Context, filter Filter) ([]Meta, error) {
	if err := d.precheck(ctx); err != nil {
		return nil, err
	}
	// 另一个实例可能刚改过索引：一次 stat 判断，变了才重载（单实例不读盘）。
	d.refreshListIfStale(ctx)

	d.indexMu.RLock()
	snapshot := make([]Meta, 0, len(d.index))
	for _, meta := range d.index {
		snapshot = append(snapshot, meta)
	}
	d.indexMu.RUnlock()

	var notices []string
	var ghosts []string
	kept := snapshot[:0]
	for _, meta := range snapshot {
		info, err := os.Stat(d.sessionPath(meta.ID))
		switch {
		case errors.Is(err, os.ErrNotExist):
			ghosts = append(ghosts, meta.ID)
			notices = append(notices, fmt.Sprintf("session store: 丢弃索引里已无文件的会话 %q", meta.ID))
			continue
		case err != nil:
			notices = append(notices, fmt.Sprintf("session store: 无法 stat 会话 %q: %v", meta.ID, err))
			kept = append(kept, meta)
			continue
		}
		if info.Size() != meta.Bytes {
			lock := d.shardLock(meta.ID)
			lock.Lock()
			read, readErr := readRecords(d.sessionPath(meta.ID))
			notices = append(notices, read.notices...)
			if readErr != nil {
				// 损坏但仍在索引里：保留条目（Load 会给调用方明确错误），
				// 只把物理大小记下来，避免每次 List 都重读。
				if corrected := d.correctMeta(meta.ID, meta.Messages, info.Size(), nil); corrected.ID != "" {
					meta = corrected
				}
				lock.Unlock()
				kept = append(kept, meta)
				continue
			}
			if corrected := d.correctMeta(meta.ID, read.records, read.size, nil); corrected.ID != "" {
				meta = corrected
			}
			lock.Unlock()
		}
		kept = append(kept, meta)
	}
	for _, id := range ghosts {
		d.removeIndexEntry(id, "会话文件缺失")
	}
	d.emitAll(notices)

	out := make([]Meta, 0, len(kept))
	for _, meta := range kept {
		if filter.Workspace != "" && meta.Workspace != filter.Workspace {
			continue
		}
		if !filter.Since.IsZero() && meta.UpdatedAt.Before(filter.Since) {
			continue
		}
		out = append(out, meta)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// Delete 实现 Store：先删数据文件，再删索引条目（崩溃在中间只会留下孤儿文件，
// 不会留下"索引里有、数据没了"的假会话）。
func (d *Dir) Delete(ctx context.Context, id string) error {
	if err := d.precheck(ctx); err != nil {
		return err
	}
	if err := checkID(id); err != nil {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	d.refreshBeforeWrite(ctx, id)
	if _, ok := d.lookupWithRefresh(ctx, id); !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	d.processMu.Lock()
	defer d.processMu.Unlock()
	lock := d.shardLock(id)
	lock.Lock()
	defer lock.Unlock()

	if err := os.Remove(d.sessionPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session store: remove session file %q: %w", d.sessionPath(id), err)
	}
	if err := syncDir(d.sessionsDir); err != nil {
		return fmt.Errorf("session store: sync sessions dir: %w", err)
	}
	d.indexMu.Lock()
	defer d.indexMu.Unlock()
	if _, ok := d.index[id]; !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	delete(d.index, id)
	return d.saveIndexLocked()
}

// Replace 原子替换某个会话的全部消息（实现 rig.HistoryReplacer 的底层语义）。
//
// 用于上下文压缩：把 ndjson 整体重写到临时文件（tmp + rename），因此读者永远
// 看到"旧的完整内容"或"新的完整内容"。会话必须已存在，否则返回 ErrNotFound；
// History.Replace 会在缺失时自动建会话。
//
// 注意：它不保留旧记录的任何副本（压缩后的窗口就是唯一事实），需要审计请自行
// 备份 sessions 目录。
func (d *Dir) Replace(ctx context.Context, id string, msgs []rig.Message) (Meta, error) {
	if err := d.precheck(ctx); err != nil {
		return Meta{}, err
	}
	if err := checkID(id); err != nil {
		return Meta{}, err
	}
	payload, err := encodeMessages(msgs)
	if err != nil {
		return Meta{}, err
	}
	d.refreshBeforeWrite(ctx, id)
	if _, ok := d.lookupWithRefresh(ctx, id); !ok {
		return Meta{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	d.processMu.Lock()
	defer d.processMu.Unlock()
	lock := d.shardLock(id)
	lock.Lock()
	defer lock.Unlock()

	if _, ok := d.lookup(id); !ok {
		return Meta{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	path := d.sessionPath(id)
	if err := replaceRecords(path, payload); err != nil {
		return Meta{}, err
	}
	size, err := fileSize(path)
	if err != nil {
		return Meta{}, fmt.Errorf("session store: stat session file %q: %w", path, err)
	}
	return d.commitReplace(id, len(msgs), size, firstUserTitle(msgs))
}

// Close 实现 Store。
func (d *Dir) Close() error {
	d.closedMu.Lock()
	defer d.closedMu.Unlock()
	d.closed = true
	return nil
}

// precheck 统一处理"已关闭"与 context 取消。
func (d *Dir) precheck(ctx context.Context) error {
	d.closedMu.RLock()
	closed := d.closed
	d.closedMu.RUnlock()
	if closed {
		return ErrClosed
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// lookup 读一次索引。
func (d *Dir) lookup(id string) (Meta, bool) {
	d.indexMu.RLock()
	defer d.indexMu.RUnlock()
	meta, ok := d.index[id]
	return meta, ok
}

// commitAppend 在索引锁内累加计数并持久化索引。调用方必须持有该会话的分片锁。
func (d *Dir) commitAppend(id string, delta int, size int64, derivedTitle string) (Meta, error) {
	return d.commit(id, delta, true, size, derivedTitle)
}

// commitReplace 在索引锁内**覆盖**计数并持久化索引（Replace/压缩用）。
//
// 与 commitAppend 的区别是语义而不是风格：Replace 之后文件里只有 len(msgs) 条，
// 累加会让索引永久偏大（Bytes 又是对的，List 因此不会触发核对）。
func (d *Dir) commitReplace(id string, total int, size int64, derivedTitle string) (Meta, error) {
	return d.commit(id, total, false, size, derivedTitle)
}

// commit 是两条路径的公共实现：accumulate 为 true 时累加，否则覆盖。
// 调用方必须持有该会话的分片锁。
func (d *Dir) commit(id string, count int, accumulate bool, size int64, derivedTitle string) (Meta, error) {
	d.indexMu.Lock()
	defer d.indexMu.Unlock()
	meta, ok := d.index[id]
	if !ok {
		return Meta{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if accumulate {
		meta.Messages += count
	} else {
		meta.Messages = count
	}
	meta.Bytes = size
	meta.UpdatedAt = d.clock().UTC()
	if meta.Title == "" && derivedTitle != "" {
		meta.Title = clampTitle(derivedTitle)
	}
	d.index[id] = meta
	if err := d.saveIndexLocked(); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

// correctMeta 用真实读数校正索引并回写（崩溃自愈点）。返回校正后的 Meta。
// 第二个返回值为空 Meta 表示该会话已不在索引里。
func (d *Dir) correctMeta(id string, records int, size int64, notices []string) Meta {
	d.indexMu.Lock()
	meta, ok := d.index[id]
	if !ok {
		d.indexMu.Unlock()
		d.emitAll(notices)
		return Meta{}
	}
	changed := meta.Messages != records || meta.Bytes != size
	meta.Messages = records
	meta.Bytes = size
	d.index[id] = meta
	var saveErr error
	if changed {
		saveErr = d.saveIndexLocked()
	}
	d.indexMu.Unlock()

	if changed {
		notices = append(notices, fmt.Sprintf("session store: 校正会话 %q 的索引为 %d 条 / %d 字节", id, records, size))
	}
	if saveErr != nil {
		notices = append(notices, fmt.Sprintf("session store: 回写索引失败: %v", saveErr))
	}
	d.emitAll(notices)
	return meta
}

// removeIndexEntry 删除一条索引记录（用于自愈丢弃幽灵会话）。
func (d *Dir) removeIndexEntry(id, reason string) {
	d.indexMu.Lock()
	_, ok := d.index[id]
	if ok {
		delete(d.index, id)
		_ = d.saveIndexLocked()
	}
	d.indexMu.Unlock()
	if ok {
		d.emitf("session store: %s，已从索引中移除 %q", reason, id)
	}
}

// dropSession 是 Load 发现"索引有、文件无"时的自愈入口。
func (d *Dir) dropSession(id, reason string) { d.removeIndexEntry(id, reason) }

// emitAll 在锁外按顺序上报诊断。
func (d *Dir) emitAll(notices []string) {
	for _, notice := range notices {
		d.emitf("%s", notice)
	}
}

// emitf 调用 Notice 回调（串行化）。
func (d *Dir) emitf(format string, args ...any) {
	if d.notice == nil {
		return
	}
	d.noticeMu.Lock()
	defer d.noticeMu.Unlock()
	if d.notice != nil {
		d.notice(format, args...)
	}
}

// shardLock 返回某个会话 ID 对应的分片锁。
func (d *Dir) shardLock(id string) *sync.Mutex {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(id))
	return &d.shards[int(hasher.Sum32()%shardCount)]
}

// sessionPath 返回会话数据文件路径。
func (d *Dir) sessionPath(id string) string {
	return filepath.Join(d.sessionsDir, fileStem(id)+sessionExt)
}

// ownerOfStemLocked 报告某个文件名 stem 是否已被别的会话占用。调用方需持有索引锁。
func (d *Dir) ownerOfStemLocked(stem string) (string, bool) {
	for id := range d.index {
		if fileStem(id) == stem {
			return id, true
		}
	}
	return "", false
}

// generateIDLocked 生成一个未被占用的时间序 ID。调用方需持有索引锁。
func (d *Dir) generateIDLocked(at time.Time) (string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		var suffix [4]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", fmt.Errorf("session store: read entropy for session id: %w", err)
		}
		id := at.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(suffix[:])
		if _, exists := d.index[id]; exists {
			continue
		}
		if _, statErr := os.Stat(d.sessionPath(id)); statErr == nil {
			continue
		}
		return id, nil
	}
	return "", errors.New("session store: could not generate a unique session id")
}

// fileStem 把会话 ID 映射成安全的文件名（不含路径分隔符）。
//
// 合法 ID（字母/数字/-/_/./@）直接使用，文件名可读；其它 ID（例如宿主用带 "/"
// 的会话键）退化为 "id-<sha256 前 16 字节>"，仍能安全落盘。注意：后者在索引丢失
// 后无法从文件名还原原始 ID，重建时会把哈希名当作 ID（见 NewDir 文档）。
func fileStem(id string) string {
	if validStem(id) {
		return id
	}
	return "id-" + hashStem(id)
}

// checkID 校验外部传入的会话 ID。
//
// 拒绝的是**危险或歧义**的 ID（路径分隔符、只有点、隐藏文件前缀、控制字符、
// 超长），因为它们会让"会话 ID"与"文件名"的关系变得不安全或不透明；其它怪异
// 字符（空格、中文、Windows 保留标点）由 fileStem 哈希编码后照样能落盘。
func checkID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: 空 ID", ErrInvalidID)
	}
	if len(id) > maxIDLength {
		return fmt.Errorf("%w: 长度 %d 超过上限 %d", ErrInvalidID, len(id), maxIDLength)
	}
	if id == "." || id == ".." || strings.HasPrefix(id, ".") {
		return fmt.Errorf("%w: %q 不能用作会话 ID（点前缀/点目录）", ErrInvalidID, id)
	}
	for _, r := range id {
		if r == '/' || r == '\\' || r == 0 || r < 0x20 {
			return fmt.Errorf("%w: %q 含有路径分隔符或控制字符", ErrInvalidID, id)
		}
	}
	return nil
}

// windowsReservedStems 是 Windows 的保留设备名。
//
// Windows 的路径解析器把"设备名[:任意扩展名]"整体当作设备：在 Windows 上
// <dir>/con.ndjson 打开的是控制台设备而不是文件，Create 因此会拿不到一个真正的
// 会话文件（错误码与"ID 冲突"毫无关系），却可能已经把索引条目写下去。
// 这些名字必须在文件名映射阶段退化到哈希形式（ID 本身仍然合法）。
var windowsReservedStems = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {}, "CONIN$": {}, "CONOUT$": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

// isWindowsReservedStem 报告一个文件名 stem 是否会被 Windows 当作设备名。
//
// 只看第一个 "." 之前的部分：con.txt → 设备 "con"。
func isWindowsReservedStem(id string) bool {
	base := id
	if index := strings.IndexByte(base, '.'); index >= 0 {
		base = base[:index]
	}
	_, reserved := windowsReservedStems[strings.ToUpper(base)]
	return reserved
}

// validStem 报告 ID 能否直接作为文件名。
func validStem(id string) bool {
	if id == "" || len(id) > maxIDLength {
		return false
	}
	switch id {
	case ".", "..":
		return false
	}
	if isWindowsReservedStem(id) {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '@':
		default:
			return false
		}
	}
	return true
}

// firstUserTitle 返回一批消息里首条用户消息的摘要（没有则空串）。
func firstUserTitle(msgs []rig.Message) string {
	for _, msg := range msgs {
		if msg.Role != rig.RoleUser {
			continue
		}
		if text := summarize(msg.Text()); text != "" {
			return text
		}
	}
	return ""
}

// summarize 把一段文本压成单行摘要（折叠空白、按 rune 截断到 MaxTitleRunes）。
func summarize(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	return clampTitle(strings.Join(fields, " "))
}

// clampTitle 把标题限制在 MaxTitleRunes 个 rune 以内（含省略号）。
func clampTitle(title string) string {
	title = strings.TrimSpace(title)
	runes := []rune(title)
	if len(runes) <= MaxTitleRunes {
		return title
	}
	return strings.TrimRight(string(runes[:MaxTitleRunes-1]), " ") + "…"
}
