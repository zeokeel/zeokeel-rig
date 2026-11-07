package gate_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
)

// recordingGate 是记录调用并可返回固定决定/错误的 rig.Gate 测试替身。
type recordingGate struct {
	mu       sync.Mutex
	calls    int
	decision rig.Decision
	err      error
}

func (g *recordingGate) Approve(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return g.decision, g.err
}

func (g *recordingGate) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// observerRecorder 收集 ReviewOutcome（并发安全）。
type observerRecorder struct {
	mu       sync.Mutex
	outcomes []gate.ReviewOutcome
}

func (r *observerRecorder) observe(outcome gate.ReviewOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes = append(r.outcomes, outcome)
}

func (r *observerRecorder) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.outcomes)
}

func (r *observerRecorder) last() gate.ReviewOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcomes[len(r.outcomes)-1]
}

func TestReviewAllowAndDeny(t *testing.T) {
	var requests []gate.ToolReviewRequest
	observer := &observerRecorder{}
	reviewer := gate.ToolReviewerFunc(func(_ context.Context, request gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
		requests = append(requests, request)
		if request.Tool == "safe" {
			return gate.ToolReviewDecision{Allow: true}, nil
		}
		return gate.ToolReviewDecision{Allow: false, Reason: "  touches production  "}, nil
	})
	g := gate.Review(reviewer,
		gate.WithReviewEnvironment(" /workspace "),
		gate.WithReviewUserRequest(" clean up the repo "),
		gate.WithReviewObserver(observer.observe),
	)

	decision, err := approve(t, g, stubTool{name: "safe"}, rig.ToolCall{Name: "safe", Arguments: `{"path":"a"}`})
	if err != nil || decision != rig.DecisionAllow {
		t.Fatalf("allow = (%q, %v), want (allow, nil)", decision, err)
	}
	decision, err = approve(t, g, stubTool{name: "deploy"}, rig.ToolCall{Name: "deploy", Arguments: `{"env":"prod"}`})
	if err != nil {
		t.Fatalf("deny returned a Go error (should be tool-error semantics): %v", err)
	}
	if decision != rig.DecisionDeny {
		t.Fatalf("deny decision = %q, want %q", decision, rig.DecisionDeny)
	}

	if len(requests) != 2 {
		t.Fatalf("reviewer called %d times, want 2", len(requests))
	}
	first := requests[0]
	if first.Tool != "safe" || string(first.Args) != `{"path":"a"}` {
		t.Fatalf("request = %+v, want tool=safe args={\"path\":\"a\"}", first)
	}
	if first.Environment != "/workspace" || first.UserRequest != "clean up the repo" {
		t.Fatalf("request environment/user = (%q, %q)", first.Environment, first.UserRequest)
	}
	last := observer.last()
	if last.Decision != rig.DecisionDeny || last.Review.Reason != "touches production" {
		t.Fatalf("observed outcome = %+v, want deny with trimmed reason", last)
	}
	if last.Fallback || last.Err != nil {
		t.Fatalf("observed outcome fallback/err = (%v, %v), want (false, nil)", last.Fallback, last.Err)
	}
}

