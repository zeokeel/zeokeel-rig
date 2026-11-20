package kit_test

// D2：`kit.Runtime` 的运行期变更面——多可写根、MCP 客户端热增删、runner 派生。
//
// 三条承诺：
//  1. 多 root：`AddRoot` 只影响**之后新建**的 Agent，`RemoveRoot` 对**已存在**的
//     Agent 立刻生效（撤销永远是收紧，可以即时生效）；
//  2. MCP 热增删：增删之后新建的 Agent 工具面随之变化；已存在的 Agent 工具面在
//     构造期固定（不强改 active 集），只留一条 notice；
//  3. runner 派生：`DeriveRunner` 给"这个 Agent 要更严的档位"用，不动共享 runner。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/sandbox/osrunner"
	"github.com/zeokeel/zeokeel-rig/tools/mcp"
)

// canonicalPath 把期望路径归一到策略会返回的写法（解析符号链接）。
func canonicalPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", path, err)
	}
	return resolved
}

// containsPath 报告 roots 里是否有 path（按平台路径语义比较）。
func containsPath(roots []string, path string) bool {
	for _, root := range roots {
		if sandbox.PathEqual(root, path) {
			return true
		}
	}
	return false
}

// TestRuntimeAddRootAppliesToNewAgents：AddRoot 之后新建的 Agent 拿到额外可写根，
// 已存在的 Agent 不被追溯修改。
func TestRuntimeAddRootAppliesToNewAgents(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	rt, err := kit.NewRuntime(kit.RuntimeConfig{})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	cfg := kit.Config{Model: "test-model", Root: root, Gate: gate.AllowAll()}
	before, err := rt.NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent(before): %v", err)
	}
	t.Cleanup(func() { _ = before.Shutdown(context.Background()) })
	if containsPath(before.Policy().Roots(), canonicalPath(t, extra)) {
		t.Fatalf("AddRoot 之前 Agent 就已经拥有 %q", extra)
	}

	if err := rt.AddRoot(extra); err != nil {
		t.Fatalf("AddRoot(%q): %v", extra, err)
	}
	after, err := rt.NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent(after): %v", err)
	}
	t.Cleanup(func() { _ = after.Shutdown(context.Background()) })

	roots := after.Policy().Roots()
	if !containsPath(roots, canonicalPath(t, root)) {
		t.Fatalf("新 Agent 丢了工作区根 %q: %v", root, roots)
	}
	if !containsPath(roots, canonicalPath(t, extra)) {
		t.Fatalf("AddRoot 之后新建的 Agent 没有拿到 %q: %v", extra, roots)
	}
	if containsPath(before.Policy().Roots(), canonicalPath(t, extra)) {
		t.Fatalf("AddRoot 追溯修改了已存在的 Agent: %v", before.Policy().Roots())
	}
}

// rootFenceRunner 是模拟 OS 层可写面围栏的 runner：测试关心的目标不可写就拒绝执行。
// kit 的承诺是"策略围栏由 runner 执行"；撤销可写根之后，runner 看到的就是收紧后的
// 可写面（而不是 kit 自己另判一套）。
type rootFenceRunner struct {
	target string
}

func (r rootFenceRunner) Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	if policy == nil || !policy.AllowsWrite(r.target) {
		return sandbox.ConfinedCommand{}, fmt.Errorf("sandbox: write to %q is not allowed by the policy", r.target)
	}
	return sandbox.ConfinedCommand{Argv: append([]string(nil), argv...), Enforcement: sandbox.EnforcementFull}, nil
}

