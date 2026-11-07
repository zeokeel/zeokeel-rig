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

// Member statuses shown by team_list.
const (
	// StatusActive is a live member whose runs can be sent to and waited for.
	StatusActive = "active"
	// StatusInterrupted is a member whose current run was stopped; it stays
	// listed until team_close removes it and cannot run again.
	StatusInterrupted = "interrupted"
	// StatusClosed marks a member being stopped and removed. It is only
	// observable transiently; closed members do not stay in the table.
	StatusClosed = "closed"
)

// Defaults and limits. They are exported so hosts and tests can assert the
// documented behavior.
const (
	// DefaultMaxMembers is the default number of live members per Manager.
	DefaultMaxMembers = 8
	// DefaultWaitTimeout bounds team_wait when the call passes no timeout_ms.
	DefaultWaitTimeout = 30 * time.Second
	// MaxWaitTimeout caps a per-call team_wait timeout.
	MaxWaitTimeout = 10 * time.Minute
	// DefaultIDPrefix is the prefix of generated member ids ("member-1", ...).
	DefaultIDPrefix = "member-"
)

// Sentinel errors. Tool failures wrap them so a host can distinguish them with
// errors.Is; tools turn them into readable model-facing messages.
var (
	// ErrNoRunner reports that the manager has no SubagentRunner configured.
	ErrNoRunner = errors.New("no subagent runner is configured")
	// ErrManagerClosed reports work attempted after Manager.CloseAll.
	ErrManagerClosed = errors.New("team manager is closed")
	// ErrTeamFull reports a spawn beyond the manager's member limit.
	ErrTeamFull = errors.New("team is full")
	// ErrUnknownMember reports an id/name that matches no live member.
	ErrUnknownMember = errors.New("unknown team member")
	// ErrAmbiguousMember reports a name shared by several live members.
	ErrAmbiguousMember = errors.New("ambiguous team member name")
	// ErrMemberInactive reports a run sent to an interrupted member.
	ErrMemberInactive = errors.New("team member is not active")
)

// Option configures a Manager.
type Option func(*Manager)

// WithMaxMembers bounds the number of live members (non-positive keeps
// DefaultMaxMembers). Interrupted members still count until they are closed.
func WithMaxMembers(max int) Option {
	return func(manager *Manager) {
		if max > 0 {
			manager.maxMembers = max
		}
	}
}

// WithNamePrefix sets the prefix of generated member ids (default "member-");
// ids are prefix + sequence number, for example "agent-1" with
// WithNamePrefix("agent-").
func WithNamePrefix(prefix string) Option {
	return func(manager *Manager) {
		if prefix = strings.TrimSpace(prefix); prefix != "" {
			manager.idPrefix = prefix
		}
	}
}

// Member is a read-only snapshot of one team member.
type Member struct {
	// ID is the generated member id ("member-1", ...).
	ID string
	// Name is the display name from team_spawn.
	Name string
	// Status is StatusActive or StatusInterrupted.
	Status string
	// Created is when the member was spawned.
	Created time.Time
	// Last is the newest completed run result, when one exists.
	Last *rig.SubagentResult
}

// member is the mutable state of one live member.
type member struct {
	id       string
	name     string
	subagent rig.Subagent
	created  time.Time

	// callMu serializes manager-level Send and Wait calls for this member.
	// rig.Subagent implementations are safe for concurrent calls and serialize
	// their own runs; the mutex keeps one blocking tool call from racing another
	// at the manager boundary too.
	callMu sync.Mutex

	// mu guards status and last.
	mu     sync.Mutex
	status string
	last   *rig.SubagentResult
}

// Manager owns the subagents of one agent session: spawn, name/id resolution,
// message passing, result access and the member limit.
//
// It is safe for concurrent use. A Manager cannot be reused after CloseAll.
type Manager struct {
	runner     rig.SubagentRunner
	maxMembers int
	idPrefix   string

	mu      sync.Mutex
	members []*member
	notices []string
	nextID  int
	closed  bool
	// spawning counts Spawn calls that reserved a slot but have not registered
	// their member yet (the runner may block on its own concurrency limit).
	spawning int
}

// NewManager returns a manager that starts children through runner. A nil
// runner is allowed: team_spawn then reports that no runner is configured,
// while list/wait/close still work for members started elsewhere.
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

// Len reports the number of live members (active and interrupted).
func (m *Manager) Len() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.members)
}

// Members returns a snapshot of the live members in spawn order.
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

