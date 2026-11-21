// Package children persists the durable record of delegated child sessions:
// teammates, synchronous subagents and other long-running assistants a parent
// session spawns.
//
// The record is what lets a host list children after a restart, resume a
// historical child from its own conversation, and deliver exactly one
// completion receipt per child. The store is deliberately policy-light: the
// host supplies the file path, the id prefix, retention bounds and a normalize
// hook (truncation and status vocabulary are host decisions).
package children

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultVersion is the document version written when Options.Version is zero.
const DefaultVersion = 1

// Activity is one persisted activity row of a child session.
type Activity struct {
	ID    string    `json:"id,omitempty"`
	Kind  string    `json:"kind,omitempty"`
	Text  string    `json:"text,omitempty"`
	Time  time.Time `json:"time,omitempty"`
	Error bool      `json:"error,omitempty"`
}

// Record is one durable child session.
type Record struct {
	// ID is the unified child-session identity (host id prefix + random hex).
	ID string `json:"id"`
	// ParentSessionID is the conversation that spawned the child.
	ParentSessionID string `json:"parentSessionId,omitempty"`
	// BackendRef is the live manager/registry reference (backend-only).
	BackendRef string `json:"backendRef,omitempty"`
	// Kind is the host's child kind (subagent, teammate, ...).
	Kind string `json:"kind,omitempty"`
	// Name is the display name.
	Name string `json:"name,omitempty"`
	// Model is the model the child ran with.
	Model string `json:"model,omitempty"`
	// Prompt is the delegated task.
	Prompt string `json:"prompt,omitempty"`
	// Status is the host's status vocabulary.
	Status string `json:"status,omitempty"`
	// Output is the final answer.
	Output string `json:"output,omitempty"`
	// Error is the failure text.
	Error string `json:"error,omitempty"`
	// Fork reports a forked child.
	Fork bool `json:"fork,omitempty"`
	// Notified marks a finished child whose completion receipt was already
	// delivered to the parent (one receipt per child).
	Notified bool `json:"notified,omitempty"`
	// Started/Finished/Updated are the child's clocks.
	Started  time.Time `json:"started,omitempty"`
	Finished time.Time `json:"finished,omitempty"`
	Updated  time.Time `json:"updated,omitempty"`
	// UsageTokens/Iterations are the child's billed work.
	UsageTokens int `json:"usageTokens,omitempty"`
	Iterations  int `json:"iterations,omitempty"`
	// Activity is the child's activity log.
	Activity []Activity `json:"activity,omitempty"`
}

// document is the persisted file.
type document struct {
	Version  int      `json:"version"`
	Children []Record `json:"children,omitempty"`
}

// Options configures one store.
type Options struct {
	// Path is the JSON document path; empty keeps the store in memory.
	Path string
	// Logger receives warnings (nil discards them).
	Logger func(format string, args ...any)
	// IDPrefix is prepended to generated ids (default "child-").
	IDPrefix string
	// Keep bounds the retained records (0 = unbounded).
	Keep int
	// TTL drops records older than this by their newest clock (0 = keep all).
	TTL time.Duration
	// Normalize adjusts a record before it is stored (host status vocabulary,
	// truncation). nil keeps records as given.
	Normalize func(*Record)
	// Version is the document schema version this store writes and accepts
	// (0 = DefaultVersion). A document with another version is quarantined.
	Version int
	// OnLoad adjusts loaded records (host status vocabulary: a record still
	// "running" after a restart is interrupted). nil keeps records as stored.
	OnLoad func([]Record) []Record
	// Legacy seeds the store when the document does not exist yet (a host
	// migrating an older sidecar). Called once, before the first persist.
	Legacy func() []Record
	// Now is the clock (nil uses time.Now).
	Now func() time.Time
}

// Store is the durable child record document. It is safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	opts   Options
	path   string
	items  []Record
	closed bool
}

