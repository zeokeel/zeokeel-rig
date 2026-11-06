package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// Tool names. They are stable model-facing identifiers.
const (
	ToolSearch      = "session_search"
	ToolTrace       = "session_trace"
	ToolEventRead   = "session_event_read"
	ToolEventSearch = "session_event_search"
	ToolEventTrace  = "session_event_trace"
)

// Limits and output caps. They are exported so hosts and tests can assert the
// documented behavior.
const (
	// DefaultLimit bounds one search when the call passes no limit.
	DefaultLimit = 20
	// MaxLimit caps a per-call search limit.
	MaxLimit = 100
	// DefaultContextEvents is how many events before/after event_trace shows.
	DefaultContextEvents = 2
	// MaxContextEvents caps before/after.
	MaxContextEvents = 10
	// MaxTraceEvents caps how many events session_trace prints.
	MaxTraceEvents = 100
	// maxOutputBytes caps one tool response (32 KiB).
	maxOutputBytes = 32 << 10
	// eventPayloadBytes caps the payload printed by session_event_read.
	eventPayloadBytes = 4 << 10
	// payloadExcerptBytes caps a payload excerpt in search summaries.
	payloadExcerptBytes = 240
)

// Tools returns the five read-only session-history tools backed by log.
//
// A nil log is allowed at construction: every tool then returns a readable
// "no session log is configured" tool error, which keeps a tool catalog stable
// for agents whose host does not provide session storage.
func Tools(log rig.SessionLog) []rig.Tool {
	return []rig.Tool{
		searchTool(log),
		traceTool(log),
		eventReadTool(log),
		eventSearchTool(log),
		eventTraceTool(log),
	}
}

// sessionTool is the common rig.Tool implementation of the session tool set.
type sessionTool struct {
	log         rig.SessionLog
	name        string
	description string
	parameters  map[string]any
	run         func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error)
}

func (t *sessionTool) Name() string           { return t.name }
func (t *sessionTool) Description() string    { return t.description }
func (t *sessionTool) Schema() map[string]any { return t.parameters }
func (t *sessionTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	return t.run(ctx, call)
}

// --- session_search ---------------------------------------------------------

func searchTool(log rig.SessionLog) rig.Tool {
	return &sessionTool{
		log:  log,
		name: ToolSearch,
		description: "Search the session history for events matching a literal, case-insensitive query " +
			"(matched against session ids, event types and payloads). Returns matching events with their session, " +
			"sequence number, type, time and a payload excerpt. Use session_event_read for a full event and " +
			"session_trace for one session's ordered trace.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"query": toolutil.StringProp("Literal text to search for (required; case-insensitive)."),
			"limit": toolutil.IntegerProp("Maximum number of matching events (default 20, maximum 100)."),
		}, "query"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args struct {
				Query string `json:"query"`
				Limit int    `json:"limit"`
			}
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			tool, failure := requireLog(log, ToolSearch)
			if failure != nil {
				return *failure, nil
			}
			query := strings.TrimSpace(args.Query)
			if query == "" {
				return toolutil.ErrorResultf("%s: query is required and must contain non-whitespace text", ToolSearch), nil
			}
			limit := clampLimit(args.Limit, DefaultLimit, MaxLimit)
			events, err := tool.log.Search(ctx, query, limit)
			if err != nil {
				return logFailure(ctx, ToolSearch, err), nil
			}
			if len(events) == 0 {
				return toolutil.TextResult(fmt.Sprintf("no session events match %q", query)), nil
			}
			lines := make([]string, 0, len(events)+1)
			lines = append(lines, fmt.Sprintf("%d matching event(s) for %q (limit %d):", len(events), query, limit))
			for _, event := range events {
				lines = append(lines, summaryLine(event, true, payloadExcerptBytes))
			}
			return toolutil.TextResult(toolutil.TruncateBytes(strings.Join(lines, "\n"), maxOutputBytes, "\n[truncated: output exceeded 32 KiB]")), nil
		},
	}
}

// --- session_trace ----------------------------------------------------------