// Spawn starts a child agent and registers it as a member. The first run
// continues asynchronously; use Wait (or team_wait) to read its result.
//
// ctx bounds only the start call (including a wait for the runner's concurrency
// limit), not the child's lifetime: the runner owns a background context for the
// child after Start returns. The manager lock is not held while Start runs, so a
// spawn blocked on runner capacity cannot deadlock listing, waiting on or
// closing other members. The manager owns the child until
// Interrupt/Close/CloseAll.
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

	// Reserve a slot, then start the child without holding the manager lock: the
	// runner may block on its own concurrency limit until another child finishes,
	// and holding m.mu there would deadlock every other team tool.
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
		// CloseAll ran while the child was starting: do not register it.
		m.mu.Unlock()
		_ = subagent.Stop()
		return Member{}, ErrManagerClosed
	}
	m.nextID++
	current := &member{
		id:       m.idPrefix + strconv.Itoa(m.nextID),
		name:     name,
		subagent: subagent,
		created:  time.Now(),
		status:   StatusActive,
	}
	m.members = append(m.members, current)
	m.mu.Unlock()
	return current.snapshot(), nil
}

// Send delivers a follow-up message to one member (by id, then unique name) and
// blocks until that run ends, returning its result.
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
	m.recordResult(current, result)
	return result, nil
}

// SendAsync accepts a message for a live member and returns immediately after
// the inbox handoff (DSH send_message semantics). The member's reply is recorded
// as its latest result and is available through Wait or Last.
func (m *Manager) SendAsync(ref, message string) error {
	current, err := m.resolve(ref)
	if err != nil {
		return err
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return errors.New("team: message is required")
	}
	if err := current.ensureActive(); err != nil {
		return err
	}
	current.callMu.Lock()
	// Hand the call to a background goroutine so the tool returns after
	// acceptance; the per-member call lock serializes it with team_send/team_wait.
	go func() {
		defer current.callMu.Unlock()
		if err := current.ensureActive(); err != nil {
			return
		}
		result, err := current.subagent.Send(context.Background(), message)
		if err != nil {
			return
		}
		m.recordResult(current, result)
	}()
	return nil
}

// Wait blocks for the member's current (or first) run and returns its result.
// Calling it after the run finished returns the same result.
func (m *Manager) Wait(ctx context.Context, ref string) (rig.SubagentResult, error) {
	current, err := m.resolve(ref)
	if err != nil {
		return rig.SubagentResult{}, err
	}
	if err := contextError(ctx); err != nil {
		return rig.SubagentResult{}, err
	}
	if err := current.ensureActive(); err != nil {
		return rig.SubagentResult{}, err
	}
	current.callMu.Lock()
	defer current.callMu.Unlock()
	if err := current.ensureActive(); err != nil {
		return rig.SubagentResult{}, err
	}
	result, err := current.subagent.Wait(ctx)
	if err != nil {
		return rig.SubagentResult{}, fmt.Errorf("team: member %q (%s) wait failed: %w", current.name, current.id, err)
	}
	m.recordResult(current, result)
	return result, nil
}

// Last returns the newest completed run result of one member. ok is false when
// the member has not completed a run yet.
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

// Interrupt stops the member's current run and keeps the member listed as
// interrupted. It is idempotent for an already interrupted member.
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

// Close stops the member and removes it from the team. The member is resolved
// by id first, then by unique name.
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

// CloseAll stops every live member and closes the manager. It is idempotent;
// after it the manager rejects spawn and member operations.
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

// resolve finds one live member: an exact id match wins; otherwise a unique
// name match is used and a shared name reports ErrAmbiguousMember.
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

// remove drops one member from the live table.
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

// snapshot returns a read-only copy of the member's state.
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

// ensureActive rejects runs on members that are not active.
func (current *member) ensureActive() error {
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.status == StatusActive {
		return nil
	}
	return fmt.Errorf("%w: member %q (%s) is %s; spawn a new member with team_spawn",
		ErrMemberInactive, current.name, current.id, current.status)
}

// recordResult stores the newest completed run result and queues a DSH-style
// completion notice for the owning model turn.
func (current *member) recordResult(result rig.SubagentResult) {
	current.mu.Lock()
	stored := result
	current.last = &stored
	current.mu.Unlock()
}

// recordResult stores a member result and queues one completion notice.
func (m *Manager) recordResult(current *member, result rig.SubagentResult) {
	current.recordResult(result)
	output := strings.TrimSpace(result.Output)
	if len(output) > 400 {
		output = output[:400] + "…"
	}
	m.queueNotice(fmt.Sprintf("subagent %s (%s) finished: %s", current.id, current.name, output))
}

// queueNotice appends one pending DSH-style completion notice.
func (m *Manager) queueNotice(notice string) {
	if m == nil || strings.TrimSpace(notice) == "" {
		return
	}
	m.mu.Lock()
	m.notices = append(m.notices, notice)
	m.mu.Unlock()
}

// PendingNotices drains queued completion notices (implements rig.NoticeSource).
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

// watchCompletion waits for the member's current run and records its result so
// the agent receives a DSH-style completion notice at its next step.
func (m *Manager) watchCompletion(ref string) {
	go func() {
		if _, err := m.Wait(context.Background(), ref); err != nil {
			m.queueNotice(fmt.Sprintf("subagent %s ended: %v", ref, err))
		}
	}()
}

// contextError returns ctx.Err(), tolerating a nil context.
func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
