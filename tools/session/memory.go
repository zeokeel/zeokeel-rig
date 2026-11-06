package session

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/zeokeel/zeokeel-rig"
)

// MemoryLog is an in-memory rig.SessionLog. It is safe for concurrent use and
// keeps events in sequence order; Append assigns the next sequence number when
// the event has none.
//
// It also implements the optional SessionLister extension used by tools/inspect
// (Sessions), so an embedder that only needs a scratch session log can use one
// type for both.
type MemoryLog struct {
	mu      sync.Mutex
	events  []rig.SessionEvent
	nextSeq int64
}

// NewMemoryLog returns an empty session log.
func NewMemoryLog() *MemoryLog { return &MemoryLog{} }

// Append stores one event and returns the stored copy. A non-positive Seq is
// replaced by the log's next sequence number; a higher Seq advances the
// counter. Payload bytes are copied, so the caller may reuse its slice.
func (l *MemoryLog) Append(event rig.SessionEvent) rig.SessionEvent {
	if l == nil {
		return event
	}
	if event.Payload != nil {
		event.Payload = append(json.RawMessage(nil), event.Payload...)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if event.Seq <= 0 {
		l.nextSeq++
		event.Seq = l.nextSeq
	} else if event.Seq > l.nextSeq {
		l.nextSeq = event.Seq
	}
	l.events = append(l.events, event)
	return event
}

// Len reports the number of stored events.
func (l *MemoryLog) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

// Sessions returns the session ids present in the log, sorted. It is the
// optional SessionLister extension used by tools/inspect.
func (l *MemoryLog) Sessions(ctx context.Context) ([]string, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if l == nil {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := make(map[string]struct{}, len(l.events))
	for _, event := range l.events {
		if id := strings.TrimSpace(event.SessionID); id != "" {
			seen[id] = struct{}{}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// Events implements rig.SessionLog: events of sessionID with from <= seq <= to.
// An empty sessionID matches every session (useful for cross-session tracing);
// from <= 0 starts at the beginning and to <= 0 means "until the end".
func (l *MemoryLog) Events(ctx context.Context, sessionID string, from, to int64) ([]rig.SessionEvent, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if l == nil {
		return nil, nil
	}
	sessionID = strings.TrimSpace(sessionID)
	l.mu.Lock()
	defer l.mu.Unlock()
	events := make([]rig.SessionEvent, 0)
	for _, event := range l.events {
		if sessionID != "" && event.SessionID != sessionID {
			continue
		}
		if from > 0 && event.Seq < from {
			continue
		}
		if to > 0 && event.Seq > to {
			continue
		}
		events = append(events, copyEvent(event))
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
	return events, nil
}

// Search implements rig.SessionLog: events whose session id, type or payload
// contains query, case-insensitively. An empty query matches every event; a
// limit <= 0 returns all matches.
func (l *MemoryLog) Search(ctx context.Context, query string, limit int) ([]rig.SessionEvent, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if l == nil {
		return nil, nil
	}
	query = strings.ToLower(strings.TrimSpace(query))
	l.mu.Lock()
	defer l.mu.Unlock()
	events := make([]rig.SessionEvent, 0)
	for _, event := range l.events {
		if limit > 0 && len(events) >= limit {
			break
		}
		if query != "" && !matches(event, query) {
			continue
		}
		events = append(events, copyEvent(event))
	}
	return events, nil
}

// Last implements rig.SessionLog: the most recent events of one session. A
// limit <= 0 returns every event of the session.
func (l *MemoryLog) Last(ctx context.Context, sessionID string, limit int) ([]rig.SessionEvent, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, errors.New("session: session id is required")
	}
	if l == nil {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	events := make([]rig.SessionEvent, 0)
	for _, event := range l.events {
		if event.SessionID == sessionID {
			events = append(events, copyEvent(event))
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	return events, nil
}

func matches(event rig.SessionEvent, lowerQuery string) bool {
	if strings.Contains(strings.ToLower(event.SessionID), lowerQuery) ||
		strings.Contains(strings.ToLower(event.Type), lowerQuery) {
		return true
	}
	return strings.Contains(strings.ToLower(string(event.Payload)), lowerQuery)
}

func copyEvent(event rig.SessionEvent) rig.SessionEvent {
	if event.Payload != nil {
		event.Payload = append(json.RawMessage(nil), event.Payload...)
	}
	return event
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
