// Package inbox implements a host-agnostic, durable, per-session inbox of
// human prompts: the prompts a human submits while a session's turn is already
// running.
//
// It is the rig port of the DSH prompt queue. A host that rejects a prompt
// while the session is busy can append it here instead, and the durable record
// survives a restart without the desktop UI owning a private in-memory queue.
//
// # Durable shape
//
// Every mutation appends one event of type EventType ("queue/change") whose
// payload is the complete inbox snapshot, oldest first:
//
//	{"prompts":[{"id":"prompt-...","text":"...","createdAt":"..."}]}
//
// Replay keeps only the latest snapshot (highest seq) and never folds
// add/remove deltas, so a lost or reordered intermediate event cannot corrupt
// the queue. An empty snapshot is authoritative: it clears the inbox. Replay
// drops malformed entries (blank id/text, duplicate ids) and caps the result at
// MaxEntries; a corrupt payload never fails a restore.
//
// # Settle semantics (host-agnostic part of the DSH order)
//
//   - Order is FIFO. Take removes the oldest entry durably *before* the caller
//     starts its run, so a crash cannot replay a prompt that already ran. The
//     accepted opposite trade-off is losing the prompt if the process dies
//     after Take persisted but before the run started.
//   - If starting the run fails, the caller puts the entry back at the front
//     with Requeue, so transient failures never drop human input.
//   - The package never clears the inbox on its own. A user Stop or a cancelled
//     run leaves it intact; a host records the "waiting for a human gesture"
//     fact with MarkPending and drains on the next prompt. Only an empty inbox
//     should let a goal-continuation driver admit another round; callers gate
//     on Count or List.
//   - A deleted session drops its inbox with Drop. DeleteSession must also
//     delete the durable session events; this package cannot do that through
//     EventLog, so Drop only resets this process's view and tombstones the id
//     for later reads.
//
// # Limits of the port
//
// This package cannot validate that a session exists (that registry belongs to
// the host) and cannot start a run. Enqueue only queues; the host decides when
// to drain with Take, mirroring "start immediately when idle" at its own
// scheduling point. A nil EventLog keeps the inbox in memory only.
package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

const (
	// EventType is the durable session-event type of an inbox snapshot. Hosts
	// can use it to render or replay the inbox outside this package.
	EventType = "queue/change"

	// MaxEntries bounds one session's inbox for Enqueue. It only holds prompts
	// a human sent during a running turn, so an unbounded queue would be a bug
	// (or a runaway automation loop); excess prompts are rejected with
	// ErrInboxFull. Requeue is exempt: it restores an already-accepted entry
	// and never drops human input.
	MaxEntries = 200

	// idPrefix is prepended to generated entry ids.
	idPrefix = "prompt-"
)

var (
	// ErrEmptyPrompt reports an Enqueue with a blank prompt.
	ErrEmptyPrompt = errors.New("inbox: prompt is empty")

	// ErrInboxFull reports an Enqueue that would exceed MaxEntries.
	ErrInboxFull = errors.New("inbox: too many queued prompts")

	// ErrNoSession reports an operation without a session id.
	ErrNoSession = errors.New("inbox: session id is required")

	// errNilInbox reports a method call on a nil *Inbox.
	errNilInbox = errors.New("inbox: nil inbox")
)

// EventLog is the slice of the session log the durable inbox needs: replay and
// append. A log implementation that assigns sequence numbers itself may ignore
// SessionEvent.Seq (see rig.SessionEvent).
type EventLog interface {
	// Events returns the session's events with from <= seq <= to; to <= 0
	// means "until the end". Order is not assumed: replay selects the highest
	// seq.
	Events(ctx context.Context, sessionID string, from, to int64) ([]rig.SessionEvent, error)
	// AppendEvent records one event.
	AppendEvent(ctx context.Context, event rig.SessionEvent) error
}

