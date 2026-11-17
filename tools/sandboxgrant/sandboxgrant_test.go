package sandboxgrant_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/sandboxgrant"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// policyWithRoot 构造一个以 root 为工作区、workspace-write 的策略。
func policyWithRoot(t *testing.T, root string, opts ...sandbox.Option) *sandbox.Policy {
	t.Helper()
	base := []sandbox.Option{sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite)}
	return sandbox.New(append(base, opts...)...)
}

// argsJSON 把 path/reason 编码成工具参数 JSON。
func argsJSON(t *testing.T, path, reason string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"path": path, "reason": reason})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return string(payload)
}

// invoke 直接调用一次工具（绕过 Gate：本测试只验证工具自身行为，审批由宿主完成）。
func invoke(t *testing.T, tool rig.Tool, arguments string) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "c1", Name: tool.Name(), Arguments: arguments})
	if err != nil {
		t.Fatalf("Invoke returned a Go error: %v", err)
	}
	return result
}

// decodeResult 把成功结果的 JSON 解成 map，便于逐字段断言。
func decodeResult(t *testing.T, result rig.ToolResult) map[string]any {
	t.Helper()
	if result.IsError {
		t.Fatalf("result is an error: %q", result.Content)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(result.Content), &decoded); err != nil {
		t.Fatalf("result %q is not JSON: %v", result.Content, err)
	}
	return decoded
}

// requireContains 断言文本型错误结果包含所有片段且被标记为 IsError。
func requireContains(t *testing.T, result rig.ToolResult, fragments ...string) {
	t.Helper()
	if !result.IsError {
		t.Fatalf("result = %+v, want IsError", result)
	}
	for _, fragment := range fragments {
		if !strings.Contains(result.Content, fragment) {
			t.Fatalf("result %q does not contain %q", result.Content, fragment)
		}
	}
}

