// Package review provides the code-review tools (code_review and auto_review)
// and a rig.Reviewer implementation backed by an in-process subagent.
//
// The tools depend only on the frozen rig.Reviewer contract: a host wires it to
// a model, a human, or the SubagentReviewer in this package. A nil reviewer is a
// readable configuration error, never a panic.
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
)

// Model-facing tool names.
const (
	ToolCodeReview = "code_review"
	ToolAutoReview = "auto_review"
)

// Review kinds accepted by code_review.
const (
	KindCode = "code"
	KindDiff = "diff"
	KindPlan = "plan"
)

// Severity levels used for findings and the auto_review threshold.
const (
	SeverityInfo    = "info"
	SeverityWarning = "warning"
	SeverityError   = "error"
)

// Display limits. They are exported so hosts and tests can assert the
// documented behavior.
const (
	// DefaultKind is code_review's kind when the call omits it.
	DefaultKind = KindCode
	// DefaultSeverityThreshold is auto_review's default blocking threshold.
	DefaultSeverityThreshold = SeverityWarning
	// DefaultReviewPrompt is used when a call passes no prompt.
	DefaultReviewPrompt = "Review the material and report concrete problems that should be fixed."
	// MaxSummaryChars bounds the summary in the formatted output.
	MaxSummaryChars = 4000
	// MaxFindingMessageChars bounds one finding message in the formatted output.
	MaxFindingMessageChars = 2000
	// MaxFindingsDisplay bounds how many findings are rendered.
	MaxFindingsDisplay = 100
)

// Tools returns the review tools bound to reviewer: code_review then
// auto_review. A nil reviewer produces tools that report a readable
// configuration error when invoked.
func Tools(reviewer rig.Reviewer) []rig.Tool {
	return []rig.Tool{NewCodeReviewTool(reviewer), NewAutoReviewTool(reviewer)}
}

// reviewTool is the common rig.Tool implementation.
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

// NewCodeReviewTool returns code_review: it asks a rig.Reviewer to review code, a
// diff or a plan and formats the verdict, summary and findings.
func NewCodeReviewTool(reviewer rig.Reviewer) rig.Tool {
	return &reviewTool{
		name: ToolCodeReview,
		description: "Review code, a diff or a plan with the configured reviewer and return its verdict, summary and " +
			"findings (severity, file:line, message). Use it after changing code, before merging, or when the user " +
			"asks for a review.",
		parameters: objectSchema(map[string]any{
			"target":  stringProp("Path, diff identifier or session id of the material to review."),
			"kind":    kindProp("What is being reviewed: code (default), diff or plan."),
			"prompt":  stringProp("Extra review focus or instruction for the reviewer."),
			"content": stringProp("The material to review when it is not loadable from target."),
		}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args codeReviewArgs
			if err := decodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			kind, err := normalizeKind(args.Kind)
			if err != nil {
				return errorResult("code_review: %v", err), nil
			}
			target := strings.TrimSpace(args.Target)
			prompt := strings.TrimSpace(args.Prompt)
			if target == "" && prompt == "" && strings.TrimSpace(args.Content) == "" {
				return errorResult("code_review: target, prompt or content is required"), nil
			}
			if prompt == "" {
				prompt = DefaultReviewPrompt
			}
			if isNil(reviewer) {
				return errorResult("code_review: no rig.Reviewer is configured; the host must wire one"), nil
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
				return errorResult("code_review: review failed: %v", err), nil
			}
			return textResult(formatReview(ToolCodeReview, result, kind, target)), nil
		},
	}
}

// --- auto_review ------------------------------------------------------------

type autoReviewArgs struct {
	Target            string `json:"target"`
	Prompt            string `json:"prompt"`
	SeverityThreshold string `json:"severity_threshold"`
}

