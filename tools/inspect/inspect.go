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

// ToolName 是模型可见的工具名。
const ToolName = "inspect"

// inspect 工具支持的 action。
const (
	ActionSessions  = "sessions"
	ActionEvents    = "events"
	ActionWorkspace = "workspace"
	ActionStats     = "stats"
)

// 限制与上限。它们被导出，便于宿主与测试断言文档中承诺的行为。
const (
	// DefaultLimit 是会话类 action 的默认事件/会话数量上限。
	DefaultLimit = 20
	// MaxLimit 限制事件/会话数量。
	MaxLimit = 200
	// DefaultWalkLimit 是 action=workspace 默认统计的文件数。
	DefaultWalkLimit = 1000
	// MaxWalkLimit 限制工作区文件遍历数量。
	MaxWalkLimit = 10000
	// topLevelEntries 控制工作区顶层条目的列出数量。
	topLevelEntries = 12
	// maxOutputBytes 限制单次 inspect 响应的字节数（32 KiB）。
	maxOutputBytes = 32 << 10
	// payloadExcerptBytes 限制事件载荷摘录的字节数。
	payloadExcerptBytes = 200
)

// SessionLister 是可选的扩展接口：rig.SessionLog 的实现可以额外实现它，以枚举日志
// 已知的会话 id。冻结的 rig.SessionLog 没有枚举方法，因此在缺少该扩展时，inspect 会
// 退化为从 Search 结果中推断会话。
type SessionLister interface {
	Sessions(ctx context.Context) ([]string, error)
}

// Options 配置 inspect 工具。
type Options struct {
	// Root 是 action=workspace 与文件计数所报告的工作区根目录。可选：为空时报告
	// "(not configured)"。
	Root string
	// Log 为 sessions/events/stats 动作读取会话事件。为 nil 时这些动作降级为
	// 仅工作区检查。
	Log rig.SessionLog
}

// NewInspectTool 返回 inspect 工具。构造不会失败；缺少配置由 Invoke 报告。
func NewInspectTool(opts Options) rig.Tool {
	return &inspectTool{root: normalizeRoot(opts.Root), log: opts.Log}
}

// New 等同于 NewInspectTool。
func New(opts Options) rig.Tool { return NewInspectTool(opts) }

type inspectTool struct {
	root string
	log  rig.SessionLog
}

// Name 实现 rig.Tool。
func (t *inspectTool) Name() string { return ToolName }

// Description 实现 rig.Tool。
func (t *inspectTool) Description() string {
	return "Inspect this agent's environment (read-only): action=sessions lists known sessions, action=events shows " +
		"one session's recent events (session_id required), action=workspace reports the workspace root and file count, " +
		"action=stats summarizes event types, tool calls and errors (optionally scoped to one session). " +
		"Without a host session log the session actions degrade to a workspace-only snapshot. Use it to orient " +
		"yourself before searching history with the session_* tools."
}

// Schema 实现 rig.Tool。
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

// Invoke 实现 rig.Tool。
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

// sessions 报告已知会话。有可选的 SessionLister 扩展时优先使用，否则从 Search 结果
// 推断会话。
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

	// 兜底：按会话对可搜索事件分组。
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

// sessionLine 为 action=sessions 汇总单个会话。
func (t *inspectTool) sessionLine(ctx context.Context, sessionID string) string {
	last, err := t.log.Last(ctx, sessionID, 1)
	if err != nil || len(last) == 0 {
		return sessionID
	}
	event := last[0]
	return fmt.Sprintf("%s  last=#%d %s %s", sessionID, event.Seq, display(event.Type), formatTime(event.Time))
}

// events 报告某个会话的近期事件。
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

// stats 汇总事件类型、工具调用与错误。
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

// workspace 报告工作区根目录以及有界的文件/目录计数。
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
			return nil // 不可读的条目直接跳过，不算致命错误
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

// --- 共享辅助函数 ---------------------------------------------------------

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
	return min(value, maximum)
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
