package filewatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 默认参数。Options 里的零值或负值都表示"使用默认"。
const (
	// DefaultMaxFiles 是最多跟踪的条目数。
	DefaultMaxFiles = 20000
	// DefaultDebounce 是同一路径连续变化的合并窗口。
	DefaultDebounce = 200 * time.Millisecond
	// DefaultInterval 是两次扫描之间的间隔。
	DefaultInterval = time.Second
)

// eventBuffer 是事件通道容量。
//
// 用缓冲而不是无缓冲，是因为事件来自后台轮询 goroutine：消费者短暂落后不应该
// 变成"整个 watcher 卡住"。Close/ctx 取消时的收尾冲刷是尽力而为的（缓冲满即
// 丢弃），这样 Close 永远不会被"不读事件的消费者"锁死。
const eventBuffer = 256

// EventKind 是事件类型。
type EventKind string

// 事件类型常量。
const (
	// KindCreated 表示路径在两次扫描之间出现。
	KindCreated EventKind = "created"
	// KindModified 表示路径仍在，但 size/mtime（或摘要）变了。
	KindModified EventKind = "modified"
	// KindRemoved 表示路径在两次扫描之间消失。
	KindRemoved EventKind = "removed"
)

// Event 是一次文件变化。
type Event struct {
	// Path 是系统原生的绝对路径（不统一成斜杠；比较时用 filepath.Clean）。
	Path string
	// Kind 是变化类型。
	Kind EventKind
	// Size 是变化后的字节数；KindRemoved 时是最后已知大小。
	Size int64
	// ModTime 是变化后的修改时间；KindRemoved 时是最后已知时间。
	ModTime time.Time
	// Digest 是内容摘要（小写 hex 的 sha256），只在 Options.Digest 开启且该条目
	// 是普通文件时填写；符号链接、非普通文件与"未开启 Digest"时为空。
	Digest string
}

// FileInfo 是一个路径在某一时刻的可比较状态。
type FileInfo struct {
	// Size 是字节数（符号链接取 lstat 结果，即链接本身的长度）。
	Size int64
	// ModTime 是修改时间。
	ModTime time.Time
	// Digest 是内容摘要，语义同 Event.Digest。
	Digest string
}

// Options 配置 Watcher。所有字段的零值都表示"使用默认值"。
type Options struct {
	// Ignore 是要跳过的目录名或相对路径前缀（相对 root，用 / 分隔）。
	// nil 表示使用默认列表 .git/node_modules/vendor/dist/build；
	// 显式传空切片表示"什么都不忽略"。
	Ignore []string
	// MaxFiles 是最多跟踪的条目数，<=0 时用 DefaultMaxFiles。
	MaxFiles int
	// Digest 开启后额外用 sha256 比对内容，能发现"size 与 mtime 都没变"的改写。
	Digest bool
	// Debounce 是同一路径连续变化的合并窗口，<=0 时用 DefaultDebounce。
	Debounce time.Duration
	// Interval 是扫描间隔，<=0 时用 DefaultInterval。
	Interval time.Duration
	// Notice 接收诊断信息（例如触顶 MaxFiles、扫描失败）。可为 nil。
	// 它在后台轮询 goroutine 上被调用，实现必须自行保证线程安全且不得阻塞。
	Notice func(format string, args ...any)
}

// lifecycle 是 watcher 的状态机。用一把锁保护，避免"关闭"与"启动"竞争时
// 出现向已关闭 channel 发送事件的情况。
type lifecycle int

const (
	stateNew lifecycle = iota
	stateRunning
	stateStopping
	stateClosed
)

// Watcher 轮询扫描 root 并产出事件。零值不可用，请用 New 构造。
type Watcher struct {
	root string

	interval time.Duration
	debounce time.Duration
	maxFiles int
	digest   bool
	ignore   []string
	notice   func(format string, args ...any)

	events   chan Event
	stopCh   chan struct{}
	loopDone chan struct{}

	startOnce sync.Once
	stopOnce  sync.Once

	mu sync.Mutex
	// state 是生命周期状态。
	state lifecycle
	// emitted 是"消费者已经看到的状态"，也是 Snapshot 的内容。
	emitted map[string]FileInfo
	// lastScan 是最近一次扫描的结果（去抖窗口结束时以它为准）。
	lastScan map[string]FileInfo
	// noticedTruncation 记录是否已就触顶发过 Notice（只发一次）。
	noticedTruncation bool
	// noticedScanErr 记录上一次已报告的扫描错误，避免每秒刷同一条。
	noticedScanErr string
}

