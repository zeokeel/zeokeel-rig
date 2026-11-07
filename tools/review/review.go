package review

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// 模型可见的工具名。
const (
	ToolCodeReview = "code_review"
	ToolAutoReview = "auto_review"
)

// code_review 接受的评审类型。
const (
	KindCode = "code"
	KindDiff = "diff"
	KindPlan = "plan"
)

// 用于发现项与 auto_review 阈值的严重级别。
const (
	SeverityInfo    = "info"
	SeverityWarning = "warning"
	SeverityError   = "error"
)

// 展示限制。它们被导出，便于宿主与测试断言文档中承诺的行为。
const (
	// DefaultKind 是 code_review 未提供 kind 时的默认类型。
	DefaultKind = KindCode
	// DefaultSeverityThreshold 是 auto_review 的默认阻塞阈值。
	DefaultSeverityThreshold = SeverityWarning
	// DefaultReviewPrompt 是调用未提供 prompt 时使用的默认评审提示。
	DefaultReviewPrompt = "Review the material and report concrete problems that should be fixed."
	// MaxSummaryChars 限制格式化输出中的摘要长度。
	MaxSummaryChars = 4000
	// MaxFindingMessageChars 限制格式化输出中单条发现消息的长度。
	MaxFindingMessageChars = 2000
	// MaxFindingsDisplay 限制渲染的发现数量。
	MaxFindingsDisplay = 100
)

// Tools 返回绑定到 reviewer 的评审工具：code_review 后跟 auto_review。reviewer
// 为 nil 时，工具在调用时会返回可读的配置错误。
func Tools(reviewer rig.Reviewer) []rig.Tool {
	return []rig.Tool{NewCodeReviewTool(reviewer), NewAutoReviewTool(reviewer)}
}

// reviewTool 是两个评审工具共用的 rig.Tool 实现。
type reviewTool struct {
	name        string
	description string
	parameters  map[string]any
	run         func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error)
}

func (t *reviewTool) Name() string           { return t.name }
func (t *reviewTool) Description() string    { return t.description }
func (t *reviewTool) Schema() map[string]any { return t.parameters }
func (t *reviewTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	return t.run(ctx, call)
}

// --- code_review ------------------------------------------------------------

type codeReviewArgs struct {
	Target  string `json:"target"`
	Kind    string `json:"kind"`
	Prompt  string `json:"prompt"`
	Content string `json:"content"`
}

// NewCodeReviewTool 返回 code_review：请求 rig.Reviewer 评审代码、diff 或计划，并
// 格式化结论、摘要与发现项。
func NewCodeReviewTool(reviewer rig.Reviewer) rig.Tool {
	return &reviewTool{
		name: ToolCodeReview,
		description: "Review code, a diff or a plan with the configured reviewer and return its verdict, summary and " +
			"findings (severity, file:line, message). Use it after changing code, before merging, or when the user " +
			"asks for a review.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"target":  toolutil.StringProp("Path, diff identifier or session id of the material to review."),
			"kind":    kindProp("What is being reviewed: code (default), diff or plan."),
			"prompt":  toolutil.StringProp("Extra review focus or instruction for the reviewer."),
			"content": toolutil.StringProp("The material to review when it is not loadable from target."),
		}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args codeReviewArgs
			if err := toolutil.DecodeArgsAs("review", call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			kind, err := normalizeKind(args.Kind)
			if err != nil {
				return toolutil.ErrorResultf("code_review: %v", err), nil
			}
			target := strings.TrimSpace(args.Target)
			prompt := strings.TrimSpace(args.Prompt)
			if target == "" && prompt == "" && strings.TrimSpace(args.Content) == "" {
				return toolutil.ErrorResultf("code_review: target, prompt or content is required"), nil
			}
			if prompt == "" {
				prompt = DefaultReviewPrompt
			}
			if isNil(reviewer) {
				return toolutil.ErrorResultf("code_review: no rig.Reviewer is configured; the host must wire one"), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			result, err := reviewer.Review(ctx, rig.ReviewRequest{
				Kind:    kind,
				Target:  target,
				Prompt:  prompt,
				Content: args.Content,
			})
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return rig.ToolResult{}, ctxErr
				}
				return toolutil.ErrorResultf("code_review: review failed: %v", err), nil
			}
			return toolutil.TextResult(formatReview(ToolCodeReview, result, kind, target)), nil
		},
	}
}

// --- auto_review ------------------------------------------------------------

