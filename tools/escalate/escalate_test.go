package escalate

// 本文件是构造期校验与 schema 的用例：宿主配置写错必须在构造时就炸，而不是等模型
// 某次调用才发现（安全纪律：fail closed）。

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox/escalate"
)

// rootDir 返回一个存在的绝对目录（测试用工作根）。
func rootDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// okApprover 是"一律同意"的审批回调。
func okApprover(_ context.Context, _ escalate.Request) (bool, string, error) {
	return true, "", nil
}

// baseOptions 返回一份最小可用配置（Allow 只有 echo）。
func baseOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Roots: []string{rootDir(t)},
		Allow: [][]string{{"echo"}},
	}
}

// newTestTool 用 fake 连接构造一个工具；返回工具与连接以便断言。
func newTestTool(t *testing.T, mutate func(*Options)) (rig.Tool, *fakeConn) {
	t.Helper()
	opts := baseOptions(t)
	if mutate != nil {
		mutate(&opts)
	}
	conn := newFakeConn(nil)
	tool, err := New(escalate.NewClient(conn), okApprover, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := tool.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	})
	return tool, conn
}

func TestNewRequiresApprover(t *testing.T) {
	opts := baseOptions(t)
	if _, err := New(escalate.NewClient(newFakeConn(nil)), nil, opts); err == nil {
		t.Fatal("nil approver 必须构造失败（否则等于不需要审批）")
	}
	if _, err := NewWithBroker(newFakeConn(nil), nil, opts); err == nil {
		t.Fatal("nil approver 必须构造失败（NewWithBroker）")
	}
}

func TestNewRequiresClient(t *testing.T) {
	if _, err := New(nil, okApprover, baseOptions(t)); err == nil {
		t.Fatal("nil client 必须构造失败")
	}
	if _, err := NewWithBroker(nil, okApprover, baseOptions(t)); err == nil {
		t.Fatal("nil conn 必须构造失败")
	}
}

func TestNewRequiresUsableRoots(t *testing.T) {
	cases := map[string][]string{
		"空":    nil,
		"相对路径": {filepath.Join("relative", "dir")},
		"不存在":  {filepath.Join(t.TempDir(), "missing")},
		"空字符串": {""},
	}
	for name, roots := range cases {
		roots := roots
		t.Run(name, func(t *testing.T) {
			opts := Options{Roots: roots, Allow: [][]string{{"echo"}}}
			if _, err := New(escalate.NewClient(newFakeConn(nil)), okApprover, opts); err == nil {
				t.Fatalf("Roots=%v 必须构造失败", roots)
			}
		})
	}
}

func TestNewRequiresAllow(t *testing.T) {
	opts := Options{Roots: []string{rootDir(t)}}
	if _, err := New(escalate.NewClient(newFakeConn(nil)), okApprover, opts); err == nil {
		t.Fatal("空 Allow 必须构造失败（空 Allow 等于什么都不允许）")
	}
}

func TestNewRejectsInterpreters(t *testing.T) {
	programs := []string{
		"sh", "bash", "zsh", "python", "python3", "node", "env", "xargs", "find", "make", "awk", "perl",
		// 变体：绝对路径、大写、.exe 都必须被认出来。
		"/usr/bin/bash", "/bin/SH", "python.exe", "C:\\Windows\\System32\\cmd.exe",
		"ruby", "php", "pwsh", "powershell",
	}
	for _, program := range programs {
		program := program
		t.Run(program, func(t *testing.T) {
			opts := Options{Roots: []string{rootDir(t)}, Allow: [][]string{{program, "anything"}}}
			if _, err := New(escalate.NewClient(newFakeConn(nil)), okApprover, opts); err == nil {
				t.Fatalf("Allow 里出现解释器 %q 必须构造失败", program)
			}
		})
	}
}