// TestRuntimeRemoveRootRevokesExistingAgent：RemoveRoot 之后，已存在 Agent 的 bash
// 写入该 root 被拒。
func TestRuntimeRemoveRootRevokesExistingAgent(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	target := filepath.Join(extra, "out.txt")
	rt, err := kit.NewRuntime(kit.RuntimeConfig{SandboxRunner: rootFenceRunner{target: target}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	if err := rt.AddRoot(extra); err != nil {
		t.Fatalf("AddRoot(%q): %v", extra, err)
	}
	agent, err := rt.NewAgent(kit.Config{Model: "test-model", Root: root, Gate: gate.AllowAll()})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = agent.Shutdown(context.Background()) })

	bash, ok := agent.Tool("bash")
	if !ok {
		t.Fatal("agent 没有装配 bash")
	}
	write := map[string]any{"command": "touch " + target, "description": "write into the extra root"}
	if result := runtimeInvoke(t, bash, write); result.IsError {
		t.Fatalf("撤销前写入 %q 就被拒了: %q", target, result.Content)
	}

	if err := rt.RemoveRoot(extra); err != nil {
		t.Fatalf("RemoveRoot(%q): %v", extra, err)
	}
	if containsPath(agent.Policy().Roots(), canonicalPath(t, extra)) {
		t.Fatalf("RemoveRoot 之后已存在 Agent 仍拥有 %q: %v", extra, agent.Policy().Roots())
	}
	result := runtimeInvoke(t, bash, write)
	if !result.IsError {
		t.Fatalf("RemoveRoot 之后 bash 仍能写入 %q: %q", target, result.Content)
	}
	if !strings.Contains(result.Content, "not allowed") {
		t.Fatalf("拒绝文案没有来自 runner 的可写面判定: %q", result.Content)
	}
	// 撤销之后新建的 Agent 也不该再拿到它。
	later, err := rt.NewAgent(kit.Config{Model: "test-model", Root: root, Gate: gate.AllowAll()})
	if err != nil {
		t.Fatalf("NewAgent(later): %v", err)
	}
	t.Cleanup(func() { _ = later.Shutdown(context.Background()) })
	if containsPath(later.Policy().Roots(), canonicalPath(t, extra)) {
		t.Fatalf("RemoveRoot 之后新建的 Agent 仍拿到 %q: %v", extra, later.Policy().Roots())
	}
}

// TestConfigRootsSupersedesRoot：Config.Roots 与 Config.Root 同时出现时以 Roots 为
// 准并告警——两个字段都表达"工作区"时，静默取其一会让宿主的配置读起来像是另一件事。
func TestConfigRootsSupersedesRoot(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	stale := t.TempDir()
	warnings := captureSlog(t)

	agent, err := kit.New(kit.Config{
		Model:    "test-model",
		Provider: &scriptProvider{},
		Root:     stale,
		Roots:    []string{first, second},
		Gate:     gate.AllowAll(),
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	t.Cleanup(func() { _ = agent.Shutdown(context.Background()) })

	roots := agent.Policy().Roots()
	if !containsPath(roots, canonicalPath(t, first)) || !containsPath(roots, canonicalPath(t, second)) {
		t.Fatalf("Policy.Roots = %v, want it to contain %q and %q", roots, first, second)
	}
	if containsPath(roots, canonicalPath(t, stale)) {
		t.Fatalf("Config.Root 没有被 Roots 覆盖: %v", roots)
	}
	if got := warnings(); !strings.Contains(got, "Config.Roots") || !strings.Contains(got, "Config.Root") {
		t.Fatalf("同时设置 Root 与 Roots 时应当告警并点名字段，实际日志: %q", got)
	}
}

// fakeMCPHandler 是最小的 Streamable HTTP MCP 服务端：只回答握手与 tools/list。
func fakeMCPHandler(t *testing.T, server string, toolNames []string) http.HandlerFunc {
	t.Helper()
	writeResult := func(w http.ResponseWriter, id json.RawMessage, result any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}); err != nil {
			t.Errorf("encode result: %v", err)
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(body, &msg); err != nil {
			t.Errorf("decode body %q: %v", body, err)
			return
		}
		switch msg.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "d2-session")
			writeResult(w, msg.ID, map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": server, "version": "0.0.0"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			tools := make([]map[string]any, 0, len(toolNames))
			for _, name := range toolNames {
				tools = append(tools, map[string]any{
					"name":        name,
					"description": "fake tool " + name,
					"inputSchema": map[string]any{"type": "object"},
				})
			}
			writeResult(w, msg.ID, map[string]any{"tools": tools})
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"error":   map[string]any{"code": -32601, "message": "method not found"},
			})
		}
	}
}

