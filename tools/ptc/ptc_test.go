package ptc_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/sandbox/osrunner"
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

// fakeRunner 记录一次 Confine 并把 argv 改写为一条固定命令，用于证明 run_code
// 在挂接 runner 后确实走了沙箱包装（而不是直接 spawn 解释器）。
type fakeRunner struct {
	calls  int
	argv   []string
	policy *sandbox.Policy
}

func (r *fakeRunner) Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	r.calls++
	r.argv = append([]string(nil), argv...)
	r.policy = policy
	return sandbox.ConfinedCommand{
		Argv:        append([]string{"/bin/sh", "-c", "echo CONFINE-MARKER"}, argv...),
		Enforcement: sandbox.EnforcementFull,
	}, nil
}

func TestRunCodeUsesSandboxRunner(t *testing.T) {
	runner := &fakeRunner{}
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()))
	tool := ptc.New(ptc.Config{Runner: runner, Policy: policy})
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"code":"print(1)"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("runner.Confine calls = %d, want 1", runner.calls)
	}
	if runner.policy != policy {
		t.Fatal("runner received a different policy")
	}
	if !strings.Contains(result.Content, "CONFINE-MARKER") {
		t.Fatalf("result = %q, want the runner wrapper to be executed", result.Content)
	}
	if len(runner.argv) < 2 || !strings.HasSuffix(runner.argv[len(runner.argv)-1], ".py") {
		t.Fatalf("confined argv = %v, want it to end with the program path", runner.argv)
	}
}

func TestRunCodeRunnerWithoutPolicyFailsClosed(t *testing.T) {
	runner := &fakeRunner{}
	tool := ptc.New(ptc.Config{Runner: runner})
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"code":"print(1)"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "no sandbox policy") {
		t.Fatalf("result = %+v, want a fail-closed configuration error", result)
	}
	if runner.calls != 0 {
		t.Fatalf("runner must not be called without a policy, got %d calls", runner.calls)
	}
}

// TestRunCodeProgramLivesWhereTheSandboxCanReadIt 证明程序文件落在策略根里：
// bwrap 的 workspace-write 会挂私有 tmpfs /tmp，宿主 /tmp 里的脚本在沙箱内不存在。
func TestRunCodeProgramLivesWhereTheSandboxCanReadIt(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root))
	var captured []string
	runner := &capturingRunner{capture: &captured}
	tool := ptc.New(ptc.Config{Runner: runner, Policy: policy})
	if _, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"code":"print(1)"}`}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(captured) == 0 {
		t.Fatal("runner was not invoked")
	}
	program := captured[len(captured)-1]
	if !strings.HasPrefix(program, root) {
		t.Fatalf("program path = %q, want it under the policy root %q", program, root)
	}
}

// capturingRunner 记录 argv 并立即成功退出（不做真实包装）。
type capturingRunner struct{ capture *[]string }

func (r *capturingRunner) Confine(argv []string, _ *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	*r.capture = append([]string(nil), argv...)
	return sandbox.ConfinedCommand{Argv: []string{"/bin/sh", "-c", "true"}, Enforcement: sandbox.EnforcementNone}, nil
}

// TestRunCodeConfinedByRealRunner 是回归测试：run_code 曾经是 OS 沙箱的旁路。
// 在真的能强制沙箱的机器上（bwrap / Landlock / Seatbelt 可用），程序写入工作区
// 内被保护（只读）的路径必须被拒绝并得到结构化归因；没有可用 runner 时跳过。
func TestRunCodeConfinedByRealRunner(t *testing.T) {
	requirePython(t)
	runner, err := osrunner.New(osrunner.Config{})
	if err != nil {
		t.Skipf("no usable OS sandbox runner: %v", err)
	}
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	if mkErr := os.Mkdir(vault, 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithProtectedPaths(vault),
	)
	tool := ptc.New(ptc.Config{Runner: runner, Policy: policy})

	code := fmt.Sprintf("open(%q, 'w').write('x')\nprint('WROTE-PROTECTED')", filepath.Join(vault, "probe.txt"))
	result, invokeErr := tool.Invoke(context.Background(), rig.ToolCall{Arguments: fmt.Sprintf(`{"code":%q}`, code)})
	if invokeErr != nil {
		t.Fatalf("Invoke: %v", invokeErr)
	}
	if _, statErr := os.Stat(filepath.Join(vault, "probe.txt")); statErr == nil {
		t.Fatalf("program wrote to a protected path; result = %+v", result)
	}
	if !strings.Contains(result.Content, "[sandbox: denied ") {
		t.Fatalf("result = %q, want structured sandbox attribution", result.Content)
	}

	// 反证：工作区普通路径必须可写（证明不是"整个 run_code 都坏了"）。
	inside := fmt.Sprintf("open(%q, 'w').write('x')\nprint('WROTE-INSIDE')", filepath.Join(root, "ok.txt"))
	okResult, okErr := tool.Invoke(context.Background(), rig.ToolCall{Arguments: fmt.Sprintf(`{"code":%q}`, inside)})
	if okErr != nil {
		t.Fatalf("Invoke: %v", okErr)
	}
	if !strings.Contains(okResult.Content, "WROTE-INSIDE") {
		t.Fatalf("inside-workspace run = %q, want success", okResult.Content)
	}
	if _, statErr := os.Stat(filepath.Join(root, "ok.txt")); statErr != nil {
		t.Fatalf("inside-workspace write missing: %v", statErr)
	}
}
