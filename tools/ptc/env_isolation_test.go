package ptc_test

// 本文件钉住 run_code 的**环境隔离**：解释器不得继承宿主全部环境变量。
//
// 修复前 ptc 起解释器时没有设置 command.Env（与原 bash/pwsh/jobs/terminal 的
// policy.CommandEnv 白名单不一致），于是 `print(os.environ)` 就能把
// OPENAI_API_KEY / AWS_* 之类打进工具结果、再由模型发给 provider。网络不在沙箱
// 承诺内，因此这条泄漏路径是实打实的凭据外泄。

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

func assertEnvIsolated(t *testing.T, content string) {
	t.Helper()
	if strings.Contains(content, "s3cr3t") {
		t.Fatalf("解释器继承了宿主的 RIG_AUDIT_SECRET（凭据泄漏）:\n%s", content)
	}
	if !strings.Contains(content, "secret=unset") {
		t.Fatalf("探针没有按预期报告 secret=unset:\n%s", content)
	}
	if !strings.Contains(content, "path=True") {
		t.Fatalf("PATH 被误删，解释器无法正常工作:\n%s", content)
	}
	if _, ok := os.LookupEnv("HOME"); ok && !strings.Contains(content, "home=True") {
		t.Fatalf("HOME 在白名单里但被删掉了:\n%s", content)
	}
	if lang, ok := os.LookupEnv("LANG"); ok && strings.TrimSpace(lang) != "" &&
		!strings.Contains(content, "lang="+lang) {
		t.Fatalf("LANG=%q 在白名单里但被删掉了:\n%s", lang, content)
	}
	if !strings.Contains(content, "ok") {
		t.Fatalf("程序没有正常跑完:\n%s", content)
	}
}

// TestRunCodeFiltersEnvironmentWithoutPolicy：没有沙箱策略时也必须用最小白名单，
// 而不是原样继承宿主环境。
func TestRunCodeFiltersEnvironmentWithoutPolicy(t *testing.T) {
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
	assertEnvIsolated(t, result.Content)
}

// TestRunCodeFiltersEnvironmentWithPolicy：挂了策略时走 Policy.CommandEnv，
// 与 bash/pwsh/jobs/terminal 完全一致。
func TestRunCodeFiltersEnvironmentWithPolicy(t *testing.T) {
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
	assertEnvIsolated(t, result.Content)
	// 策略的 env 白名单必须真的生效：策略只放行 PATH 时，HOME 也不该出现。
	strict := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithEnvAllowlist("PATH"))
	strictTool := ptc.New(ptc.Config{Policy: strict})
	strictResult, err := strictTool.Invoke(context.Background(), rig.ToolCall{Arguments: runCodeArgs(t, auditEnvProbe)})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strictResult.IsError && strings.Contains(strictResult.Content, "home=True") {
		t.Fatalf("WithEnvAllowlist(\"PATH\") 没有生效，HOME 仍然可见:\n%s", strictResult.Content)
	}
}

// TestRunCodeInnerToolCallStillWorksWithFilteredEnv：环境收紧后内层工具调用与
// Gate 语义必须不变（解释器仍能通过 stdin/stdout 协议回调宿主工具）。
func TestRunCodeInnerToolCallStillWorksWithFilteredEnv(t *testing.T) {
	requirePython(t)
	t.Setenv("RIG_AUDIT_SECRET", "s3cr3t")
	tool := ptc.New(ptc.Config{Tools: []rig.Tool{echoTool{}}})
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
