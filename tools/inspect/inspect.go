// Package inspect provides the read-only inspect tool: a compact, structured
// snapshot of what the agent can see — its session history, event statistics and
// the workspace root.
//
// The tool depends only on contracts/rig.SessionLog (plus an optional session
// enumeration extension) and the filesystem: it never mutates sessions, events
// or workspace files. When the host provides no SessionLog, inspect degrades to
// a workspace-only report instead of failing.
package inspect

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolName is the model-facing tool name.
const ToolName = "inspect"

// Actions supported by the inspect tool.
const (
	ActionSessions  = "sessions"
	ActionEvents    = "events"
	ActionWorkspace = "workspace"
	ActionStats     = "stats"
)

// Limits and caps. They are exported so hosts and tests can assert the
// documented behavior.
const (
	// DefaultLimit is the default event/session limit for session actions.
	DefaultLimit = 20
	// MaxLimit caps the event/session limit.
	MaxLimit = 200
	// DefaultWalkLimit is how many files action=workspace counts by default.
	DefaultWalkLimit = 1000
	// MaxWalkLimit caps the workspace file walk.
	MaxWalkLimit = 10000
	// topLevelEntries is how many workspace top-level entries are listed.
	topLevelEntries = 12
	// maxOutputBytes caps one inspect response (32 KiB).
	maxOutputBytes = 32 << 10
	// payloadExcerptBytes caps an event payload excerpt.
	payloadExcerptBytes = 200
)

// SessionLister is an optional extension a contracts/rig.SessionLog
// implementation may also implement: it enumerates the session ids the log
// knows about. The frozen rig.SessionLog has no enumeration method, so inspect
// falls back to inferring sessions from Search results when the extension is
// absent.
type SessionLister interface {
	Sessions(ctx context.Context) ([]string, error)
}

// Options configures the inspect tool.
type Options struct {
	// Root is the workspace root reported by action=workspace and file
	// counting. Optional: an empty root reports "(not configured)".
	Root string
	// Log reads session events for the sessions/events/stats actions. Nil
	// degrades those actions to a workspace-only inspection.
	Log rig.SessionLog
}

// NewInspectTool returns the inspect tool. Construction never fails; missing
// configuration is reported by Invoke.
func NewInspectTool(opts Options) rig.Tool {
	return &inspectTool{root: normalizeRoot(opts.Root), log: opts.Log}
}

// New is NewInspectTool.
func New(opts Options) rig.Tool { return NewInspectTool(opts) }

type inspectTool struct {
	root string
	log  rig.SessionLog
}

// Name implements rig.Tool.
func (t *inspectTool) Name() string { return ToolName }

// Description implements rig.Tool.
func (t *inspectTool) Description() string {
	return "Inspect this agent's environment (read-only): action=sessions lists known sessions, action=events shows " +
		"one session's recent events (session_id required), action=workspace reports the workspace root and file count, " +
		"action=stats summarizes event types, tool calls and errors (optionally scoped to one session). " +
		"Without a host session log the session actions degrade to a workspace-only snapshot. Use it to orient " +
		"yourself before searching history with the session_* tools."
}

// Schema implements rig.Tool.
func (t *inspectTool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"action": map[string]any{
			"type":        "string",
			"enum":        []any{ActionSessions, ActionEvents, ActionWorkspace, ActionStats},
			"description": "Section to inspect.",
		},
		"session_id": toolutil.StringProp("Session id for action=events (required) or to scope action=stats."),
		"limit": toolutil.IntegerProp("Event/session limit for session actions (default 20, max 200); " +
			"for action=workspace, the maximum number of files to count (default 1000, max 10000)."),
	}, "action")
}

// Invoke implements rig.Tool.
func (t *inspectTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args struct {
		Action    string `json:"action"`
		SessionID string `json:"session_id"`
		Limit     int    `json:"limit"`
	}
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	sessionID := strings.TrimSpace(args.SessionID)
	switch action {
	case ActionSessions:
		return toolutil.TextResult(toolutil.TruncateBytes(strings.Join(t.sessions(ctx, args.Limit), "\n"), maxOutputBytes, "\n[truncated: output exceeded 32 KiB]")), nil
	case ActionEvents:
		if sessionID == "" {
			return toolutil.ErrorResultf("inspect: session_id is required for action=events"), nil
		}
		return toolutil.TextResult(toolutil.TruncateBytes(strings.Join(t.events(ctx, sessionID, args.Limit), "\n"), maxOutputBytes, "\n[truncated: output exceeded 32 KiB]")), nil
	case ActionWorkspace:
		return toolutil.TextResult(toolutil.TruncateBytes(strings.Join(t.workspace(args.Limit), "\n"), maxOutputBytes, "\n[truncated: output exceeded 32 KiB]")), nil
	case ActionStats:
		return toolutil.TextResult(toolutil.TruncateBytes(strings.Join(t.stats(ctx, sessionID, args.Limit), "\n"), maxOutputBytes, "\n[truncated: output exceeded 32 KiB]")), nil
	case "":
		return toolutil.ErrorResultf("inspect: action is required (sessions, events, workspace or stats)"), nil
	default:
		return toolutil.ErrorResultf("inspect: unknown action %q (want sessions, events, workspace or stats)", action), nil
	}
}