func traceTool(log rig.SessionLog) rig.Tool {
	return &sessionTool{
		log:  log,
		name: ToolTrace,
		description: "Summarize one session's ordered event trace by sequence number: seq, type and time per event. " +
			"Use from/to (sequence numbers, inclusive) to page through a long trace; to=0 means until the end.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"session_id": toolutil.StringProp("Session id whose trace is read (team/agent session id)."),
			"from":       toolutil.IntegerProp("First sequence number to include (0 = beginning)."),
			"to":         toolutil.IntegerProp("Last sequence number to include (0 = until the end)."),
		}, "session_id"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args struct {
				SessionID string `json:"session_id"`
				From      int64  `json:"from"`
				To        int64  `json:"to"`
			}
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			tool, failure := requireLog(log, ToolTrace)
			if failure != nil {
				return *failure, nil
			}
			sessionID := strings.TrimSpace(args.SessionID)
			if sessionID == "" {
				return toolutil.ErrorResultf("%s: session_id is required", ToolTrace), nil
			}
			events, err := tool.log.Events(ctx, sessionID, args.From, args.To)
			if err != nil {
				return logFailure(ctx, ToolTrace, err), nil
			}
			if len(events) == 0 {
				return toolutil.TextResult(fmt.Sprintf("session %q has no events in the requested sequence range", sessionID)), nil
			}
			display := events
			truncated := false
			if len(display) > MaxTraceEvents {
				display = display[:MaxTraceEvents]
				truncated = true
			}
			lines := make([]string, 0, len(display)+1)
			lines = append(lines, fmt.Sprintf("session %q: %d event(s) (showing %d):", sessionID, len(events), len(display)))
			for _, event := range display {
				lines = append(lines, summaryLine(event, false, 0))
			}
			if truncated {
				lines = append(lines, fmt.Sprintf("[truncated: showing the first %d of %d events]", MaxTraceEvents, len(events)))
			}
			return toolutil.TextResult(toolutil.TruncateBytes(strings.Join(lines, "\n"), maxOutputBytes, "\n[truncated: output exceeded 32 KiB]")), nil
		},
	}
}

// --- session_event_read -----------------------------------------------------

func eventReadTool(log rig.SessionLog) rig.Tool {
	return &sessionTool{
		log:  log,
		name: ToolEventRead,
		description: "Read one complete session event by session id and sequence number, including its payload " +
			"(payloads over 4 KiB are truncated with a notice). Get the sequence number from session_search, " +
			"session_trace or session_event_search.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"session_id": toolutil.StringProp("Session id the event belongs to (required: sequence numbers are per session)."),
			"seq":        toolutil.IntegerProp("Sequence number of the event to read (from session_trace/search)."),
		}, "session_id", "seq"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args struct {
				SessionID string `json:"session_id"`
				Seq       int64  `json:"seq"`
			}
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			tool, failure := requireLog(log, ToolEventRead)
			if failure != nil {
				return *failure, nil
			}
			sessionID := strings.TrimSpace(args.SessionID)
			if sessionID == "" {
				return toolutil.ErrorResultf("%s: session_id is required (sequence numbers are per session)", ToolEventRead), nil
			}
			if args.Seq <= 0 {
				return toolutil.ErrorResultf("%s: seq must be a positive sequence number", ToolEventRead), nil
			}
			events, err := tool.log.Events(ctx, sessionID, args.Seq, args.Seq)
			if err != nil {
				return logFailure(ctx, ToolEventRead, err), nil
			}
			var target *rig.SessionEvent
			for index := range events {
				if events[index].Seq == args.Seq {
					target = &events[index]
					break
				}
			}
			if target == nil {
				return toolutil.TextResult(fmt.Sprintf("event #%d not found in session %q", args.Seq, sessionID)), nil
			}
			return toolutil.TextResult(toolutil.TruncateBytes(formatEvent(*target, true), maxOutputBytes, "\n[truncated: output exceeded 32 KiB]")), nil
		},
	}
}

// --- session_event_search ---------------------------------------------------