// newFakeMCPClient 起一个假 MCP 服务端并连上它。
func newFakeMCPClient(t *testing.T, server string, toolNames ...string) *mcp.Client {
	t.Helper()
	endpoint := httptest.NewServer(fakeMCPHandler(t, server, toolNames))
	t.Cleanup(endpoint.Close)
	client, err := mcp.StartHTTP(context.Background(), mcp.HTTPConfig{URL: endpoint.URL, Timeout: mcp.DefaultTimeout})
	if err != nil {
		t.Fatalf("mcp.StartHTTP: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestRuntimeAddMCPClientExposesToolsToNewAgents：AddMCPClient 之后新建的 Agent
// 工具面包含该前缀工具；已存在的 Agent 只收到一条 notice。
func TestRuntimeAddMCPClientExposesToolsToNewAgents(t *testing.T) {
	root := t.TempDir()
	rt, err := kit.NewRuntime(kit.RuntimeConfig{})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	var events bytes.Buffer
	cfg := kit.Config{Model: "test-model", Root: root, Gate: gate.AllowAll(), EventWriter: &events}
	before, err := rt.NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent(before): %v", err)
	}
	t.Cleanup(func() { _ = before.Shutdown(context.Background()) })

	client := newFakeMCPClient(t, "fake", "echo")
	name := "mcp__fake__echo"
	if slices.Contains(before.RegisteredTools(), name) {
		t.Fatalf("注册 MCP 之前 %q 就已经在工具面里", name)
	}
	if err := rt.AddMCPClient("fake", client); err != nil {
		t.Fatalf("AddMCPClient: %v", err)
	}
	after, err := rt.NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent(after): %v", err)
	}
	t.Cleanup(func() { _ = after.Shutdown(context.Background()) })
	if !slices.Contains(after.RegisteredTools(), name) {
		t.Fatalf("新建 Agent 的工具面缺少 %q: %v", name, after.RegisteredTools())
	}
	// 已存在 Agent 的工具面在构造期固定：不强改 active 集，但要留一条 notice。
	if slices.Contains(before.RegisteredTools(), name) {
		t.Fatalf("AddMCPClient 不应追溯修改已存在 Agent 的工具面: %v", before.RegisteredTools())
	}
	if got := events.String(); !strings.Contains(got, "notice") || !strings.Contains(got, "fake") {
		t.Fatalf("已存在 Agent 没有收到 MCP 增删 notice: %q", got)
	}
}

// TestRuntimeRemoveMCPClientHidesToolsFromNewAgents：RemoveMCPClient 之后新建的
// Agent 工具面不含该前缀工具；已存在的 Agent 不被追溯修改。
func TestRuntimeRemoveMCPClientHidesToolsFromNewAgents(t *testing.T) {
	root := t.TempDir()
	rt, err := kit.NewRuntime(kit.RuntimeConfig{})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	client := newFakeMCPClient(t, "fake", "echo")
	name := "mcp__fake__echo"
	if err := rt.AddMCPClient("fake", client); err != nil {
		t.Fatalf("AddMCPClient: %v", err)
	}
	cfg := kit.Config{Model: "test-model", Root: root, Gate: gate.AllowAll()}
	before, err := rt.NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent(before): %v", err)
	}
	t.Cleanup(func() { _ = before.Shutdown(context.Background()) })
	if !slices.Contains(before.RegisteredTools(), name) {
		t.Fatalf("AddMCPClient 之后新建的 Agent 缺少 %q: %v", name, before.RegisteredTools())
	}

	if err := rt.RemoveMCPClient("fake"); err != nil {
		t.Fatalf("RemoveMCPClient: %v", err)
	}
	after, err := rt.NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent(after): %v", err)
	}
	t.Cleanup(func() { _ = after.Shutdown(context.Background()) })
	if slices.Contains(after.RegisteredTools(), name) {
		t.Fatalf("RemoveMCPClient 之后新建的 Agent 仍有 %q: %v", name, after.RegisteredTools())
	}
	if !slices.Contains(before.RegisteredTools(), name) {
		t.Fatalf("RemoveMCPClient 追溯修改了已存在 Agent 的工具面: %v", before.RegisteredTools())
	}
	if err := rt.RemoveMCPClient("missing"); !errors.Is(err, kit.ErrMCPClientNotFound) {
		t.Fatalf("删除未注册的客户端 = %v, want ErrMCPClientNotFound", err)
	}
}