// New 构造一个监听 root 的 watcher。root 会被转成绝对路径并 Clean，但**不**
// 解析符号链接：事件里的 Path 沿用你传入的拼法。要让快照键与 sandbox 策略层的
// 规范化路径（EvalSymlinks 之后）可比，请传入已经规范化的根——macOS 上
// /var → /private/var、/tmp → /private/tmp 这类别名会让两种拼法对不上。
//
// 它不启动后台循环：需要显式调用 Start。root 不存在或不是目录时返回错误。
func New(root string, opts Options) (*Watcher, error) {
	abs, err := resolveRoot(root)
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		root:     abs,
		interval: opts.Interval,
		debounce: opts.Debounce,
		maxFiles: opts.MaxFiles,
		digest:   opts.Digest,
		notice:   opts.Notice,
		events:   make(chan Event, eventBuffer),
		stopCh:   make(chan struct{}),
		loopDone: make(chan struct{}),
		state:    stateNew,
		emitted:  make(map[string]FileInfo),
		lastScan: make(map[string]FileInfo),
	}
	if w.interval <= 0 {
		w.interval = DefaultInterval
	}
	if w.debounce <= 0 {
		w.debounce = DefaultDebounce
	}
	if w.maxFiles <= 0 {
		w.maxFiles = DefaultMaxFiles
	}
	if opts.Ignore == nil {
		w.ignore = defaultIgnore()
	} else {
		w.ignore = normalizeIgnore(opts.Ignore)
	}
	return w, nil
}

// Events 返回只读事件通道。Close（或 ctx 取消）之后它会被关闭。
func (w *Watcher) Events() <-chan Event {
	if w == nil {
		return nil
	}
	return w.events
}

// Start 建立基线并启动后台轮询。幂等：只有第一次调用生效。
//
// 基线是**同步**建立的（Start 返回时基线已就绪），因此"Start 之后立刻改文件"
// 不会漏事件；代价是首次扫描会阻塞调用方（1 万文件约几十毫秒）。
//
// ctx 取消或 Close 都会停止轮询并关闭 Events 通道。
func (w *Watcher) Start(ctx context.Context) {
	if w == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.startOnce.Do(func() {
		w.mu.Lock()
		if w.state != stateNew {
			w.mu.Unlock()
			return
		}
		w.state = stateRunning
		w.mu.Unlock()

		w.establishBaseline()
		go w.loop(ctx)
	})
}

// Snapshot 返回当前已知状态的**拷贝**（路径 → 大小/时间/摘要）。
//
// 它可以作为 Changed 的 from 参数留到写入点上做冲突检测。Start 之前为空。
func (w *Watcher) Snapshot() map[string]FileInfo {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]FileInfo, len(w.emitted))
	for path, info := range w.emitted {
		out[path] = info
	}
	return out
}

// Close 停止轮询并关闭 Events。幂等；未 Start 时也安全。
//
// Close 会尽量把"尚未冲刷的变化"投递出去（尽力而为：事件缓冲满则丢弃），
// 然后才关闭通道。它不会因为消费者不读事件而卡住。
func (w *Watcher) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	state := w.state
	switch state {
	case stateNew:
		// 没有后台循环，通道由这里负责关闭。
		w.state = stateClosed
		w.mu.Unlock()
		close(w.events)
		return nil
	case stateRunning:
		w.state = stateStopping
		w.mu.Unlock()
		w.stopOnce.Do(func() { close(w.stopCh) })
		<-w.loopDone
		return nil
	default:
		w.mu.Unlock()
	}
	if state == stateStopping {
		<-w.loopDone
	}
	return nil
}

// establishBaseline 扫描一次并把结果同时作为"已见状态"与"最近扫描状态"，
// 因此基线本身不产出事件。
func (w *Watcher) establishBaseline() {
	files, truncated, err := w.scanner().scan()
	if err != nil {
		w.noticeScanError(err)
		files = make(map[string]FileInfo)
	}
	w.mu.Lock()
	w.emitted = files
	w.lastScan = files
	w.mu.Unlock()
	if truncated {
		w.noticeTruncation()
	}
}

// loop 是后台轮询循环：唯二负责关闭 events 的地方之一（另一处是未启动时的 Close）。
func (w *Watcher) loop(ctx context.Context) {
	defer func() {
		w.mu.Lock()
		w.state = stateClosed
		w.mu.Unlock()
		close(w.events)
		close(w.loopDone)
	}()

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	var pendingSince time.Time
	for {
		select {
		case <-w.stopCh:
			w.flushFinal()
			return
		case <-ctx.Done():
			w.flushFinal()
			return
		case <-ticker.C:
			w.poll(&pendingSince)
		}
	}
}