func eventSearchTool(log rig.SessionLog) rig.Tool {
	return &sessionTool{
		log:  log,
		name: ToolEventSearch,
		description: "Search session events like session_search, but print a payload excerpt per hit so the model " +
			"can identify the right event without reading each one. Pair it with session_event_read or " +
			"session_event_trace.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"query": toolutil.StringProp("Literal text to search for (required; case-insensitive, matched against session id, event type and payload)."),
			"limit": toolutil.IntegerProp("Maximum number of matching events (default 20, maximum 100)."),
		}, "query"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args struct {
				Query string `json:"query"`
				Limit int    `json:"limit"`
			}
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			tool, failure := requireLog(log, ToolEventSearch)
			if failure != nil {
				return *failure, nil
			}
			query := strings.TrimSpace(args.Query)
			if query == "" {
				return toolutil.ErrorResultf("%s: query is required and must contain non-whitespace text", ToolEventSearch), nil
			}
			limit := clampLimit(args.Limit, DefaultLimit, MaxLimit)
			events, err := tool.log.Search(ctx, query, limit)
			if err != nil {
				return logFailure(ctx, ToolEventSearch, err), nil
			}
			if len(events) == 0 {
				return toolutil.TextResult(fmt.Sprintf("no session events match %q", query)), nil
			}
			lines := make([]string, 0, len(events)+1)
			lines = append(lines, fmt.Sprintf("%d matching event(s) for %q (limit %d):", len(events), query, limit))
			for _, event := range events {
				line := summaryLine(event, true, payloadExcerptBytes)
				if excerpt := payloadExcerpt(event.Payload, payloadExcerptBytes); excerpt != "" {
					line += "\n    payload: " + excerpt
				}
				lines = append(lines, line)
			}
			return toolutil.TextResult(toolutil.TruncateBytes(strings.Join(lines, "\n"), maxOutputBytes, "\n[truncated: output exceeded 32 KiB]")), nil
		},
	}
}

// --- session_event_trace ----------------------------------------------------

func eventTraceTool(log rig.SessionLog) rig.Tool {
	return &sessionTool{
		log:  log,
		name: ToolEventTrace,
		description: "Show the events immediately before and after one sequence number (default 2 each side, " +
			"maximum 10). Pass session_id when known; without it the session is resolved by looking the sequence " +
			"number up across sessions (best effort).",
		parameters: toolutil.ObjectSchema(map[string]any{
			"seq":        toolutil.IntegerProp("Sequence number of the target event."),
			"session_id": toolutil.StringProp("Session id (optional when the sequence number is unique across sessions)."),
			"before":     toolutil.IntegerProp("How many earlier events to include (default 2, maximum 10)."),
			"after":      toolutil.IntegerProp("How many later events to include (default 2, maximum 10)."),
		}, "seq"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args struct {
				Seq       int64  `json:"seq"`
				SessionID string `json:"session_id"`
				Before    int    `json:"before"`
				After     int    `json:"after"`
			}
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			tool, failure := requireLog(log, ToolEventTrace)
			if failure != nil {
				return *failure, nil
			}
			if args.Seq <= 0 {
				return toolutil.ErrorResultf("%s: seq must be a positive sequence number", ToolEventTrace), nil
			}
			if args.Before < 0 || args.After < 0 {
				return toolutil.ErrorResultf("%s: before and after must not be negative", ToolEventTrace), nil
			}
			before := clampLimit(args.Before, DefaultContextEvents, MaxContextEvents)
			after := clampLimit(args.After, DefaultContextEvents, MaxContextEvents)

			sessionID := strings.TrimSpace(args.SessionID)
			if sessionID == "" {
				resolved, err := resolveSession(ctx, tool.log, args.Seq)
				if err != nil {
					return logFailure(ctx, ToolEventTrace, err), nil
				}
				sessionID = resolved
			}
			from := args.Seq - int64(before)
			if from < 0 {
				from = 0
			}
			to := args.Seq + int64(after)
			events, err := tool.log.Events(ctx, sessionID, from, to)
			if err != nil {
				return logFailure(ctx, ToolEventTrace, err), nil
			}
			if len(events) == 0 {
				return toolutil.TextResult(fmt.Sprintf("no events around #%d in session %q", args.Seq, sessionID)), nil
			}
			lines := make([]string, 0, len(events)+1)
			lines = append(lines, fmt.Sprintf("event trace around #%d in session %q (before=%d after=%d):",
				args.Seq, sessionID, before, after))
			found := false
			for _, event := range events {
				marker := "  "
				if event.Seq == args.Seq {
					marker = ">>"
					found = true
				}
				lines = append(lines, marker+" "+summaryLine(event, false, 0))
			}
			if !found {
				lines = append(lines, fmt.Sprintf("(target event #%d is not in this session's sequence range)", args.Seq))
			}
			return toolutil.TextResult(toolutil.TruncateBytes(strings.Join(lines, "\n"), maxOutputBytes, "\n[truncated: output exceeded 32 KiB]")), nil
		},
	}
}