// containsPath 报告 roots 中是否有与 want 相同的路径。
func containsPath(roots []string, want string) bool {
	for _, root := range roots {
		if root == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// tool surface
// ---------------------------------------------------------------------------

func TestToolSurface(t *testing.T) {
	tool := sandboxgrant.New(nil)
	if tool.Name() != "request_sandbox_access" {
		t.Fatalf("Name() = %q", tool.Name())
	}

	schema := tool.Schema()
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v", schema["type"])
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("additionalProperties = %v, want false", schema["additionalProperties"])
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 2 || required[0] != "path" || required[1] != "reason" {
		t.Fatalf("required = %v, want [path reason]", schema["required"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %T", schema["properties"])
	}
	if len(properties) != 2 {
		t.Fatalf("properties = %v, want exactly path and reason", properties)
	}
	for _, forbidden := range []string{"scope", "mode", "pathScope"} {
		if _, exists := properties[forbidden]; exists {
			t.Fatalf("schema exposes %q, but a grant is session-wide", forbidden)
		}
	}
	reason, ok := properties["reason"].(map[string]any)
	if !ok {
		t.Fatalf("reason property = %T", properties["reason"])
	}
	if reason["minLength"] != 8 || reason["maxLength"] != 500 {
		t.Fatalf("reason bounds = %v..%v, want 8..500", reason["minLength"], reason["maxLength"])
	}

	// Description 面向模型，必须说明"审批由人决定"与"被拒后不要重复申请"。
	description := tool.Description()
	for _, fragment := range []string{"only submits the request", "do not ask again"} {
		if !strings.Contains(description, fragment) {
			t.Fatalf("Description %q does not contain %q", description, fragment)
		}
	}
}

// ---------------------------------------------------------------------------
// success / redundant / budget
// ---------------------------------------------------------------------------

func TestGrantSucceedsAndBecomesWritable(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	policy := policyWithRoot(t, root)
	tool := sandboxgrant.New(policy)

	result := invoke(t, tool, argsJSON(t, target, "the tool keeps its own credential cache here"))
	decoded := decodeResult(t, result)
	if decoded["granted"] != true {
		t.Fatalf("granted = %v, want true", decoded["granted"])
	}
	if decoded["path"] != target {
		t.Fatalf("path = %v, want %q", decoded["path"], target)
	}
	if _, exists := decoded["alreadyWritable"]; exists {
		t.Fatalf("alreadyWritable must be absent on a new grant: %v", decoded)
	}
	if decoded["budgetLeft"] != float64(sandbox.DefaultGrantBudget-1) {
		t.Fatalf("budgetLeft = %v, want %d", decoded["budgetLeft"], sandbox.DefaultGrantBudget-1)
	}
	if decoded["note"] != "the path is now writable for this session; retry the failed command once and do not request it again" {
		t.Fatalf("note = %v", decoded["note"])
	}

	// 授权必须立刻对策略可见（其他工具每次调用都会重新读取策略）。
	if !containsPath(policy.AdditionalWritableRoots(), target) {
		t.Fatalf("WritableRoots() = %v, want it to contain %q", policy.AdditionalWritableRoots(), target)
	}
	grants := policy.Grants()
	if len(grants) != 1 || grants[0].Path != target {
		t.Fatalf("Grants() = %+v, want one grant for %q", grants, target)
	}
	if grants[0].At.IsZero() {
		t.Fatalf("Grant.At is zero: %+v", grants[0])
	}
}

func TestSecondRequestIsRedundant(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	policy := policyWithRoot(t, root)
	tool := sandboxgrant.New(policy)
	arguments := argsJSON(t, target, "the tool keeps its own credential cache here")

	first := decodeResult(t, invoke(t, tool, arguments))
	if first["alreadyWritable"] != nil {
		t.Fatalf("first call alreadyWritable = %v", first["alreadyWritable"])
	}
	second := decodeResult(t, invoke(t, tool, arguments))
	if second["granted"] != true {
		t.Fatalf("second granted = %v, want true (redundant is not a failure)", second["granted"])
	}
	if second["alreadyWritable"] != true {
		t.Fatalf("second alreadyWritable = %v, want true", second["alreadyWritable"])
	}
	if second["budgetLeft"] != float64(sandbox.DefaultGrantBudget-1) {
		t.Fatalf("second budgetLeft = %v, want %d (no grant consumed)", second["budgetLeft"], sandbox.DefaultGrantBudget-1)
	}
	if len(policy.Grants()) != 1 {
		t.Fatalf("Grants() = %+v, want exactly one", policy.Grants())
	}
}

func TestPathInsideWorkspaceIsRedundant(t *testing.T) {
	root := t.TempDir()
	policy := policyWithRoot(t, root)
	tool := sandboxgrant.New(policy)

	decoded := decodeResult(t, invoke(t, tool, argsJSON(t, filepath.Join(root, "sub"), "the tool writes its cache inside the workspace")))
	if decoded["alreadyWritable"] != true {
		t.Fatalf("alreadyWritable = %v, want true", decoded["alreadyWritable"])
	}
	if len(policy.Grants()) != 0 {
		t.Fatalf("Grants() = %+v, want none", policy.Grants())
	}
}

func TestDangerFullAccessIsRedundant(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeDangerFullAccess))
	tool := sandboxgrant.New(policy)

	decoded := decodeResult(t, invoke(t, tool, argsJSON(t, target, "nothing is fenced in this mode anyway")))
	if decoded["alreadyWritable"] != true {
		t.Fatalf("alreadyWritable = %v, want true", decoded["alreadyWritable"])
	}
}

func TestBudgetExhausted(t *testing.T) {
	root := t.TempDir()
	first := t.TempDir()
	second := t.TempDir()
	policy := policyWithRoot(t, root, sandbox.WithGrantBudget(1))
	if _, err := policy.AddWritableRoot(first, "the tool keeps its credential cache here"); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	tool := sandboxgrant.New(policy)

	result := invoke(t, tool, argsJSON(t, second, "the tool also needs a second cache directory"))
	requireContains(t, result, "grant budget", "exhausted", "ask the user")
	if len(policy.Grants()) != 1 {
		t.Fatalf("Grants() = %+v, want the seeded grant only", policy.Grants())
	}
	if containsPath(policy.AdditionalWritableRoots(), second) {
		t.Fatalf("WritableRoots() = %v, must not contain %q", policy.AdditionalWritableRoots(), second)
	}
}

// ---------------------------------------------------------------------------
// refusals
// ---------------------------------------------------------------------------

func TestRefusals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("these refusals are expressed with POSIX paths")
	}
	// 与具体机器无关的 HOME：需要它的用例会覆盖它。
	home := t.TempDir()
	t.Setenv("HOME", home)

	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	filePath := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cases := []struct {
		name     string
		tool     func(t *testing.T) rig.Tool
		path     string
		reason   string
		contains []string
	}{
		{
			name:   "relative path",
			path:   "relative/dir",
			reason: "the tool keeps its credential cache here",
			contains: []string{
				"absolute path",
			},
		},
		{
			name:     "dot dot segment",
			path:     base + "/a/../b", // 不用 filepath.Join：它会先清理掉 ".."。
			reason:   "the tool keeps its credential cache here",
			contains: []string{".."},
		},
		{
			name:     "system directory",
			path:     "/etc",
			reason:   "the tool wants to write global configuration",
			contains: []string{"system directory", "narrowest non-system directory"},
		},
		{
			name:     "home directory itself",
			path:     home,
			reason:   "the tool writes somewhere under home",
			contains: []string{"home directory", "specific subdirectory"},
		},
		{
			name:     "ancestor of workspace root",
			path:     base,
			reason:   "the tool writes next to the workspace",
			contains: []string{"contains the workspace root", "too broad"},
		},
		{
			name:     "path is a file",
			path:     filePath,
			reason:   "the tool writes to a file path",
			contains: []string{"not a directory"},
		},
		{
			name:     "empty reason",
			path:     t.TempDir(),
			reason:   "   ",
			contains: []string{"reason is too short", "8-500"},
		},
		{
			name:     "short reason",
			path:     t.TempDir(),
			reason:   "because",
			contains: []string{"reason is too short"},
		},
		{
			name:     "long reason",
			path:     t.TempDir(),
			reason:   strings.Repeat("a", 501),
			contains: []string{"reason is too long", "500"},
		},
		{
			name:     "empty path",
			path:     "  ",
			reason:   "the tool keeps its credential cache here",
			contains: []string{"path is required"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			tool := sandboxgrant.New(policyWithRoot(t, root))
			if testCase.tool != nil {
				tool = testCase.tool(t)
			}
			result := invoke(t, tool, argsJSON(t, testCase.path, testCase.reason))
			requireContains(t, result, testCase.contains...)
		})
	}
}

