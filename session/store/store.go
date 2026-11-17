// Package store 提供会话历史的内置落盘实现：一个会话一个 NDJSON 文件。
//
// 定位是**个人 agent 客户端**：单用户、单进程、单实例。因此这里刻意不做索引
// 文件、不做跨实例协调、不做文件锁——磁盘布局就是"一个会话一个文件"：
//
//	<dir>/<id>.ndjson   —— 一行一条 rig.Message（追加写 + fsync）
//
// # 索引 = 扫目录
//
// NewDir 时扫一遍 <dir>，按文件名（→ 会话 ID）与文件 mtime/大小生成内存索引：
//
//   - ID 来自文件名（不安全字符的 ID 用可逆编码落盘，重启后能还原原始 ID）；
//   - UpdatedAt 用文件 mtime（List 按它倒序，即"最近会话在最前"的续传顺序）；
//   - Bytes 用文件大小；
//   - **Title**：默认开启 `WithScanContents` 时，顺带读每个会话的**开头一小段**
//     （≤ 256 KiB、最多前 4 条记录）找出首条 user 消息作为标题——"最近会话"侧栏
//     必须有标题，没有标题的列表等于没用。代价是 O(会话数) 的小读，不是 O(总字节)。
//     不需要标题（超大目录极致启动）时用 `WithScanContents(false)` 关掉；
//   - **Messages** 始终为 0，等该会话第一次 Load/Append 时补全（数条数必须读完整
//     文件，不值得在启动时付这个钱）。
//
// 之后 Create/Append/Replace/Delete 只维护内存索引，**不落任何索引文件**：没有
// 索引文件，就没有"索引与数据不一致"这一问题，重启也不会有自愈/重建逻辑。
//
// # 崩溃安全（写入顺序即协议）
//
//   - Append/AppendBatch 把整批消息编码成 NDJSON 后**一次写入 + Sync**；
//   - Create 用 O_CREATE|O_EXCL 建空文件，成功后才加入内存索引；
//   - Replace（上下文压缩）写临时文件后 rename，读者只会看到旧的或新的完整内容。
//
// 文件尾部若出现"没有换行结尾的半行"（写一行时的撕裂），读取时丢弃该行；后续
// Append 会先把半行 Truncate 掉再追加，避免新记录粘在坏行后面。
//
// # 损坏容忍
//
// 中间某行不是合法 rig.Message（位翻转、被编辑器改坏）时跳过该行并通过 WithNotice
// 上报；只有"文件里的完整行全部无法解析"才返回 ErrCorrupt——让调用方显式决策，
// 而不是静默丢光会话。
//
// # 并发
//
// 单实例内并发安全：同一会话的写入/读取按 ID 分片串行化，索引由一把 RWMutex 保护。
// **同目录多实例（多进程，或同进程多个 Dir）不受支持**：两个实例各自的内存索引
// 是独立的，因此看不到对方新建/写入的会话——数据都在盘上（不会丢），但列表不完整。
// 一个目录请只由一个 Dir 使用；"重启后继续"只需要关掉旧实例再 NewDir——索引就是
// 扫目录，没有迁移，也没有需要修复的索引文件。
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
//	// 会话列表 / 续传 UI 就是 List + Load：
//	metas, err := st.List(ctx, store.Filter{Limit: 20}) // 最近的在最前
//	meta, messages, err := st.Load(ctx, metas[0].ID)
package store

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
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
	// sessionExt 是会话文件名后缀。
	sessionExt = ".ndjson"
	// shardCount 是按 ID 分片的互斥锁数量。
	shardCount = 64
	// maxIDLength 是外部传入 ID 的长度上限。
	maxIDLength = 128
	// maxEncodedIDBytes 是"可逆编码文件名"能接受的 ID 字节数上限。
	// 编码后长度 = 4(前缀) + 2×字节数，留出 255 字节文件名的余量。
	maxEncodedIDBytes = 96
	// encodedStemPrefix 是可逆编码文件名的前缀。
	encodedStemPrefix = "enc-"
	// hashedStemPrefix 是不可逆哈希文件名的前缀（仅超长 ID 使用）。
	hashedStemPrefix = "id-"
	// titleScanBytes 是扫目录时为"取标题"每个会话最多读取的字节数。
	// 单条记录上限是 MaxMessageBytes，但标题只需要开头几条，256 KiB 足够且够便宜。
	titleScanBytes = 256 << 10
	// titleScanRecords 是扫目录时最多检查的前几条记录（用于找首条 user 消息）。
	titleScanRecords = 4
	// titleScanDiscardBytes 是"跳过一条超长记录"时最多丢弃的字节数。
	//
	// 单行超过 titleScanBytes 时我们跳过它继续找标题（用户"粘贴一大段日志开场"很
	// 常见，不该让侧栏永久空白）；但为了不给启动成本留下无界项，整次扫描丢弃的
	// 字节总数受此预算约束，超出就放弃（标题留空）。
	titleScanDiscardBytes = 4 << 20
	// titleScanBufferBytes 是逐行读取时的缓冲大小（按需增长，不按上限预分配）。
	titleScanBufferBytes = 4 << 10
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