// --- shared helpers ---------------------------------------------------------

// requireLog returns the tool with its log, or a readable failure when the host
// does not provide session storage.
func requireLog(log rig.SessionLog, tool string) (*sessionTool, *rig.ToolResult) {
	if log == nil {
		result := toolutil.ErrorResultf("%s: no session log is configured (the host provides contracts/rig.SessionLog)", tool)
		return nil, &result
	}
	return &sessionTool{log: log}, nil
}

// clampLimit applies the default and maximum for a positive limit.
func clampLimit(value, fallback, maximum int) int {
	if value <= 0 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}

// summaryLine renders one event's seq/type/time (+ session) and optional
// payload excerpt.
func summaryLine(event rig.SessionEvent, includeSession bool, excerptBytes int) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "#%d", event.Seq)
	if includeSession {
		fmt.Fprintf(&builder, " [%s]", displaySession(event.SessionID))
	}
	fmt.Fprintf(&builder, " %s %s", displayType(event.Type), formatTime(event.Time))
	if excerptBytes > 0 {
		if excerpt := payloadExcerpt(event.Payload, excerptBytes); excerpt != "" {
			builder.WriteString(" " + excerpt)
		}
	}
	return builder.String()
}

// formatEvent renders one complete event for session_event_read.
func formatEvent(event rig.SessionEvent, withPayload bool) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "event #%d\n", event.Seq)
	fmt.Fprintf(&builder, "session: %s\n", displaySession(event.SessionID))
	fmt.Fprintf(&builder, "type: %s\n", displayType(event.Type))
	fmt.Fprintf(&builder, "time: %s\n", formatTime(event.Time))
	if withPayload {
		builder.WriteString("payload:\n")
		if text := payloadText(event.Payload, eventPayloadBytes); text != "" {
			builder.WriteString(text)
		} else {
			builder.WriteString("(no payload)")
		}
	}
	return builder.String()
}

// resolveSession finds the session owning seq by searching across sessions.
// It reports a readable ambiguity error when the sequence number appears in
// more than one session.
func resolveSession(ctx context.Context, log rig.SessionLog, seq int64) (string, error) {
	events, err := log.Search(ctx, "", MaxLimit)
	if err != nil {
		return "", err
	}
	sessions := make([]string, 0, 1)
	seen := make(map[string]bool)
	for _, event := range events {
		if event.Seq != seq || event.SessionID == "" || seen[event.SessionID] {
			continue
		}
		seen[event.SessionID] = true
		sessions = append(sessions, event.SessionID)
	}
	switch len(sessions) {
	case 1:
		return sessions[0], nil
	case 0:
		return "", fmt.Errorf("no event #%d found; pass session_id (and check the sequence number)", seq)
	default:
		return "", fmt.Errorf("event #%d exists in several sessions (%s); pass session_id",
			seq, strings.Join(sessions, ", "))
	}
}

func logFailure(ctx context.Context, tool string, err error) rig.ToolResult {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return rig.ToolResult{Content: ctxErr.Error(), IsError: true}
	}
	return toolutil.ErrorResultf("%s: %v", tool, err)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Format(time.RFC3339)
}

func displaySession(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return "-"
	}
	return strings.TrimSpace(sessionID)
}

func displayType(eventType string) string {
	if strings.TrimSpace(eventType) == "" {
		return "-"
	}
	return strings.TrimSpace(eventType)
}

// payloadExcerpt compacts a payload to one truncated line.
func payloadExcerpt(payload json.RawMessage, max int) string {
	text := payloadText(payload, max)
	if text == "" {
		return ""
	}
	return "payload=" + text
}

// payloadText renders a payload as compact text, capped at max bytes.
func payloadText(payload json.RawMessage, max int) string {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	compact := strings.Join(strings.Fields(trimmed), " ")
	if len(compact) <= max {
		return compact
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(compact[cut]) {
		cut--
	}
	return compact[:cut] + "…"
}
