// Command rigmcp 把一个工作区的**只读**工具子集暴露为 MCP server（stdio）。
//
// 它是最小参考实现。MCP 调用方（另一个 agent、编辑器、CLI）**没有本仓库的审批
// UI**，因此这里的默认姿态是"宁可少暴露"：
//
//   - 只暴露一个显式白名单里的只读工具（read_file/list_dir/glob/grep/inspect/
//     report/sandbox_status）。bash、pwsh、write_file、edit_file、apply_patch、
//     run_code、terminal_*、job_*、undo、request_sandbox_access、memory、
//     session_search、subagent、web_fetch、web_search 都**不在**默认列表里
//     ——它们要么会改文件、要么会执行/联网、要么会泄露跨会话内容。
//   - 双层防御：白名单决定"构造并注册哪些工具"；此外每个工具外面还包一层
//     gate.AllowTools(最终工具名)，即使将来有人绕过过滤，调用也会被静态门禁拒绝。
//   - policy 一定生效：read-only（或显式 -mode workspace-write）+ roots +
//     -deny 的 glob 直接进 sandbox.WithDeniedGlobs，因此 read_file 读不到凭据类文件。
//   - 每次 tools/call 都有超时（-tool-timeout，默认 60s），超时转成工具级错误，
//     不会让一个挂住的工具一直占住连接。
//   - 日志一律写 stderr：stdout 只能承载 JSON-RPC 行。可选的 -audit-log 每次调用
//     追加一行 JSON（时间/工具名/参数长度/耗时/isError/outcome），便于事后追查。
//
// 要暴露写/执行工具，必须同时给 -allow-writes **和** -mode workspace-write **和**
// 显式的 -allow 列表——三道都到位才放行，缺一条就报错退出。
//
// 用法：
//
//	rigmcp -root . -deny '**/.env' -deny '**/*.pem' -audit-log /var/log/rigmcp.jsonl
//	printf '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}\n' | rigmcp
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/mcpserver"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/full"
)

// DefaultToolTimeout 是单次 tools/call 的默认超时。
const DefaultToolTimeout = 60 * time.Second

// defaultReadOnlyTools 是默认暴露给 MCP 客户端的工具白名单（顺序即 tools/list 顺序）。
//
// 刻意逐条手写而不是复用 gate.SandboxReadOnlyTools：MCP 的暴露面比"本地审批流里的
// 只读工具"更敏感（调用方没有审批 UI），因此这份清单要能被人一眼审阅。
var defaultReadOnlyTools = []string{
	"read_file",
	"list_dir",
	"glob",
	"grep",
	"inspect",
	"report",
	"sandbox_status",
}

// dangerousTools 是需要"写/执行"能力才允许暴露的工具：即使宿主把它们写进 -allow，
// 也必须同时给 -allow-writes 且 -mode workspace-write，否则启动即报错。
var dangerousTools = map[string]bool{
	"bash":                   true,
	"pwsh":                   true,
	"write_file":             true,
	"edit_file":              true,
	"str_replace_editor":     true,
	"apply_patch":            true,
	"run_code":               true,
	"undo":                   true,
	"request_sandbox_access": true,
	"memory":                 true,
}

// dangerousPrefixes 是按前缀判定的有副作用工具族（terminal_*、job_*）。
var dangerousPrefixes = []string{"terminal_", "job_"}

func main() {
	log.SetOutput(os.Stderr)
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		log.Fatalf("rigmcp: %v", err)
	}
}

// config 是解析后的命令行配置。
type config struct {
	root         string
	mode         string
	allowWrites  bool
	allow        stringList
	deny         stringList
	toolTimeout  time.Duration
	auditLogPath string
	instructions string
}

