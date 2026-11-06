// Package report provides the report tool: it summarizes a session (or the
// workspace when no session log is configured) into a Markdown file written
// inside the agent's sandbox.
//
// The tool is read-only with respect to sessions and only writes the report
// file; every filesystem access goes through sandbox.Policy, so a path outside
// the configured roots or a read-only policy is refused with a readable model
// error. A host provides the session log; without one the report degrades to a
// workspace-only summary instead of failing.
package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolName is the model-facing tool name.
const ToolName = "report"

// Limits and file defaults. They are exported so hosts and tests can assert the
// documented behavior.
const (
	// DefaultLimit is the default number of session events scanned.
	DefaultLimit = 200
	// MaxLimit caps how many events one report scans.
	MaxLimit = 2000
	// FileMode is the mode of the written report file.
	FileMode = 0o644
	// timestampLayout is the default file name's timestamp format.
	timestampLayout = "20060102-150405"
	// recentEvents is how many recent events the report lists.
	recentEvents = 10
	// payloadExcerptBytes caps an event payload excerpt in the report.
	payloadExcerptBytes = 200
	// defaultTitle is used when the call passes no title.
	defaultTitle = "Rig report"
)

// Options configures the report tool.
type Options struct {
	// Log provides the session events summarized in the report. Nil reports
	// the workspace only.
	Log rig.SessionLog
	// Policy confines where the report file may be written. Required at call
	// time; nil reports a readable tool error.
	Policy *sandbox.Policy
	// Now overrides the clock (default filename and "generated" line); nil
	// means time.Now. Tests inject a deterministic clock.
	Now func() time.Time
}

// NewReportTool returns the report tool. Construction never fails; missing
// configuration is reported by Invoke.
func NewReportTool(opts Options) rig.Tool {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &reportTool{log: opts.Log, policy: opts.Policy, now: now}
}

// New is NewReportTool.
func New(opts Options) rig.Tool { return NewReportTool(opts) }

type reportTool struct {
	log    rig.SessionLog
	policy *sandbox.Policy
	now    func() time.Time
}

// Name implements rig.Tool.
func (t *reportTool) Name() string { return ToolName }

// Description implements rig.Tool.
func (t *reportTool) Description() string {
	return "Write a Markdown report of the session's work into the sandbox: event counts, tool-call and error totals, " +
		"event-type breakdown, recent events and optional extra content. Use it when the user asks for a written " +
		"summary or deliverable. The report is written inside the workspace root; a missing session log produces a " +
		"workspace-only report. Returns the file path, size and word count."
}

// Schema implements rig.Tool.
func (t *reportTool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"path":       toolutil.StringProp("Where to write the report, relative to the sandbox root (default rig-report-<timestamp>.md in the primary root)."),
		"title":      toolutil.StringProp("Report title (default \"Rig report\")."),
		"session_id": toolutil.StringProp("Session whose events are summarized; omitted = searchable events across sessions, no session log = workspace only."),
		"limit":      toolutil.IntegerProp("Maximum number of events to scan (default 200, maximum 2000)."),
		"content":    toolutil.StringProp("Optional extra Markdown appended as the report's Content section."),
	})
}

// Invoke implements rig.Tool.
func (t *reportTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args struct {
		Path      string `json:"path"`
		Title     string `json:"title"`
		SessionID string `json:"session_id"`
		Limit     int    `json:"limit"`
		Content   string `json:"content"`
	}
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	if t.policy == nil {
		return toolutil.ErrorResultf("report: no sandbox policy is configured; cannot write a report file"), nil
	}
	if t.policy.ReadOnly() {
		return toolutil.ErrorResultf("report: the sandbox is read-only; cannot write a report file"), nil
	}
	if len(t.policy.Roots()) == 0 {
		return toolutil.ErrorResultf("report: the sandbox allows no filesystem root; cannot write a report file"), nil
	}
	limit := clampLimit(args.Limit, DefaultLimit, MaxLimit)
	summary, err := t.summarize(ctx, strings.TrimSpace(args.SessionID), limit)
	if err != nil {
		return rig.ToolResult{}, err
	}

	now := t.now()
	title := strings.TrimSpace(args.Title)
	if title == "" {
		title = defaultTitle
	}
	body := renderReport(title, now, summary, args.Content)

	name := strings.TrimSpace(args.Path)
	if name == "" {
		name = fmt.Sprintf("rig-report-%s.md", now.Format(timestampLayout))
	}
	root, relative, absolute, _, openErr := t.policy.OpenPath(name)
	if openErr != nil {
		if errors.Is(openErr, sandbox.ErrDenied) {
			return toolutil.ErrorResultf("report: path %q refused by the sandbox: %v", name, openErr), nil
		}
		return toolutil.ErrorResultf("report: open %q: %v", name, openErr), nil
	}
	if dir := filepath.Dir(relative); dir != "." && dir != string(filepath.Separator) {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return toolutil.ErrorResultf("report: create directory %q: %v", dir, err), nil
		}
	}
	if err := root.WriteFile(relative, []byte(body), FileMode); err != nil {
		return toolutil.ErrorResultf("report: write %q: %v", name, err), nil
	}

	words := len(strings.Fields(body))
	result := fmt.Sprintf("wrote report %s\nscope: %s\nevents: %d, tool calls: %d, errors: %d\nbytes: %d, words: %d",
		absolute, summary.scope, summary.events, summary.toolCalls, summary.errors, len(body), words)
	if summary.note != "" {
		result += "\nnote: " + summary.note
	}
	return toolutil.TextResult(result), nil
}