func TestReviewDefaultsArgumentsToEmptyObject(t *testing.T) {
	var got gate.ToolReviewRequest
	g := gate.Review(gate.ToolReviewerFunc(func(_ context.Context, request gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
		got = request
		return gate.ToolReviewDecision{Allow: true}, nil
	}))
	if _, err := approve(t, g, stubTool{name: "read_file"}, rig.ToolCall{Name: "read_file"}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if string(got.Args) != "{}" {
		t.Fatalf("Args = %q, want {}", got.Args)
	}
	if !json.Valid(got.Args) {
		t.Fatalf("Args is not valid JSON: %q", got.Args)
	}
}

func TestReviewAllowlistSkipsReviewer(t *testing.T) {
	var reviewed int32
	reviewer := gate.ToolReviewerFunc(func(_ context.Context, request gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
		atomic.AddInt32(&reviewed, 1)
		return gate.ToolReviewDecision{Allow: false, Reason: "would deny"}, nil
	})
	g := gate.Review(reviewer, gate.WithReviewAllowlist(" read_file ", "", "list_dir"))
	decision, err := approve(t, g, stubTool{name: "read_file"}, rig.ToolCall{Name: "read_file"})
	if err != nil || decision != rig.DecisionAllow {
		t.Fatalf("allow-listed = (%q, %v), want (allow, nil)", decision, err)
	}
	if got := atomic.LoadInt32(&reviewed); got != 0 {
		t.Fatalf("reviewer called %d times for an allow-listed tool, want 0", got)
	}
	decision, err = approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash"})
	if err != nil || decision != rig.DecisionDeny {
		t.Fatalf("non-listed = (%q, %v), want (deny, nil)", decision, err)
	}
	if got := atomic.LoadInt32(&reviewed); got != 1 {
		t.Fatalf("reviewer called %d times, want 1", got)
	}
}

func TestReviewTimeoutFallsBackToBaseGate(t *testing.T) {
	observer := &observerRecorder{}
	reviewer := gate.ToolReviewerFunc(func(ctx context.Context, _ gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
		<-ctx.Done()
		return gate.ToolReviewDecision{}, ctx.Err()
	})
	fallback := &recordingGate{decision: rig.DecisionAllow}
	g := gate.Review(reviewer,
		gate.WithReviewTimeout(20*time.Millisecond),
		gate.WithReviewFallback(fallback),
		gate.WithReviewObserver(observer.observe),
	)
	decision, err := approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{}`})
	if err != nil || decision != rig.DecisionAllow {
		t.Fatalf("timeout fallback = (%q, %v), want (allow, nil)", decision, err)
	}
	if fallback.callCount() != 1 {
		t.Fatalf("fallback called %d times, want 1", fallback.callCount())
	}
	outcome := observer.last()
	if !outcome.Fallback || outcome.Decision != rig.DecisionAllow {
		t.Fatalf("outcome = %+v, want fallback allow", outcome)
	}
	if !errors.Is(outcome.Err, context.DeadlineExceeded) {
		t.Fatalf("outcome.Err = %v, want context.DeadlineExceeded", outcome.Err)
	}
}

func TestReviewErrorWithoutFallbackDenies(t *testing.T) {
	observer := &observerRecorder{}
	reviewErr := errors.New("review model offline")
	reviewer := gate.ToolReviewerFunc(func(context.Context, gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
		return gate.ToolReviewDecision{}, reviewErr
	})
	g := gate.Review(reviewer, gate.WithReviewObserver(observer.observe))
	decision, err := approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash"})
	if err != nil {
		t.Fatalf("review failure returned a Go error (should fail closed as deny): %v", err)
	}
	if decision != rig.DecisionDeny {
		t.Fatalf("decision = %q, want deny", decision)
	}
	outcome := observer.last()
	if !errors.Is(outcome.Err, reviewErr) || outcome.Fallback {
		t.Fatalf("outcome = %+v, want the review error and no fallback", outcome)
	}
}

func TestReviewTimeoutFallsBackToAskGate(t *testing.T) {
	// 审查超时后回退到人工审批（AskGate）：用户 allow -> 放行。
	asker := &fakeAsker{fn: func(context.Context, string, []string) (string, error) { return "allow", nil }}
	humanGate := gate.Ask(asker)
	reviewer := gate.ToolReviewerFunc(func(ctx context.Context, _ gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
		<-ctx.Done()
		return gate.ToolReviewDecision{}, ctx.Err()
	})
	g := gate.Review(reviewer, gate.WithReviewTimeout(20*time.Millisecond), gate.WithReviewFallback(humanGate))
	decision, err := approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{"command":"id"}`})
	if err != nil || decision != rig.DecisionAllow {
		t.Fatalf("review->ask fallback = (%q, %v), want (allow, nil)", decision, err)
	}
	if asker.count() != 1 {
		t.Fatalf("Asker called %d times, want 1", asker.count())
	}
}

func TestReviewFallbackErrorPropagates(t *testing.T) {
	fallbackErr := errors.New("approval UI unavailable")
	reviewer := gate.ToolReviewerFunc(func(context.Context, gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
		return gate.ToolReviewDecision{}, errors.New("review timeout")
	})
	fallback := &recordingGate{err: fallbackErr}
	g := gate.Review(reviewer, gate.WithReviewTimeout(10*time.Millisecond), gate.WithReviewFallback(fallback))
	_, err := approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash"})
	if !errors.Is(err, fallbackErr) {
		t.Fatalf("err = %v, want fallback error", err)
	}
}

func TestReviewCallerCancellationEndsReview(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	reviewer := gate.ToolReviewerFunc(func(ctx context.Context, _ gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
		close(started)
		<-ctx.Done()
		<-release // 模拟不响应 ctx 的慢审查者，验证调用方取消能立即返回
		return gate.ToolReviewDecision{}, ctx.Err()
	})
	fallback := &recordingGate{decision: rig.DecisionAllow}
	g := gate.Review(reviewer, gate.WithReviewFallback(fallback))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := g.Approve(ctx, stubTool{name: "bash"}, rig.ToolCall{Name: "bash"})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not return after cancellation")
	}
	close(release)
	// 调用方取消不应回退到人工审批。
	if fallback.callCount() != 0 {
		t.Fatalf("fallback called %d times after cancellation, want 0", fallback.callCount())
	}
}

