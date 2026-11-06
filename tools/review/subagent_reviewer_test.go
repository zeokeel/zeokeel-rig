package review_test

// Tests for SubagentReviewer: prompt construction, plain-text parsing, error
// propagation and one end-to-end run through agent.NewRunner.

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig/agent"
	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/tools/review"
)

// scriptedSubagent is a fake v1.Subagent returning a fixed output.
type scriptedSubagent struct {
	mu        sync.Mutex
	output    string
	waitErr   error
	stopCalls int
}

func (s *scriptedSubagent) Name() string { return "reviewer" }

func (s *scriptedSubagent) Wait(ctx context.Context) (v1.SubagentResult, error) {
	if err := ctx.Err(); err != nil {
		return v1.SubagentResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waitErr != nil {
		return v1.SubagentResult{}, s.waitErr
	}
	return v1.SubagentResult{Name: "reviewer", Output: s.output}, nil
}

func (s *scriptedSubagent) Send(ctx context.Context, _ string) (v1.SubagentResult, error) {
	return s.Wait(ctx)
}

func (s *scriptedSubagent) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopCalls++
	return nil
}

func (s *scriptedSubagent) stops() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopCalls
}

// scriptedRunner records the SubagentSpec it receives.
type scriptedRunner struct {
	mu       sync.Mutex
	specs    []v1.SubagentSpec
	startErr error
	sub      *scriptedSubagent
}

func (r *scriptedRunner) Start(_ context.Context, spec v1.SubagentSpec) (v1.Subagent, error) {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	startErr := r.startErr
	sub := r.sub
	if sub == nil {
		sub = &scriptedSubagent{output: "VERDICT: pass"}
		r.sub = sub
	}
	r.mu.Unlock()
	if startErr != nil {
		return nil, startErr
	}
	return sub, nil
}

func (r *scriptedRunner) lastSpec(t *testing.T) v1.SubagentSpec {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.specs) == 0 {
		t.Fatal("runner.Start was not called")
	}
	return r.specs[len(r.specs)-1]
}

func reviewContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

func TestParseReviewOutput(t *testing.T) {
	cases := []struct {
		name         string
		text         string
		wantVerdict  string
		wantFindings int
		wantSummary  string
	}{
		{
			name:         "full report",
			text:         "VERDICT: FAIL\nSummary: two problems\nLooks mostly fine otherwise.\nsrc/a.go:12 ERROR: nil deref\n- internal/b.go:3 warn: unused variable\n",
			wantVerdict:  "fail",
			wantFindings: 2,
			wantSummary:  "two problems\nLooks mostly fine otherwise.",
		},
		{
			name:        "no markers defaults to pass",
			text:        "This change looks good.\nNo issues found.",
			wantVerdict: "pass",
			wantSummary: "This change looks good.\nNo issues found.",
		},
		{
			name:         "no-file finding",
			text:         "VERDICT: warn\n-:8 error: global problem",
			wantVerdict:  "warn",
			wantFindings: 1,
		},
		{
			name:        "summary label stripped",
			text:        "Summary: only a summary",
			wantVerdict: "pass",
			wantSummary: "only a summary",
		},
		{
			name:         "windows path finding",
			text:         "VERDICT: fail\nC:\\repo\\a.go:9 critical: boom",
			wantVerdict:  "fail",
			wantFindings: 1,
		},
		{
			name:        "non-finding colon lines stay in summary",
			text:        "VERDICT: pass\nNote: this is prose, not a finding.",
			wantVerdict: "pass",
			wantSummary: "Note: this is prose, not a finding.",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := review.ParseReviewOutput(testCase.text)
			if result.Verdict != testCase.wantVerdict {
				t.Fatalf("verdict = %q, want %q", result.Verdict, testCase.wantVerdict)
			}
			if len(result.Findings) != testCase.wantFindings {
				t.Fatalf("findings = %+v, want %d", result.Findings, testCase.wantFindings)
			}
			if testCase.wantSummary != "" {
				if got := strings.TrimSpace(result.Summary); got != testCase.wantSummary {
					t.Fatalf("summary = %q, want %q", got, testCase.wantSummary)
				}
			}
			// Spot-check parsed finding fields.
			if testCase.name == "full report" {
				first := result.Findings[0]
				if first.Severity != "error" || first.File != "src/a.go" || first.Line != 12 ||
					first.Message != "nil deref" {
					t.Fatalf("finding[0] = %+v", first)
				}
				second := result.Findings[1]
				if second.Severity != "warning" || second.File != "internal/b.go" || second.Line != 3 {
					t.Fatalf("finding[1] = %+v", second)
				}
			}
			if testCase.name == "no-file finding" {
				if result.Findings[0].File != "" || result.Findings[0].Line != 8 {
					t.Fatalf("finding = %+v, want no file and line 8", result.Findings[0])
				}
			}
			if testCase.name == "windows path finding" {
				if result.Findings[0].File != `C:\repo\a.go` || result.Findings[0].Severity != "critical" {
					t.Fatalf("finding = %+v", result.Findings[0])
				}
			}
		})
	}
}

