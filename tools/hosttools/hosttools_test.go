package hosttools_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/hosttools"
)

func toolMap(tools []rig.Tool) map[string]rig.Tool {
	byName := make(map[string]rig.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	return byName
}

func invoke(t *testing.T, tools map[string]rig.Tool, name, arguments string) rig.ToolResult {
	t.Helper()
	tool, ok := tools[name]
	if !ok {
		t.Fatalf("tool %q not found", name)
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "call-1", Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", name, err)
	}
	return result
}

func invokeError(t *testing.T, tools map[string]rig.Tool, name, arguments string) string {
	t.Helper()
	result := invoke(t, tools, name, arguments)
	if !result.IsError {
		t.Fatalf("%s: result = %q, want IsError", name, result.Content)
	}
	return result.Content
}

func TestToolsShape(t *testing.T) {
	tools := hosttools.Tools(hosttools.Options{})
	if len(tools) != 6 {
		t.Fatalf("hosttools.Tools returned %d tools, want 6", len(tools))
	}
	byName := toolMap(tools)
	want := []string{"ask_user", "present", "present_artifact", "plan_review", "exit_plan_mode", "goal"}
	for _, name := range want {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("missing tool %q", name)
		}
		if tool.Description() == "" {
			t.Fatalf("tool %q has no description", name)
		}
		schema := tool.Schema()
		if schema["type"] != "object" {
			t.Fatalf("tool %q schema type = %v", name, schema["type"])
		}
		if _, err := json.Marshal(schema); err != nil {
			t.Fatalf("tool %q schema is not JSON-marshalable: %v", name, err)
		}
	}
}

func TestNilCapabilitiesReturnReadableErrors(t *testing.T) {
	byName := toolMap(hosttools.Tools(hosttools.Options{Interactive: true}))
	cases := map[string]string{
		"ask_user":         `{"question":"ok?"}`,
		"present_artifact": `{"title":"report"}`,
		"plan_review":      `{"content":"plan"}`,
		"exit_plan_mode":   `{"action":"status"}`,
		"goal":             `{"action":"get"}`,
	}
	interfaces := map[string]string{
		"ask_user":         "Asker",
		"present_artifact": "Presenter",
		"plan_review":      "PlanController",
		"exit_plan_mode":   "PlanController",
		"goal":             "GoalStore",
	}
	for name, arguments := range cases {
		text := invokeError(t, byName, name, arguments)
		if !strings.Contains(text, interfaces[name]) {
			t.Fatalf("%s nil-interface error = %q, want mention of %s", name, text, interfaces[name])
		}
	}
}

func TestAskUserRequiresInteractive(t *testing.T) {
	host := hosttools.NewMemoryHost()
	host.SetAnswers("yes")
	byName := toolMap(hosttools.Tools(hosttools.Options{Asker: host, Interactive: false}))
	text := invokeError(t, byName, "ask_user", `{"question":"continue?"}`)
	if !strings.Contains(text, "no interactive UI") {
		t.Fatalf("non-interactive ask error = %q", text)
	}
	if len(host.Questions()) != 0 {
		t.Fatalf("non-interactive ask should not reach the Asker")
	}
}

func TestAskUserMemoryHost(t *testing.T) {
	host := hosttools.NewMemoryHost()
	host.SetAnswers("yes", "no")
	byName := toolMap(hosttools.Tools(hosttools.Options{Asker: host, Interactive: true}))

	result := invoke(t, byName, "ask_user", `{"question":"continue?","options":["yes","no"]}`)
	if result.IsError || result.Content != "yes" {
		t.Fatalf("ask_user = %q (IsError=%v), want yes", result.Content, result.IsError)
	}
	result = invoke(t, byName, "ask_user", `{"question":"really?"}`)
	if result.IsError || result.Content != "no" {
		t.Fatalf("second ask_user = %q (IsError=%v), want no", result.Content, result.IsError)
	}
	questions := host.Questions()
	if len(questions) != 2 {
		t.Fatalf("recorded questions = %d, want 2", len(questions))
	}
	if questions[0].Question != "continue?" || len(questions[0].Options) != 2 {
		t.Fatalf("recorded question = %+v", questions[0])
	}
	// The queue is drained: an empty answer is reported as such.
	result = invoke(t, byName, "ask_user", `{"question":"anyone?"}`)
	if result.IsError || !strings.Contains(result.Content, "empty answer") {
		t.Fatalf("unanswered ask_user = %q (IsError=%v)", result.Content, result.IsError)
	}
}

type nilPresenter struct{}

func (*nilPresenter) Present(context.Context, string, string) (string, error) {
	return "never", nil
}

