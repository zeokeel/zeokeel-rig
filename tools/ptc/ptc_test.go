package ptc_test

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/tools/ptc"
)

type echoTool struct{}

func (echoTool) Name() string           { return "echo_tool" }
func (echoTool) Description() string    { return "Echo the text argument." }
func (echoTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (echoTool) Invoke(_ context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if strings.Contains(call.Arguments, "deny") {
		return rig.ToolResult{}, fmt.Errorf("denied")
	}
	return rig.ToolResult{Content: "tool:" + call.Arguments}, nil
}

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
}

func TestRunCodePythonToolBridge(t *testing.T) {
	requirePython(t)
	tool := ptc.New(ptc.Config{Tools: []rig.Tool{echoTool{}}})
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"code":"print(tools.echo_tool(text='hi'))"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.IsError || !strings.Contains(result.Content, "tool:") || !strings.Contains(result.Content, "hi") {
		t.Fatalf("run_code = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestRunCodeGateDeniesInnerCall(t *testing.T) {
	requirePython(t)
	tool := ptc.New(ptc.Config{Tools: []rig.Tool{echoTool{}}, Gate: gate.DenyAll()})
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"code":"print(tools.echo_tool(text='hi'))"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "denied") {
		t.Fatalf("run_code denied = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestRunCodeUnknownTool(t *testing.T) {
	requirePython(t)
	tool := ptc.New(ptc.Config{})
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"code":"tools.nope()"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "unknown tool") {
		t.Fatalf("run_code unknown tool = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestRunCodeTimeout(t *testing.T) {
	requirePython(t)
	tool := ptc.New(ptc.Config{Timeout: 50 * time.Millisecond})
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"code":"import time; time.sleep(5)"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "timeout") {
		t.Fatalf("run_code timeout = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestRunCodeNodeToolBridge(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	tool := ptc.New(ptc.Config{Tools: []rig.Tool{echoTool{}}})
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"language":"node","code":"console.log(await tools.echo_tool({text: 'hi'}))"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.IsError || !strings.Contains(result.Content, "tool:") {
		t.Fatalf("run_code node = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestToolMetadataAndAliases(t *testing.T) {
	tool := ptc.New(ptc.Config{})
	if tool.Name() != ptc.ToolName {
		t.Fatalf("Name = %q", tool.Name())
	}
	if tool.Description() == "" || tool.Schema() == nil {
		t.Fatal("missing description/schema")
	}
	alias := ptc.NewTool(ptc.Config{Timeout: time.Second})
	if alias.Name() != ptc.ToolName {
		t.Fatalf("NewTool Name = %q", alias.Name())
	}
}

func TestRunCodeRejectsBadInputs(t *testing.T) {
	tool := ptc.New(ptc.Config{})
	cases := []string{
		`{}`,
		`{"code":"print(1)","language":"ruby"}`,
		`{"code":""}`,
	}
	for _, arguments := range cases {
		result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: arguments})
		if err != nil {
			t.Fatalf("%s: %v", arguments, err)
		}
		if !result.IsError {
			t.Fatalf("%s: result = %q, want error", arguments, result.Content)
		}
	}
}
