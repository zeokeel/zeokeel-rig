// Package memstore implements the host-agnostic JSON-file memory store: entry
// scopes and workspace visibility, deterministic retention consolidation,
// search, optimistic in-memory rollback and a mutation version counter.

package memstore

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/memoryfile"
)

const moduleName = "memory"

// Scope constants.
const (
	ScopeGlobal          = "global"
	ScopeWorkspace       = "workspace"
	ScopeWorkspacePrefix = "workspace:"
)

// ScopeForWorkspace binds a scope to one workspace.
func ScopeForWorkspace(id string) string { return ScopeWorkspacePrefix + id }

// ScopeWorkspaceID extracts the workspace identity of a bound scope.
func ScopeWorkspaceID(scope string) (string, bool) {
	scope = strings.TrimSpace(scope)
	if !strings.HasPrefix(strings.ToLower(scope), ScopeWorkspacePrefix) {
		return "", false
	}
	id := strings.TrimSpace(scope[len(ScopeWorkspacePrefix):])
	if id == "" {
		return "", false
	}
	return id, true
}

// NormalizeConfiguredScope validates the configured default scope.
func NormalizeConfiguredScope(scope string) (string, error) {
	switch normalized := strings.ToLower(strings.TrimSpace(scope)); normalized {
	case "":
		return ScopeGlobal, nil
	case ScopeGlobal, ScopeWorkspace:
		return normalized, nil
	default:
		return "", fmt.Errorf("memory: invalid scope %q (want %q or %q)", scope, ScopeGlobal, ScopeWorkspace)
	}
}

// normalizeScope canonicalizes one entry scope against the current workspace.
func (s *Store) normalizeScope(ctx context.Context, scope string) (string, error) {
	raw := strings.TrimSpace(scope)
	if raw == "" {
		raw = s.opts.DefaultScope
	}
	switch strings.ToLower(raw) {
	case ScopeGlobal:
		return ScopeGlobal, nil
	case ScopeWorkspace:
		if workspaceID := s.currentWorkspaceID(ctx); workspaceID != "" {
			return ScopeForWorkspace(workspaceID), nil
		}
		return ScopeWorkspace, nil
	}
	if workspaceID, ok := ScopeWorkspaceID(raw); ok {
		return ScopeForWorkspace(workspaceID), nil
	}
	return "", fmt.Errorf("memory: invalid scope %q (want %q, %q or \"workspace:<id>\")", scope, ScopeGlobal, ScopeWorkspace)
}

// Policy is the deterministic retention policy.
type Policy struct {
	MaxEntries int
	MaxAge     time.Duration
}

// Report summarizes one consolidation.
type Report struct {
	RemovedExpired int
	RemovedOverCap int
	Remaining      int
}

// Options configures a store.
type Options struct {
	// Path returns the current document path (a function so hosts/tests can
	// relocate the store).
	Path func() string
	// WorkspaceID returns the workspace current at call time ("" when none).
	WorkspaceID func(ctx context.Context) string
	// DefaultScope is the configured default entry scope.
	DefaultScope string
	// Policy returns the configured retention policy.
	Policy func() Policy
	// Logger receives warnings (nil discards them).
	Logger rig.Logger
}

// discardLogger drops memory log lines.
type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

// Store is a JSON-file backed rig.MemoryStore. All methods are safe for
// concurrent use; each mutation persists the whole (small) document atomically.
type Store struct {
	opts Options

	mu       sync.Mutex
	entries  []rig.Memory
	recovery string
	version  uint64
}

var (
	_ rig.MemoryStore = (*Store)(nil)
)

// Open loads an existing document (missing file = empty) and applies the
// configured retention policy once. A corrupt/unreadable file is quarantined.
func Open(opts Options) (*Store, error) {
	if opts.Path == nil {
		return nil, fmt.Errorf("memory: path resolver is required")
	}
	if opts.Logger == nil {
		opts.Logger = discardLogger{}
	}
	if opts.WorkspaceID == nil {
		opts.WorkspaceID = func(context.Context) string { return "" }
	}
	if opts.Policy == nil {
		opts.Policy = func() Policy { return Policy{} }
	}
	store := &Store{opts: opts}
	store.load()
	store.enforcePolicy(context.Background())
	return store, nil
}

// Path returns the current file path.
func (s *Store) Path() string { return s.opts.Path() }

// load reads the document; a corrupt file is quarantined and reported.
func (s *Store) load() {
	entries, recovery := memoryfile.Load(s.opts.Path())
	if recovery != "" {
		s.noteRecovery(recovery + "; starting with an empty memory store")
	}
	s.entries = entries
}

// RecoveryNotice returns the last recovery warning ("" when the store loaded
// healthy). It implements rig.MemoryRecoveryReporter so the memory page
// can tell the user their previous entries were backed up.
func (s *Store) RecoveryNotice() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recovery
}