func TestTypedNilInterfaceIsRejected(t *testing.T) {
	// A typed nil pointer in the interface must be detected instead of panicking.
	var presenter *nilPresenter
	byName := toolMap(hosttools.Tools(hosttools.Options{Presenter: presenter}))
	text := invokeError(t, byName, "present_artifact", `{"title":"x"}`)
	if !strings.Contains(text, "Presenter") {
		t.Fatalf("typed nil presenter error = %q", text)
	}
}

func TestPresentMemoryHost(t *testing.T) {
	host := hosttools.NewMemoryHost()
	byName := toolMap(hosttools.Tools(hosttools.Options{Presenter: host}))
	result := invoke(t, byName, "present_artifact", `{"title":"diff","body":"+ line"}`)
	if result.IsError || !strings.HasPrefix(result.Content, "artifact-") {
		t.Fatalf("present_artifact = %q (IsError=%v)", result.Content, result.IsError)
	}
	artifacts := host.Artifacts()
	if len(artifacts) != 1 || artifacts[0].ID != result.Content ||
		artifacts[0].Title != "diff" || artifacts[0].Body != "+ line" {
		t.Fatalf("artifacts = %+v", artifacts)
	}
	// Title is required; bad arguments are a tool error.
	invokeError(t, byName, "present_artifact", `{"title":"  "}`)
}

func TestPresentFilesMemoryHost(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "deliverable.md")
	if err := os.WriteFile(path, []byte("# result"), 0o644); err != nil {
		t.Fatalf("write deliverable: %v", err)
	}
	host := hosttools.NewMemoryHost()
	byName := toolMap(hosttools.Tools(hosttools.Options{Root: root, FilePresenter: host}))
	result := invoke(t, byName, "present", `{"files":[{"path":"deliverable.md","description":"report"}]}`)
	if result.IsError || !strings.Contains(result.Content, "Presented "+path) {
		t.Fatalf("present files = %q (IsError=%v)", result.Content, result.IsError)
	}
	deliverables := host.Deliverables()
	if len(deliverables) != 1 || deliverables[0].Path != path || deliverables[0].Description != "report" {
		t.Fatalf("deliverables = %+v", deliverables)
	}
	// Missing files, directories, and an unconfigured FilePresenter are tool errors.
	invokeError(t, byName, "present", `{"files":[{"path":"missing.md"}]}`)
	invokeError(t, byName, "present", `{"files":[{"path":"."}]}`)
	nilHost := toolMap(hosttools.Tools(hosttools.Options{Root: root}))
	text := invokeError(t, nilHost, "present", `{"files":[{"path":"deliverable.md"}]}`)
	if !strings.Contains(text, "FilePresenter") {
		t.Fatalf("nil FilePresenter error = %q", text)
	}
}

func TestPlanReviewAndExitMemoryHost(t *testing.T) {
	host := hosttools.NewMemoryHost()
	byName := toolMap(hosttools.Tools(hosttools.Options{Plan: host}))

	result := invoke(t, byName, "exit_plan_mode", `{"action":"status"}`)
	if result.IsError || result.Content != "inactive" {
		t.Fatalf("initial plan status = %q", result.Content)
	}
	result = invoke(t, byName, "plan_review", `{"content":"1. do the thing"}`)
	if result.IsError || result.Content != "pending" {
		t.Fatalf("plan_review = %q (IsError=%v)", result.Content, result.IsError)
	}
	if state := host.State(); state != "reviewing" {
		t.Fatalf("plan state after review = %q, want reviewing", state)
	}
	if host.PlanContent() != "1. do the thing" {
		t.Fatalf("plan content = %q", host.PlanContent())
	}
	result = invoke(t, byName, "exit_plan_mode", `{"action":"approve"}`)
	if result.IsError || !strings.Contains(result.Content, "approved") {
		t.Fatalf("exit approve = %q (IsError=%v)", result.Content, result.IsError)
	}
	if state := invoke(t, byName, "exit_plan_mode", `{"action":"status"}`); state.Content != "approved" {
		t.Fatalf("plan state after approve = %q, want approved", state.Content)
	}
	result = invoke(t, byName, "exit_plan_mode", `{"action":"cancel"}`)
	if result.IsError || !strings.Contains(result.Content, "cancelled") {
		t.Fatalf("exit cancel = %q (IsError=%v)", result.Content, result.IsError)
	}
	invokeError(t, byName, "plan_review", `{"content":"   "}`)
	invokeError(t, byName, "exit_plan_mode", `{"action":"explode"}`)
}