type autoReviewArgs struct {
	Target            string `json:"target"`
	Prompt            string `json:"prompt"`
	SeverityThreshold string `json:"severity_threshold"`
}

// NewAutoReviewTool 返回 auto_review：评审目标并明确其结果是否应阻塞进度。fail/block
// 结论、或严重级别达到 severity_threshold 的发现，会在文本中被标记为阻塞；工具结果本身
// 不是错误（由模型决定如何处理）。
func NewAutoReviewTool(reviewer rig.Reviewer) rig.Tool {
	return &reviewTool{
		name: ToolAutoReview,
		description: "Automatically review a target and report whether it should block progress. A fail/block verdict " +
			"or any finding at or above severity_threshold (default warning) is reported as blocking; a reviewer " +
			"failure is a tool error.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"target": toolutil.StringProp("Path, diff identifier or session id of the material to review."),
			"prompt": toolutil.StringProp("Extra review focus or instruction for the reviewer."),
			"severity_threshold": map[string]any{
				"type":        "string",
				"enum":        []any{SeverityInfo, SeverityWarning, SeverityError},
				"description": "Findings at or above this severity mark the review as blocking (default warning).",
			},
		}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args autoReviewArgs
			if err := toolutil.DecodeArgsAs("review", call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			threshold, err := parseThreshold(args.SeverityThreshold)
			if err != nil {
				return toolutil.ErrorResultf("auto_review: %v", err), nil
			}
			target := strings.TrimSpace(args.Target)
			if target == "" && strings.TrimSpace(args.Prompt) == "" {
				return toolutil.ErrorResultf("auto_review: target or prompt is required"), nil
			}
			prompt := strings.TrimSpace(args.Prompt)
			if prompt == "" {
				prompt = DefaultReviewPrompt
			}
			if isNil(reviewer) {
				return toolutil.ErrorResultf("auto_review: no rig.Reviewer is configured; the host must wire one"), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			result, err := reviewer.Review(ctx, rig.ReviewRequest{
				Kind:   KindCode,
				Target: target,
				Prompt: prompt,
			})
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return rig.ToolResult{}, ctxErr
				}
				return toolutil.ErrorResultf("auto_review: review failed: %v", err), nil
			}
			blocking, reasons := blockingReasons(result, threshold)
			return toolutil.TextResult(formatAutoReview(result, target, threshold, blocking, reasons)), nil
		},
	}
}

// blockingReasons 判断一次评审是否应阻塞进度。
func blockingReasons(result rig.ReviewResult, threshold int) (bool, []string) {
	reasons := make([]string, 0, 2)
	verdict := normalizeVerdict(result.Verdict)
	if isBlockingVerdict(verdict) {
		reasons = append(reasons, fmt.Sprintf("verdict %q requires changes", verdict))
	}
	blockingFindings := 0
	for _, finding := range result.Findings {
		if severityRank(finding.Severity) >= threshold {
			blockingFindings++
		}
	}
	if blockingFindings > 0 {
		reasons = append(reasons, fmt.Sprintf("%d finding(s) at or above %s", blockingFindings, severityName(threshold)))
	}
	return len(reasons) > 0, reasons
}

// --- 格式化 -----------------------------------------------------------------

// formatReview 渲染一次 code_review 结果。
func formatReview(tool string, result rig.ReviewResult, kind, target string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s: %s\n", tool, normalizeVerdict(result.Verdict))
	if target != "" {
		fmt.Fprintf(&builder, "target: %s\n", target)
	}
	fmt.Fprintf(&builder, "kind: %s\n", kind)
	fmt.Fprintf(&builder, "summary: %s\n", summaryText(result.Summary))
	writeFindings(&builder, result.Findings)
	return strings.TrimRight(builder.String(), "\n")
}

// formatAutoReview 渲染一次 auto_review 结果及其阻塞判定。
func formatAutoReview(result rig.ReviewResult, target string, threshold int, blocking bool, reasons []string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "auto_review: %s\n", normalizeVerdict(result.Verdict))
	if target != "" {
		fmt.Fprintf(&builder, "target: %s\n", target)
	}
	fmt.Fprintf(&builder, "severity_threshold: %s\n", severityName(threshold))
	if blocking {
		builder.WriteString("blocking: yes\n")
		builder.WriteString("recommendation: BLOCK — address the blocking findings before proceeding\n")
		if len(reasons) > 0 {
			fmt.Fprintf(&builder, "blocking_reason: %s\n", strings.Join(reasons, "; "))
		}
	} else {
		builder.WriteString("blocking: no\n")
		builder.WriteString("recommendation: OK — no blocking verdict or findings at or above the threshold\n")
	}
	fmt.Fprintf(&builder, "summary: %s\n", summaryText(result.Summary))
	writeFindings(&builder, result.Findings)
	return strings.TrimRight(builder.String(), "\n")
}

