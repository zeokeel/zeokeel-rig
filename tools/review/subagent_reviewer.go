package review

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
)

// Defaults for SubagentReviewer.
const (
	// DefaultReviewerName is the subagent name used by SubagentReviewer.
	DefaultReviewerName = "reviewer"
	// DefaultReviewerInstruction is prepended to the review prompt.
	DefaultReviewerInstruction = "You are a strict code reviewer. Review the material below and report concrete problems."
	// DefaultReviewContentLimit caps how much ReviewRequest.Content is embedded
	// in the subagent prompt.
	DefaultReviewContentLimit = 12000
)

// SubagentReviewer implements rig.Reviewer with an in-process review subagent: it
// starts a child agent through rig.SubagentRunner with a structured review
// prompt, waits for its final answer and parses that plain text into a
// rig.ReviewResult.
//
// The expected reply format is deliberately loose: an optional
// "VERDICT: pass|warn|fail" line (default pass), one "FILE:LINE SEVERITY:
// message" line per finding, and any remaining prose becomes the summary.
type SubagentReviewer struct {
	// Runner starts and stops the review subagent. Required.
	Runner rig.SubagentRunner
	// Name is the subagent name (default "reviewer").
	Name string
	// Instruction overrides the reviewer instruction (default
	// DefaultReviewerInstruction).
	Instruction string
	// MaxContentChars caps the embedded ReviewRequest.Content (default
	// DefaultReviewContentLimit; negative embeds no content).
	MaxContentChars int
}

var _ rig.Reviewer = (*SubagentReviewer)(nil)

// NewSubagentReviewer returns a reviewer running through runner.
func NewSubagentReviewer(runner rig.SubagentRunner) *SubagentReviewer {
	return &SubagentReviewer{Runner: runner}
}

// Review implements rig.Reviewer.
func (r *SubagentReviewer) Review(ctx context.Context, request rig.ReviewRequest) (rig.ReviewResult, error) {
	if r == nil || isNil(r.Runner) {
		return rig.ReviewResult{}, errors.New("review: no subagent runner configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return rig.ReviewResult{}, err
	}
	contentLimit := r.MaxContentChars
	if contentLimit == 0 {
		contentLimit = DefaultReviewContentLimit
	}
	name := strings.TrimSpace(r.Name)
	if name == "" {
		name = DefaultReviewerName
	}
	instruction := strings.TrimSpace(r.Instruction)
	if instruction == "" {
		instruction = DefaultReviewerInstruction
	}
	prompt := buildReviewPrompt(request, instruction, contentLimit)

	sub, err := r.Runner.Start(ctx, rig.SubagentSpec{Name: name, Prompt: prompt})
	if err != nil {
		return rig.ReviewResult{}, fmt.Errorf("review: start reviewer subagent: %w", err)
	}
	defer func() { _ = sub.Stop() }()
	result, err := sub.Wait(ctx)
	if err != nil {
		return rig.ReviewResult{}, fmt.Errorf("review: reviewer subagent failed: %w", err)
	}
	return ParseReviewOutput(result.Output), nil
}

// buildReviewPrompt assembles the child prompt from one ReviewRequest.
func buildReviewPrompt(request rig.ReviewRequest, instruction string, contentLimit int) string {
	var builder strings.Builder
	builder.WriteString(instruction)
	builder.WriteString("\n\nKind: ")
	kind := strings.ToLower(strings.TrimSpace(request.Kind))
	if kind == "" {
		kind = DefaultKind
	}
	builder.WriteString(kind)
	builder.WriteString("\n")
	if target := strings.TrimSpace(request.Target); target != "" {
		fmt.Fprintf(&builder, "Target: %s\n", target)
	}
	if focus := strings.TrimSpace(request.Prompt); focus != "" {
		fmt.Fprintf(&builder, "Review focus: %s\n", focus)
	}
	if content := strings.TrimSpace(request.Content); content != "" && contentLimit >= 0 {
		fmt.Fprintf(&builder, "\nContent:\n%s\n", truncateContent(content, contentLimit))
	}
	builder.WriteString("\nReport in this format:\n")
	builder.WriteString("VERDICT: pass|warn|fail\n")
	builder.WriteString("A short summary paragraph, then one line per finding:\n")
	builder.WriteString("FILE:LINE SEVERITY: message\n")
	builder.WriteString("(SEVERITY is info, warning or error; use - as FILE when no file applies.)\n")
	return builder.String()
}

// truncateContent bounds the reviewed content embedded in the prompt (UTF-8
// safe) and marks the cut.
func truncateContent(content string, limit int) string {
	if limit < 0 || len(content) <= limit {
		return content
	}
	if limit == 0 {
		return "[content omitted: reviewer content limit is 0]"
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	return content[:cut] + fmt.Sprintf("\n[truncated: review content exceeded %d characters]", limit)
}

// --- output parsing ---------------------------------------------------------

var (
	// verdictPattern matches an optional "VERDICT: <word>" line.
	verdictPattern = regexp.MustCompile(`(?im)^\s*VERDICT\s*:\s*([A-Za-z][A-Za-z_-]*)`)
	// findingPattern matches "FILE:LINE SEVERITY: message" (bullets allowed).
	findingPattern = regexp.MustCompile(`(?m)^\s*(?:[-*]\s*)?(.+?):(\d+)\s+([A-Za-z][A-Za-z_-]*)\s*:\s*(.+?)\s*$`)
)

// ParseReviewOutput turns a reviewer's plain text into a ReviewResult:
//
//   - "VERDICT: pass|warn|fail" (case-insensitive, optional) sets the verdict;
//     an absent verdict defaults to "pass";
//   - "FILE:LINE SEVERITY: message" lines become findings (a leading bullet is
//     allowed, "-" as FILE means no file, severity is normalized);
//   - every other non-empty line becomes the summary (a leading "Summary:"
//     label is stripped).
func ParseReviewOutput(text string) rig.ReviewResult {
	result := rig.ReviewResult{Verdict: "pass"}
	summaryLines := make([]string, 0, 4)
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if match := verdictPattern.FindStringSubmatch(line); match != nil {
			result.Verdict = strings.ToLower(match[1])
			continue
		}
		if match := findingPattern.FindStringSubmatch(line); match != nil {
			file := strings.TrimSpace(match[1])
			if file == "-" {
				file = ""
			}
			lineNumber, err := strconv.Atoi(match[2])
			if err != nil {
				lineNumber = 0
			}
			result.Findings = append(result.Findings, rig.ReviewFinding{
				Severity: normalizeFindingSeverity(match[3]),
				File:     file,
				Line:     lineNumber,
				Message:  strings.TrimSpace(match[4]),
			})
			continue
		}
		if len(line) >= len("summary:") && strings.EqualFold(line[:len("summary:")], "summary:") {
			line = strings.TrimSpace(line[len("summary:"):])
			if line == "" {
				continue
			}
		}
		summaryLines = append(summaryLines, line)
	}
	if len(summaryLines) > 0 {
		result.Summary = truncateString(strings.Join(summaryLines, "\n"), MaxSummaryChars)
	}
	return result
}

// normalizeFindingSeverity canonicalizes a parsed finding severity.
func normalizeFindingSeverity(raw string) string {
	severity := strings.ToLower(strings.TrimSpace(raw))
	switch severity {
	case "":
		return ""
	case "warn":
		return SeverityWarning
	case "err":
		return SeverityError
	default:
		return severity
	}
}