func TestGoalMemoryHost(t *testing.T) {
	host := hosttools.NewMemoryHost()
	byName := toolMap(hosttools.Tools(hosttools.Options{Goal: host}))

	result := invoke(t, byName, "goal", `{"action":"get"}`)
	if result.IsError || !strings.Contains(result.Content, "no goal set") {
		t.Fatalf("goal get before set = %q (IsError=%v)", result.Content, result.IsError)
	}
	result = invoke(t, byName, "goal", `{"action":"set","text":"ship v0.6"}`)
	if result.IsError || !strings.HasPrefix(result.Content, "goal-") {
		t.Fatalf("goal set = %q (IsError=%v)", result.Content, result.IsError)
	}
	goalID := result.Content
	result = invoke(t, byName, "goal", `{"action":"get"}`)
	if result.IsError || !strings.Contains(result.Content, goalID) ||
		!strings.Contains(result.Content, "ship v0.6") || !strings.Contains(result.Content, "active") {
		t.Fatalf("goal get = %q", result.Content)
	}
	result = invoke(t, byName, "goal", `{"action":"clear"}`)
	if result.IsError || !strings.Contains(result.Content, "cleared") {
		t.Fatalf("goal clear = %q (IsError=%v)", result.Content, result.IsError)
	}
	invokeError(t, byName, "goal", `{"action":"set","text":" "}`)
	invokeError(t, byName, "goal", `{"action":"dance"}`)
}

func TestGoalHostCanBeReplaced(t *testing.T) {
	host := hosttools.NewMemoryHost()
	byName := toolMap(hosttools.Tools(hosttools.Options{Goal: host}))
	if _, err := host.Set("first"); err != nil {
		t.Fatalf("host.Set: %v", err)
	}
	result := invoke(t, byName, "goal", `{"action":"set","text":"second"}`)
	if result.IsError || result.Content == "" {
		t.Fatalf("goal replace = %q (IsError=%v)", result.Content, result.IsError)
	}
	_, text, status, ok := host.Get()
	if !ok || text != "second" || status != "active" {
		t.Fatalf("goal state after replace: text=%q status=%q ok=%v", text, status, ok)
	}
}

func TestInvalidArgumentsJSON(t *testing.T) {
	byName := toolMap(hosttools.Tools(hosttools.Options{}))
	for _, name := range []string{"ask_user", "present", "present_artifact", "plan_review", "exit_plan_mode", "goal"} {
		tool := byName[name]
		_, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"bad":`})
		if err == nil {
			t.Fatalf("%s: invalid JSON did not return a Go error", name)
		}
	}
}

func TestContextCancellation(t *testing.T) {
	host := hosttools.NewMemoryHost()
	byName := toolMap(hosttools.Tools(hosttools.Options{
		Asker: host, Presenter: host, Plan: host, Goal: host, Interactive: true,
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, arguments := range map[string]string{
		"ask_user":         `{"question":"q"}`,
		"present_artifact": `{"title":"t"}`,
		"plan_review":      `{"content":"c"}`,
		"exit_plan_mode":   `{"action":"status"}`,
		"goal":             `{"action":"get"}`,
	} {
		if _, err := byName[name].Invoke(ctx, rig.ToolCall{Arguments: arguments}); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: cancelled invoke error = %v, want context.Canceled", name, err)
		}
	}
}

func TestConcurrentHostCalls(t *testing.T) {
	host := hosttools.NewMemoryHost()
	byName := toolMap(hosttools.Tools(hosttools.Options{
		Asker: host, Presenter: host, Plan: host, Goal: host, Interactive: true,
	}))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	record := func(message string) {
		mu.Lock()
		failures = append(failures, message)
		mu.Unlock()
	}
	for index := 0; index < 20; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, call := range []struct{ name, arguments string }{
				{"ask_user", `{"question":"q"}`},
				{"present_artifact", `{"title":"t","body":"b"}`},
				{"plan_review", `{"content":"plan"}`},
				{"exit_plan_mode", `{"action":"status"}`},
				{"goal", `{"action":"set","text":"g"}`},
				{"goal", `{"action":"get"}`},
			} {
				result, err := byName[call.name].Invoke(context.Background(), rig.ToolCall{Arguments: call.arguments})
				if err != nil || result.IsError {
					record(call.name + ": " + result.Content)
				}
			}
		}()
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Fatalf("concurrent calls failed: %v", failures)
	}
	if len(host.Artifacts()) != 20 {
		t.Fatalf("artifacts = %d, want 20", len(host.Artifacts()))
	}
	if len(host.Questions()) != 20 {
		t.Fatalf("questions = %d, want 20", len(host.Questions()))
	}
}