// summary is the aggregated session/work state rendered into the report.
type summary struct {
	scope     string
	events    int
	toolCalls int
	errors    int
	types     []countEntry
	recent    []rig.SessionEvent
	note      string
}

func (t *reportTool) summarize(ctx context.Context, sessionID string, limit int) (summary, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return summary{}, ctxErr
	}
	if t.log == nil {
		return summary{
			scope: "workspace only",
			note:  "no session log is configured; the report summarizes the workspace only",
		}, nil
	}
	var (
		events []rig.SessionEvent
		err    error
		scope  string
	)
	if sessionID != "" {
		scope = "session " + sessionID
		events, err = t.log.Events(ctx, sessionID, 0, 0)
		if err != nil {
			return summary{}, reportReadError(ctx, err)
		}
	} else {
		scope = fmt.Sprintf("searchable events across sessions (limit %d; pass session_id for one session)", limit)
		events, err = t.log.Search(ctx, "", limit)
		if err != nil {
			return summary{}, reportReadError(ctx, err)
		}
	}
	note := ""
	if len(events) > limit {
		events = events[:limit]
		note = fmt.Sprintf("only the first %d events were scanned", limit)
	}
	counts := make(map[string]int, 8)
	toolCalls, errors := 0, 0
	for _, event := range events {
		eventType := display(event.Type)
		counts[eventType]++
		if isToolCall(eventType) {
			toolCalls++
		}
		if isErrorEvent(eventType) {
			errors++
		}
	}
	recent := events
	if len(recent) > recentEvents {
		recent = recent[len(recent)-recentEvents:]
	}
	return summary{
		scope:     scope,
		events:    len(events),
		toolCalls: toolCalls,
		errors:    errors,
		types:     sortedCounts(counts),
		recent:    recent,
		note:      note,
	}, nil
}

// renderReport builds the Markdown document.
func renderReport(title string, now time.Time, summary summary, content string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# %s\n\n", title)
	fmt.Fprintf(&builder, "Generated: %s  \n", now.Format(time.RFC3339))
	fmt.Fprintf(&builder, "Scope: %s  \n", summary.scope)
	fmt.Fprintf(&builder, "Events: %d  \n", summary.events)
	fmt.Fprintf(&builder, "Tool calls: %d  \n", summary.toolCalls)
	fmt.Fprintf(&builder, "Errors: %d\n", summary.errors)
	if summary.note != "" {
		fmt.Fprintf(&builder, "\n_%s_\n", summary.note)
	}
	if len(summary.types) > 0 {
		builder.WriteString("\n## Event types\n\n")
		for _, entry := range summary.types {
			fmt.Fprintf(&builder, "- %s: %d\n", entry.name, entry.count)
		}
	}
	if len(summary.recent) > 0 {
		builder.WriteString("\n## Recent events\n\n")
		for _, event := range summary.recent {
			line := fmt.Sprintf("- #%d %s %s", event.Seq, display(event.Type), formatTime(event.Time))
			if excerpt := payloadExcerpt(event.Payload); excerpt != "" {
				line += " `" + excerpt + "`"
			}
			builder.WriteString(line + "\n")
		}
	}
	if strings.TrimSpace(content) != "" {
		builder.WriteString("\n## Content\n\n")
		builder.WriteString(strings.TrimSpace(content))
		builder.WriteString("\n")
	}
	builder.WriteString("\n---\nGenerated by the Rig `report` tool.\n")
	return builder.String()
}

// reportReadError converts a session-log failure into a tool result (or an
// error when the caller's context ended).
func reportReadError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return fmt.Errorf("report: read session events: %w", err)
}

// --- shared helpers ---------------------------------------------------------

func clampLimit(value, fallback, maximum int) int {
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
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].name < entries[j].name
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