func TestNewRejectsDangerousGitEntries(t *testing.T) {
	entries := [][]string{
		{"git", "difftool"},
		{"git", "mergetool"},
		{"git", "bisect", "run"},
		{"git", "submodule", "update"},
		{"git", "-c", "core.pager=sh"},
		{"git", "--upload-pack=/tmp/evil", "fetch"},
		{"git", "--exec-path=/tmp/evil", "fetch"},
		{"git", "--no-index"},
	}
	for _, entry := range entries {
		entry := entry
		t.Run(joinArgv(entry), func(t *testing.T) {
			opts := Options{Roots: []string{rootDir(t)}, Allow: [][]string{entry}}
			if _, err := New(escalate.NewClient(newFakeConn(nil)), okApprover, opts); err == nil {
				t.Fatalf("Allow 里出现危险 git 条目 %v 必须构造失败", entry)
			}
		})
	}
	// Deny 里出现解释器是收紧，不该报错（Allow 里才必须拒绝解释器）。
	denyOpts := Options{Roots: []string{rootDir(t)}, Allow: [][]string{{"echo"}}, Deny: [][]string{{"sh"}, {"bash", "-c"}}}
	if _, err := New(escalate.NewClient(newFakeConn(nil)), okApprover, denyOpts); err != nil {
		t.Fatalf("Deny 里列解释器不该构造失败: %v", err)
	}
	// 反例：正常子命令级条目必须被接受。
	for _, entry := range [][]string{{"git", "fetch"}, {"git", "status"}, {"echo"}} {
		opts := Options{Roots: []string{rootDir(t)}, Allow: [][]string{entry}}
		if _, err := New(escalate.NewClient(newFakeConn(nil)), okApprover, opts); err != nil {
			t.Fatalf("正常条目 %v 不该失败: %v", entry, err)
		}
	}
}

func TestNewRejectsMalformedPrefixLists(t *testing.T) {
	cases := map[string][][]string{
		"空前缀条目":     {{}},
		"空 token":   {{"git", ""}},
		"空白 token":  {{"git", "   "}},
		"NUL token": {{"git", "push\x00x"}},
	}
	for name, allow := range cases {
		allow := allow
		t.Run(name, func(t *testing.T) {
			opts := Options{Roots: []string{rootDir(t)}, Allow: allow}
			if _, err := New(escalate.NewClient(newFakeConn(nil)), okApprover, opts); err == nil {
				t.Fatalf("Allow=%v 必须构造失败", allow)
			}
		})
	}
}

func TestNewRejectsContradictoryApproval(t *testing.T) {
	root := rootDir(t)
	// Approval 没被 Allow 覆盖。
	opts := Options{Roots: []string{root}, Allow: [][]string{{"git", "fetch"}}, Approval: [][]string{{"git", "push"}}}
	if _, err := New(escalate.NewClient(newFakeConn(nil)), okApprover, opts); err == nil {
		t.Fatal("Approval 未被 Allow 覆盖时必须构造失败")
	}
	// Approval 同时被 Deny 覆盖。
	opts = Options{
		Roots:    []string{root},
		Allow:    [][]string{{"git"}},
		Deny:     [][]string{{"git", "push"}},
		Approval: [][]string{{"git", "push"}},
	}
	if _, err := New(escalate.NewClient(newFakeConn(nil)), okApprover, opts); err == nil {
		t.Fatal("Approval 与 Deny 冲突时必须构造失败")
	}
	// 正常配置：Approval 被 Allow 覆盖。
	opts = Options{Roots: []string{root}, Allow: [][]string{{"git", "fetch"}}, Approval: [][]string{{"git", "fetch"}}}
	if _, err := New(escalate.NewClient(newFakeConn(nil)), okApprover, opts); err != nil {
		t.Fatalf("正常 Approval 配置不该失败: %v", err)
	}
}

func TestNewRejectsNegativeBounds(t *testing.T) {
	opts := baseOptions(t)
	opts.MaxQueue = -1
	if _, err := New(escalate.NewClient(newFakeConn(nil)), okApprover, opts); err == nil {
		t.Fatal("负 MaxQueue 必须构造失败（工具不允许无限排队）")
	}
}

func TestDefaultsApplied(t *testing.T) {
	instance, _ := newTestTool(t, nil)
	typed := instance.(*tool)
	if typed.opts.maxTimeout != DefaultMaxTimeout {
		t.Fatalf("maxTimeout = %s, want %s", typed.opts.maxTimeout, DefaultMaxTimeout)
	}
	if typed.opts.maxOutputBytes != DefaultMaxOutputBytes {
		t.Fatalf("maxOutputBytes = %d, want %d", typed.opts.maxOutputBytes, DefaultMaxOutputBytes)
	}
	if typed.opts.maxConcurrent != DefaultMaxConcurrent {
		t.Fatalf("maxConcurrent = %d, want %d", typed.opts.maxConcurrent, DefaultMaxConcurrent)
	}
	if typed.opts.maxQueue != DefaultMaxQueue {
		t.Fatalf("maxQueue = %d, want %d", typed.opts.maxQueue, DefaultMaxQueue)
	}
	if DefaultMaxTimeout != 20*time.Second || DefaultMaxOutputBytes != 128<<10 {
		t.Fatal("默认值必须与文档一致（20s / 128KiB）")
	}
}

