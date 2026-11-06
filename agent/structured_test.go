package agent_test

// Tests for subagent structured output: the runner adds a structured_output
// tool, captures its arguments, accepts a JSON text fallback, and reports a
// readable error when the child does not satisfy the schema.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
)

func reportSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{"type": "string"},
			"count": map[string]any{"type": "integer"},
		},
		"required": []string{"title", "count"},
	}
}

func startStructuredRunner(t *testing.T, provider rig.Provider) rig.SubagentRunner {
	t.Helper()
	runner, err := agent.NewRunner(rig.AgentSpec{Model: "structured-model", Provider: provider}, agent.DefaultRunnerOptions())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return runner
}

func TestStructuredOutputCapturedViaTool(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call-1", agent.StructuredOutputToolName, `{"title":"report","count":2}`),
		textScript("done"),
	}}
	runner := startStructuredRunner(t, provider)
	sub, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "write a report", OutputSchema: reportSchema()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sub.Stop() }()
	result := waitSubagent(t, sub)
	if result.Output != "done" {
		t.Fatalf("output = %q, want the final text", result.Output)
	}
	if result.Structured == nil {
		t.Fatal("Structured is nil, want the captured tool arguments")
	}
	if result.Structured["title"] != "report" {
		t.Fatalf("structured title = %v, want report", result.Structured["title"])
	}
	if count, ok := result.Structured["count"].(float64); !ok || count != 2 {
		t.Fatalf("structured count = %v (%T), want 2", result.Structured["count"], result.Structured["count"])
	}

	request := provider.request(0)
	foundTool := false
	for _, toolSpec := range request.Tools {
		if toolSpec.Name != agent.StructuredOutputToolName {
			continue
		}
		foundTool = true
		if toolSpec.Parameters["type"] != "object" {
			t.Fatalf("%s schema = %v, want an object schema", agent.StructuredOutputToolName, toolSpec.Parameters)
		}
	}
	if !foundTool {
		t.Fatalf("child tools %v do not include %s", specNames(request.Tools), agent.StructuredOutputToolName)
	}
	if len(request.Messages) == 0 || !strings.Contains(request.Messages[0].Text(), agent.StructuredOutputToolName) {
		t.Fatalf("system prompt does not instruct the child to use structured_output: %+v", request.Messages)
	}
}

func TestStructuredOutputTextFallback(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript(`{"title":"from-text","count":7}`)}}
	runner := startStructuredRunner(t, provider)
	sub, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "answer", OutputSchema: reportSchema()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sub.Stop() }()
	result := waitSubagent(t, sub)
	if result.Structured["title"] != "from-text" {
		t.Fatalf("structured = %+v, want the JSON text fallback", result.Structured)
	}
	if result.Output != `{"title":"from-text","count":7}` {
		t.Fatalf("output = %q, want the raw JSON text", result.Output)
	}
}

func TestStructuredOutputMissingIsReadableError(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("plain prose without json")}}
	runner := startStructuredRunner(t, provider)
	sub, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "answer", OutputSchema: reportSchema()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sub.Stop() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := sub.Wait(ctx)
	if err == nil {
		t.Fatalf("Wait = %+v, want a structured-output error", result)
	}
	if !strings.Contains(err.Error(), "did not produce structured output") {
		t.Fatalf("Wait error = %v, want a readable structured-output error", err)
	}
	if result.Output != "plain prose without json" {
		t.Fatalf("result output = %q", result.Output)
	}
}

func TestStructuredOutputTextFallbackWrongShape(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript(`{"title":"x","count":"not-a-number"}`)}}
	runner := startStructuredRunner(t, provider)
	sub, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "answer", OutputSchema: reportSchema()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sub.Stop() }()
	if _, err := sub.Wait(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "did not produce structured output") {
		t.Fatalf("Wait error = %v, want a schema mismatch error", err)
	}
}

func TestStructuredOutputSchemaValidationAtStart(t *testing.T) {
	provider := &scriptProvider{repeatLast: true, scripts: [][]rig.Chunk{textScript("x")}}
	runner := startStructuredRunner(t, provider)
	cases := []struct {
		name   string
		schema map[string]any
		want   string
	}{
		{"non-object root", map[string]any{"type": "string"}, "top-level type"},
		{"unsupported type", map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "date"}}}, "unsupported type"},
		{"bad required", map[string]any{"type": "object", "required": "x"}, "required"},
		{"bad properties", map[string]any{"type": "object", "properties": "nope"}, "properties"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "x", OutputSchema: testCase.schema}); err == nil ||
				!strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Start error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestStructuredOutputReservedToolNameConflict(t *testing.T) {
	provider := &scriptProvider{repeatLast: true, scripts: [][]rig.Chunk{textScript("x")}}
	runner, err := agent.NewRunner(rig.AgentSpec{
		Model:    "m",
		Provider: provider,
		Tools:    []rig.Tool{&fakeTool{name: agent.StructuredOutputToolName}},
	}, agent.DefaultRunnerOptions())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if _, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "x", OutputSchema: reportSchema()}); err == nil ||
		!strings.Contains(err.Error(), "reserved tool name") {
		t.Fatalf("Start error = %v, want a reserved-name error", err)
	}
}

func TestStructuredOutputSendKeepsSchema(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call-1", agent.StructuredOutputToolName, `{"title":"first","count":1}`),
		textScript("done"),
		textScript(`{"title":"second","count":2}`),
		textScript("plain third"),
	}}
	runner := startStructuredRunner(t, provider)
	sub, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "first task", OutputSchema: reportSchema()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sub.Stop() }()
	first := waitSubagent(t, sub)
	if first.Structured["title"] != "first" {
		t.Fatalf("first structured = %+v", first.Structured)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	second, err := sub.Send(ctx, "second task")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if second.Structured["title"] != "second" {
		t.Fatalf("second structured = %+v, want a fresh capture", second.Structured)
	}

	// The third run produces no structured output: Send reports it instead of
	// reusing the previous capture.
	if _, err := sub.Send(ctx, "third task"); err == nil ||
		!strings.Contains(err.Error(), "did not produce structured output") {
		t.Fatalf("third Send error = %v, want a structured-output error", err)
	}
}

func TestStructuredOutputNestedAndArrays(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"meta": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tags": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "string"},
					},
					"level": map[string]any{"type": "integer"},
				},
				"required": []string{"tags"},
			},
		},
		"required": []string{"meta"},
	}
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call-1", agent.StructuredOutputToolName, `{"meta":{"tags":["a","b"],"level":3}}`),
		textScript("done"),
	}}
	runner := startStructuredRunner(t, provider)
	sub, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "nested", OutputSchema: schema})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sub.Stop() }()
	result := waitSubagent(t, sub)
	meta, ok := result.Structured["meta"].(map[string]any)
	if !ok {
		t.Fatalf("structured = %+v", result.Structured)
	}
	tags, ok := meta["tags"].([]any)
	if !ok || len(tags) != 2 {
		t.Fatalf("nested tags = %v (%T)", meta["tags"], meta["tags"])
	}
}

func TestStructuredOutputAbsentWithoutSchema(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("plain")}}
	runner := startStructuredRunner(t, provider)
	sub, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "plain"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sub.Stop() }()
	result := waitSubagent(t, sub)
	if result.Structured != nil {
		t.Fatalf("Structured = %+v, want nil without an OutputSchema", result.Structured)
	}
}