// run 解析命令行并启动 server。
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	cfg := config{}
	flags := flag.NewFlagSet("rigmcp", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&cfg.root, "root", ".", "workspace root exposed to MCP clients")
	flags.StringVar(&cfg.mode, "mode", string(sandbox.ModeReadOnly), "sandbox mode: read-only or workspace-write")
	flags.BoolVar(&cfg.allowWrites, "allow-writes", false, "expose write/execute tools too (requires -mode workspace-write and an explicit -allow list)")
	flags.Var(&cfg.allow, "allow", "extra tool names to expose (comma-separated, repeatable); dangerous tools require -allow-writes")
	flags.Var(&cfg.deny, "deny", "denied glob pattern relative to the root, e.g. '**/.env' (repeatable); appended to sandbox.WithDeniedGlobs")
	flags.DurationVar(&cfg.toolTimeout, "tool-timeout", DefaultToolTimeout, "per tools/call timeout (0 uses the default, negative disables)")
	flags.StringVar(&cfg.auditLogPath, "audit-log", "", "append one JSON line per tools/call to this file (empty disables)")
	flags.StringVar(&cfg.instructions, "instructions", "", "optional text returned by the initialize response")
	if err := flags.Parse(args); err != nil {
		return err
	}

	absRoot, err := resolveRoot(cfg.root)
	if err != nil {
		return err
	}
	policyMode, err := resolveMode(cfg.mode)
	if err != nil {
		return err
	}
	exposed, err := resolveExposedTools(cfg, policyMode)
	if err != nil {
		return err
	}

	policy := sandbox.New(
		sandbox.WithRoots(absRoot),
		sandbox.WithMode(policyMode),
		sandbox.WithDeniedGlobs(cfg.deny...),
	)
	// SandboxGrants 打开只是为了拿到只读诊断工具 sandbox_status；request_sandbox_access
	// 会在下面的白名单过滤里被扔掉（除非宿主显式 -allow 它——那需要 -allow-writes）。
	all := full.All(full.AllOptions{Policy: policy, Root: absRoot, SandboxGrants: true})

	selected, missing := selectTools(all, exposed)
	if len(missing) > 0 {
		return fmt.Errorf("unknown or unavailable tool(s) in -allow: %s (available: %s)",
			strings.Join(missing, ", "), strings.Join(toolNames(all), ", "))
	}
	if len(selected) == 0 {
		return errors.New("no tools selected; check -allow / -mode")
	}

	audit, err := openAuditLog(cfg.auditLogPath)
	if err != nil {
		return err
	}
	if audit != nil {
		defer func() { _ = audit.Close() }()
	}

	// 双层防御：白名单决定"构造哪些"，gate 决定"放行哪些"。MCP 没有审批 UI，
	// 因此用静态名单门禁（AllowTools）。
	toolGate := gate.AllowTools(toolNames(selected)...)
	wrapped := make([]rig.Tool, 0, len(selected))
	for _, tool := range selected {
		wrapped = append(wrapped, &governedTool{
			tool:    tool,
			gate:    toolGate,
			timeout: cfg.toolTimeout,
			audit:   audit,
		})
	}

	opts := []mcpserver.Option{mcpserver.WithServerInfo("rigmcp", rig.Version)}
	if text := strings.TrimSpace(cfg.instructions); text != "" {
		opts = append(opts, mcpserver.WithInstructions(text))
	}
	server, err := mcpserver.New(wrapped, opts...)
	if err != nil {
		return err
	}

	log.Printf("rigmcp: %d tool(s) exposed: %s", len(wrapped), strings.Join(toolNames(wrapped), ", "))
	log.Printf("rigmcp: root=%s mode=%s denied_globs=%v tool_timeout=%s", absRoot, policyMode, cfg.deny, cfg.toolTimeout)
	if cfg.auditLogPath != "" {
		log.Printf("rigmcp: audit log -> %s", cfg.auditLogPath)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.Serve(ctx, stdin, stdout)
}

// stringList 是支持重复传参的字符串 flag。
type stringList []string

// String 实现 flag.Value。
func (l *stringList) String() string { return strings.Join(*l, ",") }

// Set 实现 flag.Value。
func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

// resolveMode 校验 -mode。
func resolveMode(mode string) (sandbox.Mode, error) {
	policyMode := sandbox.Mode(strings.TrimSpace(mode))
	switch policyMode {
	case sandbox.ModeReadOnly, sandbox.ModeWorkspaceWrite:
		return policyMode, nil
	default:
		return "", fmt.Errorf("unsupported mode %q (want read-only or workspace-write)", mode)
	}
}

// resolveExposedTools 计算最终要暴露的工具名列表（去重、保持顺序）。
//
// 默认只读模式：默认白名单 + -allow 追加项（危险工具必须先 -allow-writes）。
// -allow-writes：必须以显式 -allow 列表为准（默认白名单不自动参与），避免"打开写"
// 时顺手把一堆工具都放出去。
func resolveExposedTools(cfg config, policyMode sandbox.Mode) ([]string, error) {
	extras, err := splitToolNames(cfg.allow)
	if err != nil {
		return nil, err
	}
	if cfg.allowWrites {
		if policyMode != sandbox.ModeWorkspaceWrite {
			return nil, errors.New("-allow-writes requires -mode workspace-write")
		}
		if len(extras) == 0 {
			return nil, errors.New("-allow-writes requires at least one explicit -allow tool name")
		}
	} else {
		for _, name := range extras {
			if isDangerousTool(name) {
				return nil, fmt.Errorf("tool %q writes or executes; add -allow-writes and -mode workspace-write to expose it", name)
			}
		}
	}

	exposed := make([]string, 0, len(defaultReadOnlyTools)+len(extras))
	seen := make(map[string]bool, len(defaultReadOnlyTools)+len(extras))
	appendName := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		exposed = append(exposed, name)
	}
	if !cfg.allowWrites {
		for _, name := range defaultReadOnlyTools {
			appendName(name)
		}
	}
	for _, name := range extras {
		appendName(name)
	}
	return exposed, nil
}

// splitToolNames 把 -allow 的多个值拆成工具名（支持逗号分隔与重复传参）。
func splitToolNames(values []string) ([]string, error) {
	names := make([]string, 0, len(values))
	for _, value := range values {
		for _, name := range strings.Split(value, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if strings.ContainsAny(name, " \t/\\") {
				return nil, fmt.Errorf("invalid tool name %q in -allow", name)
			}
			names = append(names, name)
		}
	}
	return names, nil
}

