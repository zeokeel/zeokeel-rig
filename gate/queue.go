package gate

// 人工审批的通用队列：宿主把"需要人确认的工具调用"登记进来，UI 读取 pending、
// 调用 Resolve 交回决定，等待方拿到决定继续执行。
//
// 它只做队列机制（编号、等待、幂等回答、取消、订阅、可选超时），**不做策略**：
// 哪些工具免问、auto/never 怎么映射、等待文案与通知都归宿主（或上层的门禁组合）。
// 这样桌面 UI、TUI、服务器 loop 可以共用同一份"排队—回答"语义。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// Queue 的错误。Resolve 对未知/已取消的 ID 返回 ErrUnknownRequest；非法决定返回
// ErrInvalidDecision；AskTimeout 超时返回 ErrTimeout（并已把该请求移出队列）。
var (
	// ErrUnknownRequest 表示 ID 既不在 pending 里也从未被回答过。
	ErrUnknownRequest = errors.New("gate: unknown approval request")
	// ErrInvalidDecision 表示决定不是 allow/allow-always/deny。
	ErrInvalidDecision = errors.New("gate: invalid approval decision")
	// ErrTimeout 表示等待超时（请求已移出队列）。
	ErrTimeout = errors.New("gate: approval timed out")
)

// QueueRequest 是一条待审批请求（与 UI 无关的最小字段集）。
type QueueRequest struct {
	// ID 由队列分配（Ask 的返回值通过 Pending/Resolve 使用）。
	ID string
	// Tool 是被审批的工具名。
	Tool string
	// Summary 是一句人可读的调用描述。
	Summary string
	// Arguments 是原始参数（展示用，可截断）。
	Arguments string
	// SessionID 是发起调用的会话（并发会话时用于标注来源）。
	SessionID string
	// Source 是来源（user / automation / ...）。
	Source string
	// CreatedAt 是入队时间。
	CreatedAt time.Time
}

// QueueOption 配置一个队列。
type QueueOption func(*Queue)

// WithQueuePrefix 设置请求 ID 前缀（默认 "approval-"）。
func WithQueuePrefix(prefix string) QueueOption {
	return func(q *Queue) {
		if prefix = strings.TrimSpace(prefix); prefix != "" {
			q.prefix = prefix
		}
	}
}

// WithQueueLogger 接收被吞掉的订阅者 panic（默认丢弃）。
func WithQueueLogger(logf func(format string, args ...any)) QueueOption {
	return func(q *Queue) {
		if logf != nil {
			q.logf = logf
		}
	}
}

// WithQueueEnqueueHook 在一条请求入队后调用（通知宿主：有人在等）。回调在队列锁
// 之外、与 Ask 同一 goroutine 上执行，必须快速且不得重入队列。
func WithQueueEnqueueHook(fn func(QueueRequest)) QueueOption {
	return func(q *Queue) {
		q.onEnqueue = fn
	}
}

// WithQueueResolvedLimit 设置"已回答 ID"的幂等缓存上限（默认 128）。
func WithQueueResolvedLimit(limit int) QueueOption {
	return func(q *Queue) {
		if limit > 0 {
			q.resolvedLimit = limit
		}
	}
}

type queuedRequest struct {
	request QueueRequest
	result  chan queuedResult
}

type queuedResult struct {
	decision rig.Decision
	err      error
}

// Queue 是一条按会话/跨会话共享的审批队列，并发安全。
type Queue struct {
	prefix        string
	logf          func(string, ...any)
	onEnqueue     func(QueueRequest)
	resolvedLimit int

	mu            sync.Mutex
	nextSeq       uint64
	pending       map[string]*queuedRequest
	pendingOrder  []string
	resolved      map[string]rig.Decision
	resolvedOrder []string
	subscribers   []func()
}

// NewQueue returns an empty queue.
func NewQueue(opts ...QueueOption) *Queue {
	q := &Queue{
		prefix:        "approval-",
		resolvedLimit: 128,
		pending:       make(map[string]*queuedRequest),
		resolved:      make(map[string]rig.Decision),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(q)
		}
	}
	return q
}

// Ask enqueues one request and blocks until Resolve answers it or ctx ends.
// The generated ID is written into the returned request passed to the enqueue
// hook (callers that need it read Pending or the hook's copy).
func (q *Queue) Ask(ctx context.Context, request QueueRequest) (rig.Decision, error) {
	return q.AskTimeout(ctx, request, 0)
}