// NewAutoReviewTool returns auto_review: it reviews a target and states whether
// the result should block progress. A fail/block verdict or a finding at or
// above severity_threshold marks the result as blocking in the text; the tool
// result itself is not an error (the model decides what to do with it).
func NewAutoReviewTool(reviewer rig.Reviewer) rig.Tool {
	return &reviewTool{
		name: ToolAutoReview,
		description: "Automatically review a target and report whether it should block progress. A fail/block verdict " +
			"or any finding at or above severity_threshold (default warning) is reported as blocking; a reviewer " +
			"failure is a tool error.",
		parameters: objectSchema(map[string]any{
			"target": stringProp("Path, diff identifier or session id of the material to review."),
			"prompt": stringProp("Extra review focus or instruction for the reviewer."),
			"severity_threshold": map[string]any{
				"type":        "string",
				"enum":        []any{SeverityInfo, SeverityWarning, SeverityError},
				"description": "Findings at or above this severity mark the review as blocking (default warning).",
			},
		}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args autoReviewArgs
			if err := decodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			threshold, err := parseThreshold(args.SeverityThreshold)
			if err != nil {
				return errorResult("auto_review: %v", err), nil
			}
			target := strings.TrimSpace(args.Target)
			if target == "" && strings.TrimSpace(args.Prompt) == "" {
				return errorResult("auto_review: target or prompt is required"), nil
			}
			prompt := strings.TrimSpace(args.Prompt)
			if prompt == "" {
				prompt = DefaultReviewPrompt
			}
			if isNil(reviewer) {
				return errorResult("auto_review: no rig.Reviewer is configured; the host must wire one"), nil
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
				return errorResult("auto_review: review failed: %v", err), nil
			}
			blocking, reasons := blockingReasons(result, threshold)
			return textResult(formatAutoReview(result, target, threshold, blocking, reasons)), nil
		},
	}
}

// blockingReasons decides whether a review should block progress.
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

// --- formatting -------------------------------------------------------------

// formatReview renders one code_review result.
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

// formatAutoReview renders one auto_review result with its blocking decision.
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

// writeFindings renders the findings block.
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
			truncateString(strings.TrimSpace(finding.Message), MaxFindingMessageChars))
	}
	if len(findings) > MaxFindingsDisplay {
		fmt.Fprintf(builder, "- [truncated] %d more finding(s) not shown\n", len(findings)-MaxFindingsDisplay)
	}
}

// summaryText renders a summary with its empty/truncated states.
func summaryText(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "(no summary)"
	}
	return truncateString(summary, MaxSummaryChars)
}

// locationLabel renders "file:line", "file" or "(no location)".
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

// --- severity / verdict helpers --------------------------------------------

// severityRank maps a severity spelling to info=0, warning=1, error=2.
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

// severityName renders rank 0/1/2.
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

// severityLabel renders one finding severity compactly.
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

// parseThreshold parses the auto_review severity threshold. An empty value means
// the default.
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

// normalizeKind validates and defaults the review kind.
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

// normalizeVerdict renders a verdict for display (empty means pass).
func normalizeVerdict(verdict string) string {
	trimmed := strings.ToLower(strings.TrimSpace(verdict))
	if trimmed == "" {
		return "pass"
	}
	return trimmed
}

// isBlockingVerdict reports whether a verdict requires changes.
func isBlockingVerdict(verdict string) bool {
	lower := strings.ToLower(verdict)
	return strings.Contains(lower, "fail") || strings.Contains(lower, "block")
}

// kindProp builds the kind schema property.
func kindProp(description string) map[string]any {
	return map[string]any{
		"type":        "string",
		"enum":        []any{KindCode, KindDiff, KindPlan},
		"description": description,
	}
}

// decodeArgs decodes one call's arguments; empty arguments are an empty object.
func decodeArgs(call rig.ToolCall, target any) error {
	raw := strings.TrimSpace(call.Arguments)
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("review: invalid arguments JSON: %w", err)
	}
	return nil
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func textResult(content string) rig.ToolResult { return rig.ToolResult{Content: content} }

func errorResult(format string, args ...any) rig.ToolResult {
	return rig.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}

// truncateString shortens text to max bytes without splitting a UTF-8 rune and
// marks the cut.
func truncateString(text string, max int) string {
	if max <= 0 || len(text) <= max {
		return text
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

// isNil reports whether value is a nil interface or a typed nil behind one.
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
