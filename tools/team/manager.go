package team

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// team_list 展示的成员状态。
const (
	// StatusActive 表示可被发送消息并等待结果的存活成员。
	StatusActive = "active"
	// StatusInterrupted 表示当前运行已被停止的成员；在 team_close 移除它之前仍会列出，且不能
	// 再次运行。
	StatusInterrupted = "interrupted"
	// StatusClosed 标记正在停止并移除的成员。它只短暂可观测；已关闭成员不会留在表格中。
	StatusClosed = "closed"
)

// 默认值与限制。它们被导出，便于宿主与测试断言文档中承诺的行为。
const (
	// DefaultMaxMembers 是每个 Manager 默认的存活成员数。
	DefaultMaxMembers = 8
	// DefaultWaitTimeout 是 team_wait 未提供 timeout_ms 时的等待上限。
	DefaultWaitTimeout = 30 * time.Second
	// MaxWaitTimeout 限制单次 team_wait 的超时。
	MaxWaitTimeout = 10 * time.Minute
	// DefaultIDPrefix 是生成的成员 id 前缀（"member-1" 等）。
	DefaultIDPrefix = "member-"
)

// 哨兵错误。工具失败会包装它们，宿主可用 errors.Is 区分；工具会把它们转换为可读的模型
// 可见消息。
var (
	// ErrNoRunner 表示 Manager 没有配置 SubagentRunner。
	ErrNoRunner = errors.New("no subagent runner is configured")
	// ErrManagerClosed 表示在 Manager.CloseAll 之后尝试操作。
	ErrManagerClosed = errors.New("team manager is closed")
	// ErrTeamFull 表示 spawn 超出成员上限。
	ErrTeamFull = errors.New("team is full")
	// ErrUnknownMember 表示 id/名称没有匹配任何存活成员。
	ErrUnknownMember = errors.New("unknown team member")
	// ErrAmbiguousMember 表示名称被多个存活成员共享。
	ErrAmbiguousMember = errors.New("ambiguous team member name")
	// ErrMemberInactive 表示向已被中断的成员发送运行。
	ErrMemberInactive = errors.New("team member is not active")
)

// Option 配置 Manager。
type Option func(*Manager)

// WithMaxMembers 限制存活成员数（非正数保留 DefaultMaxMembers）。被中断的成员在关闭前
// 仍占名额。
func WithMaxMembers(max int) Option {
	return func(manager *Manager) {
		if max > 0 {
			manager.maxMembers = max
		}
	}
}

// WithNamePrefix 设置生成的成员 id 前缀（默认 "member-"）；id 为前缀 + 序号，例如
// WithNamePrefix("agent-") 得到 "agent-1"。
func WithNamePrefix(prefix string) Option {
	return func(manager *Manager) {
		if prefix = strings.TrimSpace(prefix); prefix != "" {
			manager.idPrefix = prefix
		}
	}
}

// Member 是一个团队成员的只读快照。
type Member struct {
	// ID 是生成的成员 id（"member-1" 等）。
	ID string
	// Name 是 team_spawn 传入的显示名。
	Name string
	// Status 是 StatusActive 或 StatusInterrupted。
	Status string
	// Created 是成员被创建的时间。
	Created time.Time
	// Last 是最新一次已完成运行的结果（存在时）。
	Last *rig.SubagentResult
}

// member 是一个存活成员的可变状态。
type member struct {
	id       string
	name     string
	subagent rig.Subagent
	created  time.Time

	// callMu 串行化该成员的 Manager 级 Send 与 Wait 调用。rig.Subagent 实现本身可安全并发，
	// 并串行化自己的运行；此互斥锁在 Manager 边界避免一个阻塞的工具调用与另一个调用竞争。
	callMu sync.Mutex

	// inboxMu 保护异步投递队列。**投递路径不得共用 callMu**：队友一在跑，
	// waitCompletion→Wait 就整段持有 callMu，任何需要 callMu 的投递都会卡住整轮对话。
	inboxMu      sync.Mutex
	inbox        []inboxMessage
	inboxRunning bool
	// queuedSeq 是已入队的最大代次；appliedSeq 是已处理完（成功或失败）的最大代次。
	// Wait 在 appliedSeq < queuedSeq 时继续等：SendAsync 只入队，排空是异步的，
	// 不等这一代完成就会把上一轮结果当成本轮的答复返回。
	queuedSeq     int64
	appliedSeq    int64
	appliedSignal chan struct{}

	// mu 保护 status、last 与 resultSeq。
	mu     sync.Mutex
	status string
	last   *rig.SubagentResult
	// resultSeq 是 last 对应的收件箱代次（0 表示没有代次信息，例如首轮结果）。
	resultSeq int64
}

