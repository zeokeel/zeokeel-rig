package main

// rigmcp 的单元测试：白名单/危险工具判定/flags 组合校验/门禁/超时/审计。
//
// 这些用例只覆盖纯逻辑与包装层（不启动真实的 MCP 会话），端到端冒烟在
// README/报告里用 printf | rigmcp 的方式做。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// fakeTool 是测试用工具。
type fakeTool struct {
	name   string
	invoke func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error)
}

func (f *fakeTool) Name() string           { return f.name }
func (f *fakeTool) Description() string    { return "fake " + f.name }
func (f *fakeTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (f *fakeTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if f.invoke != nil {
		return f.invoke(ctx, call)
	}
	return rig.ToolResult{Content: "ok"}, nil
}

// TestDefaultWhitelistIsReadOnly 钉住默认暴露面：只有明确的只读工具，危险工具一个都不在。
func TestDefaultWhitelistIsReadOnly(t *testing.T) {
	exposed, err := resolveExposedTools(config{}, sandbox.ModeReadOnly)
	if err != nil {
		t.Fatalf("resolveExposedTools: %v", err)
	}
	want := []string{"read_file", "list_dir", "glob", "grep", "inspect", "report", "sandbox_status"}
	if len(exposed) != len(want) {
		t.Fatalf("exposed = %v, want %v", exposed, want)
	}
	for index := range want {
		if exposed[index] != want[index] {
			t.Fatalf("exposed = %v, want %v", exposed, want)
		}
	}
	forbidden := []string{
		"bash", "pwsh", "write_file", "edit_file", "apply_patch", "run_code",
		"terminal_open", "terminal_send", "job_start", "job_output", "undo",
		"request_sandbox_access", "memory", "session_search", "subagent",
		"web_fetch", "web_search",
	}
	for _, name := range forbidden {
		for _, selected := range exposed {
			if selected == name {
				t.Fatalf("default whitelist exposes %q", name)
			}
		}
	}
}

// TestResolveExposedToolsExtras 证明 -allow 追加项会去重并保持在默认列表之后。
func TestResolveExposedToolsExtras(t *testing.T) {
	cfg := config{allow: stringList{"web_fetch,read_file", "web_search"}}
	exposed, err := resolveExposedTools(cfg, sandbox.ModeReadOnly)
	if err != nil {
		t.Fatalf("resolveExposedTools: %v", err)
	}
	joined := strings.Join(exposed, ",")
	if !strings.HasSuffix(joined, "web_fetch,web_search") {
		t.Fatalf("exposed = %v, want web_fetch/web_search appended after the defaults", exposed)
	}
	count := 0
	for _, name := range exposed {
		if name == "read_file" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("read_file appears %d times, want deduped", count)
	}
}

// TestAllowWritesRequiresExplicitListAndMode 覆盖 -allow-writes 的三道门槛。
func TestAllowWritesRequiresExplicitListAndMode(t *testing.T) {
	cases := []struct {
		name string
		cfg  config
		mode sandbox.Mode
	}{
		{"write mode without allow", config{allowWrites: true}, sandbox.ModeWorkspaceWrite},
		{"allow writes in read-only mode", config{allowWrites: true, allow: stringList{"write_file"}}, sandbox.ModeReadOnly},
	}
	for _, tc := range cases {
		if _, err := resolveExposedTools(tc.cfg, tc.mode); err == nil {
			t.Fatalf("%s: accepted, want an error", tc.name)
		}
	}
	exposed, err := resolveExposedTools(config{allowWrites: true, allow: stringList{"write_file,bash"}}, sandbox.ModeWorkspaceWrite)
	if err != nil {
		t.Fatalf("valid allow-writes rejected: %v", err)
	}
	if strings.Join(exposed, ",") != "write_file,bash" {
		t.Fatalf("exposed = %v, want exactly the explicit list (no default whitelist)", exposed)
	}
}

// TestDangerousToolsNeedAllowWrites 证明默认只读模式下显式放行 bash 会被拒绝。
func TestDangerousToolsNeedAllowWrites(t *testing.T) {
	for _, name := range []string{"bash", "terminal_send", "job_kill", "undo", "request_sandbox_access", "app ly"} {
		_, err := resolveExposedTools(config{allow: stringList{name}}, sandbox.ModeReadOnly)
		if name == "app ly" {
			if err == nil {
				t.Fatalf("invalid tool name %q accepted", name)
			}
			continue
		}
		if err == nil {
			t.Fatalf("dangerous tool %q accepted without -allow-writes", name)
		}
		if !strings.Contains(err.Error(), "allow-writes") {
			t.Fatalf("error for %q = %v, want it to mention -allow-writes", name, err)
		}
	}
}

// TestSplitToolNames 覆盖逗号、重复传参与非法字符。
func TestSplitToolNames(t *testing.T) {
	names, err := splitToolNames(stringList{"a,b", " c ", "a"})
	if err != nil {
		t.Fatalf("splitToolNames: %v", err)
	}
	if strings.Join(names, ",") != "a,b,c,a" {
		t.Fatalf("names = %v", names)
	}
	if _, err := splitToolNames(stringList{"bad/name"}); err == nil {
		t.Fatal("path-like tool name accepted")
	}
}

// TestSelectToolsReportsMissing 证明名单里不存在的工具会被明确报出来（而不是静默少暴露）。
func TestSelectToolsReportsMissing(t *testing.T) {
	all := []rig.Tool{&fakeTool{name: "read_file"}, &fakeTool{name: "grep"}}
	selected, missing := selectTools(all, []string{"read_file", "nope"})
	if len(selected) != 1 || selected[0].Name() != "read_file" {
		t.Fatalf("selected = %v", toolNames(selected))
	}
	if len(missing) != 1 || missing[0] != "nope" {
		t.Fatalf("missing = %v, want [nope]", missing)
	}
}

// TestGovernedToolGateDenies 证明门禁拒绝时返回工具级错误（不调用底层工具）。
func TestGovernedToolGateDenies(t *testing.T) {
	called := false
	base := &fakeTool{name: "read_file", invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
		called = true
		return rig.ToolResult{Content: "should not run"}, nil
	}}
	// 门禁只允许别的工具名 → 本次调用被拒。
	wrapped := &governedTool{tool: base, gate: gate.AllowTools("grep")}
	result, err := wrapped.Invoke(context.Background(), rig.ToolCall{Name: "read_file", Arguments: "{}"})
	if err != nil {
		t.Fatalf("Invoke returned Go error: %v", err)
	}
	if !result.IsError || called {
		t.Fatalf("denied call: called=%v result=%+v", called, result)
	}
	if !strings.Contains(result.Content, "not exposed") {
		t.Fatalf("content = %q", result.Content)
	}
}