func TestSchemaShape(t *testing.T) {
	tool, _ := newTestTool(t, nil)
	raw, err := json.Marshal(tool.Schema())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("type = %v", schema["type"])
	}
	if schema["additionalProperties"] != false {
		t.Fatal("additionalProperties 必须为 false（模型不能塞 dir/allow/id）")
	}
	required, ok := schema["required"].([]any)
	if !ok || len(required) != 2 || required[0] != "command" || required[1] != "reason" {
		t.Fatalf("required = %v", schema["required"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %v", schema["properties"])
	}
	if len(properties) != 3 {
		t.Fatalf("properties = %v（只允许 command/reason/timeout_ms）", properties)
	}
	command, ok := properties["command"].(map[string]any)
	if !ok || command["type"] != "array" {
		t.Fatalf("command schema = %v", properties["command"])
	}
	items, ok := command["items"].(map[string]any)
	if !ok || items["type"] != "string" {
		t.Fatalf("command.items = %v", command["items"])
	}
	if _, exists := properties["dir"]; exists {
		t.Fatal("schema 不得暴露 dir（工作目录由宿主派生）")
	}
	if _, exists := properties["allow"]; exists {
		t.Fatal("schema 不得暴露 allow（Allow 只能由宿主给定）")
	}
}

func TestMatchAllowSemantics(t *testing.T) {
	allow := [][]string{{"git", "fetch"}, {"echo"}}
	cases := []struct {
		name  string
		argv  []string
		want  bool
		entry []string
	}{
		{"精确命中", []string{"git", "fetch", "origin"}, true, []string{"git", "fetch"}},
		{"裸程序名命中", []string{"echo", "hi"}, true, []string{"echo"}},
		{"子命令不同", []string{"git", "push"}, false, nil},
		{"不做子串匹配", []string{"gitea", "fetch"}, false, nil},
		{"末元素允许等号形式", []string{"git", "fetch=origin"}, true, []string{"git", "fetch"}},
		{"参数不参与匹配", []string{"echo", "allow"}, true, []string{"echo"}},
		{"空 argv", nil, false, nil},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			entry, ok := MatchAllow(allow, testCase.argv)
			if ok != testCase.want {
				t.Fatalf("MatchAllow(%v) = %v, want %v", testCase.argv, ok, testCase.want)
			}
			if ok && !reflect.DeepEqual(entry, testCase.entry) {
				t.Fatalf("entry = %v, want %v", entry, testCase.entry)
			}
		})
	}
	if _, ok := MatchAllow(nil, []string{"echo"}); ok {
		t.Fatal("空 Allow 不得命中任何命令")
	}
}

// TestDocumentedOptionsExample 保证 doc.go 里"可以直接抄"的示例是能构造出来的
// （示例一旦漂移，这个用例会红）。
func TestDocumentedOptionsExample(t *testing.T) {
	workspaceRoot := rootDir(t)
	opts := Options{
		Roots: []string{workspaceRoot},
		Allow: [][]string{
			{"git", "fetch"},
		},
		Deny: [][]string{
			{"git", "push"},
		},
		MaxTimeout:     30 * time.Second,
		MaxOutputBytes: 256 << 10,
		MaxConcurrent:  2,
		MaxQueue:       8,
		Auditor: func(escalate.Request, escalate.Response) {
			// 文档示例：宿主在这里写审计日志。
		},
	}
	tool, err := NewWithBroker(newPipePair(), okApprover, opts)
	if err != nil {
		t.Fatalf("文档示例必须能构造成功: %v", err)
	}
	if closer, ok := tool.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
	// 示例里的 git push 必须被 Deny 拦住（Allow 的 {"git", ...} 子命令级条目不含 push，
	// 但 Deny 显式列了它；用本地判定验证不需要真的执行 git）。
	if _, denied := matchPrefix(opts.Deny, []string{"git", "push"}); !denied {
		t.Fatal("示例里的 Deny 必须拦住 git push")
	}
}

func TestToolMetadata(t *testing.T) {
	tool, _ := newTestTool(t, nil)
	if tool.Name() != ToolName || ToolName != "request_command_escalation" {
		t.Fatalf("Name = %q", tool.Name())
	}
	if !strings.Contains(tool.Description(), "escalation denied") {
		t.Fatalf("Description 应说明结果三态: %q", tool.Description())
	}
}

// joinArgv 把 argv 拼成可读的测试名。
func joinArgv(argv []string) string {
	out := ""
	for i, token := range argv {
		if i > 0 {
			out += " "
		}
		out += token
	}
	if out == "" {
		return "(empty)"
	}
	return out
}