func TestSubagentReviewerBuildsPromptAndParses(t *testing.T) {
	runner := &scriptedRunner{sub: &scriptedSubagent{output: strings.Join([]string{
		"VERDICT: fail",
		"Summary: two problems",
		"src/a.go:10 error: nil dereference",
		"- internal/b.go:3 warn: unused variable",
		"Rest of the review prose.",
	}, "\n")}}
	reviewer := review.NewSubagentReviewer(runner)
	result, err := reviewer.Review(reviewContext(t), v1.ReviewRequest{
		Kind:    "diff",
		Target:  "src/a.go",
		Prompt:  "focus on concurrency",
		Content: "func main() {}",
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if result.Verdict != "fail" || len(result.Findings) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Summary, "two problems") || !strings.Contains(result.Summary, "Rest of the review prose.") {
		t.Fatalf("summary = %q", result.Summary)
	}

	spec := runner.lastSpec(t)
	if spec.Name != review.DefaultReviewerName {
		t.Fatalf("subagent name = %q, want %q", spec.Name, review.DefaultReviewerName)
	}
	for _, want := range []string{
		review.DefaultReviewerInstruction,
		"Kind: diff",
		"Target: src/a.go",
		"Review focus: focus on concurrency",
		"Content:",
		"func main() {}",
		"VERDICT: pass|warn|fail",
		"FILE:LINE SEVERITY: message",
	} {
		if !strings.Contains(spec.Prompt, want) {
			t.Fatalf("subagent prompt does not contain %q:\n%s", want, spec.Prompt)
		}
	}
	if stops := runner.sub.stops(); stops != 1 {
		t.Fatalf("Stop calls = %d, want 1", stops)
	}
}

func TestSubagentReviewerCustomFieldsAndTruncation(t *testing.T) {
	runner := &scriptedRunner{sub: &scriptedSubagent{output: "VERDICT: pass"}}
	reviewer := review.NewSubagentReviewer(runner)
	reviewer.Name = "auditor"
	reviewer.Instruction = "CUSTOM REVIEW RULE"
	reviewer.MaxContentChars = 20
	longContent := strings.Repeat("x", 100)
	if _, err := reviewer.Review(reviewContext(t), v1.ReviewRequest{Content: longContent}); err != nil {
		t.Fatalf("Review: %v", err)
	}
	spec := runner.lastSpec(t)
	if spec.Name != "auditor" {
		t.Fatalf("name = %q, want auditor", spec.Name)
	}
	if !strings.Contains(spec.Prompt, "CUSTOM REVIEW RULE") {
		t.Fatalf("prompt does not use the custom instruction:\n%s", spec.Prompt)
	}
	if strings.Contains(spec.Prompt, longContent) {
		t.Fatal("prompt embeds the full content despite MaxContentChars")
	}
	if !strings.Contains(spec.Prompt, "[truncated: review content exceeded 20 characters]") {
		t.Fatalf("prompt has no truncation marker:\n%s", spec.Prompt)
	}
	if !strings.Contains(spec.Prompt, "Kind: code") { // default kind
		t.Fatalf("prompt does not default the kind:\n%s", spec.Prompt)
	}
}

func TestSubagentReviewerErrors(t *testing.T) {
	if _, err := review.NewSubagentReviewer(nil).Review(reviewContext(t), v1.ReviewRequest{}); err == nil ||
		!strings.Contains(err.Error(), "no subagent runner") {
		t.Fatalf("nil runner error = %v", err)
	}
	startFailure := &scriptedRunner{startErr: errors.New("depth limit reached")}
	if _, err := review.NewSubagentReviewer(startFailure).Review(reviewContext(t), v1.ReviewRequest{}); err == nil ||
		!strings.Contains(err.Error(), "depth limit reached") {
		t.Fatalf("start error = %v", err)
	}
	waitFailure := &scriptedRunner{sub: &scriptedSubagent{waitErr: errors.New("model exploded")}}
	_, err := review.NewSubagentReviewer(waitFailure).Review(reviewContext(t), v1.ReviewRequest{})
	if err == nil || !strings.Contains(err.Error(), "model exploded") {
		t.Fatalf("wait error = %v", err)
	}
	if stops := waitFailure.sub.stops(); stops != 1 {
		t.Fatalf("Stop calls after wait error = %d, want 1", stops)
	}

	// Caller cancellation stays a context error (errors.Is works through the wrap).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = review.NewSubagentReviewer(&scriptedRunner{}).Review(ctx, v1.ReviewRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled review error = %v, want context.Canceled", err)
	}
}

