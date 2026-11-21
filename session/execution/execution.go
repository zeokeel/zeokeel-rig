// Package execution folds durable execution lifecycle events back into a
// bounded read model of background jobs and terminal sessions, with opt-in live
// overlay for resources the current process still owns.
package execution

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

const (
	DefaultEventWindow = 4000
	DefaultLimit       = 50
	MaxLimit           = 200

	eventJobStarted     = "execution/job_started"
	eventJobFinished    = "execution/job_finished"
	eventTerminalOpened = "execution/terminal_opened"
	eventTerminalClosed = "execution/terminal_closed"

	KindJob      = "job"
	KindTerminal = "terminal"

	StateRunning     = "running"
	StateFinished    = "finished"
	StateInterrupted = "interrupted"
)

// Record is one execution resource.
type Record struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId,omitempty"`
	Kind      string    `json:"kind"`
	Command   string    `json:"command,omitempty"`
	CWD       string    `json:"cwd,omitempty"`
	Root      string    `json:"root,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	Transport string    `json:"transport,omitempty"`
	Started   time.Time `json:"started,omitempty"`
	Ended     time.Time `json:"ended,omitempty"`
	State     string    `json:"state,omitempty"`
	Outcome   string    `json:"outcome,omitempty"`
}

// Liveness reports whether the live manager still owns a resource.
type Liveness interface {
	Running(kind, id string) bool
}

// EventLog is the slice of the session log the read model needs.
type EventLog interface {
	Last(ctx context.Context, sessionID string, limit int) ([]rig.SessionEvent, error)
}

// Service overlays the durable fold with live ownership.
type Service struct {
	store EventLog
	live  Liveness
}

// NewService builds the read model (both arguments may be nil).
func NewService(store EventLog, live Liveness) *Service {
	return &Service{store: store, live: live}
}

// History returns the newest bounded records of one session.
func (s *Service) History(sessionID string, limit int) []Record {
	if s == nil || s.store == nil {
		return nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	events, err := s.store.Last(context.Background(), sessionID, DefaultEventWindow)
	if err != nil {
		return nil
	}
	records := Fold(events, sessionID, s.live)
	if limit = NormalizeLimit(limit); len(records) > limit {
		records = records[:limit]
	}
	return records
}

// NormalizeLimit applies the default and the cap.
func NormalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}

type startedPayload struct {
	ID        string `json:"id"`
	Command   string `json:"command"`
	CWD       string `json:"cwd"`
	Root      string `json:"root"`
	Mode      string `json:"mode"`
	Transport string `json:"transport"`
}

type endedPayload struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// Fold pairs start/end events per resource id, overlays live managers and
// returns records newest start first. Orphan ends and unreadable payloads are
// skipped; a duplicate start/end is treated as replay noise.
func Fold(events []rig.SessionEvent, sessionID string, live Liveness) []Record {
	records := make([]Record, 0, 8)
	byID := make(map[string]int, 8)
	for _, event := range events {
		if event.SessionID != "" && event.SessionID != sessionID {
			continue
		}
		switch event.Type {
		case eventJobStarted, eventTerminalOpened:
			var payload startedPayload
			if len(event.Payload) == 0 || json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			id := strings.TrimSpace(payload.ID)
			if id == "" {
				continue
			}
			if _, exists := byID[id]; exists {
				continue
			}
			record := Record{
				ID:        id,
				SessionID: sessionID,
				Kind:      KindJob,
				Command:   payload.Command,
				CWD:       payload.CWD,
				Root:      payload.Root,
				Mode:      payload.Mode,
				Transport: payload.Transport,
				Started:   event.Time,
			}
			if event.Type == eventTerminalOpened {
				record.Kind = KindTerminal
				record.Command = ""
			}
			byID[id] = len(records)
			records = append(records, record)
		case eventJobFinished, eventTerminalClosed:
			var payload endedPayload
			if len(event.Payload) == 0 || json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			index, ok := byID[strings.TrimSpace(payload.ID)]
			if !ok {
				continue
			}
			record := &records[index]
			if !record.Ended.IsZero() {
				continue
			}
			record.Ended = event.Time
			record.State = StateFinished
			if event.Type == eventJobFinished {
				record.Outcome = strings.TrimSpace(payload.State)
			} else {
				record.Outcome = "closed"
			}
		}
	}
	for index := range records {
		if records[index].State != "" {
			continue
		}
		if live != nil && live.Running(records[index].Kind, records[index].ID) {
			records[index].State = StateRunning
			continue
		}
		records[index].State = StateInterrupted
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Started.Equal(records[j].Started) {
			return records[i].ID > records[j].ID
		}
		return records[i].Started.After(records[j].Started)
	})
	return records
}