// sessions reports the known sessions. It uses the optional SessionLister
// extension when available and otherwise infers sessions from Search results.
func (t *inspectTool) sessions(ctx context.Context, limit int) []string {
	lines := []string{"inspect: sessions"}
	if t.log == nil {
		lines = append(lines, "session log: not configured (workspace-only inspection)")
		return append(lines, t.workspace(limit)...)
	}
	limit = clamp(limit, DefaultLimit, MaxLimit)

	if lister, ok := t.log.(SessionLister); ok {
		ids, err := lister.Sessions(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return []string{"inspect: sessions", "error: " + ctx.Err().Error()}
			}
			lines = append(lines, fmt.Sprintf("error: list sessions: %v", err))
			return append(lines, t.workspace(limit)...)
		}
		slices.Sort(ids)
		displayed := ids
		truncated := false
		if len(displayed) > limit {
			displayed = displayed[:limit]
			truncated = true
		}
		lines = append(lines, "session log: configured")
		lines = append(lines, fmt.Sprintf("sessions: %d", len(ids)))
		for _, id := range displayed {
			lines = append(lines, "  "+t.sessionLine(ctx, id))
		}
		if truncated {
			lines = append(lines, fmt.Sprintf("[truncated: showing %d of %d sessions]", len(displayed), len(ids)))
		}
		return lines
	}

	// Fallback: group searchable events by session.
	events, err := t.log.Search(ctx, "", limit)
	if err != nil {
		if ctx.Err() != nil {
			return []string{"inspect: sessions", "error: " + ctx.Err().Error()}
		}
		lines = append(lines, fmt.Sprintf("error: search events: %v", err))
		return append(lines, t.workspace(limit)...)
	}
	latest := make(map[string]rig.SessionEvent)
	order := make([]string, 0)
	for _, event := range events {
		id := strings.TrimSpace(event.SessionID)
		if id == "" {
			continue
		}
		current, ok := latest[id]
		if !ok {
			order = append(order, id)
		}
		if !ok || event.Seq > current.Seq {
			latest[id] = event
		}
	}
	slices.Sort(order)
	lines = append(lines, "session log: configured (no session enumeration; sessions inferred from searchable events)")
	lines = append(lines, fmt.Sprintf("sessions: %d", len(order)))
	for _, id := range order {
		event := latest[id]
		lines = append(lines, fmt.Sprintf("  %s  last=#%d %s %s", id, event.Seq, display(event.Type), formatTime(event.Time)))
	}
	return lines
}

// sessionLine summarizes one session for action=sessions.
func (t *inspectTool) sessionLine(ctx context.Context, sessionID string) string {
	last, err := t.log.Last(ctx, sessionID, 1)
	if err != nil || len(last) == 0 {
		return sessionID
	}
	event := last[0]
	return fmt.Sprintf("%s  last=#%d %s %s", sessionID, event.Seq, display(event.Type), formatTime(event.Time))
}

// events reports one session's recent events.
func (t *inspectTool) events(ctx context.Context, sessionID string, limit int) []string {
	lines := []string{"inspect: events", "session: " + sessionID}
	if t.log == nil {
		lines = append(lines, "session log: not configured (workspace-only inspection)")
		return append(lines, t.workspace(limit)...)
	}
	limit = clamp(limit, DefaultLimit, MaxLimit)
	events, err := t.log.Last(ctx, sessionID, limit)
	if err != nil {
		if ctx.Err() != nil {
			return []string{"inspect: events", "error: " + ctx.Err().Error()}
		}
		lines = append(lines, fmt.Sprintf("error: read events: %v", err))
		return lines
	}
	if len(events) == 0 {
		lines = append(lines, fmt.Sprintf("events: 0 (no events for session %q)", sessionID))
		return lines
	}
	lines = append(lines, fmt.Sprintf("events: %d (limit %d)", len(events), limit))
	for _, event := range events {
		line := fmt.Sprintf("  #%d %s %s", event.Seq, display(event.Type), formatTime(event.Time))
		if excerpt := payloadExcerpt(event.Payload); excerpt != "" {
			line += " " + excerpt
		}
		lines = append(lines, line)
	}
	return lines
}

