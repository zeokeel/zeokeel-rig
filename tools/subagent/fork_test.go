package subagent_test

// Tests for the fork/output_schema extension and the subagent_fork tool.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/subagent"
)

func TestSubagentForkAndSchemaPassthrough(t *testing.T) {
	runner := &fakeRunner{}
	arguments := `{"prompt":"inherit the plan","fork":true,` +
		`"output_schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}}`
	result := invokeTool(t, subagent.New(runner), arguments)
	if result.IsError {
		t.Fatalf("result = %q (IsError=true)", result.Content)
	}
	specs := runner.recorded()
	if len(specs) != 1 {
		t.Fatalf("runner.Start calls = %d, want 1", len(specs))
	}
	spec := specs[0]
	if !spec.Fork {
		t.Fatalf("spec.Fork = false, want true")
	}
	var wantSchema map[string]any
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`), &wantSchema); err != nil {
		t.Fatalf("expected schema JSON: %v", err)
	}
	if !reflect.DeepEqual(spec.OutputSchema, wantSchema) {
		t.Fatalf("spec.OutputSchema = %#v, want %#v", spec.OutputSchema, wantSchema)
	}
}

func TestSubagentDefaultsToFreshContextWithoutSchema(t *testing.T) {
	runner := &fakeRunner{}
	invokeTool(t, subagent.New(runner), `{"prompt":"simple task"}`)
	specs := runner.recorded()
	if len(specs) != 1 {
		t.Fatalf("runner.Start calls = %d, want 1", len(specs))
	}
	if specs[0].Fork {
		t.Fatal("spec.Fork = true for a plain subagent call")
	}
	if specs[0].OutputSchema != nil {
		t.Fatalf("spec.OutputSchema = %#v, want nil", specs[0].OutputSchema)
	}
}

func TestOutputSchemaValidation(t *testing.T) {
	runner := &fakeRunner{}
	tool := subagent.New(runner)
	for _, value := range []string{`"not-an-object"`, `123`, `[1,2]`, `true`} {
		arguments := `{"prompt":"x","output_schema":` + value + `}`
		result := invokeTool(t, tool, arguments)
		if !result.IsError || !strings.Contains(result.Content, "output_schema") {
			t.Fatalf("output_schema=%s: result = %q (IsError=%v)", value, result.Content, result.IsError)
		}
	}
	if specs := runner.recorded(); len(specs) != 0 {
		t.Fatalf("runner.Start called %d times for invalid schemas", len(specs))
	}
	// null / {} mean "no structured output".
	for _, value := range []string{`null`, `{}`} {
		arguments := `{"prompt":"x","output_schema":` + value + `}`
		if result := invokeTool(t, tool, arguments); result.IsError {
			t.Fatalf("output_schema=%s unexpectedly failed: %q", value, result.Content)
		}
	}
	specs := runner.recorded()
	if len(specs) != 2 {
		t.Fatalf("runner.Start calls = %d, want 2", len(specs))
	}
	for index, spec := range specs {
		if spec.OutputSchema != nil {
			t.Fatalf("specs[%d].OutputSchema = %#v, want nil for null/empty schema", index, spec.OutputSchema)
		}
	}
}

func TestStructuredOutputRendering(t *testing.T) {
	runner := &fakeRunner{sub: &fakeSubagent{
		name:       "child",
		output:     "the answer is 42",
		structured: map[string]any{"answer": "42", "confidence": 0.9},
	}}
	result := invokeTool(t, subagent.New(runner), `{"prompt":"x","output_schema":{"type":"object"}}`)
	if result.IsError {
		t.Fatalf("result = %q", result.Content)
	}
	if !strings.Contains(result.Content, "the answer is 42") {
		t.Fatalf("text summary missing: %q", result.Content)
	}
	if !strings.Contains(result.Content, "structured output:") {
		t.Fatalf("structured marker missing: %q", result.Content)
	}
	lines := strings.Split(strings.TrimSpace(result.Content), "\n")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &decoded); err != nil {
		t.Fatalf("structured JSON is not the last line (%q): %v", lines[len(lines)-1], err)
	}
	if decoded["answer"] != "42" {
		t.Fatalf("decoded structured output = %#v", decoded)
	}
	if confidence, ok := decoded["confidence"].(float64); !ok || confidence != 0.9 {
		t.Fatalf("confidence = %#v", decoded["confidence"])
	}
	if got := runner.sub.stopCalls.Load(); got != 1 {
		t.Fatalf("Stop calls = %d, want 1", got)
	}
}

func TestStructuredOnlyOutput(t *testing.T) {
	runner := &fakeRunner{sub: &fakeSubagent{
		name:       "child",
		structured: map[string]any{"ok": true},
	}}
	result := invokeTool(t, subagent.New(runner), `{"prompt":"x","output_schema":{"type":"object"}}`)
	if result.IsError {
		t.Fatalf("result = %q", result.Content)
	}
	if strings.Contains(result.Content, "without text output") {
		t.Fatalf("structured-only result used the empty-text placeholder: %q", result.Content)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(result.Content), &decoded); err != nil {
		t.Fatalf("structured-only content is not pure JSON (%q): %v", result.Content, err)
	}
	if decoded["ok"] != true {
		t.Fatalf("decoded structured output = %#v", decoded)
	}
}

func TestStructuredOutputMarshalError(t *testing.T) {
	runner := &fakeRunner{sub: &fakeSubagent{
		name:   "child",
		output: "some text",
		// func values cannot be encoded by encoding/json.
		structured: map[string]any{"bad": func() {}},
	}}
	result := invokeTool(t, subagent.New(runner), `{"prompt":"x","output_schema":{"type":"object"}}`)
	if !result.IsError || !strings.Contains(result.Content, "JSON-encodable") {
		t.Fatalf("marshal error result = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestSchemaAdvertisesForkAndOutputSchema(t *testing.T) {
	tool := subagent.New(nil)
	properties, ok := tool.Schema()["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v", tool.Schema()["properties"])
	}
	for _, name := range []string{"prompt", "name", "model", "max_iterations", "fork", "output_schema"} {
		if _, exists := properties[name]; !exists {
			t.Fatalf("subagent schema missing property %q", name)
		}
	}
	fork, ok := properties["fork"].(map[string]any)
	if !ok || fork["type"] != "boolean" {
		t.Fatalf("fork property = %#v", properties["fork"])
	}
	schema, ok := properties["output_schema"].(map[string]any)
	if !ok || schema["type"] != "object" {
		t.Fatalf("output_schema property = %#v", properties["output_schema"])
	}
}

func TestForkTool(t *testing.T) {
	runner := &fakeRunner{}
	tool := subagent.NewForkTool(runner)
	if tool.Name() != subagent.ToolNameFork {
		t.Fatalf("Name = %q, want %q", tool.Name(), subagent.ToolNameFork)
	}
	if !strings.Contains(tool.Description(), "inherit") {
		t.Fatalf("fork description = %q", tool.Description())
	}
	if got := subagent.ForkTool(runner).Name(); got != subagent.ToolNameFork {
		t.Fatalf("ForkTool alias name = %q", got)
	}
	result := invokeTool(t, tool, `{"prompt":"use the conversation","name":"forked","model":"m3","max_iterations":2}`)
	if result.IsError || result.Content != "child output" {
		t.Fatalf("result = %q (IsError=%v)", result.Content, result.IsError)
	}
	specs := runner.recorded()
	if len(specs) != 1 {
		t.Fatalf("runner.Start calls = %d, want 1", len(specs))
	}
	spec := specs[0]
	if !spec.Fork {
		t.Fatal("subagent_fork did not set Fork=true")
	}
	if spec.OutputSchema != nil {
		t.Fatalf("subagent_fork spec.OutputSchema = %#v, want nil", spec.OutputSchema)
	}
	if spec.Prompt != "use the conversation" || spec.Name != "forked" || spec.Model != "m3" || spec.MaxIterations != 2 {
		t.Fatalf("spec = %+v", spec)
	}
	if got := runner.sub.stopCalls.Load(); got != 1 {
		t.Fatalf("Stop calls = %d, want 1", got)
	}
}

func TestForkToolSchemaHasNoForkOrSchemaFields(t *testing.T) {
	properties, ok := subagent.NewForkTool(nil).Schema()["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v", subagent.NewForkTool(nil).Schema()["properties"])
	}
	for _, forbidden := range []string{"fork", "output_schema"} {
		if _, exists := properties[forbidden]; exists {
			t.Fatalf("subagent_fork schema unexpectedly exposes %q", forbidden)
		}
	}
	for _, required := range []string{"prompt", "name", "model", "max_iterations"} {
		if _, exists := properties[required]; !exists {
			t.Fatalf("subagent_fork schema missing property %q", required)
		}
	}
}

func TestForkToolErrorPassthrough(t *testing.T) {
	runner := &fakeRunner{startErr: errors.New("fork requires a parent session")}
	_, err := subagent.NewForkTool(runner).Invoke(context.Background(),
		rig.ToolCall{Arguments: `{"prompt":"x"}`})
	if err == nil || !strings.Contains(err.Error(), "fork requires a parent session") {
		t.Fatalf("fork start error = %v, want passthrough", err)
	}
	if !errors.Is(err, runner.startErr) {
		t.Fatalf("fork start error does not wrap the runner error: %v", err)
	}
}

func TestForkToolNilRunner(t *testing.T) {
	_, err := subagent.NewForkTool(nil).Invoke(context.Background(), rig.ToolCall{Arguments: `{"prompt":"x"}`})
	if err == nil || !strings.Contains(err.Error(), "SubagentRunner") {
		t.Fatalf("nil runner error = %v, want a configuration error", err)
	}
	// Missing prompt is a tool error, matching the subagent tool.
	result := invokeTool(t, subagent.NewForkTool(nil), `{}`)
	if !result.IsError || !strings.Contains(result.Content, "prompt") {
		t.Fatalf("fork missing prompt = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestTools(t *testing.T) {
	if tools := subagent.Tools(nil); len(tools) != 2 {
		t.Fatalf("Tools(nil) returned %d tools, want 2", len(tools))
	}
	runner := &fakeRunner{}
	tools := subagent.Tools(runner)
	if len(tools) != 2 {
		t.Fatalf("Tools(runner) returned %d tools, want 2", len(tools))
	}
	if tools[0].Name() != subagent.ToolName || tools[1].Name() != subagent.ToolNameFork {
		t.Fatalf("Tools names = %q, %q", tools[0].Name(), tools[1].Name())
	}
	for _, tool := range tools {
		if tool.Description() == "" || tool.Schema() == nil {
			t.Fatalf("tool %q has no description/schema", tool.Name())
		}
	}
}

func TestConcurrentDelegations(t *testing.T) {
	runner := &fakeRunner{}
	subagentTool := subagent.New(runner)
	forkTool := subagent.NewForkTool(runner)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for index := 0; index < 16; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			tool := subagentTool
			if index%2 == 1 {
				tool = forkTool
			}
			result, err := tool.Invoke(context.Background(), rig.ToolCall{
				Arguments: `{"prompt":"concurrent task"}`,
			})
			if err != nil {
				errs <- err
				return
			}
			if result.IsError {
				errs <- errors.New(result.Content)
			}
		}(index)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent delegation failed: %v", err)
	}
	if specs := runner.recorded(); len(specs) != 16 {
		t.Fatalf("runner.Start calls = %d, want 16", len(specs))
	}
}