// inboxMessage 是收件箱里的一条消息及其代次。
type inboxMessage struct {
	seq  int64
	text string
}

// Manager 拥有一个 agent 会话的子代理：spawn、id/名称解析、消息传递、结果读取与成员
// 上限。
//
// 它可安全并发使用。Manager 在 CloseAll 之后不可复用。
type Manager struct {
	runner     rig.SubagentRunner
	maxMembers int
	idPrefix   string

	mu      sync.Mutex
	members []*member
	notices []string
	nextID  int
	closed  bool
	// spawning 统计已预留名额但尚未注册成员的 Spawn 调用数（runner 可能阻塞在自己的并发
	// 上限上）。
	spawning int
}

// NewManager 返回通过 runner 启动子代理的 Manager。允许 runner 为 nil：此时 team_spawn
// 报告未配置 runner，而 list/wait/close 对别处启动的成员仍可用。
func NewManager(runner rig.SubagentRunner, opts ...Option) *Manager {
	manager := &Manager{
		runner:     runner,
		maxMembers: DefaultMaxMembers,
		idPrefix:   DefaultIDPrefix,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(manager)
		}
	}
	return manager
}

// Len 报告存活成员数（active 与 interrupted）。
func (m *Manager) Len() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.members)
}

// Members 按创建顺序返回存活成员快照。
func (m *Manager) Members() []Member {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	members := make([]Member, 0, len(m.members))
	for _, current := range m.members {
		members = append(members, current.snapshot())
	}
	return members
}

// Spawn 启动一个子代理并注册为成员。首次运行异步继续；用 Wait（或 team_wait）读取结果。
//
// ctx 只约束启动调用（包括等待 runner 的并发名额），不约束子代理生命周期：Start 返回后由
// runner 为子代理持有后台 context。Manager 锁在 Start 期间不持有，因此被 runner 容量阻塞的
// spawn 不会死锁其他成员的列出、等待或关闭。Manager 拥有该子代理，直到
// Interrupt/Close/CloseAll。
func (m *Manager) Spawn(ctx context.Context, spec rig.SubagentSpec) (Member, error) {
	if m == nil {
		return Member{}, ErrManagerClosed
	}
	if err := contextError(ctx); err != nil {
		return Member{}, err
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return Member{}, errors.New("team: member name is required")
	}
	prompt := strings.TrimSpace(spec.Prompt)
	if prompt == "" {
		return Member{}, errors.New("team: member prompt is required")
	}
	spec.Name = name
	spec.Prompt = prompt

	// 先预留名额，再在不持有 Manager 锁的情况下启动子代理：runner 可能阻塞在自己的并发上限，
	// 直到另一个子代理结束；此时持有 m.mu 会死锁所有其他团队工具。
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Member{}, ErrManagerClosed
	}
	runner := m.runner
	if runner == nil {
		m.mu.Unlock()
		return Member{}, ErrNoRunner
	}
	if live := len(m.members) + m.spawning; live >= m.maxMembers {
		m.mu.Unlock()
		return Member{}, fmt.Errorf("%w: %d live members (limit %d); close a member with team_close first",
			ErrTeamFull, live, m.maxMembers)
	}
	m.spawning++
	m.mu.Unlock()

	subagent, err := runner.Start(ctx, spec)

	m.mu.Lock()
	m.spawning--
	if err != nil {
		m.mu.Unlock()
		return Member{}, fmt.Errorf("team: start subagent %q: %w", name, err)
	}
	if m.closed {
		// 子代理启动期间 CloseAll 已执行：不再注册它。
		m.mu.Unlock()
		_ = subagent.Stop()
		return Member{}, ErrManagerClosed
	}
	m.nextID++
	current := &member{
		id:            m.idPrefix + strconv.Itoa(m.nextID),
		name:          name,
		subagent:      subagent,
		created:       time.Now(),
		status:        StatusActive,
		appliedSignal: make(chan struct{}),
	}
	m.members = append(m.members, current)
	m.mu.Unlock()
	return current.snapshot(), nil
}