// TestGovernedToolAllowsWhitelistedName 证明在名单内时门禁放行、正常调用。
func TestGovernedToolAllowsWhitelistedName(t *testing.T) {
	base := &fakeTool{name: "read_file"}
	wrapped := &governedTool{tool: base, gate: gate.AllowTools("read_file")}
	result, err := wrapped.Invoke(context.Background(), rig.ToolCall{Name: "read_file", Arguments: `{"path":"x"}`})
	if err != nil || result.IsError {
		t.Fatalf("Invoke = %+v, %v", result, err)
	}
}

// TestGovernedToolTimeoutProducesToolError 证明超时被转成工具级错误，且审计记为 timeout。
func TestGovernedToolTimeoutProducesToolError(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := openAuditLog(auditPath)
	if err != nil {
		t.Fatalf("openAuditLog: %v", err)
	}
	defer func() { _ = audit.Close() }()

	base := &fakeTool{name: "slow", invoke: func(ctx context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
		<-ctx.Done()
		return rig.ToolResult{Content: "late"}, nil
	}}
	wrapped := &governedTool{tool: base, gate: gate.AllowTools("slow"), timeout: 20 * time.Millisecond, audit: audit}
	started := time.Now()
	result, err := wrapped.Invoke(context.Background(), rig.ToolCall{Name: "slow", Arguments: `{"x":1}`})
	if err != nil {
		t.Fatalf("Invoke returned Go error: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "timed out") {
		t.Fatalf("result = %+v, want a timeout tool error", result)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}

	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1 (%s)", len(lines), data)
	}
	var entry auditEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("audit line is not JSON: %v (%s)", err, lines[0])
	}
	if entry.Tool != "slow" || entry.Outcome != "timeout" || !entry.IsError {
		t.Fatalf("audit entry = %+v", entry)
	}
	if entry.ArgsBytes != len(`{"x":1}`) {
		t.Fatalf("args_bytes = %d, want the argument length", entry.ArgsBytes)
	}
	// 审计只记长度，不记内容。
	if strings.Contains(lines[0], `"x"`) {
		t.Fatalf("audit line leaked the arguments: %s", lines[0])
	}
}