// Meta 是会话索引条目（内存索引：启动时扫目录，之后由增删改维护）。
type Meta struct {
	// ID 是会话 ID。为空时由 Create 生成（时间序 + 随机后缀）。
	ID string `json:"id"`
	// Title 是首条用户消息的摘要（≤ MaxTitleRunes 个 rune）。短标题由 Append
	// 在标题为空时自动填充，调用方也可以显式给出。
	//
	// 注意：进程刚启动、该会话还没被 Load/Append 过时为空——扫目录不读内容。
	Title string `json:"title"`
	// Workspace 是会话所属工作区（精确匹配的过滤维度）。
	//
	// 注意：它只存在于内存索引里，重启后无法从文件恢复（留空）。
	Workspace string `json:"workspace"`
	// Model 是会话使用的模型名（同上，重启后留空）。
	Model string `json:"model"`
	// CreatedAt 是创建时间（UTC）。重启后从文件重建时退化为文件 mtime。
	CreatedAt time.Time `json:"createdAt"`
	// UpdatedAt 是最后一次写入时间（UTC）。List 按它倒序。
	UpdatedAt time.Time `json:"updatedAt"`
	// Messages 是有效消息条数（缺失/半行/损坏行不计入）；重启后未被 Load/Append
	// 过的会话为 0（扫目录不读内容）。
	Messages int `json:"messages"`
	// Bytes 是会话文件的物理字节数（含被跳过的损坏字节）。
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
	// List 按 UpdatedAt 倒序返回匹配的会话（只读内存索引，不读数据文件）。
	List(ctx context.Context, filter Filter) ([]Meta, error)
	// Delete 删除会话（数据文件 + 内存索引条目）。
	Delete(ctx context.Context, id string) error
	// Close 关闭 Store。之后的调用都返回 ErrClosed；可重复调用。
	Close() error
}

// Option 配置 Dir。
type Option func(*Dir)

// WithNotice 注册诊断回调：损坏行被跳过、会话文件缺失等**不致命**的异常都会通过
// 它上报（format 是 printf 风格）。
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

