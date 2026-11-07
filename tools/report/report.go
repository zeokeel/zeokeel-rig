package report

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolName 是模型可见的工具名。
const ToolName = "report"

// 限制与文件默认值。它们被导出，便于宿主与测试断言文档中承诺的行为。
const (
	// DefaultLimit 是默认扫描的会话事件数。
	DefaultLimit = 200
	// MaxLimit 限制单份报告扫描的事件数。
	MaxLimit = 2000
	// FileMode 是写入报告文件的权限位。
	FileMode = 0o644
	// timestampLayout 是默认文件名使用的时间戳格式。
	timestampLayout = "20060102-150405"
	// recentEvents 是报告中列出的最近事件数。
	recentEvents = 10
	// payloadExcerptBytes 限制报告中事件载荷摘录的字节数。
	payloadExcerptBytes = 200
	// defaultTitle 是调用未提供标题时使用的默认标题。
	defaultTitle = "Rig report"
)

// Options 配置 report 工具。
type Options struct {
	// Log 提供报告中汇总的会话事件。为 nil 时只报告工作区。
	Log rig.SessionLog
	// Policy 限制报告文件可以写入的位置。调用时必需；为 nil 时返回可读的工具错误。
	Policy *sandbox.Policy
	// Now 覆盖时钟（默认文件名与 "generated" 行）；为 nil 表示 time.Now。测试可注入
	// 确定性时钟。
	Now func() time.Time
}

// NewReportTool 返回 report 工具。构造不会失败；缺少配置由 Invoke 报告。
func NewReportTool(opts Options) rig.Tool {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &reportTool{log: opts.Log, policy: opts.Policy, now: now}
}

// New 等同于 NewReportTool。
func New(opts Options) rig.Tool { return NewReportTool(opts) }

type reportTool struct {
	log    rig.SessionLog
	policy *sandbox.Policy
	now    func() time.Time
}

// Name 实现 rig.Tool。
func (t *reportTool) Name() string { return ToolName }

// Description 实现 rig.Tool。
func (t *reportTool) Description() string {
	return "Write a Markdown report of the session's work into the sandbox: event counts, tool-call and error totals, " +
		"event-type breakdown, recent events and optional extra content. Use it when the user asks for a written " +
		"summary or deliverable. The report is written inside the workspace root; a missing session log produces a " +
		"workspace-only report. Returns the file path, size and word count."
}

// Schema 实现 rig.Tool。
func (t *reportTool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"path":       toolutil.StringProp("Where to write the report, relative to the sandbox root (default rig-report-<timestamp>.md in the primary root)."),
		"title":      toolutil.StringProp("Report title (default \"Rig report\")."),
		"session_id": toolutil.StringProp("Session whose events are summarized; omitted = searchable events across sessions, no session log = workspace only."),
		"limit":      toolutil.IntegerProp("Maximum number of events to scan (default 200, maximum 2000)."),
		"content":    toolutil.StringProp("Optional extra Markdown appended as the report's Content section."),
	})
}

// Invoke 实现 rig.Tool。
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
	// 每条路径都必须释放 *os.Root 句柄：在 Windows 上，未关闭的目录句柄会让报告文件
	// 保持锁定。
	defer root.Close()
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

// summary 是渲染进报告前聚合出的会话/工作状态。
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

// renderReport 构建 Markdown 文档。
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

// reportReadError 把会话日志失败转换为工具结果（调用方上下文结束时则返回错误）。
func reportReadError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return fmt.Errorf("report: read session events: %w", err)
}

// --- 共享辅助函数 ---------------------------------------------------------

func clampLimit(value, fallback, maximum int) int {
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
