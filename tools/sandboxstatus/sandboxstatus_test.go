package sandboxstatus_test

// sandbox_status 的契约测试：nil policy 的可读错误、各模式的字段语义、
// 授权与预算渲染、字段稳定性（两次调用逐字节一致），以及“文案不含具体
// 厂商/目录名”的通用性要求。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/sandboxstatus"
)

// invoke 调用 sandbox_status 并返回结果。
func invoke(t *testing.T, tool rig.Tool) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "status", Name: sandboxstatus.ToolSandboxStatus})
	if err != nil {
		t.Fatalf("sandbox_status returned Go error: %v", err)
	}
	return result
}

// decode 把结果解析成通用 JSON 对象。
func decode(t *testing.T, result rig.ToolResult) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(result.Content), &decoded); err != nil {
		t.Fatalf("sandbox_status content is not JSON: %v\n%s", err, result.Content)
	}
	return decoded
}

// strings 把 JSON 数组取成 []string。
func stringsOf(t *testing.T, decoded map[string]any, key string) []string {
	t.Helper()
	raw, ok := decoded[key]
	if !ok {
		t.Fatalf("field %q is missing from %v", key, decoded)
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("field %q = %#v, want a JSON array", key, raw)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("field %q has a non-string element %#v", key, item)
		}
		out = append(out, text)
	}
	return out
}

// TestNilPolicyReturnsError 证明未配置策略时返回明确的工具错误而不是 panic。
func TestNilPolicyReturnsError(t *testing.T) {
	tool := sandboxstatus.New(nil)
	if tool.Name() != "sandbox_status" {
		t.Fatalf("Name = %q, want sandbox_status", tool.Name())
	}
	result := invoke(t, tool)
	if !result.IsError {
		t.Fatalf("nil policy result = %q, want IsError", result.Content)
	}
	contains(t, result.Content, "no sandbox policy", "sandbox_status")
}

// TestReadOnly 证明只读模式没有任何可写面，也不能申请可写目录。
func TestReadOnly(t *testing.T) {
	root := t.TempDir()
	tool := sandboxstatus.New(sandbox.New(sandbox.WithRoots(root), sandbox.WithReadOnly(true)))
	result := invoke(t, tool)
	if result.IsError {
		t.Fatalf("read-only result = %q, want success", result.Content)
	}
	decoded := decode(t, result)
	if got := decoded["mode"]; got != string(sandbox.ModeReadOnly) {
		t.Fatalf("mode = %v, want %s", got, sandbox.ModeReadOnly)
	}
	if got := stringsOf(t, decoded, "writableRoots"); len(got) != 0 {
		t.Fatalf("writableRoots = %v, want empty under read-only", got)
	}
	if got := decoded["canRequestWritablePath"]; got != false {
		t.Fatalf("canRequestWritablePath = %v, want false under read-only", got)
	}
	if got := decoded["network"]; got != false {
		t.Fatalf("network = %v, want false", got)
	}
}