// poll 扫描一次并（在去抖窗口结束时）冲刷变更。
func (w *Watcher) poll(pendingSince *time.Time) {
	files, truncated, err := w.scanner().scan()
	if err != nil {
		w.noticeScanError(err)
		return
	}
	if truncated {
		w.noticeTruncation()
	}

	w.mu.Lock()
	w.lastScan = files
	pending := diffPaths(w.emitted, files, w.digest)
	w.mu.Unlock()

	if len(pending) == 0 {
		*pendingSince = time.Time{}
		return
	}
	if pendingSince.IsZero() {
		*pendingSince = time.Now()
	}
	if time.Since(*pendingSince) < w.debounce {
		return
	}
	w.flush(pending, true)
	*pendingSince = time.Time{}
}

// flushFinal 在停止前再扫一次，把最后一个窗口内的变化尽量交付出去。
func (w *Watcher) flushFinal() {
	files, truncated, err := w.scanner().scan()
	if err != nil {
		w.noticeScanError(err)
	} else {
		w.mu.Lock()
		w.lastScan = files
		w.mu.Unlock()
		if truncated {
			w.noticeTruncation()
		}
	}
	w.mu.Lock()
	pending := diffPaths(w.emitted, w.lastScan, w.digest)
	w.mu.Unlock()
	if len(pending) > 0 {
		w.flush(pending, false)
	}
}

// flush 把 pending 路径渲染成事件并按字典序交付。
//
// blocking 为 true 时会在 stopCh 关闭后放弃投递；为 false（收尾冲刷）时用
// 非阻塞发送，避免 Close 被不读事件的消费者卡住。
func (w *Watcher) flush(paths []string, blocking bool) {
	sort.Strings(paths)

	w.mu.Lock()
	cur := w.lastScan
	out := make([]Event, 0, len(paths))
	for _, path := range paths {
		ev, ok := eventFor(path, w.emitted, cur, w.digest)
		if !ok {
			continue
		}
		out = append(out, ev)
		if info, exists := cur[path]; exists {
			w.emitted[path] = info
		} else {
			delete(w.emitted, path)
		}
	}
	w.mu.Unlock()

	for _, ev := range out {
		if blocking {
			if !w.sendEvent(ev) {
				return
			}
			continue
		}
		w.sendBestEffort(ev)
	}
}

// sendEvent 交付一条事件；只有 watcher 正在正常运行时调用。
func (w *Watcher) sendEvent(ev Event) bool {
	select {
	case w.events <- ev:
		return true
	case <-w.stopCh:
		return false
	}
}

// sendBestEffort 在收尾阶段交付一条事件，缓冲满则丢弃。
func (w *Watcher) sendBestEffort(ev Event) {
	select {
	case w.events <- ev:
	default:
	}
}

// scanner 用当前配置构造一次扫描器。
func (w *Watcher) scanner() *scanner {
	return &scanner{
		root:     w.root,
		ignore:   w.ignore,
		maxFiles: w.maxFiles,
		digest:   w.digest,
	}
}

// noticef 转发诊断信息；Notice 为 nil 时静默。
func (w *Watcher) noticef(format string, args ...any) {
	if w.notice == nil {
		return
	}
	w.notice(format, args...)
}

// noticeTruncation 就"触顶 MaxFiles"提示一次。
func (w *Watcher) noticeTruncation() {
	w.mu.Lock()
	seen := w.noticedTruncation
	w.noticedTruncation = true
	limit := w.maxFiles
	w.mu.Unlock()
	if seen {
		return
	}
	w.noticef("filewatch: 达到 MaxFiles=%d 上限，其余条目不再跟踪（可调大 MaxFiles）", limit)
}

// noticeScanError 报告扫描错误，同一错误只报一次（避免每秒刷屏）。
func (w *Watcher) noticeScanError(err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	w.mu.Lock()
	seen := w.noticedScanErr == msg
	w.noticedScanErr = msg
	w.mu.Unlock()
	if seen {
		return
	}
	w.noticef("filewatch: 扫描 %s 失败: %v", w.root, err)
}

// resolveRoot 把 root 归一成绝对路径并校验它是已存在的目录。
//
// 三个入口（New/Changed/ChangedPaths）共用同一套校验，避免口径漂移。
func resolveRoot(root string) (string, error) {
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		return "", fmt.Errorf("filewatch: root 不能为空")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("filewatch: 解析 root %q: %w", root, err)
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("filewatch: root %q 不可用: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("filewatch: root %q 不是目录", abs)
	}
	return abs, nil
}
