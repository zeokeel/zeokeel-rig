package review

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// SubagentReviewer 的默认值。
const (
	// DefaultReviewerName 是 SubagentReviewer 使用的子代理名称。
	DefaultReviewerName = "reviewer"
	// DefaultReviewerInstruction 会前置到评审提示词中。
	DefaultReviewerInstruction = "You are a strict code reviewer. Review the material below and report concrete problems."
	// DefaultReviewContentLimit 限制嵌入子代理提示词中的 ReviewRequest.Content 字节数。
	DefaultReviewContentLimit = 12000
)

// SubagentReviewer 用进程内评审子代理实现 rig.Reviewer：它通过 rig.SubagentRunner
// 以结构化评审提示词启动子代理、等待其最终回答，并把纯文本解析为 rig.ReviewResult。
//
// 期望的回答格式刻意宽松：可选的 "VERDICT: pass|warn|fail" 行（默认 pass）、每条发现一行
// "FILE:LINE SEVERITY: message"，其余正文都作为摘要。
type SubagentReviewer struct {
	// Runner 负责启动与停止评审子代理。必填。
	Runner rig.SubagentRunner
	// Name 是子代理名称（默认 "reviewer"）。
	Name string
	// Instruction 覆盖评审指令（默认 DefaultReviewerInstruction）。
	Instruction string
	// MaxContentChars 限制嵌入的 ReviewRequest.Content（默认 DefaultReviewContentLimit；
	// 负数表示不嵌入内容）。
	MaxContentChars int
}

var _ rig.Reviewer = (*SubagentReviewer)(nil)

// NewSubagentReviewer 返回通过 runner 执行评审的 reviewer。
func NewSubagentReviewer(runner rig.SubagentRunner) *SubagentReviewer {
	return &SubagentReviewer{Runner: runner}
}

// Review 实现 rig.Reviewer。
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

// buildReviewPrompt 根据一个 ReviewRequest 组装子代理提示词。
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

// truncateContent 限制嵌入提示词的被评审内容（UTF-8 安全）并标记截断位置。
func truncateContent(content string, limit int) string {
	if limit < 0 || len(content) <= limit {
		return content
	}
	// 评审内容上限有特殊的零值语义，通用字节截断器无法表达：零表示完全不嵌入内容。
	if limit == 0 {
		return "[content omitted: reviewer content limit is 0]"
	}
	return toolutil.TruncateBytes(content, limit,
		fmt.Sprintf("\n[truncated: review content exceeded %d characters]", limit))
}

// --- 输出解析 ---------------------------------------------------------------

var (
	// verdictPattern 匹配可选的 "VERDICT: <word>" 行。
	verdictPattern = regexp.MustCompile(`(?im)^\s*VERDICT\s*:\s*([A-Za-z][A-Za-z_-]*)`)
	// findingPattern 匹配 "FILE:LINE SEVERITY: message"（允许项目符号）。
	findingPattern = regexp.MustCompile(`(?m)^\s*(?:[-*]\s*)?(.+?):(\d+)\s+([A-Za-z][A-Za-z_-]*)\s*:\s*(.+?)\s*$`)
)

// ParseReviewOutput 把评审者的纯文本转换为 ReviewResult：
//
//   - "VERDICT: pass|warn|fail"（大小写不敏感，可选）设置结论；缺省结论为 "pass"；
//   - "FILE:LINE SEVERITY: message" 行成为发现项（允许前导项目符号，FILE 为 "-" 表示
//     无文件，严重级别会被规范化）；
//   - 其他每个非空行都作为摘要（会去掉前导的 "Summary:" 标签）。
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
		result.Summary = toolutil.TruncateBytes(strings.Join(summaryLines, "\n"), MaxSummaryChars, "…")
	}
	return result
}

// normalizeFindingSeverity 规范化解析出的发现严重级别。
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