// TestWorkspaceWriteReportsRootsGrantsAndBudget 证明 workspace-write 下工作区根、
// 静态可写根、运行时授权、保护路径与预算都被如实渲染。
func TestWorkspaceWriteReportsRootsGrantsAndBudget(t *testing.T) {
	root := t.TempDir()
	grantable := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithWritableRoots(grantable),
		sandbox.WithProtectedPaths(".git"),
		sandbox.WithGrantBudget(3),
		sandbox.WithNetwork(true),
	)

	tool := sandboxstatus.New(policy)
	decoded := decode(t, invoke(t, tool))

	if got := decoded["mode"]; got != string(sandbox.ModeWorkspaceWrite) {
		t.Fatalf("mode = %v, want %s", got, sandbox.ModeWorkspaceWrite)
	}
	if got := stringsOf(t, decoded, "workspaceRoots"); len(got) != 1 || got[0] != root {
		t.Fatalf("workspaceRoots = %v, want [%s]", got, root)
	}
	configured := stringsOf(t, decoded, "configuredWritableRoots")
	if len(configured) != 1 || configured[0] != grantable {
		t.Fatalf("configuredWritableRoots = %v, want [%s]", configured, grantable)
	}
	writable := stringsOf(t, decoded, "writableRoots")
	if !containsAll(writable, root, grantable) {
		t.Fatalf("writableRoots = %v, want it to contain the workspace root and the configured root", writable)
	}
	if got := decoded["network"]; got != true {
		t.Fatalf("network = %v, want true", got)
	}
	if got := decoded["grantBudget"]; got != float64(3) {
		t.Fatalf("grantBudget = %v, want 3", got)
	}
	if got := decoded["grantBudgetLeft"]; got != float64(3) {
		t.Fatalf("grantBudgetLeft = %v, want 3", got)
	}
	if got := decoded["canRequestWritablePath"]; got != true {
		t.Fatalf("canRequestWritablePath = %v, want true", got)
	}
	temp, ok := decoded["tempWritable"].(map[string]any)
	if !ok {
		t.Fatalf("tempWritable = %#v, want an object", decoded["tempWritable"])
	}
	if temp["tmpdirEnv"] != true {
		t.Fatalf("tempWritable.tmpdirEnv = %v, want true", temp["tmpdirEnv"])
	}
	if got := stringsOf(t, decoded, "protectedPaths"); !containsAll(got, filepath.Join(root, ".git")) {
		t.Fatalf("protectedPaths = %v, want [%s]", got, filepath.Join(root, ".git"))
	}
}

// TestGrantsReportedWithBudget 证明运行时授权出现在 grants 里，且预算剩余同步
// 减少。
func TestGrantsReportedWithBudget(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithGrantBudget(2))
	grant, err := policy.AddWritableRoot(state, "tool keeps its tokens there")
	if err != nil {
		t.Fatalf("AddWritableRoot: %v", err)
	}

	decoded := decode(t, invoke(t, sandboxstatus.New(policy)))
	raw, ok := decoded["grants"].([]any)
	if !ok || len(raw) != 1 {
		t.Fatalf("grants = %#v, want exactly one grant", decoded["grants"])
	}
	first, ok := raw[0].(map[string]any)
	if !ok {
		t.Fatalf("grants[0] = %#v, want an object", raw[0])
	}
	if first["path"] != grant.Path {
		t.Fatalf("grants[0].path = %v, want %s", first["path"], grant.Path)
	}
	if first["reason"] != "tool keeps its tokens there" {
		t.Fatalf("grants[0].reason = %v, want the requested reason", first["reason"])
	}
	if got := decoded["grantBudgetLeft"]; got != float64(1) {
		t.Fatalf("grantBudgetLeft = %v, want 1", got)
	}
	if !containsAll(stringsOf(t, decoded, "writableRoots"), grant.Path) {
		t.Fatalf("writableRoots = %v, want it to contain the granted path", decoded["writableRoots"])
	}
}

// TestFieldsAreStable 证明两次调用逐字节一致（字段顺序与内容都稳定）。
func TestFieldsAreStable(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithNetwork(true))
	tool := sandboxstatus.New(policy, sandboxstatus.WithRunnerInfo(func() (string, sandbox.Enforcement) {
		return "example-runner", sandbox.EnforcementPartial
	}))
	first := invoke(t, tool)
	second := invoke(t, tool)
	if first.Content != second.Content {
		t.Fatalf("sandbox_status is not stable:\nfirst  %s\nsecond %s", first.Content, second.Content)
	}
	decoded := decode(t, first)
	runner, ok := decoded["runner"].(map[string]any)
	if !ok {
		t.Fatalf("runner = %#v, want an object when WithRunnerInfo is set", decoded["runner"])
	}
	if runner["name"] != "example-runner" || runner["enforcement"] != string(sandbox.EnforcementPartial) {
		t.Fatalf("runner = %#v, want {example-runner partial}", runner)
	}

	withoutRunner := decode(t, invoke(t, sandboxstatus.New(policy)))
	if _, present := withoutRunner["runner"]; present {
		t.Fatalf("runner field must be omitted without WithRunnerInfo: %#v", withoutRunner["runner"])
	}
}

