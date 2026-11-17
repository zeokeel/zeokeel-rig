package ptc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/internal/procattr"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolName 是面向模型的工具名。
const ToolName = "run_code"

const (
	// DefaultTimeout 限制单个程序的运行时长。
	DefaultTimeout = 60 * time.Second
	// MaxTimeout 是单次调用可接受的最大超时。
	MaxTimeout = 10 * time.Minute
	// MaxOutputBytes 限制返回的程序输出。
	MaxOutputBytes = 64 << 10

	protocolPrefix = "__RIG_CALL__"
)

// Config 配置运行时以及程序可见的工具。
type Config struct {
	// Tools 是程序可按名调用的工具（run_code 自身会被自动排除）。为 nil
	// 时程序可以计算，但无法调用任何工具。
	Tools []rig.Tool
	// Gate 校验每次内层工具调用；nil 表示放行所有调用（只应在受信/自动
	// 批准的组合中使用）。
	Gate rig.Gate
	// Python 与 Node 可选地固定运行时可执行文件。为空时在需要该语言时于
	// PATH 上探测。
	Python string
	Node   string
	// Timeout 是程序默认预算（为零时使用 DefaultTimeout）。
	Timeout time.Duration
	// Root 是程序的初始工作目录（"" 表示进程当前目录）。同时决定临时程序文件
	// 的落地位置：程序必须能被**受限子进程**读到，所以优先放进策略根/工作区，
	// 而不是宿主 /tmp（bwrap 的 workspace-write 会挂一个私有 tmpfs /tmp，
	// 宿主 /tmp 里的文件在沙箱内根本不存在）。
	Root string
	// Runner 是可选的 OS 沙箱 runner（见 sandbox/osrunner）。非 nil 时解释器进程
	// 会先经 Confine 包装再 spawn：run_code 与 bash 一样受文件效果约束，不能成为
	// 绕过 OS 沙箱的旁路。nil 保持历史行为（进程内策略围栏）。
	Runner sandbox.Runner
	// Policy 是 Runner 使用的沙箱策略。Runner 非 nil 时为必填：不知道要强制什么
	// 就不能裸跑（fail closed）。
	Policy *sandbox.Policy
}

// Tool 是 run_code 工具。
type Tool struct {
	config Config
	byName map[string]rig.Tool
}

var _ rig.Tool = (*Tool)(nil)

// New 构建 run_code 工具。
func New(config Config) *Tool {
	if config.Timeout <= 0 {
		config.Timeout = DefaultTimeout
	}
	tool := &Tool{config: config, byName: make(map[string]rig.Tool, len(config.Tools))}
	for _, candidate := range config.Tools {
		if candidate == nil || candidate.Name() == ToolName {
			continue
		}
		tool.byName[candidate.Name()] = candidate
	}
	return tool
}

// NewTool 是具名构造函数，等价于 New。
func NewTool(config Config) *Tool { return New(config) }

// Name 实现 rig.Tool。
func (t *Tool) Name() string { return ToolName }

// Description 实现 rig.Tool。
func (t *Tool) Description() string {
	return "Run a small program in a system-installed Python 3 or Node.js runtime. The program can call the " +
		"agent's tools through `tools.<name>(...)`: Python uses keyword arguments, Node uses one object argument " +
		"and await. Inner tool calls are checked by the approval gate. Use it for batch tool orchestration that " +
		"would otherwise take many tool rounds; use the Go workflow tool for plain multi-step orchestration."
}

// Schema 实现 rig.Tool。
func (t *Tool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"code": toolutil.StringProp("Program source. Python: call tools.name(arg=value). Node: await tools.name({arg: value}) at top level."),
		"language": map[string]any{
			"type":        "string",
			"description": "Runtime language (default: python3 when available, else node).",
			"enum":        []string{"python", "node"},
		},
		"timeout_ms": toolutil.IntegerProp("Program timeout in milliseconds (default 60000, maximum 600000)."),
	}, "code")
}

// runtime 描述一个受支持的语言运行环境。
type runtime struct {
	language string
	path     string
	prelude  string
	suffix   string
}

