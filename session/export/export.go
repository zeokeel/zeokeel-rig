// Package export archives one session's durable event log as a ZIP.
//
// The archive is the durable truth, not a rendered transcript: events.jsonl
// holds one JSON object per persisted session event (seq / type / time / the
// payload's raw JSON) and meta.json names the session and the export. It
// deliberately contains no model prompt injections, no UI state and nothing
// that only lives in memory, and it is bounded: an event log past the limit is
// reported as too large instead of being truncated into a silently partial
// audit artifact.
//
// The package is host-agnostic: it reads events through a narrow EventLog
// (either a whole-log Events source or a tail-only Last source) and takes the
// session identity as a Meta value. The product's exporter over a durable store
// (internal/modules/agent/session_log_export.go) maps onto this package by
// passing the store's Events method and its session meta.
package export

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
)

const (
	// ArchiveVersion is the archive format version written to meta.json
	// ("version"); bump it when the archive layout changes.
	ArchiveVersion = "1.0"
	// ArchiveSchema is the events record schema written to meta.json ("schema").
	ArchiveSchema = 1
	// MaxEvents bounds how many durable events one archive can carry. A longer
	// log is reported as too large instead of truncated.
	MaxEvents = 20000
	// MaxBytes bounds the events.jsonl part of the archive (64 MiB).
	MaxBytes = 64 << 20
)

// Errors the archive reports; callers should match with errors.Is rather than
// compare strings.
var (
	// ErrNoLog means Archive was given no event source (nil).
	ErrNoLog = errors.New("export: no event log")
	// ErrSessionRequired means the session id was empty after trimming.
	ErrSessionRequired = errors.New("export: session id is required")
	// ErrTooLarge means the event log exceeds the event or byte bound.
	ErrTooLarge = errors.New("export: archive exceeds its bound")
	// ErrUnsupportedLog means the event source provides neither Events nor Last.
	ErrUnsupportedLog = errors.New("export: event log provides neither Events nor Last")
)

// EventsLog is a store that can return one session's whole event log.
type EventsLog interface {
	// Events returns from <= seq <= to; to <= 0 means through the end.
	Events(ctx context.Context, sessionID string, from, to int64) ([]rig.SessionEvent, error)
}

// RecentLog is a store that only keeps the newest events of a session. It is
// the fallback for hosts that cannot replay a whole log; Archive asks for at
// most MaxEvents+1 events so an over-long log is still detected.
type RecentLog interface {
	// Last returns up to limit of the session's newest events, in ascending
	// sequence order.
	Last(ctx context.Context, sessionID string, limit int) ([]rig.SessionEvent, error)
}

// EventLog is the durable event source an archive is read from. A value
// satisfies it by implementing EventsLog (preferred: the whole log) or
// RecentLog (tail-only stores). Archive prefers Events when both are present.
type EventLog interface{}

// Meta is the host-supplied archive identity. Its fields are copied into
// meta.json; the format version and schema are owned by this package.
type Meta struct {
	// ID is the session identity written to meta.json; empty uses the
	// sessionID passed to Archive.
	ID string
	// Title is the human-readable session title (may be empty).
	Title string
	// ExportedAt is the archive clock; zero uses the current time. The value is
	// always written as UTC RFC3339, so a caller can pin it for a reproducible
	// archive.
	ExportedAt time.Time
}

// eventLine is one events.jsonl record: the durable event, verbatim. The
// session id is not repeated on every line (meta.json owns identity).
type eventLine struct {
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Time    time.Time       `json:"time"`
	Payload json.RawMessage `json:"payload"`
}

// metaDocument is meta.json: the export header, not UI state.
type metaDocument struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	ExportedAt string `json:"exportedAt"`
	Version    string `json:"version"`
	Schema     int    `json:"schema"`
}

// limits are the archive bounds; tests shrink them through archiveWithLimits.
type limits struct {
	maxEvents int
	maxBytes  int
}