// WithScanContents 控制扫目录时是否顺带读会话开头以恢复标题（**默认开启**）。
//
// 无参数表示开启（`WithScanContents()`），`WithScanContents(false)` 关闭。
//
// 开启时 NewDir 会为每个会话读一小段开头（≤ 256 KiB、最多前 4 条记录），取首条
// user 消息压成标题（≤ MaxTitleRunes 个 rune）；这让"最近会话"侧栏在重启后立刻有
// 标题。关闭时只 stat、不读内容——超大目录（上万会话）追求极致启动速度时用。
//
// 无论开关，Messages 都不在扫目录时统计（数条数要读完整文件），等 Load 补全。
func WithScanContents(enabled ...bool) Option {
	value := true
	if len(enabled) > 0 {
		value = enabled[0]
	}
	return func(d *Dir) {
		d.scanContents = value
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
//
// 一个目录**只应由一个 Dir 使用**（单进程单实例），见包文档。
type Dir struct {
	// root 是数据根目录（绝对路径），会话文件直接放在它下面。
	root string

	// indexMu 保护 index（内存索引）。
	// 锁顺序：分片锁 → indexMu。任何路径都不得反向获取，避免死锁。
	indexMu sync.RWMutex
	index   map[string]Meta

	// shards 把"同一会话的写入/读取"串行化。
	shards [shardCount]sync.Mutex

	// closedMu 保护 closed。
	closedMu sync.RWMutex
	closed   bool

	// noticeMu 串行化诊断回调的调用。
	noticeMu sync.Mutex
	notice   func(format string, args ...any)

	// clock 是时间源，构造后不再变化。
	clock func() time.Time

	// scanContents 决定扫目录时是否读会话开头以恢复标题（默认 true）。
	scanContents bool
}

// 编译期断言：Dir 实现 Store。
var _ Store = (*Dir)(nil)

// NewDir 打开（必要时创建）一个目录型 Store，并扫一遍目录建内存索引。
//
// 扫目录只看文件名与 mtime/大小（不读内容）：所以重启后 List 里的 Messages/Title
// 要等该会话被 Load/Append 过才有值。子目录、点文件与非 .ndjson 条目一律忽略
// （包括旧版本遗留的 sessions/ 子目录——不迁移，也不当成会话）。
func NewDir(dir string, opts ...Option) (*Dir, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("session store: empty directory")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("session store: resolve %q: %w", dir, err)
	}
	store := &Dir{
		root:         absolute,
		index:        make(map[string]Meta),
		clock:        time.Now,
		scanContents: true,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(store)
		}
	}
	if err := os.MkdirAll(store.root, 0o700); err != nil {
		return nil, fmt.Errorf("session store: create %q: %w", store.root, err)
	}
	if err := store.scan(); err != nil {
		return nil, err
	}
	return store, nil
}

// scan 扫一遍目录，按文件名 + mtime/大小建内存索引。
func (d *Dir) scan() error {
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return fmt.Errorf("session store: read dir %q: %w", d.root, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, sessionExt) {
			continue
		}
		id, ok := idFromStem(strings.TrimSuffix(name, sessionExt))
		if !ok {
			// 不是本包写的文件名：不猜它的 ID，直接忽略（文件仍在，不会丢）。
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("session store: stat %q: %w", name, statErr)
		}
		stamp := info.ModTime().UTC()
		meta := Meta{
			ID:        id,
			CreatedAt: stamp,
			UpdatedAt: stamp,
			Bytes:     info.Size(),
		}
		if d.scanContents {
			// 读失败/损坏只影响这一条（Notice 一次），不阻断整个启动。
			meta.Title = d.scanTitle(id)
		}
		d.index[id] = meta
	}
	return nil
}