// writeFindings 渲染发现项区块。
func writeFindings(builder *strings.Builder, findings []rig.ReviewFinding) {
	if len(findings) == 0 {
		builder.WriteString("findings: none\n")
		return
	}
	fmt.Fprintf(builder, "findings (%d):\n", len(findings))
	shown := findings
	if len(shown) > MaxFindingsDisplay {
		shown = shown[:MaxFindingsDisplay]
	}
	for _, finding := range shown {
		fmt.Fprintf(builder, "- [%s] %s: %s\n",
			severityLabel(finding.Severity),
			locationLabel(finding),
			toolutil.TruncateBytes(strings.TrimSpace(finding.Message), MaxFindingMessageChars, "…"))
	}
	if len(findings) > MaxFindingsDisplay {
		fmt.Fprintf(builder, "- [truncated] %d more finding(s) not shown\n", len(findings)-MaxFindingsDisplay)
	}
}

// summaryText 渲染摘要，并处理为空与截断的情况。
func summaryText(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "(no summary)"
	}
	return toolutil.TruncateBytes(summary, MaxSummaryChars, "…")
}

// locationLabel 渲染 "file:line"、"file" 或 "(no location)"。
func locationLabel(finding rig.ReviewFinding) string {
	file := strings.TrimSpace(finding.File)
	switch {
	case file != "" && finding.Line > 0:
		return fmt.Sprintf("%s:%d", file, finding.Line)
	case file != "":
		return file
	case finding.Line > 0:
		return fmt.Sprintf("line %d", finding.Line)
	default:
		return "(no location)"
	}
}

// --- 严重级别 / 结论辅助函数 ------------------------------------------------

// severityRank 把严重级别拼写映射为 info=0、warning=1、error=2。
func severityRank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "info", "note", "low", "minor", "nit":
		return 0
	case "warn", "warning", "medium", "moderate":
		return 1
	case "error", "high", "critical", "blocker", "blocking", "fatal", "major":
		return 2
	default:
		return 1
	}
}

// severityName 渲染序号 0/1/2 对应的名称。
func severityName(rank int) string {
	switch {
	case rank <= 0:
		return SeverityInfo
	case rank == 1:
		return SeverityWarning
	default:
		return SeverityError
	}
}

// severityLabel 紧凑地渲染一条发现的严重级别。
func severityLabel(severity string) string {
	trimmed := strings.ToLower(strings.TrimSpace(severity))
	switch trimmed {
	case "":
		return "unknown"
	case "warn":
		return SeverityWarning
	case "err":
		return SeverityError
	default:
		return trimmed
	}
}

// parseThreshold 解析 auto_review 的严重级别阈值。空值表示使用默认值。
func parseThreshold(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return severityRank(DefaultSeverityThreshold), nil
	}
	switch strings.ToLower(raw) {
	case SeverityInfo:
		return 0, nil
	case SeverityWarning:
		return 1, nil
	case SeverityError:
		return 2, nil
	default:
		return 0, fmt.Errorf("invalid severity_threshold %q (want info, warning or error)", raw)
	}
}

// normalizeKind 校验并补默认值，返回评审类型。
func normalizeKind(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return DefaultKind, nil
	case KindCode:
		return KindCode, nil
	case KindDiff:
		return KindDiff, nil
	case KindPlan:
		return KindPlan, nil
	default:
		return "", fmt.Errorf("invalid kind %q (want code, diff or plan)", raw)
	}
}

// normalizeVerdict 把结论渲染为展示文本（空表示通过）。
func normalizeVerdict(verdict string) string {
	trimmed := strings.ToLower(strings.TrimSpace(verdict))
	if trimmed == "" {
		return "pass"
	}
	return trimmed
}

// isBlockingVerdict 判断结论是否要求修改。
func isBlockingVerdict(verdict string) bool {
	lower := strings.ToLower(verdict)
	return strings.Contains(lower, "fail") || strings.Contains(lower, "block")
}

// kindProp 构造 kind 的 schema 属性。
func kindProp(description string) map[string]any {
	return map[string]any{
		"type":        "string",
		"enum":        []any{KindCode, KindDiff, KindPlan},
		"description": description,
	}
}

// isNil 判断 value 是否为 nil 接口或其后的带类型 nil。
func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