// stats summarizes event types, tool calls and errors.
func (t *inspectTool) stats(ctx context.Context, sessionID string, limit int) []string {
	lines := []string{"inspect: stats"}
	if t.log == nil {
		lines = append(lines, "session log: not configured (workspace-only inspection)")
		return append(lines, t.workspace(limit)...)
	}
	limit = clamp(limit, DefaultLimit, MaxLimit)

	var (
		events   []rig.SessionEvent
		scope    string
		err      error
		capped   bool
		scanNote string
	)
	if sessionID != "" {
		scope = "session " + sessionID
		events, err = t.log.Events(ctx, sessionID, 0, 0)
		if err == nil && len(events) > limit {
			events = events[:limit]
			capped = true
		}
	} else {
		scope = "searchable events (pass session_id for exact per-session counts)"
		events, err = t.log.Search(ctx, "", limit)
		scanNote = "search limit applied"
	}
	if err != nil {
		if ctx.Err() != nil {
			return []string{"inspect: stats", "error: " + ctx.Err().Error()}
		}
		lines = append(lines, fmt.Sprintf("error: read events: %v", err))
		return lines
	}

	counts := make(map[string]int, 8)
	toolCalls, errors := 0, 0
	firstSeq, lastSeq := int64(0), int64(0)
	for _, event := range events {
		eventType := display(event.Type)
		counts[eventType]++
		if isToolCall(eventType) {
			toolCalls++
		}
		if isErrorEvent(eventType) {
			errors++
		}
		if firstSeq == 0 || event.Seq < firstSeq {
			firstSeq = event.Seq
		}
		if event.Seq > lastSeq {
			lastSeq = event.Seq
		}
	}
	lines = append(lines, "scope: "+scope)
	lines = append(lines, fmt.Sprintf("events: %d (limit %d)", len(events), limit))
	if capped {
		lines = append(lines, fmt.Sprintf("[truncated: counted the first %d events only]", limit))
	} else if scanNote != "" && len(events) >= limit {
		lines = append(lines, fmt.Sprintf("[truncated: %s; counters cover at most %d events]", scanNote, limit))
	}
	if len(events) > 0 {
		lines = append(lines, fmt.Sprintf("sequence range: #%d..#%d", firstSeq, lastSeq))
	}
	lines = append(lines, fmt.Sprintf("tool calls: %d", toolCalls))
	lines = append(lines, fmt.Sprintf("errors: %d", errors))
	if len(counts) > 0 {
		lines = append(lines, "event types:")
		for _, entry := range sortedCounts(counts) {
			lines = append(lines, fmt.Sprintf("  %s: %d", entry.name, entry.count))
		}
	}
	recent := events
	if len(recent) > 10 {
		recent = recent[len(recent)-10:]
	}
	if len(recent) > 0 {
		lines = append(lines, "recent events:")
		for _, event := range recent {
			lines = append(lines, fmt.Sprintf("  #%d %s %s", event.Seq, display(event.Type), formatTime(event.Time)))
		}
	}
	return lines
}

// workspace reports the workspace root and a bounded file/dir count.
func (t *inspectTool) workspace(limit int) []string {
	lines := []string{"inspect: workspace"}
	if t.root == "" {
		lines = append(lines, "root: (not configured)")
		return lines
	}
	limit = clamp(limit, DefaultWalkLimit, MaxWalkLimit)
	lines = append(lines, "root: "+t.root)

	files, dirs := 0, 0
	truncated := false
	walkErr := filepath.WalkDir(t.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if path == t.root {
			return nil
		}
		if entry.IsDir() {
			dirs++
			return nil
		}
		files++
		if files >= limit {
			truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		lines = append(lines, fmt.Sprintf("walk error: %v", walkErr))
	}
	lines = append(lines, fmt.Sprintf("files: %d", files))
	lines = append(lines, fmt.Sprintf("directories: %d", dirs))
	if truncated {
		lines = append(lines, fmt.Sprintf("[truncated: counted the first %d files]", limit))
	}

	entries, err := os.ReadDir(t.root)
	if err != nil {
		return lines
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		names = append(names, name)
		if len(names) >= topLevelEntries {
			break
		}
	}
	if len(names) > 0 {
		lines = append(lines, "top-level: "+strings.Join(names, " "))
	}
	return lines
}

// --- shared helpers ---------------------------------------------------------

func normalizeRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	if abs, err := filepath.Abs(root); err == nil {
		return abs
	}
	return root
}

func clamp(value, fallback, maximum int) int {
	if value <= 0 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}

type countEntry struct {
	name  string
	count int
}

func sortedCounts(counts map[string]int) []countEntry {
	entries := make([]countEntry, 0, len(counts))
	for name, count := range counts {
		entries = append(entries, countEntry{name: name, count: count})
	}
	slices.SortFunc(entries, func(a, b countEntry) int {
		return cmp.Or(cmp.Compare(b.count, a.count), strings.Compare(a.name, b.name))
	})
	return entries
}

func isToolCall(eventType string) bool {
	lower := strings.ToLower(eventType)
	return strings.Contains(lower, "tool") && strings.Contains(lower, "call")
}

func isErrorEvent(eventType string) bool {
	return strings.Contains(strings.ToLower(eventType), "error")
}

func display(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return strings.TrimSpace(value)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Format(time.RFC3339)
}

func payloadExcerpt(payload json.RawMessage) string {
	compact := strings.Join(strings.Fields(string(payload)), " ")
	if compact == "" || compact == "null" {
		return ""
	}
	if len(compact) > payloadExcerptBytes {
		cut := payloadExcerptBytes
		for cut > 0 && !utf8.RuneStart(compact[cut]) {
			cut--
		}
		compact = compact[:cut] + "…"
	}
	return compact
}