// Send 向一个成员（先按 id、再按唯一名称）发送后续消息，阻塞到该轮结束并返回结果。
func (m *Manager) Send(ctx context.Context, ref, message string) (rig.SubagentResult, error) {
	current, err := m.resolve(ref)
	if err != nil {
		return rig.SubagentResult{}, err
	}
	if err := contextError(ctx); err != nil {
		return rig.SubagentResult{}, err
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return rig.SubagentResult{}, errors.New("team: message is required")
	}
	if err := current.ensureActive(); err != nil {
		return rig.SubagentResult{}, err
	}
	current.callMu.Lock()
	defer current.callMu.Unlock()
	if err := current.ensureActive(); err != nil {
		return rig.SubagentResult{}, err
	}
	result, err := current.subagent.Send(ctx, message)
	if err != nil {
		return rig.SubagentResult{}, fmt.Errorf("team: member %q (%s) send failed: %w", current.name, current.id, err)
	}
	m.recordResult(current, result, 0)
	return result, nil
}

// SendAsync 是 SendAsyncContext 的兼容包装（无调用方 ctx）。
//
// Deprecated: 用 SendAsyncContext 传入调用方的 ctx；本方法以 context.Background()
// 调用它，因此无法随宿主取消而放弃投递。保留它只为不破坏既有宿主的编译。
func (m *Manager) SendAsync(ref, message string) error {
	return m.SendAsyncContext(context.Background(), ref, message)
}