// isDangerousTool 报告工具是否需要写/执行能力。
func isDangerousTool(name string) bool {
	if dangerousTools[name] {
		return true
	}
	for _, prefix := range dangerousPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// resolveRoot 把 -root 规范成绝对路径并确认是目录。
func resolveRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("workspace root %q: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace root %q is not a directory", abs)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("workspace root %q: resolve symlinks: %w", abs, err)
	}
	return resolved, nil
}

// selectTools 按名单过滤工具，返回选中的工具与"名单里但没构造出来"的名字。
func selectTools(tools []rig.Tool, names []string) ([]rig.Tool, []string) {
	byName := make(map[string]rig.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	selected := make([]rig.Tool, 0, len(names))
	var missing []string
	for _, name := range names {
		tool, ok := byName[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		selected = append(selected, tool)
	}
	return selected, missing
}

// toolNames 返回工具名列表。
func toolNames(tools []rig.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	return names
}

// governedTool 是 MCP 暴露面的每个工具的共同包装：静态门禁 + 单次调用超时 + 审计。
//
// 它实现 rig.Tool，因此对 mcpserver 完全透明。包装顺序是刻意的：门禁→超时→调用，
// 被拒绝的调用不消耗超时预算（审计里 outcome="gate_denied"）。
type governedTool struct {
	tool    rig.Tool
	gate    rig.Gate
	timeout time.Duration
	audit   *auditLog
}

// Name 实现 rig.Tool。
func (t *governedTool) Name() string { return t.tool.Name() }

// Description 实现 rig.Tool。
func (t *governedTool) Description() string { return t.tool.Description() }

// Schema 实现 rig.Tool。
func (t *governedTool) Schema() map[string]any { return t.tool.Schema() }

// Invoke 实现 rig.Tool。
func (t *governedTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	started := time.Now()
	if t.gate != nil {
		decision, err := t.gate.Approve(ctx, t.tool, call)
		if err != nil || (decision != rig.DecisionAllow && decision != rig.DecisionAllowAlways) {
			t.record(call, started, true, "gate_denied")
			if err != nil {
				return rig.ToolResult{
					Content: fmt.Sprintf("rigmcp: tool %q was denied by the MCP gate: %v", t.tool.Name(), err),
					IsError: true,
				}, nil
			}
			return rig.ToolResult{
				Content: fmt.Sprintf("rigmcp: tool %q is not exposed by this MCP server", t.tool.Name()),
				IsError: true,
			}, nil
		}
	}

	callCtx := ctx
	cancel := func() {}
	if t.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, t.timeout)
	}
	defer cancel()

	result, err := t.tool.Invoke(callCtx, call)
	if t.timeout > 0 && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		t.record(call, started, true, "timeout")
		return rig.ToolResult{
			Content: fmt.Sprintf("rigmcp: tool %q timed out after %s", t.tool.Name(), t.timeout),
			IsError: true,
		}, nil
	}
	outcome := "ok"
	isError := result.IsError
	if err != nil || isError {
		outcome = "tool_error"
		isError = true
	}
	t.record(call, started, isError, outcome)
	return result, err
}

// record 写一行审计（audit 为 nil 时是空操作）。
func (t *governedTool) record(call rig.ToolCall, started time.Time, isError bool, outcome string) {
	if t.audit == nil {
		return
	}
	t.audit.write(auditEntry{
		Time:       time.Now().UTC().Format(time.RFC3339Nano),
		Tool:       t.tool.Name(),
		ArgsBytes:  len(call.Arguments),
		DurationMS: time.Since(started).Milliseconds(),
		IsError:    isError,
		Outcome:    outcome,
	})
}

// auditEntry 是一条审计记录。
//
// 刻意只记参数**长度**而不是内容：MCP 调用参数里可能有凭据或大段代码，审计日志
// 不应该变成第二份敏感数据副本。
type auditEntry struct {
	Time       string `json:"time"`
	Tool       string `json:"tool"`
	ArgsBytes  int    `json:"args_bytes"`
	DurationMS int64  `json:"duration_ms"`
	IsError    bool   `json:"is_error"`
	Outcome    string `json:"outcome"`
}

// auditLog 是追加写的 JSONL 审计日志（并发安全）。
type auditLog struct {
	mu     sync.Mutex
	file   *os.File
	failed bool
}

// openAuditLog 打开（或创建）审计日志；path 为空时返回 (nil, nil)。
func openAuditLog(path string) (*auditLog, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log %q: %w", path, err)
	}
	return &auditLog{file: file}, nil
}

// write 追加一行 JSON；写失败只在 stderr 报一次（不影响调用本身）。
func (a *auditLog) write(entry auditEntry) {
	if a == nil || a.file == nil {
		return
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.file.Write(append(data, '\n')); err != nil && !a.failed {
		a.failed = true
		log.Printf("rigmcp: audit log write failed: %v", err)
	}
}

// Close 关闭审计日志。
func (a *auditLog) Close() error {
	if a == nil || a.file == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	err := a.file.Close()
	a.file = nil
	return err
}
