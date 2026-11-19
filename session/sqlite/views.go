package sqlite

// The views the agent loop consumes: message history, interruption checkpoints
// and the session event log. They are thin adapters over the same tables, so a
// host passes them to agent.AgentSpec without learning a second API.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
)

// SessionLog is the writable event log: the read contract rig defines plus the
// append the agent and the host use to record what happened.
type SessionLog interface {
	rig.SessionLog
	// AppendEvent stores one event, assigning its sequence number and time when
	// the caller left them empty.
	AppendEvent(ctx context.Context, event rig.SessionEvent) error
}

// History returns the message history view for rig.AgentSpec.History.
func (s *Store) History() rig.History { return historyView{store: s} }

// Checkpoints returns the checkpoint view for rig.AgentSpec.CheckpointStore.
func (s *Store) Checkpoints() rig.CheckpointStore { return checkpointView{store: s} }

// SessionLog returns the event log view for rig.AgentSpec.SessionLog.
func (s *Store) SessionLog() SessionLog { return eventView{store: s} }

// historyView adapts the database to rig.History.
type historyView struct{ store *Store }

var (
	_ rig.History         = historyView{}
	_ rig.HistoryReplacer = historyView{}
)

// Load returns a session's messages, or nothing for a session that was never
// written: rig treats an unknown session as an empty history, not an error.
func (h historyView) Load(ctx context.Context, sessionID string) ([]rig.Message, error) {
	_, messages, err := h.store.Load(ctx, sessionID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return messages, err
}

// Append stores one call's messages atomically.
func (h historyView) Append(ctx context.Context, sessionID string, messages ...rig.Message) error {
	_, err := h.store.AppendBatch(ctx, sessionID, messages...)
	return err
}

// Replace swaps the whole window (manual compaction).
func (h historyView) Replace(ctx context.Context, sessionID string, messages []rig.Message) error {
	_, err := h.store.Replace(ctx, sessionID, messages)
	return err
}

// checkpointView adapts the database to rig.CheckpointStore.
type checkpointView struct{ store *Store }

var _ rig.CheckpointStore = checkpointView{}

// Load returns the session's latest checkpoint, or nil when there is none.
func (c checkpointView) Load(ctx context.Context, sessionID string) (*rig.Checkpoint, error) {
	if err := c.store.usable(); err != nil {
		return nil, err
	}
	var payload []byte
	err := c.store.db.QueryRowContext(ctx,
		`SELECT payload FROM rig_checkpoints WHERE session_id = ?`, sessionID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: load checkpoint %q: %w", sessionID, err)
	}
	var checkpoint rig.Checkpoint
	if err := json.Unmarshal(payload, &checkpoint); err != nil {
		return nil, fmt.Errorf("sqlite: decode checkpoint %q: %w", sessionID, err)
	}
	return &checkpoint, nil
}

// Save overwrites the session's checkpoint.
func (c checkpointView) Save(ctx context.Context, sessionID string, checkpoint *rig.Checkpoint) error {
	if err := c.store.usable(); err != nil {
		return err
	}
	if checkpoint == nil {
		return c.Clear(ctx, sessionID)
	}
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		return fmt.Errorf("sqlite: encode checkpoint %q: %w", sessionID, err)
	}
	// A checkpoint can be the first thing written for a session: a turn that
	// fails before its first append still has to be resumable, so the row is
	// created here rather than refused by the foreign key.
	now := time.Now().UTC().UnixMilli()
	if _, err := c.store.db.ExecContext(ctx,
		`INSERT INTO rig_sessions(id, title, workspace_id, created_at, updated_at)
		 VALUES(?, '', '', ?, ?) ON CONFLICT(id) DO NOTHING`,
		sessionID, now, now); err != nil {
		return fmt.Errorf("sqlite: save checkpoint %q: ensure session: %w", sessionID, err)
	}
	if _, err := c.store.db.ExecContext(ctx,
		`INSERT INTO rig_checkpoints(session_id, payload, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET payload = excluded.payload, updated_at = excluded.updated_at`,
		sessionID, payload, now); err != nil {
		return fmt.Errorf("sqlite: save checkpoint %q: %w", sessionID, err)
	}
	return nil
}