// AskTimeout is Ask with an optional timeout (0 = wait until Resolve or ctx).
// On timeout the request leaves the queue and ErrTimeout is returned.
func (q *Queue) AskTimeout(ctx context.Context, request QueueRequest, timeout time.Duration) (rig.Decision, error) {
	if q == nil {
		return rig.DecisionDeny, errors.New("gate: nil approval queue")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return rig.DecisionDeny, err
	}
	entry := &queuedRequest{result: make(chan queuedResult, 1)}
	q.mu.Lock()
	q.nextSeq++
	request.ID = q.prefix + strconv.FormatUint(q.nextSeq, 10)
	if request.CreatedAt.IsZero() {
		request.CreatedAt = time.Now()
	}
	entry.request = request
	q.pending[request.ID] = entry
	q.pendingOrder = append(q.pendingOrder, request.ID)
	hook := q.onEnqueue
	q.mu.Unlock()
	q.notify()

	if hook != nil {
		hook(request)
	}

	var timeoutC <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutC = timer.C
	}
	select {
	case answer := <-entry.result:
		return answer.decision, answer.err
	case <-timeoutC:
		// A timeout is an answer: record deny so a late Resolve stays
		// idempotent (the caller already reported the denial).
		q.recordResolved(request.ID, rig.DecisionDeny)
		q.notify()
		return rig.DecisionDeny, ErrTimeout
	case <-ctx.Done():
		q.removePending(request.ID)
		q.notify()
		return rig.DecisionDeny, ctx.Err()
	}
}

// Pending returns the requests still waiting, in enqueue order.
func (q *Queue) Pending() []QueueRequest {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]QueueRequest, 0, len(q.pendingOrder))
	for _, id := range q.pendingOrder {
		if entry, ok := q.pending[id]; ok {
			out = append(out, entry.request)
		}
	}
	return out
}

// Resolve answers one pending request. A second Resolve of an already answered
// ID is a no-op (nil); an unknown ID returns ErrUnknownRequest; a decision
// outside allow/allow-always/deny returns ErrInvalidDecision.
func (q *Queue) Resolve(id string, decision rig.Decision) error {
	if q == nil {
		return errors.New("gate: nil approval queue")
	}
	switch decision {
	case rig.DecisionAllow, rig.DecisionAllowAlways, rig.DecisionDeny:
	default:
		return ErrInvalidDecision
	}
	id = strings.TrimSpace(id)
	q.mu.Lock()
	entry, ok := q.pending[id]
	if !ok {
		_, answered := q.resolved[id]
		q.mu.Unlock()
		if answered {
			return nil
		}
		return ErrUnknownRequest
	}
	delete(q.pending, id)
	q.pendingOrder = removeString(q.pendingOrder, id)
	q.resolved[id] = decision
	q.resolvedOrder = append(q.resolvedOrder, id)
	for len(q.resolvedOrder) > q.resolvedLimit {
		oldest := q.resolvedOrder[0]
		q.resolvedOrder = q.resolvedOrder[1:]
		delete(q.resolved, oldest)
	}
	q.mu.Unlock()
	// The waiter has a buffered channel: delivering outside the lock can never block.
	entry.result <- queuedResult{decision: decision}
	q.notify()
	return nil
}

// Subscribe registers a change callback (pending/answered changed). The callback
// runs synchronously outside the queue lock; a panic is logged and contained.
func (q *Queue) Subscribe(fn func()) (func(), error) {
	if q == nil {
		return nil, errors.New("gate: nil approval queue")
	}
	if fn == nil {
		return nil, errors.New("gate: nil approval subscriber")
	}
	q.mu.Lock()
	q.subscribers = append(q.subscribers, fn)
	index := len(q.subscribers) - 1
	q.mu.Unlock()
	return func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		if index < len(q.subscribers) {
			q.subscribers[index] = nil
		}
	}, nil
}

// notify runs every live subscriber; panics are contained.
func (q *Queue) notify() {
	q.mu.Lock()
	subs := make([]func(), 0, len(q.subscribers))
	for _, fn := range q.subscribers {
		if fn != nil {
			subs = append(subs, fn)
		}
	}
	logf := q.logf
	q.mu.Unlock()
	for _, fn := range subs {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil && logf != nil {
					logf("subscriber panicked: %v", recovered)
				}
			}()
			fn()
		}()
	}
}

// recordResolved removes the pending entry and remembers the answer.
func (q *Queue) recordResolved(id string, decision rig.Decision) {
	q.mu.Lock()
	delete(q.pending, id)
	q.pendingOrder = removeString(q.pendingOrder, id)
	q.resolved[id] = decision
	q.resolvedOrder = append(q.resolvedOrder, id)
	for len(q.resolvedOrder) > q.resolvedLimit {
		oldest := q.resolvedOrder[0]
		q.resolvedOrder = q.resolvedOrder[1:]
		delete(q.resolved, oldest)
	}
	q.mu.Unlock()
}

// removePending drops one still-waiting request (cancellation/timeout).
func (q *Queue) removePending(id string) {
	q.mu.Lock()
	delete(q.pending, id)
	q.pendingOrder = removeString(q.pendingOrder, id)
	q.mu.Unlock()
}

func removeString(list []string, want string) []string {
	for index, value := range list {
		if value == want {
			return append(list[:index], list[index+1:]...)
		}
	}
	return list
}