type callRequest struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type callResponse struct {
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Invoke 实现 rig.Tool。
func (t *Tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args struct {
		Code      string `json:"code"`
		Language  string `json:"language"`
		TimeoutMS int    `json:"timeout_ms"`
	}
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	if strings.TrimSpace(args.Code) == "" {
		return toolutil.ErrorResultf("run_code: code is required"), nil
	}
	selected, err := t.selectRuntime(args.Language)
	if err != nil {
		return toolutil.ErrorResultf("run_code: %v", err), nil
	}
	timeout := t.config.Timeout
	if args.TimeoutMS > 0 {
		timeout = time.Duration(args.TimeoutMS) * time.Millisecond
	}
	timeout = min(timeout, MaxTimeout)

	dir, err := t.programDir()
	if err != nil {
		return toolutil.ErrorResultf("run_code: %v", err), nil
	}
	defer os.RemoveAll(dir) //nolint:errcheck
	script := selected.prelude + "\n" + args.Code
	if selected.language == "node" {
		script = selected.prelude + "\n(async () => {\n" + args.Code + "\n})().then(() => { _rigReader.close(); process.exit(0); }).catch((e) => { console.error(e); process.exit(1); });\n"
	}
	scriptPath := filepath.Join(dir, "program"+selected.suffix)
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		return toolutil.ErrorResultf("run_code: write program: %v", err), nil
	}

	argv := []string{selected.path, scriptPath}
	confined := sandbox.ConfinedCommand{Argv: argv}
	if t.config.Runner != nil {
		if t.config.Policy == nil {
			return toolutil.ErrorResultf("run_code: a sandbox runner is attached but no sandbox policy is configured; refusing to run unconfined"), nil
		}
		wrapped, confineErr := t.config.Runner.Confine(argv, t.config.Policy)
		if confineErr != nil {
			return toolutil.ErrorResultf("run_code: sandbox confinement failed: %v", confineErr), nil
		}
		if len(wrapped.Argv) == 0 {
			return toolutil.ErrorResultf("run_code: sandbox runner returned an empty argv; refusing to run unconfined"), nil
		}
		confined = wrapped
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(runCtx, confined.Argv[0], confined.Argv[1:]...)
	procattr.HideWindow(command)
	if t.config.Root != "" {
		command.Dir = t.config.Root
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return toolutil.ErrorResultf("run_code: stdin pipe: %v", err), nil
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return toolutil.ErrorResultf("run_code: stdout pipe: %v", err), nil
	}
	stderr := &boundedWriter{limit: MaxOutputBytes}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return toolutil.ErrorResultf("run_code: start %s: %v", selected.language, err), nil
	}
	writer := bufio.NewWriter(stdin)
	var output strings.Builder
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	protocolErr := error(nil)
	for scanner.Scan() {
		line := scanner.Text()
		if request, ok := parseCall(line); ok {
			response := t.dispatch(ctx, request)
			payload, _ := json.Marshal(response)
			if _, err := writer.WriteString(string(payload) + "\n"); err != nil {
				protocolErr = err
				break
			}
			if err := writer.Flush(); err != nil {
				protocolErr = err
				break
			}
			continue
		}
		if output.Len() < MaxOutputBytes {
			output.WriteString(line)
			output.WriteByte('\n')
		}
	}
	waitErr := command.Wait()
	_ = stdin.Close()
	if ctx.Err() != nil {
		return rig.ToolResult{}, ctx.Err()
	}
	denialSuffix := runCodeDenialSuffix(output.String(), stderr.String(), confined, t.config.Policy)
	if runCtx.Err() == context.DeadlineExceeded {
		return rig.ToolResult{Content: truncate(output.String()) + "\n[run_code timeout: program exceeded " + timeout.String() + "]" + denialSuffix, IsError: true}, nil
	}
	if protocolErr != nil {
		return rig.ToolResult{Content: truncate(output.String()) + "\n[run_code protocol error: " + protocolErr.Error() + "]", IsError: true}, nil
	}
	content := truncate(output.String())
	if errOutput := strings.TrimSpace(stderr.String()); errOutput != "" {
		if content != "" {
			content += "\n[stderr]\n"
		}
		content += truncate(errOutput)
	}
	if waitErr != nil {
		return rig.ToolResult{Content: content + "\n[run_code exit: " + waitErr.Error() + "]" + denialSuffix, IsError: true}, nil
	}
	// 程序可能把沙箱拒绝吞掉并以 0 退出；命中方言时仍然归因（与 shell 工具
	// 的"不要求退出码非零"保持一致）。
	if strings.TrimSpace(content) == "" && denialSuffix == "" {
		return toolutil.TextResult("(program produced no output)"), nil
	}
	return toolutil.TextResult(strings.TrimRight(content, "\n") + denialSuffix), nil
}

// programDir 选择一个**受限子进程能读到**的目录放临时程序文件。
//
// 优先级：策略主根 → Config.Root → os.TempDir()。前两者在 workspace-write 下被
// 读写绑定、在 read-only 下只读可见，因此沙箱里的解释器一定能读到脚本；直接用
// 宿主 /tmp 时 bwrap 挂的是私有 tmpfs，脚本在沙箱内不存在。
func (t *Tool) programDir() (string, error) {
	parent := ""
	if t.config.Policy != nil {
		if roots := t.config.Policy.Roots(); len(roots) > 0 {
			parent = roots[0]
		}
	}
	if parent == "" {
		parent = strings.TrimSpace(t.config.Root)
	}
	return os.MkdirTemp(parent, ".rig-run-code-")
}

// runCodeDenialSuffix 渲染 run_code 的沙箱拒绝归因（命中方言才追加）。
func runCodeDenialSuffix(stdout, stderr string, confined sandbox.ConfinedCommand, policy *sandbox.Policy) string {
	content := stdout
	if trimmed := strings.TrimSpace(stderr); trimmed != "" {
		content += "\n" + trimmed
	}
	note := toolutil.SandboxDenialNoteFor(content, confined, policy)
	if note == "" {
		return ""
	}
	return "\n" + strings.TrimPrefix(note, "\n")
}