// --- end-to-end through the real subagent runner ---------------------------

type staticReviewProvider struct{ text string }

func (p staticReviewProvider) Name() string { return "static-review" }

func (p staticReviewProvider) Stream(context.Context, v1.Request) (v1.Stream, error) {
	return &oneShotReviewStream{text: p.text}, nil
}

type oneShotReviewStream struct {
	text string
	sent bool
}

func (s *oneShotReviewStream) Recv() (v1.Chunk, error) {
	if s.sent {
		return v1.Chunk{}, io.EOF
	}
	s.sent = true
	return v1.Chunk{Delta: s.text, FinishReason: "stop"}, nil
}

func TestSubagentReviewerEndToEnd(t *testing.T) {
	provider := staticReviewProvider{text: "VERDICT: warn\nSummary: one issue\nsrc/app.go:7 error: unchecked error\n"}
	runner, err := agent.NewRunner(v1.AgentSpec{Model: "review-model", Provider: provider}, agent.DefaultRunnerOptions())
	if err != nil {
		t.Fatalf("agent.NewRunner: %v", err)
	}
	reviewer := review.NewSubagentReviewer(runner)
	tools := review.Tools(reviewer)
	if len(tools) != 2 {
		t.Fatalf("Tools = %d", len(tools))
	}

	codeResult := invokeReviewTool(t, tools[0], `{"target":"src/app.go"}`)
	if codeResult.IsError {
		t.Fatalf("code_review = %q", codeResult.Content)
	}
	requireContains(t, codeResult.Content, "code_review: warn", "summary: one issue",
		"- [error] src/app.go:7: unchecked error")

	autoResult := invokeReviewTool(t, tools[1], `{"target":"src/app.go"}`)
	if autoResult.IsError {
		t.Fatalf("auto_review = %q", autoResult.Content)
	}
	requireContains(t, autoResult.Content, "blocking: yes", "recommendation: BLOCK",
		"1 finding(s) at or above warning")
}