// noteRecovery records and logs one recovery warning.
func (s *Store) noteRecovery(message string) {
	s.recovery = message
	s.opts.Logger.Printf("memory: %s", message)
}

// Version returns the successful-mutation counter (rig.MemoryVersioner):
// the UI compares it per frame to notice entries written out of band.
func (s *Store) Version() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// currentWorkspaceID returns the current workspace id ("" when none).
func (s *Store) currentWorkspaceID(ctx context.Context) string {
	if ctx == nil {
		ctx = context.Background()
	}
	return strings.TrimSpace(s.opts.WorkspaceID(ctx))
}

// visibleInWorkspace reports whether an entry belongs in the current
// workspace's view. Global entries and legacy/unbound scopes are always visible
// so upgrading never hides existing data; a bound workspace entry is only
// visible in its own workspace.
func visibleInWorkspace(scope, currentWorkspaceID string) bool {
	scope = strings.TrimSpace(scope)
	if workspaceID, ok := ScopeWorkspaceID(scope); ok {
		return workspaceID == strings.TrimSpace(currentWorkspaceID)
	}
	// Global entries, the legacy bare "workspace" scope and historical custom
	// scopes cannot be attributed to one workspace, so they stay visible.
	return true
}

// List implements rig.MemoryStore: the current workspace's view in
// insertion order (global + legacy entries + this workspace's entries).
func (s *Store) List(ctx context.Context) ([]rig.Memory, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	currentWorkspaceID := s.currentWorkspaceID(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	visible := make([]rig.Memory, 0, len(s.entries))
	for _, memory := range s.entries {
		if visibleInWorkspace(memory.Scope, currentWorkspaceID) {
			visible = append(visible, memory)
		}
	}
	return visible, nil
}

// ListAll implements rig.MemoryAllLister: every entry of every
// workspace, for the settings management page.
func (s *Store) ListAll(ctx context.Context) ([]rig.Memory, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneEntries(s.entries), nil
}

// Add implements rig.MemoryStore. A missing ID becomes "mem-<unixnano>",
// a missing scope the configured default (a "workspace" default is bound to the
// current workspace), a missing Created now (UTC). An unknown scope is rejected
// with the same rule Update uses (M-04). A persistence failure rolls the
// in-memory list back; the automatic policy runs after a successful add.
func (s *Store) Add(ctx context.Context, memory rig.Memory) (rig.Memory, error) {
	if err := contextErr(ctx); err != nil {
		return rig.Memory{}, err
	}
	scope, err := s.normalizeScope(ctx, memory.Scope)
	if err != nil {
		return rig.Memory{}, err
	}
	s.mu.Lock()
	if id := strings.TrimSpace(memory.ID); id == "" {
		memory.ID = s.newIDLocked()
	} else {
		memory.ID = id
	}
	memory.Scope = scope
	if memory.Created.IsZero() {
		memory.Created = time.Now().UTC()
	}

	previous := s.entries
	s.entries = append(previous, memory)
	if err := s.persistLocked(); err != nil {
		s.entries = previous
		s.mu.Unlock()
		return rig.Memory{}, err
	}
	s.version++
	s.mu.Unlock()
	// Enforce the configured cap/retention on write as well as on open. The
	// new entry is the newest so a cap keeps it; only expired legacy entries
	// can be removed. Policy failures are logged, never fail the add.
	s.enforcePolicy(ctx)
	return memory, nil
}

// newIDLocked generates a unique "mem-<unixnano>" ID.
func (s *Store) newIDLocked() string {
	base := time.Now().UnixNano()
	for offset := int64(0); ; offset++ {
		id := fmt.Sprintf("mem-%d", base+offset)
		if !s.hasIDLocked(id) {
			return id
		}
		if offset > 1024 {
			return fmt.Sprintf("mem-%d-%d", base, len(s.entries))
		}
	}
}

// hasIDLocked reports whether an entry already uses id.
func (s *Store) hasIDLocked(id string) bool {
	for _, memory := range s.entries {
		if memory.ID == id {
			return true
		}
	}
	return false
}

// Update implements rig.MemoryUpdater: it replaces one entry's text while
// keeping its ID, creation time and (unless a non-empty scope is passed) its
// scope. Unknown IDs, blank text and invalid scopes are reported without
// touching the document; a persistence failure rolls the in-memory entry back.
func (s *Store) Update(ctx context.Context, id, text, scope string) (rig.Memory, error) {
	if err := contextErr(ctx); err != nil {
		return rig.Memory{}, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return rig.Memory{}, fmt.Errorf("%s: update: id is required", moduleName)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return rig.Memory{}, fmt.Errorf("%s: update: text is required", moduleName)
	}
	// An omitted scope keeps the entry's current scope; a provided one is
	// normalized exactly like a stored scope.
	normalizedScope := ""
	if strings.TrimSpace(scope) != "" {
		normalized, err := s.normalizeScope(ctx, scope)
		if err != nil {
			return rig.Memory{}, err
		}
		normalizedScope = normalized
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for position, memory := range s.entries {
		if memory.ID == id {
			index = position
			break
		}
	}
	if index < 0 {
		return rig.Memory{}, fmt.Errorf("%s: update: no memory with id %q", moduleName, id)
	}
	updated := s.entries[index]
	updated.Text = text
	if normalizedScope != "" {
		updated.Scope = normalizedScope
	}
	previous := cloneEntries(s.entries)
	s.entries[index] = updated
	if err := s.persistLocked(); err != nil {
		s.entries = previous
		return rig.Memory{}, err
	}
	s.version++
	return updated, nil
}

// Remove implements rig.MemoryStore. It reports whether the entry existed
// and keeps the document unchanged when persistence fails.
func (s *Store) Remove(_ context.Context, id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for position, memory := range s.entries {
		if memory.ID == id {
			index = position
			break
		}
	}
	if index < 0 {
		return false
	}
	previous := cloneEntries(s.entries)
	s.entries = append(append([]rig.Memory(nil), s.entries[:index]...), s.entries[index+1:]...)
	if err := s.persistLocked(); err != nil {
		s.entries = previous
		return false
	}
	s.version++
	return true
}

// Consolidate implements rig.MemoryConsolidator with the current clock.
// It always operates on every entry of every workspace (management semantics).
func (s *Store) Consolidate(ctx context.Context, policy Policy) (Report, error) {
	return s.ConsolidateAt(ctx, policy, time.Now().UTC())
}

// ConsolidateAt runs one deterministic consolidation at now.
func (s *Store) ConsolidateAt(ctx context.Context, policy Policy, now time.Time) (Report, error) {
	if err := contextErr(ctx); err != nil {
		return Report{}, err
	}
	return s.consolidate(policy, now)
}

// consolidate runs one deterministic consolidation at now: first the expiry
// window (entries created strictly before now-MaxAge), then the entry cap
// (removing the oldest entries until MaxEntries remain). It persists once (or
// not at all) and returns the counts; a persistence failure leaves the store
// untouched and is reported.
func (s *Store) consolidate(policy Policy, now time.Time) (Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept, report := memoryfile.Consolidate(s.entries, memoryfile.Policy{
		MaxEntries: policy.MaxEntries,
		MaxAge:     policy.MaxAge,
	}, now)
	if report.RemovedExpired == 0 && report.RemovedOverCap == 0 {
		return Report{Remaining: report.Remaining}, nil
	}
	previous := cloneEntries(s.entries)
	s.entries = kept
	if err := s.persistLocked(); err != nil {
		s.entries = previous
		return Report{Remaining: len(previous)},
			fmt.Errorf("%s: consolidate: persist: %w", moduleName, err)
	}
	s.version++
	return Report{
		RemovedExpired: report.RemovedExpired,
		RemovedOverCap: report.RemovedOverCap,
		Remaining:      report.Remaining,
	}, nil
}

// enforcePolicy applies the configured automatic policy and logs failures.
// It is a no-op when no policy is configured.
func (s *Store) enforcePolicy(ctx context.Context) {
	present := s.opts.Policy().MaxAge > 0 || s.opts.Policy().MaxEntries > 0
	if !present {
		return
	}
	report, err := s.Consolidate(ctx, s.opts.Policy())
	if err != nil {
		s.opts.Logger.Printf("memory: automatic memory consolidation: %v", err)
		return
	}
	if removed := report.RemovedExpired + report.RemovedOverCap; removed > 0 {
		s.opts.Logger.Printf("memory: automatic memory consolidation removed %d entr(ies) (expired %d, over cap %d, remaining %d)",
			removed, report.RemovedExpired, report.RemovedOverCap, report.Remaining)
	}
}

// Search implements rig.MemoryStore: a case-insensitive substring match
// over the text of the current workspace's view; an empty query returns every
// visible entry.
func (s *Store) Search(ctx context.Context, query string) ([]rig.Memory, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	query = strings.ToLower(strings.TrimSpace(query))
	currentWorkspaceID := s.currentWorkspaceID(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	matches := make([]rig.Memory, 0, len(s.entries))
	for _, memory := range s.entries {
		if !visibleInWorkspace(memory.Scope, currentWorkspaceID) {
			continue
		}
		if query == "" || strings.Contains(strings.ToLower(memory.Text), query) {
			matches = append(matches, memory)
		}
	}
	return matches, nil
}

// persistLocked writes the document atomically: a temporary file in the same
// directory is written, synced, closed and renamed over the target, then the
// directory is synced so the rename itself survives a power loss.
func (s *Store) persistLocked() error {
	return memoryfile.Save(s.opts.Path(), s.entries)
}

// cloneEntries returns a shallow copy of the entry slice (entries contain no
// references, so a slice copy is a deep copy for callers).
func cloneEntries(entries []rig.Memory) []rig.Memory {
	return append([]rig.Memory(nil), entries...)
}

// contextErr returns ctx.Err(), tolerating a nil context.
func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
