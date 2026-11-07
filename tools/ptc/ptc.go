// Package ptc exposes the model-facing run_code tool: a program written by the
// model runs in a system-installed runtime (Python 3 or Node.js) and calls the
// agent's existing tools through a line protocol on stdin/stdout.
//
// The runtime is an ordinary child process; Rig does not embed a JavaScript
// engine. The generated SDK prelude exposes `tools.<name>(...)`: Python calls
// tools.web_search(query="...") and Node calls
// await tools.web_search({query:"..."}). Inner calls go through the configured
// rig.Gate, so enabling run_code does not bypass approvals when a gate is set.
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
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolName is the model-facing tool name.
const ToolName = "run_code"

const (
	// DefaultTimeout bounds one program.
	DefaultTimeout = 60 * time.Second
	// MaxTimeout is the largest accepted per-call timeout.
	MaxTimeout = 10 * time.Minute
	// MaxOutputBytes caps the returned program output.
	MaxOutputBytes = 64 << 10

	protocolPrefix = "__RIG_CALL__"
)

// Config wires the runtime and the tools visible to the program.
type Config struct {
	// Tools are the tools a program may call by name (run_code itself is
	// excluded automatically). Nil means programs can compute but call no tools.
	Tools []rig.Tool
	// Gate checks each inner tool call; nil allows every call (only use that in
	// trusted/auto-approval compositions).
	Gate rig.Gate
	// Python and Node optionally pin a runtime executable. Empty values are
	// detected on PATH when the language needs them.
	Python string
	Node   string
	// Timeout is the default program budget (DefaultTimeout when zero).
	Timeout time.Duration
	// Root is the initial working directory for the program ("" = process cwd).
	Root string
}

// Tool is the run_code tool.
type Tool struct {
	config Config
	byName map[string]rig.Tool
}

var _ rig.Tool = (*Tool)(nil)

// New builds the run_code tool.
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

// Tool is the named constructor; it is New.
func NewTool(config Config) *Tool { return New(config) }

// Name implements rig.Tool.
func (t *Tool) Name() string { return ToolName }

// Description implements rig.Tool.
func (t *Tool) Description() string {
	return "Run a small program in a system-installed Python 3 or Node.js runtime. The program can call the " +
		"agent's tools through `tools.<name>(...)`: Python uses keyword arguments, Node uses one object argument " +
		"and await. Inner tool calls are checked by the approval gate. Use it for batch tool orchestration that " +
		"would otherwise take many tool rounds; use the Go workflow tool for plain multi-step orchestration."
}

// Schema implements rig.Tool.
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

// runtime describes one supported language harness.
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

// Invoke implements rig.Tool.
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

	dir, err := os.MkdirTemp("", "rig-run-code-")
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

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(runCtx, selected.path, scriptPath)
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
	if runCtx.Err() == context.DeadlineExceeded {
		return rig.ToolResult{Content: truncate(output.String()) + "\n[run_code timeout: program exceeded " + timeout.String() + "]", IsError: true}, nil
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
		return rig.ToolResult{Content: content + "\n[run_code exit: " + waitErr.Error() + "]", IsError: true}, nil
	}
	if strings.TrimSpace(content) == "" {
		return toolutil.TextResult("(program produced no output)"), nil
	}
	return toolutil.TextResult(strings.TrimRight(content, "\n")), nil
}

// selectRuntime picks an explicitly requested language or the first available
// detected runtime (Python 3 preferred, then Node).
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