// TestSchemaAndNoVendorSpecifics 证明 schema 是空参对象，且 Description 与
// guidance 不含任何具体厂商/工具目录名。
func TestSchemaAndNoVendorSpecifics(t *testing.T) {
	tool := sandboxstatus.New(sandbox.New())
	schema := tool.Schema()
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v, want object", schema["type"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok || len(properties) != 0 {
		t.Fatalf("schema properties = %#v, want none", schema["properties"])
	}
	// 模型可见文案必须通用：不点名厂商、产品或具体工具的目录。
	for _, forbidden := range []string{"dws", "aws", "gcloud", "kubectl", "azure", ".config", "$HOME/", "npm", "docker"} {
		if strings.Contains(strings.ToLower(tool.Description()), forbidden) {
			t.Fatalf("description names %q: %q", forbidden, tool.Description())
		}
	}
	decoded := decode(t, invoke(t, tool))
	text, ok := decoded["guidance"].(string)
	if !ok || text == "" {
		t.Fatalf("guidance = %#v, want a non-empty string", decoded["guidance"])
	}
	for _, forbidden := range []string{"dws", "aws", "gcloud", "kubectl", "azure", ".config", "$HOME/", "npm", "docker"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Fatalf("guidance names %q: %q", forbidden, text)
		}
	}
	// guidance 必须覆盖通用处置顺序的三步。
	for _, fragment := range []string{"redirect", "writable root", "never repeat"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("guidance is missing %q: %q", fragment, text)
		}
	}
}

// contains 断言子串。
func contains(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Fatalf("missing %q in %q", fragment, text)
		}
	}
}

// containsAll 报告 values 是否包含全部 want 项。
func containsAll(values []string, want ...string) bool {
	for _, expected := range want {
		found := false
		for _, value := range values {
			if value == expected {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestRuntimeViewFields 断言运行时诊断三个字段的存在与语义：
// toolchainCaches（实际存在的缓存根）、networkCoverage、maskedSockets。
//
// 审计者踩过的坑：白名单到底生效没有只能靠 `go build` 试错，网络断没断只能靠
// curl 看状态码。这三个字段把"实际生效"暴露出来。
func TestRuntimeViewFields(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	tool := sandboxstatus.New(policy, sandboxstatus.WithRuntimeView(func() sandboxstatus.RuntimeView {
		return sandboxstatus.RuntimeView{
			ToolchainCaches: []string{"/home/u/.cache", "/home/u/go"},
			NetworkCoverage: "full",
			MaskedSockets:   []string{"/run/docker.sock"},
			MissingCaches:   []string{"/home/u/.m2"},
		}
	}))
	payload := decode(t, invoke(t, tool))

	caches, ok := payload["toolchainCaches"].([]any)
	if !ok || len(caches) != 2 {
		t.Fatalf("toolchainCaches = %v，want 两个条目", payload["toolchainCaches"])
	}
	if got := payload["networkCoverage"]; got != "full" {
		t.Fatalf("networkCoverage = %v, want full", got)
	}
	sockets, ok := payload["maskedSockets"].([]any)
	if !ok || len(sockets) != 1 || sockets[0] != "/run/docker.sock" {
		t.Fatalf("maskedSockets = %v，want [/run/docker.sock]", payload["maskedSockets"])
	}
}

// 没有 provider 时字段仍然渲染成 [] 与 "none"——绝不能是 null（模型会读成"未知"）。
func TestRuntimeViewDefaultsAreExplicit(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	payload := decode(t, invoke(t, sandboxstatus.New(policy)))
	if _, ok := payload["toolchainCaches"].([]any); !ok {
		t.Fatalf("toolchainCaches 应为 []，实际 %v", payload["toolchainCaches"])
	}
	if _, ok := payload["maskedSockets"].([]any); !ok {
		t.Fatalf("maskedSockets 应为 []，实际 %v", payload["maskedSockets"])
	}
	if got := payload["networkCoverage"]; got != "none" {
		t.Fatalf("networkCoverage = %v，want none（没有 runner 时不假设能隔离）", got)
	}
}