// TestAuditLogConcurrentWrites 证明审计日志在并发下不串行破坏 JSONL。
func TestAuditLogConcurrentWrites(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := openAuditLog(auditPath)
	if err != nil {
		t.Fatalf("openAuditLog: %v", err)
	}
	const workers = 8
	done := make(chan struct{})
	for index := 0; index < workers; index++ {
		go func(slot int) {
			defer func() { done <- struct{}{} }()
			for round := 0; round < 10; round++ {
				audit.write(auditEntry{Time: "t", Tool: "read_file", DurationMS: 0})
			}
		}(index)
	}
	for index := 0; index < workers; index++ {
		<-done
	}
	if err := audit.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != workers*10 {
		t.Fatalf("audit lines = %d, want %d", len(lines), workers*10)
	}
	for _, line := range lines {
		var entry auditEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("corrupted audit line: %v (%q)", err, line)
		}
	}
}

// TestOpenAuditLogEmptyPathIsNoop 证明不传 -audit-log 时审计是空操作。
func TestOpenAuditLogEmptyPathIsNoop(t *testing.T) {
	audit, err := openAuditLog("")
	if err != nil || audit != nil {
		t.Fatalf("openAuditLog(\"\") = (%v, %v), want (nil, nil)", audit, err)
	}
	audit.write(auditEntry{Tool: "x"})
	if err := audit.Close(); err != nil {
		t.Fatalf("Close on nil audit: %v", err)
	}
}

// TestResolveMode 覆盖模式校验。
func TestResolveMode(t *testing.T) {
	for _, ok := range []string{"read-only", "workspace-write"} {
		if _, err := resolveMode(ok); err != nil {
			t.Fatalf("resolveMode(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "danger-full-access", "READ ONLY"} {
		if _, err := resolveMode(bad); err == nil {
			t.Fatalf("resolveMode(%q) accepted", bad)
		}
	}
}

// TestRunRejectsBadFlags 覆盖启动期校验（不进入 Serve）。
func TestRunRejectsBadFlags(t *testing.T) {
	root := t.TempDir()
	cases := [][]string{
		{"-root", root, "-mode", "danger-full-access"},
		{"-root", root, "-allow-writes"},
		{"-root", filepath.Join(root, "missing")},
		{"-root", root, "-allow", "bash"},
		{"-root", root, "-allow", "does-not-exist"},
	}
	for _, args := range cases {
		err := run(args, strings.NewReader(""), &strings.Builder{})
		if err == nil {
			t.Fatalf("run(%v) succeeded, want an error", args)
		}
		if errors.Is(err, context.Canceled) {
			t.Fatalf("run(%v) returned the cancellation sentinel: %v", args, err)
		}
	}
}
