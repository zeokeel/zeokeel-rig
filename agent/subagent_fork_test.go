package agent_test

// Tests for forked subagents: the child copies the configured parent session's
// completed history, forks are independent, and misconfiguration is readable.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/agent"
	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/session"
)

// seedHistory appends messages to one session.
func seedHistory(t *testing.T, history *session.Memory, sessionID string, messages ...v1.Message) {
	t.Helper()
	if err := history.Append(context.Background(), sessionID, messages...); err != nil {
		t.Fatalf("seed history: %v", err)
	}
}

func parentMessageTexts(messages []v1.Message) []string {
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		if text := message.Text(); text != "" {
			texts = append(texts, text)
		}
	}
	return texts
}

func TestRunnerForkSeedsChildHistory(t *testing.T) {
	history := session.NewMemory()
	seedHistory(t, history, "parent-session",
		v1.NewMessage(v1.RoleUser, "parent question"),
		v1.NewMessage(v1.RoleAssistant, "parent answer"))
	provider := &scriptProvider{scripts: [][]v1.Chunk{textScript("child answer")}}
	parent := v1.AgentSpec{Model: "m", Provider: provider, History: history}
	options := agent.DefaultRunnerOptions()
	options.ParentSessionID = "parent-session"
	runner, err := agent.NewRunner(parent, options)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	sub, err := runner.Start(context.Background(), v1.SubagentSpec{
		Name:   "forked",
		Prompt: "child follow-up",
		Fork:   true,
	})
	if err != nil {
		t.Fatalf("Start(fork): %v", err)
	}
	defer func() { _ = sub.Stop() }()
	result := waitSubagent(t, sub)
	if result.Output != "child answer" {
		t.Fatalf("output = %q", result.Output)
	}

	messages := provider.request(0).Messages
	texts := parentMessageTexts(messages)
	want := []string{"parent question", "parent answer", "child follow-up"}
	if len(texts) != len(want) {
		t.Fatalf("child messages = %v, want %v", texts, want)
	}
	for index := range want {
		if texts[index] != want[index] {
			t.Fatalf("child messages = %v, want %v (parent messages first, then the prompt)", texts, want)
		}
	}

	// The parent session is untouched by the fork.
	stored, err := history.Load(context.Background(), "parent-session")
	if err != nil {
		t.Fatalf("load parent history: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("parent history length = %d, want 2 (fork must not append)", len(stored))
	}
}

func TestRunnerForkConfigurationErrors(t *testing.T) {
	history := session.NewMemory()
	seedHistory(t, history, "parent-session", v1.NewMessage(v1.RoleUser, "question"))
	provider := &scriptProvider{scripts: [][]v1.Chunk{textScript("ok")}}

	// No ParentSessionID configured.
	noSession, err := agent.NewRunner(v1.AgentSpec{Model: "m", Provider: provider, History: history}, agent.DefaultRunnerOptions())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if _, err := noSession.Start(context.Background(), v1.SubagentSpec{Prompt: "x", Fork: true}); err == nil ||
		!strings.Contains(err.Error(), "ParentSessionID") {
		t.Fatalf("fork without session id error = %v", err)
	}

	// No history configured anywhere.
	options := agent.DefaultRunnerOptions()
	options.ParentSessionID = "parent-session"
	noHistory, err := agent.NewRunner(v1.AgentSpec{Model: "m", Provider: provider}, options)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if _, err := noHistory.Start(context.Background(), v1.SubagentSpec{Prompt: "x", Fork: true}); err == nil ||
		!strings.Contains(err.Error(), "parent history") {
		t.Fatalf("fork without history error = %v", err)
	}

	// A history Load failure is wrapped and readable.
	options.ParentHistory = v1.History(failingHistory{err: errors.New("disk exploded")})
	failing, err := agent.NewRunner(v1.AgentSpec{Model: "m", Provider: provider}, options)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if _, err := failing.Start(context.Background(), v1.SubagentSpec{Prompt: "x", Fork: true}); err == nil ||
		!strings.Contains(err.Error(), "disk exploded") {
		t.Fatalf("fork load error = %v", err)
	}
}

// failingHistory is a v1.History whose Load always fails.
type failingHistory struct{ err error }

func (h failingHistory) Load(context.Context, string) ([]v1.Message, error)  { return nil, h.err }
func (h failingHistory) Append(context.Context, string, ...v1.Message) error { return h.err }

func TestRunnerForkConcurrentIsolated(t *testing.T) {
	history := session.NewMemory()
	seedHistory(t, history, "parent-session",
		v1.NewMessage(v1.RoleUser, "parent question"),
		v1.NewMessage(v1.RoleAssistant, "parent answer"))
	provider := &scriptProvider{repeatLast: true, scripts: [][]v1.Chunk{textScript("fork answer")}}
	parent := v1.AgentSpec{Model: "m", Provider: provider, History: history}
	options := agent.DefaultRunnerOptions()
	options.ParentSessionID = "parent-session"
	runner, err := agent.NewRunner(parent, options)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	prompts := []string{"first fork", "second fork"}
	subs := make([]v1.Subagent, len(prompts))
	var waitGroup sync.WaitGroup
	for index, prompt := range prompts {
		index, prompt := index, prompt
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			sub, startErr := runner.Start(context.Background(), v1.SubagentSpec{Prompt: prompt, Fork: true})
			if startErr != nil {
				t.Errorf("Start(%s): %v", prompt, startErr)
				return
			}
			subs[index] = sub
		}()
	}
	waitGroup.Wait()
	for _, sub := range subs {
		if sub == nil {
			t.Fatal("a fork failed to start")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, sub := range subs {
		if _, err := sub.Wait(ctx); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		_ = sub.Stop()
	}

	// Every fork request carries the parent messages exactly once and exactly one
	// of the prompts: each fork sees only its own prompt.
	if provider.callCount() != len(prompts) {
		t.Fatalf("provider calls = %d, want %d", provider.callCount(), len(prompts))
	}
	seenPrompts := make(map[string]int, len(prompts))
	for requestIndex := 0; requestIndex < provider.callCount(); requestIndex++ {
		request := provider.request(requestIndex)
		texts := parentMessageTexts(request.Messages)
		parentCount := 0
		matchedPrompt := ""
		for _, text := range texts {
			if text == "parent question" {
				parentCount++
			}
			for _, prompt := range prompts {
				if text == prompt {
					if matchedPrompt != "" && matchedPrompt != prompt {
						t.Fatalf("request %d contains two fork prompts: %v", requestIndex, texts)
					}
					matchedPrompt = prompt
				}
			}
		}
		if parentCount != 1 {
			t.Fatalf("request %d has %d parent messages, want 1: %v", requestIndex, parentCount, texts)
		}
		if matchedPrompt == "" {
			t.Fatalf("request %d contains no fork prompt: %v", requestIndex, texts)
		}
		seenPrompts[matchedPrompt]++
	}
	for _, prompt := range prompts {
		if seenPrompts[prompt] != 1 {
			t.Fatalf("prompt %q appeared %d times, want 1", prompt, seenPrompts[prompt])
		}
	}
}

func TestRunnerForkAndStructuredOutput(t *testing.T) {
	history := session.NewMemory()
	seedHistory(t, history, "parent-session",
		v1.NewMessage(v1.RoleUser, "parent question"),
		v1.NewMessage(v1.RoleAssistant, "parent answer"))
	provider := &scriptProvider{scripts: [][]v1.Chunk{
		toolCallScript("call-1", agent.StructuredOutputToolName, `{"answer":"42"}`),
		textScript("done"),
	}}
	// The parent must expose the schema-shaped tool list check: the runner adds
	// structured_output to the child only.
	parent := v1.AgentSpec{Model: "m", Provider: provider, History: history}
	options := agent.DefaultRunnerOptions()
	options.ParentSessionID = "parent-session"
	runner, err := agent.NewRunner(parent, options)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"answer": map[string]any{"type": "string"}},
		"required":   []string{"answer"},
	}
	sub, err := runner.Start(context.Background(), v1.SubagentSpec{
		Name:         "forked-structured",
		Prompt:       "report",
		Fork:         true,
		OutputSchema: schema,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sub.Stop() }()
	result := waitSubagent(t, sub)
	if result.Structured["answer"] != "42" {
		t.Fatalf("structured = %+v, want answer=42", result.Structured)
	}

	request := provider.request(0)
	toolNames := make([]string, 0, len(request.Tools))
	for _, spec := range request.Tools {
		toolNames = append(toolNames, spec.Name)
	}
	if !containsString(toolNames, agent.StructuredOutputToolName) {
		t.Fatalf("child tools = %v, want %s", toolNames, agent.StructuredOutputToolName)
	}
	if len(request.Messages) == 0 || !strings.Contains(request.Messages[0].Text(), agent.StructuredOutputToolName) {
		t.Fatalf("system prompt does not mention structured_output: %+v", request.Messages)
	}
	texts := parentMessageTexts(request.Messages)
	if !containsString(texts, "parent question") || !containsString(texts, "parent answer") || !containsString(texts, "report") {
		t.Fatalf("fork+structured request messages = %v", texts)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