// SendAsyncContext 接收发给存活成员的消息，投递到收件箱后立即返回（线协议 send_message 语义）。
// 成员回复会记录为其最新结果，可通过 Wait 或 Last 获取。
//
// 投递路径**不碰 callMu**：队友运行期间（waitCompletion→Wait 持有 callMu 的整段时间）
// 也必须能立刻收下消息。排空收件箱的后台 goroutine 自己串行调用 subagent.Send，因此
// 多条消息仍按投递顺序送达。ctx 只约束"投递动作"（已取消则不收下消息）。
func (m *Manager) SendAsyncContext(ctx context.Context, ref, message string) error {
	current, err := m.resolve(ref)
	if err != nil {
		return err
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return errors.New("team: message is required")
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := current.ensureActive(); err != nil {
		return err
	}
	start := false
	current.inboxMu.Lock()
	current.queuedSeq++
	current.inbox = append(current.inbox, inboxMessage{seq: current.queuedSeq, text: message})
	if !current.inboxRunning {
		current.inboxRunning = true
		start = true
	}
	current.inboxMu.Unlock()
	if start {
		go m.drainInbox(current)
	}
	return nil
}

// drainInbox 依次把收件箱里的消息交给 subagent.Send，并把结果记为最新结果。
// 队列空时退出（inboxRunning 复位），因此不会留下常年挂着的 goroutine。
func (m *Manager) drainInbox(current *member) {
	for {
		current.inboxMu.Lock()
		if len(current.inbox) == 0 {
			current.inboxRunning = false
			current.inboxMu.Unlock()
			return
		}
		message := current.inbox[0]
		current.inbox = current.inbox[1:]
		current.inboxMu.Unlock()

		if err := current.ensureActive(); err != nil {
			// 成员已失效：这一代次没有结果，但必须推进 appliedSeq，
			// 否则等待该代次的 Wait 会一直挂着。
			current.markApplied(message.seq)
			continue
		}
		result, err := current.subagent.Send(context.Background(), message.text)
		if err != nil {
			current.markApplied(message.seq)
			continue
		}
		m.recordResult(current, result, message.seq)
		current.markApplied(message.seq)
	}
}

// markApplied 记录某个代次的消息已处理完（成功或失败），并唤醒等待该代次的 Wait。
func (current *member) markApplied(seq int64) {
	current.inboxMu.Lock()
	if seq <= current.appliedSeq {
		current.inboxMu.Unlock()
		return
	}
	current.appliedSeq = seq
	signal := current.appliedSignal
	current.appliedSignal = make(chan struct{})
	current.inboxMu.Unlock()
	if signal != nil {
		close(signal)
	}
}

// queuedSeqSnapshot 返回当前已入队的最大代次。
func (current *member) queuedSeqSnapshot() int64 {
	current.inboxMu.Lock()
	defer current.inboxMu.Unlock()
	return current.queuedSeq
}

// waitApplied 等待到 target 代次为止的消息被处理完；ctx 取消或成员失效时返回错误。
func (current *member) waitApplied(ctx context.Context, target int64) error {
	for {
		if err := current.ensureActive(); err != nil {
			return err
		}
		current.inboxMu.Lock()
		if current.appliedSeq >= target {
			current.inboxMu.Unlock()
			return nil
		}
		signal := current.appliedSignal
		current.inboxMu.Unlock()
		if signal == nil {
			return nil
		}
		select {
		case <-signal:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// resultAtLeast 返回代次不低于 seq 的已记录结果。
func (current *member) resultAtLeast(seq int64) (rig.SubagentResult, bool) {
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.last == nil || current.resultSeq < seq {
		return rig.SubagentResult{}, false
	}
	return *current.last, true
}

// Wait 阻塞等待成员的当前（或首次）运行并返回其结果。运行结束后再调用会返回相同结果。
//
// 代次语义：SendAsync 只把消息放进收件箱、排空是异步的，因此 Wait 必须等到
// "调用时刻已入队的消息"被处理完（appliedSeq >= queuedSeq）再返回；否则它会把上一轮
// 的结果当成本轮消息的答复（调用方以为新消息没生效）。
//
// 收敛：等待期间可能又有新消息入队。Wait 会**循环收敛**——重新取 queuedSeq 快照并
// 继续等，直到两次观察到同一代次（没有任何新消息）再返回。这样"Wait 已开始、排空
// 尚未完成时新入队的消息"也会被这次 Wait 覆盖，而不是返回一个比调用方预期更旧的
// 结果。
//
// 收敛**没有任何人为轮数上界**：消息持续涌入时 Wait 就一直等（调用方要"等队友停下"
// 就该拿到最新答复）。不想等就传带 deadline/取消的 ctx——它是唯一的边界。
//
// 收件箱为空（target==0）时行为不变：立即返回当前结果。ctx 到期/取消返回
// DeadlineExceeded/Canceled，不会退化成返回旧结果。
func (m *Manager) Wait(ctx context.Context, ref string) (rig.SubagentResult, error) {
	current, err := m.resolve(ref)
	if err != nil {
		return rig.SubagentResult{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := contextError(ctx); err != nil {
		return rig.SubagentResult{}, err
	}
	if err := current.ensureActive(); err != nil {
		return rig.SubagentResult{}, err
	}
	target := current.queuedSeqSnapshot()
	current.callMu.Lock()
	defer current.callMu.Unlock()
	if err := current.ensureActive(); err != nil {
		return rig.SubagentResult{}, err
	}
	if target > 0 {
		for {
			if err := current.waitApplied(ctx, target); err != nil {
				return rig.SubagentResult{}, fmt.Errorf("team: member %q (%s) wait failed: %w", current.name, current.id, err)
			}
			next := current.queuedSeqSnapshot()
			if next == target {
				break // 等待期间没有新消息 → 收敛
			}
			target = next
		}
		if result, ok := current.resultAtLeast(target); ok {
			return result, nil
		}
	}
	result, err := current.subagent.Wait(ctx)
	if err != nil {
		return rig.SubagentResult{}, fmt.Errorf("team: member %q (%s) wait failed: %w", current.name, current.id, err)
	}
	m.recordResult(current, result, target)
	return result, nil
}

// Last 返回一个成员最新一次已完成运行的结果。成员尚未完成任何运行时 ok 为 false。
func (m *Manager) Last(ref string) (rig.SubagentResult, bool, error) {
	current, err := m.resolve(ref)
	if err != nil {
		return rig.SubagentResult{}, false, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.last == nil {
		return rig.SubagentResult{}, false, nil
	}
	return *current.last, true, nil
}

// Interrupt 停止成员当前运行，并让该成员以 interrupted 状态保留在列表中。对已中断成员是
// 幂等的。
func (m *Manager) Interrupt(ref string) error {
	current, err := m.resolve(ref)
	if err != nil {
		return err
	}
	current.mu.Lock()
	status := current.status
	if status == StatusInterrupted {
		current.mu.Unlock()
		return nil
	}
	if status != StatusActive {
		current.mu.Unlock()
		return fmt.Errorf("%w: member %q (%s) is %s", ErrMemberInactive, current.name, current.id, status)
	}
	current.status = StatusInterrupted
	subagent := current.subagent
	current.mu.Unlock()
	if subagent != nil {
		if err := subagent.Stop(); err != nil {
			return fmt.Errorf("team: member %q (%s) stop failed: %w", current.name, current.id, err)
		}
	}
	return nil
}

// Close 停止成员并将其从团队中移除。成员先按 id、再按唯一名称解析。
func (m *Manager) Close(ref string) error {
	current, err := m.resolve(ref)
	if err != nil {
		return err
	}
	current.mu.Lock()
	current.status = StatusClosed
	subagent := current.subagent
	current.mu.Unlock()
	m.remove(current)
	if subagent != nil {
		if err := subagent.Stop(); err != nil {
			return fmt.Errorf("team: member %q (%s) stop failed: %w", current.name, current.id, err)
		}
	}
	return nil
}

// CloseAll 停止所有存活成员并关闭 Manager。它是幂等的；之后 Manager 拒绝 spawn 与成员
// 操作。
func (m *Manager) CloseAll() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	members := m.members
	m.members = nil
	m.mu.Unlock()

	var errs []error
	for _, current := range members {
		current.mu.Lock()
		current.status = StatusClosed
		subagent := current.subagent
		current.mu.Unlock()
		if subagent == nil {
			continue
		}
		if err := subagent.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("team: member %q (%s) stop failed: %w", current.name, current.id, err))
		}
	}
	return errors.Join(errs...)
}

// resolve 查找一个存活成员：精确 id 优先；否则按唯一名称匹配，重名时报告
// ErrAmbiguousMember。
func (m *Manager) resolve(ref string) (*member, error) {
	if m == nil {
		return nil, ErrManagerClosed
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("team: member id or name is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrManagerClosed
	}
	for _, current := range m.members {
		if current.id == ref {
			return current, nil
		}
	}
	var matches []*member
	for _, current := range m.members {
		if current.name == ref {
			matches = append(matches, current)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("%w: %q (use team_list to see member ids)", ErrUnknownMember, ref)
	default:
		ids := make([]string, len(matches))
		for index, match := range matches {
			ids[index] = match.id
		}
		return nil, fmt.Errorf("%w: name %q matches %d members (%s); pass the member id instead",
			ErrAmbiguousMember, ref, len(matches), strings.Join(ids, ", "))
	}
}

// remove 从存活成员表中移除一个成员。
func (m *Manager) remove(target *member) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for index, current := range m.members {
		if current == target {
			m.members = slices.Delete(m.members, index, index+1)
			return
		}
	}
}

// snapshot 返回成员状态的只读副本。
func (current *member) snapshot() Member {
	current.mu.Lock()
	defer current.mu.Unlock()
	member := Member{
		ID:      current.id,
		Name:    current.name,
		Status:  current.status,
		Created: current.created,
	}
	if current.last != nil {
		last := *current.last
		member.Last = &last
	}
	return member
}

// ensureActive 拒绝在非活跃成员上运行。
func (current *member) ensureActive() error {
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.status == StatusActive {
		return nil
	}
	return fmt.Errorf("%w: member %q (%s) is %s; spawn a new member with team_spawn",
		ErrMemberInactive, current.name, current.id, current.status)
}

// recordResult 保存最新一次完成结果及其收件箱代次（seq=0 表示同步调用）。
func (current *member) recordResult(result rig.SubagentResult, seq int64) {
	current.mu.Lock()
	stored := result
	current.last = &stored
	if seq > current.resultSeq {
		current.resultSeq = seq
	}
	current.mu.Unlock()
}

// recordResult 保存成员结果并排队一条完成通知。
func (m *Manager) recordResult(current *member, result rig.SubagentResult, seq int64) {
	current.recordResult(result, seq)
	output := strings.TrimSpace(result.Output)
	if len(output) > 400 {
		output = output[:400] + "…"
	}
	m.queueNotice(fmt.Sprintf("subagent %s (%s) finished: %s", current.id, current.name, output))
}

// queueNotice 追加一条待处理风格完成通知。
func (m *Manager) queueNotice(notice string) {
	if m == nil || strings.TrimSpace(notice) == "" {
		return
	}
	m.mu.Lock()
	m.notices = append(m.notices, notice)
	m.mu.Unlock()
}

// PendingNotices 取走排队的完成通知（实现 rig.NoticeSource）。
func (m *Manager) PendingNotices() []string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	notices := m.notices
	m.notices = nil
	return notices
}

// watchCompletion 等待成员当前运行并记录其结果，使 agent 在下一步收风格完成
// 通知。
func (m *Manager) watchCompletion(ref string) {
	go func() {
		if _, err := m.Wait(context.Background(), ref); err != nil {
			m.queueNotice(fmt.Sprintf("subagent %s ended: %v", ref, err))
		}
	}()
}

// contextError 返回 ctx.Err()，并容忍 nil context。
func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