// Clear removes the checkpoint; clearing an absent one is not an error.
func (c checkpointView) Clear(ctx context.Context, sessionID string) error {
	if err := c.store.usable(); err != nil {
		return err
	}
	if _, err := c.store.db.ExecContext(ctx, `DELETE FROM rig_checkpoints WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("sqlite: clear checkpoint %q: %w", sessionID, err)
	}
	return nil
}

// eventView adapts the database to the event-log contract.
type eventView struct{ store *Store }

var _ SessionLog = eventView{}

// AppendEvent stores one event. The sequence is per session and dense: the next
// value after the highest one already stored.
func (e eventView) AppendEvent(ctx context.Context, event rig.SessionEvent) error {
	if err := e.store.usable(); err != nil {
		return err
	}
	if event.SessionID == "" {
		return errors.New("sqlite: event has no session id")
	}
	if event.Type == "" {
		return errors.New("sqlite: event has no type")
	}
	at := event.Time
	if at.IsZero() {
		at = time.Now().UTC()
	}
	payload := event.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	_, err := e.store.db.ExecContext(ctx,
		`INSERT INTO rig_events(session_id, seq, kind, at, payload)
		 VALUES(?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM rig_events WHERE session_id = ?), ?, ?, ?)`,
		event.SessionID, event.SessionID, event.Type, at.UnixMilli(), []byte(payload))
	if err != nil {
		return fmt.Errorf("sqlite: append event %q: %w", event.Type, err)
	}
	return nil
}

// Events returns from <= seq <= to (to <= 0 means "to the end").
func (e eventView) Events(ctx context.Context, sessionID string, from, to int64) ([]rig.SessionEvent, error) {
	if err := e.store.usable(); err != nil {
		return nil, err
	}
	query := `SELECT seq, session_id, kind, at, payload FROM rig_events WHERE session_id = ? AND seq >= ?`
	args := []any{sessionID, from}
	if to > 0 {
		query += ` AND seq <= ?`
		args = append(args, to)
	}
	query += ` ORDER BY seq`
	return e.query(ctx, query, args...)
}

// Last returns a session's most recent events in seq order.
func (e eventView) Last(ctx context.Context, sessionID string, limit int) ([]rig.SessionEvent, error) {
	if err := e.store.usable(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	events, err := e.query(ctx,
		`SELECT seq, session_id, kind, at, payload FROM rig_events WHERE session_id = ?
		 ORDER BY seq DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	// The query walks backwards to take the tail cheaply; the caller reads
	// forward, as every other read does.
	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}
	return events, nil
}

// Search matches an event's type or payload text, newest first.
func (e eventView) Search(ctx context.Context, query string, limit int) ([]rig.SessionEvent, error) {
	if err := e.store.usable(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	like := "%" + query + "%"
	return e.query(ctx,
		`SELECT seq, session_id, kind, at, payload FROM rig_events
		 WHERE kind LIKE ? OR CAST(payload AS TEXT) LIKE ?
		 ORDER BY at DESC, seq DESC LIMIT ?`, like, like, limit)
}

// query runs an event query and decodes the rows.
func (e eventView) query(ctx context.Context, query string, args ...any) ([]rig.SessionEvent, error) {
	rows, err := e.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: read events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var events []rig.SessionEvent
	for rows.Next() {
		var (
			event rig.SessionEvent
			at    int64
			data  []byte
		)
		if err := rows.Scan(&event.Seq, &event.SessionID, &event.Type, &at, &data); err != nil {
			return nil, fmt.Errorf("sqlite: read events: %w", err)
		}
		event.Time = time.UnixMilli(at).UTC()
		event.Payload = json.RawMessage(data)
		events = append(events, event)
	}
	return events, rows.Err()
}
