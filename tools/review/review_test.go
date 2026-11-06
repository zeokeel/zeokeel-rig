package review_test

// Tests for the code_review and auto_review tools: request mapping, formatting,
// blocking decisions, validation and reviewer failures.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/tools/review"
)

// stubReviewer records Review calls and returns a scripted result.
type stubReviewer struct {
	mu       sync.Mutex
	requests []v1.ReviewRequest
	result   v1.ReviewResult
	err      error
}

func (s *stubReviewer) Review(_ context.Context, request v1.ReviewRequest) (v1.ReviewResult, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	result, err := s.result, s.err
	s.mu.Unlock()
	if err != nil {
		return v1.ReviewResult{}, err
	}
	return result, nil
}

func (s *stubReviewer) lastRequest(t *testing.T) v1.ReviewRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("reviewer was not called")
	}
	return s.requests[len(s.requests)-1]
}

func invokeReviewTool(t *testing.T, tool v1.Tool, arguments string) v1.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), v1.ToolCall{ID: "call-1", Name: tool.Name(), Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", tool.Name(), err)
	}
	return result
}

func invokeReviewToolError(t *testing.T, tool v1.Tool, arguments string) string {
	t.Helper()
	result := invokeReviewTool(t, tool, arguments)
	if !result.IsError {
		t.Fatalf("%s: result = %q, want IsError", tool.Name(), result.Content)
	}
	return result.Content
}

func requireContains(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Fatalf("text does not contain %q:\n%s", fragment, text)
		}
	}
}

func TestToolsCatalogAndSchemas(t *testing.T) {
	tools := review.Tools(&stubReviewer{})
	if len(tools) != 2 {
		t.Fatalf("Tools returned %d tools, want 2", len(tools))
	}
	if tools[0].Name() != review.ToolCodeReview || tools[1].Name() != review.ToolAutoReview {
		t.Fatalf("tool names = %q, %q", tools[0].Name(), tools[1].Name())
	}
	for _, tool := range tools {
		if tool.Description() == "" {
			t.Fatalf("%s has an empty description", tool.Name())
		}
		schema := tool.Schema()
		if schema["type"] != "object" {
			t.Fatalf("%s schema type = %v", tool.Name(), schema["type"])
		}
		if _, required := schema["required"]; required {
			t.Fatalf("%s should have no required arguments, schema = %v", tool.Name(), schema)
		}
		properties, _ := schema["properties"].(map[string]any)
		if _, ok := properties["target"]; !ok {
			t.Fatalf("%s has no target property", tool.Name())
		}
	}
	codeSchema := tools[0].Schema()
	codeProperties, _ := codeSchema["properties"].(map[string]any)
	kind, _ := codeProperties["kind"].(map[string]any)
	if got := strings.Join(toStrings(kind["enum"]), ","); got != "code,diff,plan" {
		t.Fatalf("code_review kind enum = %q", got)
	}
	autoSchema := tools[1].Schema()
	autoProperties, _ := autoSchema["properties"].(map[string]any)
	threshold, _ := autoProperties["severity_threshold"].(map[string]any)
	if got := strings.Join(toStrings(threshold["enum"]), ","); got != "info,warning,error" {
		t.Fatalf("auto_review threshold enum = %q", got)
	}
}

func toStrings(value any) []string {
	values, _ := value.([]any)
	out := make([]string, 0, len(values))
	for _, item := range values {
		out = append(out, item.(string))
	}
	return out
}

