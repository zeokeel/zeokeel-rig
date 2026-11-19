package ptc_test

// 本文件钉住 run_code 的环境语义（DSH 对齐后）：
//
//   - **默认原样继承**宿主环境——包括凭据。这不是缺陷，是"沙箱才是唯一防护层"的
//     决定（见 SECURITY.md「凭据」）；解释器看到的变量与 bash 完全一致。
//   - 只有宿主显式 `sandbox.WithEnvAllowlist(...)` 收紧时，解释器才按名单过滤；
//     此时凭据不进解释器，PATH 等必需变量保留。

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/ptc"
)

// runCodeArgs 把一段 Python 源码编成 run_code 的参数 JSON。
func runCodeArgs(t *testing.T, code string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"code": code, "language": "python"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return string(raw)
}

// auditEnvProbe 打印秘密变量是否存在、以及白名单里的关键变量是否还在。
const auditEnvProbe = `import os
print("secret=" + str(os.environ.get("RIG_AUDIT_SECRET", "unset")))
print("path=" + str(bool(os.environ.get("PATH"))))
print("home=" + str(bool(os.environ.get("HOME"))))
print("lang=" + str(os.environ.get("LANG", "")))
print("ok")
`

// assertEnvInherited 断言解释器看到了宿主的哨兵（默认继承）。
func assertEnvInherited(t *testing.T, content string) {
	t.Helper()
	if !strings.Contains(content, "secret=s3cr3t") {
		t.Fatalf("默认应当原样继承宿主环境（含 RIG_AUDIT_SECRET）:\n%s", content)
	}
	if !strings.Contains(content, "path=True") {
		t.Fatalf("PATH 丢了，解释器无法正常工作:\n%s", content)
	}
	if !strings.Contains(content, "ok") {
		t.Fatalf("程序没有正常跑完:\n%s", content)
	}
}

// assertEnvNarrowed 断言解释器只看到白名单里的变量（显式收紧）。
func assertEnvNarrowed(t *testing.T, content string) {
	t.Helper()
	if strings.Contains(content, "s3cr3t") {
		t.Fatalf("显式白名单没挡住 RIG_AUDIT_SECRET:\n%s", content)
	}
	if !strings.Contains(content, "secret=unset") {
		t.Fatalf("探针没有按预期报告 secret=unset:\n%s", content)
	}
	if !strings.Contains(content, "path=True") {
		t.Fatalf("PATH 被误删，解释器无法正常工作:\n%s", content)
	}
	if _, ok := os.LookupEnv("HOME"); ok && strings.Contains(content, "home=True") {
		t.Fatalf("显式白名单只放行 PATH，HOME 却仍然可见:\n%s", content)
	}
	if !strings.Contains(content, "ok") {
		t.Fatalf("程序没有正常跑完:\n%s", content)
	}
}

// TestRunCodeInheritsEnvironmentWithoutPolicy：没有沙箱策略时原样继承（DSH 对齐）。
func TestRunCodeInheritsEnvironmentWithoutPolicy(t *testing.T) {
	requirePython(t)
	t.Setenv("RIG_AUDIT_SECRET", "s3cr3t")
	tool := ptc.New(ptc.Config{})
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: runCodeArgs(t, auditEnvProbe)})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.IsError {
		t.Fatalf("run_code 失败: %q", result.Content)
	}
	assertEnvInherited(t, result.Content)
}

// TestRunCodeInheritsEnvironmentWithDefaultPolicy：挂了策略但没配白名单，同样继承。
func TestRunCodeInheritsEnvironmentWithDefaultPolicy(t *testing.T) {
	requirePython(t)
	t.Setenv("RIG_AUDIT_SECRET", "s3cr3t")
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()))
	tool := ptc.New(ptc.Config{Policy: policy, Root: t.TempDir()})
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: runCodeArgs(t, auditEnvProbe)})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.IsError {
		t.Fatalf("run_code 失败: %q", result.Content)
	}
	assertEnvInherited(t, result.Content)
}

// TestRunCodeFiltersEnvironmentWithExplicitAllowlist：显式白名单才收紧。
func TestRunCodeFiltersEnvironmentWithExplicitAllowlist(t *testing.T) {
	requirePython(t)
	t.Setenv("RIG_AUDIT_SECRET", "s3cr3t")
	strict := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithEnvAllowlist("PATH"))
	tool := ptc.New(ptc.Config{Policy: strict})
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: runCodeArgs(t, auditEnvProbe)})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.IsError {
		t.Fatalf("run_code 失败（PATH 之外共 0 个变量时解释器仍应能启动）: %q", result.Content)
	}
	assertEnvNarrowed(t, result.Content)
}

// TestRunCodeInnerToolCallStillWorksWithFilteredEnv：收紧环境后内层工具调用与
// Gate 语义必须不变（解释器仍能通过 stdin/stdout 协议回调宿主工具）。
func TestRunCodeInnerToolCallStillWorksWithFilteredEnv(t *testing.T) {
	requirePython(t)
	t.Setenv("RIG_AUDIT_SECRET", "s3cr3t")
	strict := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithEnvAllowlist("PATH"))
	tool := ptc.New(ptc.Config{Policy: strict, Tools: []rig.Tool{echoTool{}}})
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		Arguments: runCodeArgs(t, "print(tools.echo_tool(text='hi'))"),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.IsError || !strings.Contains(result.Content, "tool:") {
		t.Fatalf("内层工具调用被环境过滤破坏: %q (IsError=%v)", result.Content, result.IsError)
	}
	if strings.Contains(result.Content, "s3cr3t") {
		t.Fatalf("内层调用路径泄漏了宿主环境:\n%s", result.Content)
	}
}

var _ = exec.LookPath