// selectRuntime 选择显式指定的语言，或第一个可用的已探测运行时
// （优先 Python 3，其次 Node）。
func (t *Tool) selectRuntime(language string) (runtime, error) {
	languages := []string{"python", "node"}
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "", "auto":
	case "python", "python3":
		languages = []string{"python"}
	case "node", "javascript", "js":
		languages = []string{"node"}
	default:
		return runtime{}, fmt.Errorf("unsupported language %q (want python or node)", language)
	}
	var lastErr error
	for _, candidate := range languages {
		selected, err := t.runtimeFor(candidate)
		if err == nil {
			return selected, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no supported runtime found (install python3 or node)")
	}
	return runtime{}, lastErr
}

func (t *Tool) runtimeFor(language string) (runtime, error) {
	switch language {
	case "python":
		path := strings.TrimSpace(t.config.Python)
		if path == "" {
			var err error
			path, err = exec.LookPath("python3")
			if err != nil {
				return runtime{}, errors.New("python3 not found in PATH")
			}
		}
		return runtime{language: "python", path: path, suffix: ".py", prelude: pythonPrelude}, nil
	case "node":
		path := strings.TrimSpace(t.config.Node)
		if path == "" {
			var err error
			path, err = exec.LookPath("node")
			if err != nil {
				return runtime{}, errors.New("node not found in PATH")
			}
		}
		return runtime{language: "node", path: path, suffix: ".js", prelude: nodePrelude}, nil
	default:
		return runtime{}, fmt.Errorf("unsupported language %q", language)
	}
}

func parseCall(line string) (callRequest, bool) {
	if !strings.HasPrefix(line, protocolPrefix) {
		return callRequest{}, false
	}
	var request callRequest
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, protocolPrefix)), &request); err != nil {
		return callRequest{}, false
	}
	return request, true
}

func (t *Tool) dispatch(ctx context.Context, request callRequest) callResponse {
	name := strings.TrimSpace(request.Name)
	tool, ok := t.byName[name]
	if !ok {
		return callResponse{Error: fmt.Sprintf("unknown tool %q", name)}
	}
	arguments, _ := json.Marshal(request.Arguments)
	call := rig.ToolCall{Name: name, Arguments: string(arguments)}
	if t.config.Gate != nil {
		decision, err := t.config.Gate.Approve(ctx, tool, call)
		if err != nil {
			return callResponse{Error: err.Error()}
		}
		if decision != rig.DecisionAllow {
			return callResponse{Error: "tool call denied by approval gate"}
		}
	}
	result, err := tool.Invoke(ctx, call)
	if err != nil {
		return callResponse{Error: err.Error()}
	}
	if result.IsError {
		return callResponse{Error: result.Content}
	}
	return callResponse{Content: result.Content}
}

type boundedWriter struct {
	mu        sync.Mutex
	limit     int
	buffer    strings.Builder
	truncated bool
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	remaining := w.limit - w.buffer.Len()
	if remaining > 0 {
		if len(p) > remaining {
			w.buffer.Write(p[:remaining])
			w.truncated = true
		} else {
			w.buffer.Write(p)
		}
	} else {
		w.truncated = true
	}
	return len(p), nil
}

func (w *boundedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

func truncate(text string) string {
	if len(text) <= MaxOutputBytes {
		return text
	}
	return text[:MaxOutputBytes] + "\n[run_code output truncated]"
}

const pythonPrelude = `import json, sys
def _rig_call(name, arguments):
    payload = {"id": "1", "name": name, "arguments": arguments}
    sys.stdout.write("__RIG_CALL__" + json.dumps(payload) + "\n")
    sys.stdout.flush()
    line = sys.stdin.readline()
    if not line:
        raise RuntimeError("rig: tool bridge closed")
    response = json.loads(line)
    if response.get("error"):
        raise RuntimeError(response["error"])
    return response.get("content", "")
class _RigTools:
    def __getattr__(self, name):
        return lambda **kwargs: _rig_call(name, kwargs)
tools = _RigTools()
`

const nodePrelude = `const readline = require("readline");
let _rigResolve = null;
const _rigReader = readline.createInterface({ input: process.stdin });
_rigReader.on("line", (line) => {
  if (!_rigResolve) return;
  const resolve = _rigResolve;
  _rigResolve = null;
  try { resolve(JSON.parse(line)); } catch (error) { resolve({ error: String(error) }); }
});
function _rigCall(name, args) {
  return new Promise((resolve, reject) => {
    process.stdout.write("__RIG_CALL__" + JSON.stringify({ id: "1", name, arguments: args || {} }) + "\n");
    _rigResolve = (response) => {
      if (response.error) reject(new Error(response.error));
      else resolve(response.content || "");
    };
  });
}
globalThis.tools = new Proxy({}, {
  get: (_target, name) => (args) => _rigCall(String(name), args),
});
`