// Open loads (or creates) the store. A missing document is empty; an unreadable
// or corrupt document is quarantined once (renamed *.corrupt-<nanos>) so the next
// write cannot silently destroy it.
func Open(opts Options) *Store {
	if opts.IDPrefix == "" {
		opts.IDPrefix = "child-"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Version == 0 {
		opts.Version = DefaultVersion
	}
	store := &Store{opts: opts}
	if path := strings.TrimSpace(opts.Path); path != "" {
		store.path = path
		store.load()
	}
	return store
}

func (s *Store) logf(format string, args ...any) {
	if s != nil && s.opts.Logger != nil {
		s.opts.Logger(format, args...)
	}
}

// load reads the document; every failure degrades to an empty store.
func (s *Store) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.seedLegacy()
			return
		}
		s.quarantine(fmt.Sprintf("child-session document could not be read: %v", err))
		return
	}
	var decoded document
	if err := json.Unmarshal(data, &decoded); err != nil {
		s.quarantine(fmt.Sprintf("decode child-session document: %v", err))
		return
	}
	if decoded.Version != s.opts.Version {
		s.quarantine(fmt.Sprintf("child-session document has unsupported version %d", decoded.Version))
		return
	}
	if s.opts.OnLoad != nil {
		decoded.Children = s.opts.OnLoad(decoded.Children)
	}
	s.mu.Lock()
	s.items = decoded.Children
	s.pruneLocked(s.opts.Now().UTC())
	s.mu.Unlock()
	s.logf("loaded %d child-session record(s) from %s", len(decoded.Children), s.path)
}

// seedLegacy fills the store from a host migration when no document exists.
func (s *Store) seedLegacy() {
	if s.opts.Legacy == nil {
		return
	}
	records := s.opts.Legacy()
	if len(records) == 0 {
		return
	}
	for index := range records {
		if s.opts.Normalize != nil {
			s.opts.Normalize(&records[index])
		}
	}
	s.mu.Lock()
	s.items = records
	s.pruneLocked(s.opts.Now().UTC())
	s.persistLocked()
	s.mu.Unlock()
	s.logf("seeded %d child-session record(s) from the host migration", len(records))
}

// quarantine moves an unusable document out of the way.
func (s *Store) quarantine(reason string) {
	backup := fmt.Sprintf("%s.corrupt-%d", s.path, s.opts.Now().UTC().UnixNano())
	if err := os.Rename(s.path, backup); err != nil {
		reason += fmt.Sprintf(" (could not quarantine the file: %v)", err)
	} else {
		reason += " (quarantined as " + backup + ")"
	}
	s.logf("%s; starting with an empty child-session store", reason)
}

// NewID returns a fresh child id (prefix + 12 random hex chars).
func (s *Store) NewID() string {
	prefix := "child-"
	if s != nil {
		prefix = s.opts.IDPrefix
	}
	return prefix + randomHex(6)
}

// HasID reports whether id is already retained.
func (s *Store) HasID(id string) bool {
	_, ok := s.Record(id)
	return ok
}