// defaultLimits returns the exported bounds.
func defaultLimits() limits {
	return limits{maxEvents: MaxEvents, maxBytes: MaxBytes}
}

// Archive returns the ZIP bytes for one session's durable event log. The archive
// contains meta.json and events.jsonl (one JSON object per event).
//
// A nil log reports ErrNoLog, an empty session id reports ErrSessionRequired, a
// log without Events or Last reports ErrUnsupportedLog, and a log past MaxEvents
// or MaxBytes reports ErrTooLarge. Transport/store failures are returned as-is.
func Archive(ctx context.Context, log EventLog, sessionID string, meta Meta) ([]byte, error) {
	return archiveWithLimits(ctx, log, sessionID, meta, defaultLimits())
}

// archiveWithLimits is Archive with injectable bounds (deterministic tests).
func archiveWithLimits(ctx context.Context, log EventLog, sessionID string, meta Meta, bounds limits) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if log == nil {
		return nil, ErrNoLog
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, ErrSessionRequired
	}
	events, err := readEvents(ctx, log, sessionID, bounds)
	if err != nil {
		return nil, err
	}

	id := strings.TrimSpace(meta.ID)
	if id == "" {
		id = sessionID
	}
	exportedAt := meta.ExportedAt
	if exportedAt.IsZero() {
		exportedAt = time.Now()
	}
	header, err := json.Marshal(metaDocument{
		ID:         id,
		Title:      meta.Title,
		ExportedAt: exportedAt.UTC().Format(time.RFC3339),
		Version:    ArchiveVersion,
		Schema:     ArchiveSchema,
	})
	if err != nil {
		return nil, err
	}

	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	metaFile, err := archive.Create("meta.json")
	if err != nil {
		return nil, err
	}
	if _, err := metaFile.Write(header); err != nil {
		return nil, err
	}
	jsonl, err := archive.Create("events.jsonl")
	if err != nil {
		return nil, err
	}
	written := 0
	for _, event := range events {
		line, err := json.Marshal(eventLine{
			Seq:     event.Seq,
			Type:    event.Type,
			Time:    event.Time,
			Payload: event.Payload,
		})
		if err != nil {
			return nil, err
		}
		written += len(line) + 1
		if written > bounds.maxBytes {
			return nil, fmt.Errorf("%w: more than %d bytes of events", ErrTooLarge, bounds.maxBytes)
		}
		if _, err := jsonl.Write(append(line, '\n')); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// readEvents reads the session's events through whichever capability the log
// provides and enforces the event bound.
func readEvents(ctx context.Context, log EventLog, sessionID string, bounds limits) ([]rig.SessionEvent, error) {
	switch source := log.(type) {
	case EventsLog:
		events, err := source.Events(ctx, sessionID, 1, 0)
		if err != nil {
			return nil, err
		}
		if len(events) > bounds.maxEvents {
			return nil, fmt.Errorf("%w: %d events (limit %d)", ErrTooLarge, len(events), bounds.maxEvents)
		}
		return events, nil
	case RecentLog:
		// Ask for one past the bound so a too-long tail is detected rather than
		// silently truncated by the store.
		events, err := source.Last(ctx, sessionID, bounds.maxEvents+1)
		if err != nil {
			return nil, err
		}
		if len(events) > bounds.maxEvents {
			return nil, fmt.Errorf("%w: event log has more than %d events", ErrTooLarge, bounds.maxEvents)
		}
		// Tail stores return "the newest N" in ascending sequence order; sort a
		// copy defensively so the archive is chronological even if a host does
		// not, without mutating the store's own slice.
		ordered := make([]rig.SessionEvent, len(events))
		copy(ordered, events)
		sort.SliceStable(ordered, func(left, right int) bool {
			return ordered[left].Seq < ordered[right].Seq
		})
		return ordered, nil
	default:
		return nil, ErrUnsupportedLog
	}
}