func TestReviewRejectsInvalidArgumentsThroughFallback(t *testing.T) {
	var reviewed int32
	reviewer := gate.ToolReviewerFunc(func(context.Context, gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
		atomic.AddInt32(&reviewed, 1)
		return gate.ToolReviewDecision{Allow: true}, nil
	})
	fallback := &recordingGate{decision: rig.DecisionAllow}
	g := gate.Review(reviewer, gate.WithReviewFallback(fallback))
	decision, err := approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{"command":`})
	if err != nil || decision != rig.DecisionAllow {
		t.Fatalf("invalid args fallback = (%q, %v), want (allow, nil)", decision, err)
	}
	if atomic.LoadInt32(&reviewed) != 0 {
		t.Fatal("reviewer must not be called with invalid JSON arguments")
	}
	if fallback.callCount() != 1 {
		t.Fatalf("fallback called %d times, want 1", fallback.callCount())
	}
}

func TestReviewContextFuncOverridesStaticContext(t *testing.T) {
	var got gate.ToolReviewRequest
	g := gate.Review(
		gate.ToolReviewerFunc(func(_ context.Context, request gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
			got = request
			return gate.ToolReviewDecision{Allow: true}, nil
		}),
		gate.WithReviewEnvironment("static-root"),
		gate.WithReviewUserRequest("static-request"),
		gate.WithReviewContext(func(_ context.Context, tool rig.Tool, call rig.ToolCall) (string, string) {
			return " /dynamic-root ", " dynamic-request "
		}),
	)
	if _, err := approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash"}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got.Environment != "/dynamic-root" || got.UserRequest != "dynamic-request" {
		t.Fatalf("request = %+v, want dynamic context", got)
	}
}

func TestReviewNilReviewerAndNilGate(t *testing.T) {
	g := gate.Review(nil)
	if _, err := approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash"}); err == nil {
		t.Fatal("Review(nil).Approve returned nil error, want a configuration error")
	}
	var nilGate *gate.ReviewGate
	if _, err := nilGate.Approve(context.Background(), stubTool{name: "bash"}, rig.ToolCall{Name: "bash"}); err == nil {
		t.Fatal("nil *ReviewGate.Approve returned nil error")
	}
}

func TestToolReviewerFuncNil(t *testing.T) {
	var reviewer gate.ToolReviewerFunc
	if _, err := reviewer.ReviewTool(context.Background(), gate.ToolReviewRequest{Tool: "bash"}); err == nil {
		t.Fatal("nil ToolReviewerFunc returned nil error")
	}
}

func TestReviewConcurrentCalls(t *testing.T) {
	var reviews int32
	observer := &observerRecorder{}
	reviewer := gate.ToolReviewerFunc(func(_ context.Context, request gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
		atomic.AddInt32(&reviews, 1)
		if request.Tool == "danger" {
			return gate.ToolReviewDecision{Allow: false, Reason: "no"}, nil
		}
		return gate.ToolReviewDecision{Allow: true}, nil
	})
	g := gate.Review(reviewer, gate.WithReviewObserver(observer.observe))
	const workers = 64
	var wg sync.WaitGroup
	var allowed, denied int32
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		name := "safe"
		if i%2 == 0 {
			name = "danger"
		}
		go func(toolName string) {
			defer wg.Done()
			tool := stubTool{name: toolName}
			decision, err := g.Approve(context.Background(), tool, rig.ToolCall{Name: toolName, Arguments: `{"i":1}`})
			if err != nil {
				t.Errorf("Approve(%q): %v", toolName, err)
				return
			}
			switch decision {
			case rig.DecisionAllow:
				atomic.AddInt32(&allowed, 1)
			case rig.DecisionDeny:
				atomic.AddInt32(&denied, 1)
			default:
				t.Errorf("unexpected decision %q", decision)
			}
		}(name)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&reviews); got != workers {
		t.Fatalf("reviewer called %d times, want %d", got, workers)
	}
	if allowed != workers/2 || denied != workers/2 {
		t.Fatalf("allowed/denied = %d/%d, want %d/%d", allowed, denied, workers/2, workers/2)
	}
	if observer.len() != workers {
		t.Fatalf("observer saw %d outcomes, want %d", observer.len(), workers)
	}
}