// Upsert stores one record (insert or replace by ID), applies the host normalize
// hook, prunes and persists. It reports whether anything changed.
func (s *Store) Upsert(record Record) bool {
	if s == nil {
		return false
	}
	record.ID = strings.TrimSpace(record.ID)
	if record.ID == "" {
		return false
	}
	if s.opts.Normalize != nil {
		s.opts.Normalize(&record)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	now := s.opts.Now().UTC()
	if record.Updated.IsZero() {
		record.Updated = now
	}
	replaced := false
	changed := true
	for index := range s.items {
		if s.items[index].ID != record.ID {
			continue
		}
		// A later snapshot of the same finished child (a host rebuild) is often
		// produced with Notified=false; keep the already-consumed receipt flag so
		// a restart cannot replay it. A new run has a different Finished time and
		// starts unnotified again.
		previous := s.items[index]
		if previous.Notified && !record.Notified &&
			record.Status == previous.Status && record.Finished.Equal(previous.Finished) {
			record.Notified = true
		}
		changed = !recordsEqual(previous, record)
		s.items[index] = record
		replaced = true
		break
	}
	if !replaced {
		s.items = append(s.items, record)
	}
	if !changed {
		return false
	}
	s.pruneLocked(now)
	s.persistLocked()
	return true
}

// MarkNotified flags a finished child as receipt-delivered.
func (s *Store) MarkNotified(id string, finished time.Time) bool {
	if s == nil {
		return false
	}
	id = strings.TrimSpace(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	for index := range s.items {
		if s.items[index].ID != id || s.items[index].Notified {
			continue
		}
		// Refuse when the record was replaced by a newer run (different finish
		// time): a receipt staged for the old run must not silence the new one.
		if !finished.IsZero() && !s.items[index].Finished.IsZero() && !s.items[index].Finished.Equal(finished) {
			return false
		}
		s.items[index].Notified = true
		if !finished.IsZero() {
			s.items[index].Finished = finished
		}
		s.items[index].Updated = s.opts.Now().UTC()
		s.persistLocked()
		return true
	}
	return false
}

// Records returns the retained records, newest first.
func (s *Store) Records() []Record {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, len(s.items))
	copy(out, s.items)
	sort.SliceStable(out, func(i, j int) bool {
		return recordTime(out[i]).After(recordTime(out[j]))
	})
	return out
}

// Record returns one retained record by id.
func (s *Store) Record(id string) (Record, bool) {
	if s == nil {
		return Record{}, false
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return Record{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range s.items {
		if record.ID == id {
			return record, true
		}
	}
	return Record{}, false
}

// ByBackendRef resolves a live backend reference to its record for one parent
// session and kind ("" session matches any, exact session wins).
func (s *Store) ByBackendRef(backendRef, sessionID, kind string) (Record, bool) {
	if s == nil {
		return Record{}, false
	}
	backendRef = strings.TrimSpace(backendRef)
	if backendRef == "" {
		return Record{}, false
	}
	sessionID = strings.TrimSpace(sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	var fallback *Record
	for index := range s.items {
		record := s.items[index]
		if record.BackendRef != backendRef || record.Kind != kind {
			continue
		}
		if sessionID == "" || record.ParentSessionID == "" || record.ParentSessionID == sessionID {
			copied := record
			if record.ParentSessionID == sessionID {
				return copied, true
			}
			if fallback == nil {
				fallback = &copied
			}
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return Record{}, false
}

// PendingReceipts returns the finished, not-yet-notified records of one parent
// session and kind, oldest finish first. The host renders the receipts and
// commits them with MarkReceipts after the run succeeds.
func (s *Store) PendingReceipts(sessionID, kind string) []Record {
	if s == nil {
		return nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var pending []Record
	for _, record := range s.items {
		if record.ParentSessionID != sessionID || record.Finished.IsZero() || record.Notified || record.Kind != kind {
			continue
		}
		pending = append(pending, record)
	}
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].Finished.Before(pending[j].Finished) })
	return pending
}

// MarkReceipts marks the given records' receipts as delivered (only successful
// runs should call it). It is a no-op for records that were replaced.
func (s *Store) MarkReceipts(records []Record) {
	for _, record := range records {
		s.MarkNotified(record.ID, record.Finished)
	}
}

// Clear drops every record and removes the document (best effort).
func (s *Store) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.items = nil
	path := s.path
	s.mu.Unlock()
	if path != "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			s.logf("remove child-session document: %v", err)
		}
	}
}

// Prune applies the TTL and count bounds.
func (s *Store) Prune(now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pruneLocked(now) {
		s.persistLocked()
	}
}

// Close stops further writes. The last persisted document is authoritative.
func (s *Store) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// pruneLocked enforces the bounds; it reports whether anything was dropped.
func (s *Store) pruneLocked(now time.Time) bool {
	dropped := false
	if s.opts.TTL > 0 {
		cutoff := now.Add(-s.opts.TTL)
		kept := s.items[:0]
		for _, record := range s.items {
			if recordTime(record).Before(cutoff) {
				dropped = true
				continue
			}
			kept = append(kept, record)
		}
		s.items = kept
	}
	sort.SliceStable(s.items, func(i, j int) bool {
		return recordTime(s.items[i]).After(recordTime(s.items[j]))
	})
	if s.opts.Keep > 0 && len(s.items) > s.opts.Keep {
		s.items = s.items[:s.opts.Keep]
		dropped = true
	}
	return dropped
}

// persistLocked writes the document atomically.
func (s *Store) persistLocked() {
	if s.path == "" || s.closed {
		return
	}
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		s.logf("create child-session directory: %v", err)
		return
	}
	data, err := json.MarshalIndent(document{Version: s.opts.Version, Children: s.items}, "", "  ")
	if err != nil {
		s.logf("encode child-session document: %v", err)
		return
	}
	temporary, err := os.CreateTemp(directory, ".child-sessions-*.tmp")
	if err != nil {
		s.logf("create temporary child-session document: %v", err)
		return
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		s.logf("write temporary child-session document: %v", err)
		return
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		s.logf("sync temporary child-session document: %v", err)
		return
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		s.logf("close temporary child-session document: %v", err)
		return
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		_ = os.Remove(temporaryPath)
		s.logf("replace child-session document %q: %v", s.path, err)
		return
	}
	if handle, err := os.Open(directory); err == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
}

// recordTime is the timestamp used for ordering/pruning.
func recordTime(record Record) time.Time {
	for _, candidate := range []time.Time{record.Updated, record.Finished, record.Started} {
		if !candidate.IsZero() {
			return candidate
		}
	}
	return time.Time{}
}

// recordsEqual reports whether two records are semantically identical.
func recordsEqual(left, right Record) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

// randomHex returns n random bytes as hex.
func randomHex(n int) string {
	buffer := make([]byte, n)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}