func TestCodeReviewFormatsResult(t *testing.T) {
	reviewer := &stubReviewer{result: v1.ReviewResult{
		Verdict: "fail",
		Summary: "two blocking problems",
		Findings: []v1.ReviewFinding{
			{Severity: "error", File: "src/app.go", Line: 42, Message: "nil map write"},
			{Severity: "warning", Message: "missing test"},
		},
	}}
	tool := review.NewCodeReviewTool(reviewer)
	result := invokeReviewTool(t, tool, `{"target":"src/app.go","kind":"diff","prompt":"focus on concurrency"}`)
	if result.IsError {
		t.Fatalf("code_review = %q", result.Content)
	}
	requireContains(t, result.Content,
		"code_review: fail",
		"target: src/app.go",
		"kind: diff",
		"summary: two blocking problems",
		"findings (2):",
		"- [error] src/app.go:42: nil map write",
		"- [warning] (no location): missing test")

	request := reviewer.lastRequest(t)
	if request.Kind != "diff" || request.Target != "src/app.go" || request.Prompt != "focus on concurrency" {
		t.Fatalf("request = %+v, want the tool arguments", request)
	}
}

func TestCodeReviewDefaults(t *testing.T) {
	reviewer := &stubReviewer{} // empty result
	tool := review.NewCodeReviewTool(reviewer)
	result := invokeReviewTool(t, tool, `{"content":"func main() {}"}`)
	if result.IsError {
		t.Fatalf("code_review = %q", result.Content)
	}
	requireContains(t, result.Content,
		"code_review: pass", "kind: code", "summary: (no summary)", "findings: none")
	request := reviewer.lastRequest(t)
	if request.Kind != "code" || request.Prompt != review.DefaultReviewPrompt || request.Content != "func main() {}" {
		t.Fatalf("request = %+v, want defaults (kind code, default prompt)", request)
	}
}

func TestCodeReviewValidationAndErrors(t *testing.T) {
	reviewer := &stubReviewer{result: v1.ReviewResult{Verdict: "pass"}}
	tool := review.NewCodeReviewTool(reviewer)

	if text := invokeReviewToolError(t, tool, `{}`); !strings.Contains(text, "target, prompt or content") {
		t.Fatalf("empty call = %q", text)
	}
	if text := invokeReviewToolError(t, tool, `{"target":"x","kind":"blog"}`); !strings.Contains(text, "invalid kind") {
		t.Fatalf("invalid kind = %q", text)
	}
	if text := invokeReviewToolError(t, review.NewCodeReviewTool(nil), `{"target":"x"}`); !strings.Contains(text, "no v1.Reviewer") {
		t.Fatalf("nil reviewer = %q", text)
	}
	failing := &stubReviewer{err: errors.New("reviewer exploded")}
	if text := invokeReviewToolError(t, review.NewCodeReviewTool(failing), `{"target":"x"}`); !strings.Contains(text, "review failed") {
		t.Fatalf("reviewer error = %q", text)
	}
	// Malformed JSON is a Go error.
	if _, err := tool.Invoke(context.Background(), v1.ToolCall{Arguments: "{"}); err == nil {
		t.Fatal("malformed arguments returned nil error")
	}
	// Caller cancellation is a Go error, not a tool result.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Invoke(ctx, v1.ToolCall{Arguments: `{"target":"x"}`}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call error = %v, want context.Canceled", err)
	}
}

func TestAutoReviewBlockingVerdict(t *testing.T) {
	reviewer := &stubReviewer{result: v1.ReviewResult{Verdict: "FAIL", Summary: "broken"}}
	result := invokeReviewTool(t, review.NewAutoReviewTool(reviewer), `{"target":"src/app.go"}`)
	if result.IsError {
		t.Fatalf("auto_review = %q, want IsError=false for a blocking verdict", result.Content)
	}
	requireContains(t, result.Content,
		"auto_review: fail",
		"severity_threshold: warning",
		"blocking: yes",
		"recommendation: BLOCK",
		`blocking_reason: verdict "fail" requires changes`,
		"summary: broken",
		"findings: none")
	request := reviewer.lastRequest(t)
	if request.Target != "src/app.go" || request.Kind != "code" {
		t.Fatalf("request = %+v, want code review of the target", request)
	}
}

