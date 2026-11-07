package gate_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
)

// askCall 是一次 Asker.Ask 的记录。
type askCall struct {
	question string
	options  []string
}

// fakeAsker 是并发安全的 rig.Asker 测试替身。
type fakeAsker struct {
	mu    sync.Mutex
	calls []askCall
	fn    func(ctx context.Context, question string, options []string) (string, error)
}

func (f *fakeAsker) Ask(ctx context.Context, question string, options []string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, askCall{question: question, options: append([]string(nil), options...)})
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(ctx, question, options)
	}
	return "", nil
}

func (f *fakeAsker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeAsker) call(index int) askCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[index]
}

func TestAskAllowDenyAndAllowAlways(t *testing.T) {
	answers := []string{"allow", "deny", "allow-always"}
	asker := &fakeAsker{fn: func(context.Context, string, []string) (string, error) {
		answer := answers[0]
		answers = answers[1:]
		return answer, nil
	}}
	g := gate.Ask(asker)

	decision, err := approve(t, g, stubTool{name: "read_file"}, rig.ToolCall{Name: "read_file", Arguments: `{"path":"a.go"}`})
	if err != nil || decision != rig.DecisionAllow {
		t.Fatalf("allow = (%q, %v), want (allow, nil)", decision, err)
	}
	decision, err = approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{"command":"id"}`})
	if err != nil || decision != rig.DecisionDeny {
		t.Fatalf("deny = (%q, %v), want (deny, nil)", decision, err)
	}
	decision, err = approve(t, g, stubTool{name: "write_file"}, rig.ToolCall{Name: "write_file", Arguments: `{"path":"x"}`})
	if err != nil || decision != rig.DecisionAllowAlways {
		t.Fatalf("allow-always = (%q, %v), want (allow-always, nil)", decision, err)
	}
	if !g.IsAllowAlways("write_file") {
		t.Fatal("IsAllowAlways(write_file) = false, want true")
	}

	// allow-always 后再调用同一工具：不再提问，直接放行。
	decision, err = approve(t, g, stubTool{name: "write_file"}, rig.ToolCall{Name: "write_file", Arguments: `{"path":"y"}`})
	if err != nil || decision != rig.DecisionAllow {
		t.Fatalf("allow-always replay = (%q, %v), want (allow, nil)", decision, err)
	}
	if asker.count() != 3 {
		t.Fatalf("Asker called %d times, want 3 (allow-always must skip later prompts)", asker.count())
	}

	first := asker.call(0)
	if !slices.Contains(first.options, gate.AskAllow) || !slices.Contains(first.options, gate.AskAllowAlways) || !slices.Contains(first.options, gate.AskDeny) {
		t.Fatalf("options = %v, want all three canonical choices", first.options)
	}
	if !strings.Contains(first.question, `"read_file"`) || !strings.Contains(first.question, `"path":"a.go"`) {
		t.Fatalf("question = %q, want tool name and arguments", first.question)
	}
}

func TestAskOptionsArePerCallCopies(t *testing.T) {
	asker := &fakeAsker{fn: func(_ context.Context, _ string, options []string) (string, error) {
		options[0] = "tampered"
		return "allow", nil
	}}
	g := gate.Ask(asker)
	for i := 0; i < 2; i++ {
		if _, err := approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash"}); err != nil {
			t.Fatalf("Approve: %v", err)
		}
	}
	second := asker.call(1)
	if !slices.Contains(second.options, gate.AskAllow) {
		t.Fatalf("options for the second call = %v, want a fresh canonical copy", second.options)
	}
}

func TestAskTimeoutWrapsErrApprovalExpired(t *testing.T) {
	release := make(chan struct{})
	asker := &fakeAsker{fn: func(context.Context, string, []string) (string, error) {
		<-release // 模拟不响应 context 的慢 Asker
		return "allow", nil
	}}
	g := gate.Ask(asker, gate.WithAskTimeout(20*time.Millisecond))
	start := time.Now()
	_, err := g.Approve(context.Background(), stubTool{name: "bash"}, rig.ToolCall{Name: "bash"})
	elapsed := time.Since(start)
	close(release)
	if err == nil {
		t.Fatal("Approve returned nil error, want ErrApprovalExpired")
	}
	if !errors.Is(err, gate.ErrApprovalExpired) {
		t.Fatalf("err = %v, want ErrApprovalExpired", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout took %s, want about 20ms", elapsed)
	}
}

func TestAskCallerCancellationWrapsErrApprovalExpired(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	asker := &fakeAsker{fn: func(askCtx context.Context, _ string, _ []string) (string, error) {
		<-askCtx.Done()
		return "", askCtx.Err()
	}}
	g := gate.Ask(asker)
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := g.Approve(ctx, stubTool{name: "bash"}, rig.ToolCall{Name: "bash"})
	if !errors.Is(err, gate.ErrApprovalExpired) {
		t.Fatalf("err = %v, want ErrApprovalExpired", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestAskCancelledBeforePrompt(t *testing.T) {
	asker := &fakeAsker{}
	g := gate.Ask(asker)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Approve(ctx, stubTool{name: "bash"}, rig.ToolCall{Name: "bash"}); !errors.Is(err, gate.ErrApprovalExpired) {
		t.Fatalf("err = %v, want ErrApprovalExpired", err)
	}
	if asker.count() != 0 {
		t.Fatalf("Asker called %d times for an already-cancelled context, want 0", asker.count())
	}
}

func TestAskUnrecognizedAnswerDenies(t *testing.T) {
	asker := &fakeAsker{fn: func(context.Context, string, []string) (string, error) {
		return "maybe later", nil
	}}
	g := gate.Ask(asker)
	decision, err := approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash"})
	if err != nil || decision != rig.DecisionDeny {
		t.Fatalf("unknown answer = (%q, %v), want (deny, nil)", decision, err)
	}
}

func TestAskAnswerAliases(t *testing.T) {
	cases := []struct {
		answer string
		want   rig.Decision
	}{
		{"  ALLOW  ", rig.DecisionAllow},
		{"yes", rig.DecisionAllow},
		{"once", rig.DecisionAllow},
		{"Allow-Always", rig.DecisionAllowAlways},
		{"ALWAYS", rig.DecisionAllowAlways},
		{"deny", rig.DecisionDeny},
		{"no", rig.DecisionDeny},
		{"", rig.DecisionDeny},
	}
	for _, testCase := range cases {
		answer := testCase.answer
		asker := &fakeAsker{fn: func(context.Context, string, []string) (string, error) { return answer, nil }}
		g := gate.Ask(asker)
		decision, err := approve(t, g, stubTool{name: "tool"}, rig.ToolCall{Name: "tool"})
		if err != nil || decision != testCase.want {
			t.Fatalf("answer %q = (%q, %v), want (%q, nil)", answer, decision, err, testCase.want)
		}
	}
}

func TestAskNilAskerAndTypedNil(t *testing.T) {
	if _, err := gate.Ask(nil).Approve(context.Background(), stubTool{name: "bash"}, rig.ToolCall{Name: "bash"}); err == nil {
		t.Fatal("Ask(nil).Approve returned nil error")
	}
	var typedNil *fakeAsker
	if _, err := gate.Ask(typedNil).Approve(context.Background(), stubTool{name: "bash"}, rig.ToolCall{Name: "bash"}); err == nil {
		t.Fatal("Ask((*fakeAsker)(nil)).Approve returned nil error, want typed-nil detection")
	}
	var nilGate *gate.AskGate
	if _, err := nilGate.Approve(context.Background(), stubTool{name: "bash"}, rig.ToolCall{Name: "bash"}); err == nil {
		t.Fatal("nil *AskGate.Approve returned nil error")
	}
}

func TestAskAskerErrorPropagates(t *testing.T) {
	askErr := errors.New("approval UI offline")
	asker := &fakeAsker{fn: func(context.Context, string, []string) (string, error) {
		return "", askErr
	}}
	g := gate.Ask(asker)
	_, err := approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash"})
	if !errors.Is(err, askErr) {
		t.Fatalf("err = %v, want the Asker error", err)
	}
	if errors.Is(err, gate.ErrApprovalExpired) {
		t.Fatalf("err = %v, must not be classified as ErrApprovalExpired", err)
	}
}

func TestAskAllowAlwaysRules(t *testing.T) {
	asker := &fakeAsker{fn: func(context.Context, string, []string) (string, error) { return "allow", nil }}
	g := gate.Ask(asker)
	g.AllowAlways(" bash ")
	g.AllowAlways("write_file")
	g.AllowAlways("")
	if !g.IsAllowAlways("bash") || !g.IsAllowAlways(" write_file ") {
		t.Fatalf("rules = %v, want bash and write_file", g.AllowAlwaysTools())
	}
	if got := g.AllowAlwaysTools(); !slices.Equal(got, []string{"bash", "write_file"}) {
		t.Fatalf("AllowAlwaysTools = %v, want sorted [bash write_file]", got)
	}
	if _, err := approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash"}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if asker.count() != 0 {
		t.Fatalf("Asker called %d times for allow-always tool, want 0", asker.count())
	}
	g.ClearAllowAlways()
	if g.AllowAlwaysTools() != nil {
		t.Fatalf("AllowAlwaysTools after clear = %v, want nil", g.AllowAlwaysTools())
	}
	if _, err := approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash"}); err != nil {
		t.Fatalf("Approve after clear: %v", err)
	}
	if asker.count() != 1 {
		t.Fatalf("Asker called %d times after clearing rules, want 1", asker.count())
	}
}

func TestAskCustomPrompt(t *testing.T) {
	var sawTool string
	asker := &fakeAsker{fn: func(context.Context, string, []string) (string, error) { return "deny", nil }}
	g := gate.Ask(asker, gate.WithAskPrompt(func(tool rig.Tool, call rig.ToolCall) string {
		sawTool = tool.Name()
		return "approve " + call.Name + "?"
	}))
	if _, err := approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash"}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if sawTool != "bash" {
		t.Fatalf("prompt saw tool %q, want bash", sawTool)
	}
	if got := asker.call(0).question; got != "approve bash?" {
		t.Fatalf("question = %q, want custom prompt", got)
	}
	// 自定义 prompt 返回空字符串时回退到默认文案。
	g2 := gate.Ask(asker, gate.WithAskPrompt(func(rig.Tool, rig.ToolCall) string { return "   " }))
	if _, err := approve(t, g2, stubTool{name: "bash"}, rig.ToolCall{Name: "bash"}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got := asker.call(1).question; !strings.Contains(got, `"bash"`) {
		t.Fatalf("fallback question = %q, want default prompt with tool name", got)
	}
}

func TestAskDefaultQuestionTruncatesHugeArguments(t *testing.T) {
	asker := &fakeAsker{fn: func(context.Context, string, []string) (string, error) { return "deny", nil }}
	g := gate.Ask(asker)
	args := `{"x":"` + strings.Repeat("a", 4000+100) + `"}`
	if _, err := approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: args}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	question := asker.call(0).question
	if len([]rune(question)) > 5000 {
		t.Fatalf("question is %d runes, want the arguments truncated", len([]rune(question)))
	}
}

func TestAskConcurrentCalls(t *testing.T) {
	asker := &fakeAsker{fn: func(context.Context, string, []string) (string, error) { return "allow", nil }}
	g := gate.Ask(asker)
	const workers = 64
	var wg sync.WaitGroup
	var allowed int32
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			decision, err := g.Approve(context.Background(), stubTool{name: "bash"}, rig.ToolCall{Name: "bash"})
			if err != nil {
				t.Errorf("Approve: %v", err)
				return
			}
			if decision != rig.DecisionAllow {
				t.Errorf("decision = %q, want allow", decision)
				return
			}
			atomic.AddInt32(&allowed, 1)
		}()
	}
	wg.Wait()
	if atomic.LoadInt32(&allowed) != workers {
		t.Fatalf("allowed = %d, want %d", allowed, workers)
	}
}

func TestAskConcurrentAllowAlways(t *testing.T) {
	asker := &fakeAsker{fn: func(context.Context, string, []string) (string, error) { return "allow-always", nil }}
	g := gate.Ask(asker)
	const workers = 32
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			decision, err := g.Approve(context.Background(), stubTool{name: "bash"}, rig.ToolCall{Name: "bash"})
			if err != nil {
				t.Errorf("Approve: %v", err)
				return
			}
			if decision != rig.DecisionAllow && decision != rig.DecisionAllowAlways {
				t.Errorf("decision = %q, want allow or allow-always", decision)
			}
		}()
	}
	wg.Wait()
	if !g.IsAllowAlways("bash") {
		t.Fatal("allow-always rule was not recorded")
	}
	before := asker.count()
	if _, err := g.Approve(context.Background(), stubTool{name: "bash"}, rig.ToolCall{Name: "bash"}); err != nil {
		t.Fatalf("Approve after allow-always: %v", err)
	}
	if asker.count() != before {
		t.Fatalf("Asker called after allow-always rule was set (%d -> %d)", before, asker.count())
	}
}