// TestRuntimeDeriveRunnerDoesNotAffectExisting：DeriveRunner 派生更严的 runner，
// 共享 runner 与已存在的 Agent 都不受影响。
func TestRuntimeDeriveRunnerDoesNotAffectExisting(t *testing.T) {
	root := t.TempDir()
	base := osrunner.Config{
		RunnerCommand:           []string{"base-wrap"},
		RunnerFailureSignatures: []string{"wrap failed"},
	}
	shared := &kitRunner{}
	rt, err := kit.NewRuntime(kit.RuntimeConfig{SandboxRunner: shared, SandboxRunnerConfig: &base})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	derived, err := rt.DeriveRunner()
	if err != nil {
		t.Fatalf("DeriveRunner: %v", err)
	}
	stricter, err := rt.DeriveRunner(func(cfg *osrunner.Config) { cfg.RunnerCommand = []string{"strict-wrap"} })
	if err != nil {
		t.Fatalf("DeriveRunner(stricter): %v", err)
	}
	if derived == stricter {
		t.Fatal("两次派生返回了同一个 runner 实例")
	}
	plain, err := derived.Confine([]string{"/bin/sh", "-c", "echo hi"}, policy)
	if err != nil {
		t.Fatalf("derived.Confine: %v", err)
	}
	if len(plain.Argv) == 0 || plain.Argv[0] != "base-wrap" {
		t.Fatalf("派生 runner 没有沿用 Runtime 的 runner 配置: %v", plain.Argv)
	}
	tight, err := stricter.Confine([]string{"/bin/sh", "-c", "echo hi"}, policy)
	if err != nil {
		t.Fatalf("stricter.Confine: %v", err)
	}
	if len(tight.Argv) == 0 || tight.Argv[0] != "strict-wrap" {
		t.Fatalf("派生 runner 没有应用调用方的覆盖: %v", tight.Argv)
	}
	if base.RunnerCommand[0] != "base-wrap" {
		t.Fatalf("DeriveRunner 修改了 Runtime 的 runner 配置: %v", base.RunnerCommand)
	}
	if _, err := rt.DeriveRunner(func(cfg *osrunner.Config) { cfg.RunnerFailureSignatures = nil }); err == nil {
		t.Fatal("非法的派生配置没有报错（自定义 runner 必须带失败特征）")
	}

	// 共享 runner 没有被换掉：新建 Agent 的 bash 仍然经过它。
	agent, err := rt.NewAgent(kit.Config{Model: "test-model", Root: root, Gate: gate.AllowAll()})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = agent.Shutdown(context.Background()) })
	bash, ok := agent.Tool("bash")
	if !ok {
		t.Fatal("agent 没有装配 bash")
	}
	if result := runtimeInvoke(t, bash, map[string]any{"command": "echo derived", "description": "say hi"}); result.IsError {
		t.Fatalf("bash 失败: %q", result.Content)
	}
	if len(shared.calls) == 0 {
		t.Fatal("DeriveRunner 之后共享 runner 不再被执行（被意外替换了）")
	}
	for _, argv := range shared.calls {
		if len(argv) > 0 && (argv[0] == "strict-wrap" || argv[0] == "base-wrap") {
			t.Fatalf("共享 runner 被派生结果污染: %v", argv)
		}
	}
}

// RemoveRoot 撤销主根必须报错而不是静默成功。
//
// 审计实测：路径既是某 Agent 主根、又被 AddRoot 登记时，旧实现把"主根不可撤销"
// 当成"没有这根"跳过，RemoveRoot 返回 nil 而该 Agent 仍可写。
func TestRuntimeRemoveRootOfPrimaryRootReportsError(t *testing.T) {
	rt, err := kit.NewRuntime(kit.RuntimeConfig{Model: "m"})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer func() { _ = rt.Close() }()

	root := t.TempDir()
	agent, err := rt.NewAgent(kit.Config{Model: "m", Root: root})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	_ = agent

	if err := rt.RemoveRoot(root); err == nil {
		t.Fatal("撤销主根必须报错（不能静默成功）")
	} else if !strings.Contains(err.Error(), "primary root") {
		t.Fatalf("错误文案应说明是主根: %v", err)
	}
	if _, statErr := os.Stat(root); statErr != nil {
		t.Fatalf("主根必须仍然存在: %v", statErr)
	}
}