func TestAutoReviewSeverityThreshold(t *testing.T) {
	cases := []struct {
		name      string
		result    v1.ReviewResult
		arguments string
		blocking  bool
	}{
		{
			name:      "error finding blocks by default",
			result:    v1.ReviewResult{Verdict: "pass", Findings: []v1.ReviewFinding{{Severity: "error", Message: "boom"}}},
			arguments: `{"target":"x"}`,
			blocking:  true,
		},
		{
			name:      "warning finding blocks at warning",
			result:    v1.ReviewResult{Verdict: "pass", Findings: []v1.ReviewFinding{{Severity: "warning", Message: "meh"}}},
			arguments: `{"target":"x"}`,
			blocking:  true,
		},
		{
			name:      "warning finding does not block at error threshold",
			result:    v1.ReviewResult{Verdict: "pass", Findings: []v1.ReviewFinding{{Severity: "warning", Message: "meh"}}},
			arguments: `{"target":"x","severity_threshold":"error"}`,
			blocking:  false,
		},
		{
			name:      "info finding does not block",
			result:    v1.ReviewResult{Verdict: "pass", Findings: []v1.ReviewFinding{{Severity: "info", Message: "note"}}},
			arguments: `{"target":"x"}`,
			blocking:  false,
		},
		{
			name:      "blocked verdict blocks",
			result:    v1.ReviewResult{Verdict: "blocked"},
			arguments: `{"target":"x"}`,
			blocking:  true,
		},
		{
			name:      "clean pass does not block",
			result:    v1.ReviewResult{Verdict: "warn", Summary: "ok"},
			arguments: `{"target":"x"}`,
			blocking:  false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := invokeReviewTool(t, review.NewAutoReviewTool(&stubReviewer{result: testCase.result}), testCase.arguments)
			if result.IsError {
				t.Fatalf("auto_review = %q", result.Content)
			}
			want := "blocking: no"
			if testCase.blocking {
				want = "blocking: yes"
			}
			if !strings.Contains(result.Content, want) {
				t.Fatalf("auto_review = %q, want %q", result.Content, want)
			}
		})
	}
}

func TestAutoReviewValidationAndErrors(t *testing.T) {
	tool := review.NewAutoReviewTool(&stubReviewer{})
	if text := invokeReviewToolError(t, tool, `{}`); !strings.Contains(text, "target or prompt") {
		t.Fatalf("empty call = %q", text)
	}
	if text := invokeReviewToolError(t, tool, `{"target":"x","severity_threshold":"critical"}`); !strings.Contains(text, "invalid severity_threshold") {
		t.Fatalf("invalid threshold = %q", text)
	}
	if text := invokeReviewToolError(t, review.NewAutoReviewTool(nil), `{"target":"x"}`); !strings.Contains(text, "no v1.Reviewer") {
		t.Fatalf("nil reviewer = %q", text)
	}
	failing := &stubReviewer{err: errors.New("model unavailable")}
	if text := invokeReviewToolError(t, review.NewAutoReviewTool(failing), `{"target":"x"}`); !strings.Contains(text, "review failed") {
		t.Fatalf("reviewer error = %q", text)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Invoke(ctx, v1.ToolCall{Arguments: `{"target":"x"}`}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call error = %v, want context.Canceled", err)
	}
}

func TestFormattingBoundsFindingsAndSummary(t *testing.T) {
	findings := make([]v1.ReviewFinding, review.MaxFindingsDisplay+25)
	for index := range findings {
		findings[index] = v1.ReviewFinding{Severity: "warning", File: "f.go", Line: index + 1, Message: "issue"}
	}
	longSummary := strings.Repeat("s", review.MaxSummaryChars+500)
	result := invokeReviewTool(t, review.NewCodeReviewTool(&stubReviewer{result: v1.ReviewResult{
		Verdict:  "warn",
		Summary:  longSummary,
		Findings: findings,
	}}), `{"target":"x"}`)
	if result.IsError {
		t.Fatalf("code_review = %q", result.Content)
	}
	requireContains(t, result.Content,
		"- [truncated] 25 more finding(s) not shown",
		"…")
}