func TestNilPolicy(t *testing.T) {
	result := invoke(t, sandboxgrant.New(nil), argsJSON(t, t.TempDir(), "the tool keeps its credential cache here"))
	requireContains(t, result, "no sandbox policy")
}

func TestUnknownArgumentsRejected(t *testing.T) {
	target := t.TempDir()
	tool := sandboxgrant.New(policyWithRoot(t, t.TempDir()))
	arguments := `{"path":"` + target + `","reason":"the tool keeps its cache here","scope":"session"}`
	requireContains(t, invoke(t, tool, arguments), "unknown argument", "scope")
}

// ---------------------------------------------------------------------------
// observer
// ---------------------------------------------------------------------------

func TestObserverReceivesSuccessAndFailure(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	policy := policyWithRoot(t, root)

	var mu sync.Mutex
	var seen []struct {
		grant sandbox.Grant
		err   error
	}
	observer := func(_ rig.ToolCall, grant sandbox.Grant, err error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, struct {
			grant sandbox.Grant
			err   error
		}{grant: grant, err: err})
	}
	tool := sandboxgrant.New(policy, sandboxgrant.WithObserver(observer))

	invoke(t, tool, argsJSON(t, target, "the tool keeps its credential cache here"))
	requireContains(t, invoke(t, tool, argsJSON(t, "/etc", "the tool wants to write global configuration")), "system directory")
	// 冗余申请也要被审计到（err 包装 ErrGrantRedundant，但工具对模型是成功）。
	invoke(t, tool, argsJSON(t, target, "the tool keeps its credential cache here"))

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("observer calls = %d, want 3 (success, refusal, redundant)", len(seen))
	}
	if seen[0].err != nil {
		t.Fatalf("first observer err = %v, want nil", seen[0].err)
	}
	if seen[0].grant.Path != target || seen[0].grant.Reason == "" || seen[0].grant.At.IsZero() {
		t.Fatalf("first observer grant = %+v, want the approved grant", seen[0].grant)
	}
	if seen[1].err == nil {
		t.Fatalf("second observer err = nil, want the refusal")
	}
	if seen[1].grant.Path != "/etc" {
		t.Fatalf("second observer grant.Path = %q, want the rejected request", seen[1].grant.Path)
	}
	if !errors.Is(seen[2].err, sandbox.ErrGrantRedundant) {
		t.Fatalf("third observer err = %v, want ErrGrantRedundant", seen[2].err)
	}
}

func TestNilObserverIsSafe(t *testing.T) {
	policy := policyWithRoot(t, t.TempDir())
	tool := sandboxgrant.New(policy, sandboxgrant.WithObserver(nil))
	if result := invoke(t, tool, argsJSON(t, t.TempDir(), "the tool keeps its credential cache here")); result.IsError {
		t.Fatalf("result = %+v, want success", result)
	}
}