// scanTitle 读一个会话文件的开头，返回首条 user 消息的标题。
//
// 只读必要的字节：逐行读取、最多检查前 titleScanRecords 条记录、单行上限
// titleScanBytes——标题不值得为"万一会话"付 O(总字节) 的启动成本，也不该为一行
// 超长内容分配大缓冲。空文件/全损坏都只是"没有标题"，不阻断启动。
//
// **超长记录会被跳过而不是终止扫描**：用户"粘贴一大段日志开场"很常见，如果第一条
// 就放弃，这个会话在侧栏里会永远没有标题。因此单行超过 titleScanBytes 时丢弃该行
// 剩余字节并继续看下一条（丢弃总量受 titleScanDiscardBytes 约束，避免病态文件把
// 启动拖住）。全部记录都超长或预算用尽时才留空，并通过 Notice 上报一次。
func (d *Dir) scanTitle(id string) string {
	file, err := os.Open(d.sessionPath(id))
	if err != nil {
		d.emitf("session store: 读取会话 %q 的开头失败，标题留空: %v", id, err)
		return ""
	}
	defer func() { _ = file.Close() }()

	reader := bufio.NewReaderSize(file, titleScanBufferBytes)
	checked := 0
	skipped := 0
	// oversize 统计被跳过的超长记录条数；discarded 是它们消耗的丢弃预算。
	oversize := 0
	discarded := int64(0)
	for checked < titleScanRecords {
		line, complete, tooLong, readErr := readLineLimited(reader, titleScanBytes)
		if tooLong {
			// 跳过这条记录的剩余字节，继续看后面几条——不能因为第一条是"粘贴的大
			// 日志"就让这个会话在侧栏里永远没有标题。
			consumed, reachedEnd, discardErr := discardLine(reader, titleScanDiscardBytes-discarded)
			if discardErr != nil {
				d.emitf("session store: 读取会话 %q 的开头失败，标题留空: %v", id, discardErr)
				return ""
			}
			discarded += consumed
			oversize++
			checked++
			if !reachedEnd || discarded >= titleScanDiscardBytes {
				// 预算用尽或该行还没结束：继续扫不划算，放弃标题（数据本身不受影响）。
				break
			}
			continue
		}
		if readErr != nil {
			d.emitf("session store: 读取会话 %q 的开头失败，标题留空: %v", id, readErr)
			return ""
		}
		if len(line) == 0 && !complete {
			break // 文件结束
		}
		if complete {
			checked++
			if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
				var message rig.Message
				if err := json.Unmarshal(trimmed, &message); err != nil {
					skipped++
				} else if message.Role == rig.RoleUser {
					if title := summarize(message.Text()); title != "" {
						if oversize > 0 {
							d.emitf("session store: 会话 %q 的 %d 条记录超过 %d 字节，已跳过并继续查找标题",
								id, oversize, titleScanBytes)
						}
						return title
					}
				}
			}
			continue
		}
		// 没有结尾换行的尾巴是未完成的写入（半行），不是记录。
		break
	}
	if oversize > 0 {
		d.emitf("session store: 会话 %q 的 %d 条记录超过 %d 字节，已跳过并继续查找标题（未找到可用的 user 消息）",
			id, oversize, titleScanBytes)
	}
	if skipped > 0 {
		d.emitf("session store: 会话 %q 的开头有 %d 行无法解析，标题留空", id, skipped)
	}
	return ""
}

// readLineLimited 读一行（含结尾换行），但不超过 limit 字节。
//
// 返回的 complete 表示这一行以换行结尾（完整记录）；false 表示到达 EOF 且没有
// 换行（半行）。tooLong 表示该行超过 limit（调用方放弃并上报）。
func readLineLimited(reader *bufio.Reader, limit int) (line []byte, complete, tooLong bool, err error) {
	for {
		fragment, readErr := reader.ReadSlice('\n')
		if len(line)+len(fragment) > limit {
			return nil, false, true, nil
		}
		line = append(line, fragment...)
		switch {
		case readErr == nil:
			// ReadSlice 找到换行时返回 nil error，且 fragment 以换行结尾。
			return line[:len(line)-1], true, false, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF):
			return line, false, false, nil
		default:
			return nil, false, false, readErr
		}
	}
}

// discardLine 丢弃"当前行"的剩余字节（最多 budget 字节）。
//
// 用在"这一行超长、跳过它继续找标题"的路径上：readLineLimited 命中上限时停在行中间，
// 必须先把它消费到行尾，下一次 ReadSlice 才会读到下一条记录。reachedEnd 表示真的读到
// 了换行；false 表示预算用尽（调用方应当放弃扫描）。
func discardLine(reader *bufio.Reader, budget int64) (consumed int64, reachedEnd bool, err error) {
	if budget < 0 {
		budget = 0
	}
	for {
		fragment, readErr := reader.ReadSlice('\n')
		consumed += int64(len(fragment))
		switch {
		case readErr == nil:
			return consumed, true, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			if consumed >= budget {
				return consumed, false, nil
			}
			continue
		case errors.Is(readErr, io.EOF):
			return consumed, false, nil
		default:
			return consumed, false, readErr
		}
	}
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
	if err := syncDir(d.root); err != nil {
		_ = os.Remove(path)
		return Meta{}, fmt.Errorf("session store: sync directory: %w", err)
	}

	d.index[meta.ID] = meta
	return meta, nil
}