// Entry is one queued prompt.
type Entry struct {
	// ID is the durable entry id, stable across restarts (unlike a slice index).
	ID string `json:"id"`
	// Text is the trimmed prompt text.
	Text string `json:"text"`
	// Created is when the prompt joined the inbox.
	Created time.Time `json:"createdAt"`
}

// snapshot is the durable queue/change payload: the complete inbox.
type snapshot struct {
	Prompts []Entry `json:"prompts"`
}

// Inbox is a durable per-session prompt inbox. It is safe for concurrent use.
// The zero value is not usable; call New.
type Inbox struct {
	log EventLog
	now func() time.Time

	mu      sync.Mutex
	queues  map[string][]Entry
	loaded  map[string]bool
	pending map[string]bool
	seq     atomic.Uint64
}

// New returns an empty inbox backed by log. A nil log keeps the inbox in
// memory only (useful in tests and in hosts without a session log).
func New(log EventLog) *Inbox {
	return &Inbox{
		log:     log,
		now:     time.Now,
		queues:  make(map[string][]Entry),
		loaded:  make(map[string]bool),
		pending: make(map[string]bool),
	}
}

// Enqueue appends one prompt to the session's inbox and persists the new
// snapshot. The prompt is trimmed; blank prompts are rejected with
// ErrEmptyPrompt and a full inbox with ErrInboxFull. When persistence fails the
// in-memory queue is left as it was and the error is returned.
//
// Enqueue never starts a run: the host drains with Take when it can.
func (s *Inbox) Enqueue(ctx context.Context, sessionID, prompt string) (Entry, error) {
	if s == nil {
		return Entry{}, errNilInbox
	}
	ctx = nonNilContext(ctx)
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	sessionID = strings.TrimSpace(sessionID)
	prompt = strings.TrimSpace(prompt)
	if sessionID == "" {
		return Entry{}, ErrNoSession
	}
	if prompt == "" {
		return Entry{}, ErrEmptyPrompt
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(ctx, sessionID); err != nil {
		return Entry{}, err
	}
	queue := s.queues[sessionID]
	if len(queue) >= MaxEntries {
		return Entry{}, fmt.Errorf("inbox: session %q is full (%d prompts): %w", sessionID, MaxEntries, ErrInboxFull)
	}
	entry := Entry{ID: s.newIDLocked(), Text: prompt, Created: s.clock()}
	next := make([]Entry, 0, len(queue)+1)
	next = append(next, queue...)
	next = append(next, entry)
	s.queues[sessionID] = next
	if err := s.persistLocked(ctx, sessionID); err != nil {
		// Keep memory and the durable log consistent: a prompt whose snapshot
		// was not written must not look queued for this process either.
		s.restoreLocked(sessionID, queue)
		return Entry{}, err
	}
	return entry, nil
}

// List returns the session's inbox in send order, oldest first. The returned
// slice is a copy the caller may retain.
func (s *Inbox) List(ctx context.Context, sessionID string) ([]Entry, error) {
	if s == nil {
		return nil, nil
	}
	ctx = nonNilContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(ctx, sessionID); err != nil {
		return nil, err
	}
	queue := s.queues[sessionID]
	if len(queue) == 0 {
		return nil, nil
	}
	return append([]Entry(nil), queue...), nil
}

// Count reports how many prompts wait in the session's inbox, without copying
// the queue. Settle code gates goal continuation on Count() == 0.
func (s *Inbox) Count(ctx context.Context, sessionID string) (int, error) {
	if s == nil {
		return 0, nil
	}
	ctx = nonNilContext(ctx)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(ctx, sessionID); err != nil {
		return 0, err
	}
	return len(s.queues[sessionID]), nil
}

// Remove deletes one entry by id and persists the new snapshot. It is
// idempotent: an unknown session or id is a no-op. Removing the last entry also
// clears a pending-drain marker. A persistence failure rolls the removal back.
func (s *Inbox) Remove(ctx context.Context, sessionID, id string) error {
	if s == nil {
		return nil
	}
	ctx = nonNilContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	sessionID = strings.TrimSpace(sessionID)
	id = strings.TrimSpace(id)
	if sessionID == "" || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(ctx, sessionID); err != nil {
		return err
	}
	queue := s.queues[sessionID]
	index := -1
	for position, entry := range queue {
		if entry.ID == id {
			index = position
			break
		}
	}
	if index < 0 {
		return nil
	}
	next := make([]Entry, 0, len(queue)-1)
	next = append(next, queue[:index]...)
	next = append(next, queue[index+1:]...)
	s.restoreLocked(sessionID, next)
	if err := s.persistLocked(ctx, sessionID); err != nil {
		s.restoreLocked(sessionID, queue)
		return err
	}
	if len(next) == 0 {
		delete(s.pending, sessionID)
	}
	return nil
}

// Take removes and returns the oldest entry, durably persisting the new
// snapshot before returning. This is the dequeue half of the DSH settle order:
// call it before starting the run, and Requeue the entry at the front if the
// run cannot start. A persistence failure keeps the entry queued and returns
// the error (ok is false), so the caller must not start the run.
func (s *Inbox) Take(ctx context.Context, sessionID string) (Entry, bool, error) {
	if s == nil {
		return Entry{}, false, nil
	}
	ctx = nonNilContext(ctx)
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Entry{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(ctx, sessionID); err != nil {
		return Entry{}, false, err
	}
	queue := s.queues[sessionID]
	if len(queue) == 0 {
		return Entry{}, false, nil
	}
	entry := queue[0]
	s.restoreLocked(sessionID, queue[1:])
	if err := s.persistLocked(ctx, sessionID); err != nil {
		s.restoreLocked(sessionID, queue)
		return Entry{}, false, err
	}
	return entry, true, nil
}

// Requeue puts an entry back at the front of the inbox (a drain whose run
// could not start), preserving FIFO order. A duplicate id is a no-op; a blank
// id or text is rejected. A persistence failure rolls the requeue back.
//
// Requeue is the recovery half of Take and is not subject to MaxEntries: an
// entry that was already accepted must never be dropped, and a concurrent
// Enqueue may have filled the slot Take freed.
func (s *Inbox) Requeue(ctx context.Context, sessionID string, entry Entry) error {
	if s == nil {
		return nil
	}
	ctx = nonNilContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	sessionID = strings.TrimSpace(sessionID)
	entry.ID = strings.TrimSpace(entry.ID)
	entry.Text = strings.TrimSpace(entry.Text)
	if sessionID == "" {
		return ErrNoSession
	}
	if entry.ID == "" {
		return errors.New("inbox: entry id is required")
	}
	if entry.Text == "" {
		return ErrEmptyPrompt
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(ctx, sessionID); err != nil {
		return err
	}
	queue := s.queues[sessionID]
	for _, existing := range queue {
		if existing.ID == entry.ID {
			return nil
		}
	}
	next := make([]Entry, 0, len(queue)+1)
	next = append(next, entry)
	next = append(next, queue...)
	s.restoreLocked(sessionID, next)
	if err := s.persistLocked(ctx, sessionID); err != nil {
		s.restoreLocked(sessionID, queue)
		return err
	}
	return nil
}

// MarkPending records that a stopped session left queued prompts behind: they
// wait for the next human gesture, not a background timer, so Stop stays an
// explicit stop. The marker is process-local (it is not durable); it is cleared
// by ClearPending, by Take's caller after a run starts, and by Remove when the
// inbox empties.
func (s *Inbox) MarkPending(sessionID string) {
	if s == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		s.pending = make(map[string]bool)
	}
	s.pending[sessionID] = true
}

// Pending reports whether the session's inbox waits for a human gesture after
// a Stop.
func (s *Inbox) Pending(sessionID string) bool {
	if s == nil {
		return false
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending[sessionID]
}

// ClearPending drops a session's pending-drain marker.
func (s *Inbox) ClearPending(sessionID string) {
	if s == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, sessionID)
}

// Drop removes a session's inbox from this process without appending an event.
// The host deletes the durable session events with the session row; Drop
// tombstones the id for later reads so a late drain cannot resurrect the queue.
func (s *Inbox) Drop(sessionID string) {
	if s == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.queues, sessionID)
	delete(s.pending, sessionID)
	s.loaded[sessionID] = true
}

// restoreLocked installs one in-memory queue snapshot; an empty queue removes
// the map entry.
func (s *Inbox) restoreLocked(sessionID string, queue []Entry) {
	if len(queue) == 0 {
		delete(s.queues, sessionID)
		return
	}
	s.queues[sessionID] = append([]Entry(nil), queue...)
}

// loadLocked replays the session's latest durable snapshot exactly once. A log
// error is returned (and the session stays unloaded so a later call retries);
// a corrupt payload is not an error and restores an empty inbox.
func (s *Inbox) loadLocked(ctx context.Context, sessionID string) error {
	if s.loaded[sessionID] {
		return nil
	}
	if s.log == nil {
		s.loaded[sessionID] = true
		return nil
	}
	events, err := s.log.Events(ctx, sessionID, 0, 0)
	if err != nil {
		return fmt.Errorf("inbox: load session %q: %w", sessionID, err)
	}
	var (
		latest rig.SessionEvent
		found  bool
	)
	for _, event := range events {
		if event.Type != EventType {
			continue
		}
		if event.SessionID != "" && event.SessionID != sessionID {
			continue
		}
		// Events order is not part of the EventLog contract, so pick the
		// highest seq. Logs without seq keep slice order (later wins).
		if !found || event.Seq >= latest.Seq {
			latest = event
			found = true
		}
	}
	if found {
		s.restoreLocked(sessionID, decodeSnapshot(latest.Payload))
	}
	s.loaded[sessionID] = true
	return nil
}

// persistLocked appends the session's complete inbox snapshot. The caller holds
// mu, so the append serializes with the in-memory mutation and a durable
// dequeue can never be reordered behind a later enqueue.
func (s *Inbox) persistLocked(ctx context.Context, sessionID string) error {
	if s.log == nil {
		return nil
	}
	prompts := make([]Entry, len(s.queues[sessionID]))
	copy(prompts, s.queues[sessionID])
	payload, err := json.Marshal(snapshot{Prompts: prompts})
	if err != nil {
		return fmt.Errorf("inbox: encode snapshot of %q: %w", sessionID, err)
	}
	event := rig.SessionEvent{
		SessionID: sessionID,
		Type:      EventType,
		Time:      s.clock().UTC(),
		Payload:   payload,
	}
	if err := s.log.AppendEvent(ctx, event); err != nil {
		return fmt.Errorf("inbox: persist snapshot of %q: %w", sessionID, err)
	}
	return nil
}

// newIDLocked returns a durable entry id (unique per process and stable across
// restarts, unlike a slice index).
func (s *Inbox) newIDLocked() string {
	return fmt.Sprintf("%s%d-%d", idPrefix, s.clock().UnixNano(), s.seq.Add(1))
}

// clock reads the inbox clock (tests replace it).
func (s *Inbox) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// decodeSnapshot parses one durable inbox snapshot, dropping malformed entries
// (a corrupt event must not fail the restore) and capping at MaxEntries.
func decodeSnapshot(payload []byte) []Entry {
	if len(payload) == 0 {
		return nil
	}
	var decoded snapshot
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil
	}
	entries := make([]Entry, 0, len(decoded.Prompts))
	seen := make(map[string]struct{}, len(decoded.Prompts))
	for _, entry := range decoded.Prompts {
		id := strings.TrimSpace(entry.ID)
		text := strings.TrimSpace(entry.Text)
		if id == "" || text == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		entry.ID = id
		entry.Text = text
		entries = append(entries, entry)
		if len(entries) >= MaxEntries {
			break
		}
	}
	return entries
}

// nonNilContext replaces a nil context with context.Background.
func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