// Append 实现 Store。
func (d *Dir) Append(ctx context.Context, id string, msg rig.Message) (Meta, error) {
	return d.AppendBatch(ctx, id, msg)
}

// AppendBatch 实现 Store：整批消息一次写入、一次 Sync，最后更新内存索引。
func (d *Dir) AppendBatch(ctx context.Context, id string, msgs ...rig.Message) (Meta, error) {
	if err := d.precheck(ctx); err != nil {
		return Meta{}, err
	}
	if err := checkID(id); err != nil {
		return Meta{}, err
	}
	if len(msgs) == 0 {
		// 空追加是已存在的会话上的合法 no-op（History 在压缩后可能这么调用）。
		if meta, ok := d.lookup(id); ok {
			return meta, nil
		}
		return Meta{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	// 序列化与上限校验放在锁外：失败时连一行都不写。
	payload, err := encodeMessages(msgs)
	if err != nil {
		return Meta{}, err
	}

	lock := d.shardLock(id)
	lock.Lock()
	defer lock.Unlock()

	if _, ok := d.lookup(id); !ok {
		// 跨实例兜底：另一个实例刚建了这个会话。补索引后继续，计数才不会从 0 重来。
		if _, recovered := d.recoverIndex(id); !recovered {
			return Meta{}, fmt.Errorf("%w: %q", ErrNotFound, id)
		}
	}
	path := d.sessionPath(id)
	if err := appendRecords(path, payload); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 内存里有、文件没了（被外部删除）：丢弃条目并报 ErrNotFound，而不是
			// 重建一个只剩新数据的文件、让计数停在"旧 + 新"的谎言上。
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
// 尾部半行被丢弃；中间损坏的行被跳过（通过 Notice 上报）；读到的真实条数/字节数
// 与内存索引不一致时校正索引（顺便补全重启后未知的 Messages）。
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

	meta, ok := d.lookup(id)
	if !ok {
		// 跨实例兜底：文件在盘上但本实例的索引还没有它（另一个 Store 刚创建）。
		// 补索引后照常读，避免"看得见文件却读不出会话"。
		meta, ok = d.recoverIndex(id)
		if !ok {
			return Meta{}, nil, fmt.Errorf("%w: %q", ErrNotFound, id)
		}
	}
	read, err := readRecords(d.sessionPath(id))
	if err != nil {
		if errors.Is(err, ErrCorrupt) {
			d.emitAll(read.notices)
			return Meta{}, nil, err
		}
		if errors.Is(err, os.ErrNotExist) {
			// 内存里有、文件没了（被外部删除）：丢掉条目。
			d.dropSession(id, "会话文件缺失")
			return Meta{}, nil, fmt.Errorf("%w: %q", ErrNotFound, id)
		}
		d.emitAll(read.notices)
		return Meta{}, nil, err
	}
	if read.records != meta.Messages || read.size != meta.Bytes || (meta.Title == "" && read.records > 0) {
		meta = d.correctMeta(id, read.records, read.size, firstUserTitle(read.messages), read.notices)
	} else {
		d.emitAll(read.notices)
	}
	return meta, read.messages, nil
}

// List 实现 Store：按 UpdatedAt 倒序（相同则 ID 倒序，保证结果确定）。
//
// 只读内存索引，不碰数据文件——这是"个人客户端里最多几十上百个会话"的取舍：
// 列表要快，Messages/Title 由 Load/Append 在需要时补全。
func (d *Dir) List(ctx context.Context, filter Filter) ([]Meta, error) {
	if err := d.precheck(ctx); err != nil {
		return nil, err
	}
	d.indexMu.RLock()
	out := make([]Meta, 0, len(d.index))
	for _, meta := range d.index {
		out = append(out, meta)
	}
	d.indexMu.RUnlock()

	kept := out[:0]
	for _, meta := range out {
		if filter.Workspace != "" && meta.Workspace != filter.Workspace {
			continue
		}
		if !filter.Since.IsZero() && meta.UpdatedAt.Before(filter.Since) {
			continue
		}
		kept = append(kept, meta)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if !kept[i].UpdatedAt.Equal(kept[j].UpdatedAt) {
			return kept[i].UpdatedAt.After(kept[j].UpdatedAt)
		}
		return kept[i].ID > kept[j].ID
	})
	if filter.Limit > 0 && len(kept) > filter.Limit {
		kept = kept[:filter.Limit]
	}
	return kept, nil
}

// Delete 实现 Store：先删数据文件，再删内存条目（中途崩溃最多留下孤儿文件，
// 下次 NewDir 扫目录时会重新发现它）。
func (d *Dir) Delete(ctx context.Context, id string) error {
	if err := d.precheck(ctx); err != nil {
		return err
	}
	if err := checkID(id); err != nil {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	lock := d.shardLock(id)
	lock.Lock()
	defer lock.Unlock()

	path := d.sessionPath(id)
	if _, ok := d.lookup(id); !ok {
		// 跨实例兜底：索引里没有，但文件可能在（另一个实例创建的）。这里不需要读内容，
		// 一次 stat 就够；真的不存在才算 ErrNotFound。
		if _, statErr := os.Stat(path); statErr != nil {
			return fmt.Errorf("%w: %q", ErrNotFound, id)
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session store: remove session file %q: %w", path, err)
	}
	if err := syncDir(d.root); err != nil {
		return fmt.Errorf("session store: sync directory: %w", err)
	}
	d.indexMu.Lock()
	delete(d.index, id)
	d.indexMu.Unlock()
	return nil
}

// Replace 原子替换某个会话的全部消息（实现 rig.HistoryReplacer 的底层语义）。
//
// 用于上下文压缩：把 ndjson 整体重写到临时文件（tmp + rename），因此读者永远
// 看到"旧的完整内容"或"新的完整内容"。会话必须已存在，否则返回 ErrNotFound；
// History.Replace 会在缺失时自动建会话。
//
// 注意：它不保留旧记录的任何副本（压缩后的窗口就是唯一事实）。
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
	lock := d.shardLock(id)
	lock.Lock()
	defer lock.Unlock()

	if _, ok := d.lookup(id); !ok {
		path := d.sessionPath(id)
		if _, statErr := os.Stat(path); statErr != nil {
			return Meta{}, fmt.Errorf("%w: %q", ErrNotFound, id)
		}
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

// lookup 读一次内存索引。
func (d *Dir) lookup(id string) (Meta, bool) {
	d.indexMu.RLock()
	defer d.indexMu.RUnlock()
	meta, ok := d.index[id]
	return meta, ok
}

// commitAppend 在索引锁内累加计数。调用方必须持有该会话的分片锁。
func (d *Dir) commitAppend(id string, delta int, size int64, derivedTitle string) (Meta, error) {
	return d.commit(id, delta, true, size, derivedTitle)
}

// commitReplace 在索引锁内**覆盖**计数（Replace/压缩用）。
//
// 与 commitAppend 的区别是语义而不是风格：Replace 之后文件里只有 len(msgs) 条，
// 累加会让计数永久偏大。
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
	return meta, nil
}

// correctMeta 用真实读数校正内存索引。返回校正后的 Meta。
//
// 返回值 ID 为空表示该会话已不在索引里。
func (d *Dir) correctMeta(id string, records int, size int64, derivedTitle string, notices []string) Meta {
	d.indexMu.Lock()
	meta, ok := d.index[id]
	if !ok {
		d.indexMu.Unlock()
		d.emitAll(notices)
		return Meta{}
	}
	meta.Messages = records
	meta.Bytes = size
	if meta.Title == "" && derivedTitle != "" {
		meta.Title = clampTitle(derivedTitle)
	}
	d.index[id] = meta
	d.indexMu.Unlock()
	d.emitAll(notices)
	return meta
}

// recoverIndex 在内存索引未命中时按**磁盘**补一条索引，返回补好的 Meta。
//
// 它服务于"同一目录的另一个 Store 实例刚创建/写过这个会话"的场景：文件在盘上、
// 内容完整，但本实例的索引（打开时扫目录建的）里没有它。没有这一步，表现就是
// "看得见文件却读不出会话"。
//
// 边界：
//   - 文件不存在 → (Meta{}, false)，调用方按 ErrNotFound 处理；
//   - 文件损坏 → 仍然补一条（Messages=0），让后续 Load 报出**损坏**而不是"不存在"
//     （诊断信息更有用）；
//   - 只补索引，不加任何跨实例锁：同一目录仍然只推荐一个**写入者**。
func (d *Dir) recoverIndex(id string) (Meta, bool) {
	path := d.sessionPath(id)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return Meta{}, false
	}
	read, readErr := readRecords(path)
	if readErr != nil && !errors.Is(readErr, ErrCorrupt) {
		return Meta{}, false
	}
	stamp := info.ModTime().UTC()
	title := ""
	if d.scanContents {
		if derived := d.scanTitle(id); derived != "" {
			title = clampTitle(derived)
		}
	}
	meta := Meta{
		ID:        id,
		Title:     title,
		CreatedAt: stamp,
		UpdatedAt: stamp,
		Messages:  read.records,
		Bytes:     read.size,
	}
	d.indexMu.Lock()
	if existing, ok := d.index[id]; ok {
		// 竞态下别的 goroutine 已经补过：以内存里的为准。
		d.indexMu.Unlock()
		return existing, true
	}
	d.index[id] = meta
	d.indexMu.Unlock()
	d.emitAll(read.notices)
	d.emitf("session store: 会话 %q 不在本实例索引里，已按磁盘补入（同一目录建议只用一个写入者）", id)
	return meta, true
}

// dropSession 丢掉一条内存索引条目（文件缺失时用）。
func (d *Dir) dropSession(id, reason string) {
	d.indexMu.Lock()
	_, ok := d.index[id]
	if ok {
		delete(d.index, id)
	}
	d.indexMu.Unlock()
	if ok {
		d.emitf("session store: %s，已移除会话 %q", reason, id)
	}
}

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
	return filepath.Join(d.root, fileStem(id)+sessionExt)
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
// 合法 ID（字母/数字/-/_/./@）直接使用，文件名可读；其它 ID（中文、空格、冒号等）
// 用 "enc-<hex(UTF-8)>" 编码——**可逆**，因为文件名就是唯一的索引：重启扫目录时
// 必须能把原始 ID 还原出来，否则续传就断了。只有超过编码长度上限的超长 ID 才退化
// 成 "id-<sha256>"（不可逆，随机 ID 场景够用）。
func fileStem(id string) string {
	if validStem(id) {
		return id
	}
	if len(id) <= maxEncodedIDBytes {
		return encodedStemPrefix + hex.EncodeToString([]byte(id))
	}
	return hashedStemPrefix + hashStem(id)
}

// idFromStem 是 fileStem 的逆运算：从文件名 stem 还原会话 ID。
//
// 无法识别（不是本包写的文件）时返回 ok=false——扫目录会直接忽略它。
func idFromStem(stem string) (string, bool) {
	switch {
	case strings.HasPrefix(stem, encodedStemPrefix):
		raw, err := hex.DecodeString(strings.TrimPrefix(stem, encodedStemPrefix))
		if err != nil || len(raw) == 0 {
			return "", false
		}
		id := string(raw)
		// 编码名前缀只用于"不能直接当文件名"的 ID；能被 validStem 接受的 ID
		// 一律用原名，所以这里反过来校验一次，避免把伪造/混淆的文件名当成会话。
		if validStem(id) {
			return "", false
		}
		if err := checkID(id); err != nil {
			return "", false
		}
		return id, true
	case strings.HasPrefix(stem, hashedStemPrefix):
		// 哈希名不可逆：只能当"名字恰好长这样"的会话 ID 用。
		if validStem(stem) {
			return stem, true
		}
		return "", false
	case validStem(stem):
		return stem, true
	default:
		return "", false
	}
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
// <dir>/con.ndjson 打开的是控制台设备而不是文件。这些名字必须在文件名映射阶段
// 退化到哈希形式（ID 本身仍然合法）。
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
