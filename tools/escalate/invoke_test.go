package escalate

// 本文件用一条"假连接"驱动工具：可以精确控制 Broker 的响应，因此能稳定地覆盖
// 成功 / 拒绝 / 繁忙 / 通道故障四种结果，以及本地策略、审批、限流、审计。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox/escalate"
)

// fakeConn 是 io.ReadWriteCloser 的测试替身：
//   - Write 解析出 Request 并记录下来，然后按 respond 生成一条 Response 塞进读缓冲；
//   - Read 把响应行交给 Client 的读循环。
//
// respond 为 nil 时回一条"成功、stdout=ok"的响应。
type fakeConn struct {
	mu       sync.Mutex
	requests []escalate.Request
	respond  func(escalate.Request) escalate.Response

	pending []byte
	closed  chan struct{}
	once    sync.Once
}

func newFakeConn(respond func(escalate.Request) escalate.Response) *fakeConn {
	return &fakeConn{respond: respond, closed: make(chan struct{})}
}

func (c *fakeConn) Write(p []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, io.ErrClosedPipe
	default:
	}
	var req escalate.Request
	if err := json.Unmarshal(bytes.TrimSpace(p), &req); err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.requests = append(c.requests, req)
	respond := c.respond
	c.mu.Unlock()
	resp := escalate.Response{ID: req.ID, ExitCode: 0, Stdout: "ok"}
	if respond != nil {
		resp = respond(req)
		resp.ID = req.ID
	}
	line, err := json.Marshal(resp)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.pending = append(c.pending, append(line, '\n')...)
	c.mu.Unlock()
	return len(p), nil
}

func (c *fakeConn) Read(p []byte) (int, error) {
	for {
		c.mu.Lock()
		if len(c.pending) > 0 {
			n := copy(p, c.pending)
			c.pending = c.pending[n:]
			c.mu.Unlock()
			return n, nil
		}
		c.mu.Unlock()
		select {
		case <-c.closed:
			return 0, io.ErrClosedPipe
		case <-time.After(time.Millisecond):
		}
	}
}

func (c *fakeConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// recorded 返回收到过的请求（副本）。
func (c *fakeConn) recorded() []escalate.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]escalate.Request(nil), c.requests...)
}

// call 构造一次工具调用。
func call(arguments string) rig.ToolCall {
	return rig.ToolCall{ID: "call-1", Name: ToolName, Arguments: arguments}
}

// invoke 调用工具并返回结果文本。
func invoke(t *testing.T, tool rig.Tool, arguments string) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), call(arguments))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	return result
}

const okArgs = `{"command":["echo","hi"],"reason":"check the host"}`

func TestInvokeSuccessRendering(t *testing.T) {
	tool, conn := newTestTool(t, func(opts *Options) {
		opts.Allow = [][]string{{"echo"}}
	})
	result := invoke(t, tool, okArgs)
	if result.IsError {
		t.Fatalf("成功结果不该是 IsError: %q", result.Content)
	}
	content := result.Content
	if !strings.HasPrefix(content, "exit_code: 0\nstdout:\n") {
		t.Fatalf("成功文案格式不对: %q", content)
	}
	if !strings.Contains(content, "ok") {
		t.Fatalf("stdout 丢了: %q", content)
	}
	if !strings.Contains(content, "\nstderr:\n"+emptyBodyNotice) {
		t.Fatalf("空 stderr 应渲染成 (empty): %q", content)
	}
	requests := conn.recorded()
	if len(requests) != 1 {
		t.Fatalf("请求数 = %d, want 1", len(requests))
	}
	// Dir 必须由工具派生（宿主根），模型没有入口指定。
	if requests[0].Dir == "" || requests[0].Dir != filepath.Clean(requests[0].Dir) {
		t.Fatalf("Dir = %q", requests[0].Dir)
	}
	if requests[0].Reason != "check the host" {
		t.Fatalf("Reason = %q", requests[0].Reason)
	}
	if len(requests[0].Env) != 0 {
		t.Fatalf("工具不该发送 Env: %v", requests[0].Env)
	}
	// Client 自动生成 ID：工具不能把模型给的 ID 透传上去。
	if requests[0].ID == "" || requests[0].ID == "call-1" {
		t.Fatalf("请求 ID 应由 Client 生成，实际 %q", requests[0].ID)
	}
}

func TestInvokeNonZeroExitKeepsSuccessState(t *testing.T) {
	tool, conn := newTestToolWith(t, nil, func(escalate.Request) escalate.Response {
		return escalate.Response{ExitCode: 3, Stdout: "partial", Stderr: "boom"}
	})
	result := invoke(t, tool, okArgs)
	if result.IsError {
		t.Fatalf("非零退出仍是「命令跑了」，不该 IsError: %q", result.Content)
	}
	if !strings.HasPrefix(result.Content, "exit_code: 3") {
		t.Fatalf("首行必须是 exit_code: 3: %q", result.Content)
	}
	if !strings.Contains(result.Content, "partial") || !strings.Contains(result.Content, "boom") {
		t.Fatalf("输出缺失: %q", result.Content)
	}
	if len(conn.recorded()) != 1 {
		t.Fatalf("请求数 = %d", len(conn.recorded()))
	}
}

func TestInvokeNegativeExitIsFailedState(t *testing.T) {
	tool, _ := newTestToolWith(t, nil, func(escalate.Request) escalate.Response {
		return escalate.Response{ExitCode: -1, Stderr: "执行超时（上限 20ms），已终止"}
	})
	result := invoke(t, tool, okArgs)
	if !result.IsError {
		t.Fatal("ExitCode=-1 必须走 failed 通道")
	}
	if !strings.HasPrefix(result.Content, FailedPrefix) {
		t.Fatalf("文案 = %q, want 前缀 %q", result.Content, FailedPrefix)
	}
	if !strings.Contains(result.Content, "执行超时") {
		t.Fatalf("诊断丢失: %q", result.Content)
	}
}

func TestInvokeDeniedRendering(t *testing.T) {
	tool, _ := newTestToolWith(t, nil, func(escalate.Request) escalate.Response {
		return escalate.Response{Denied: true, ExitCode: escalate.DeniedExitCode, DenyReason: "命中禁止列表: git push"}
	})
	result := invoke(t, tool, okArgs)
	if !result.IsError {
		t.Fatal("策略拒绝必须是 IsError（命令没执行）")
	}
	if !strings.HasPrefix(result.Content, DeniedPrefix) {
		t.Fatalf("文案 = %q, want 前缀 %q", result.Content, DeniedPrefix)
	}
	if !strings.Contains(result.Content, "命中禁止列表") {
		t.Fatalf("拒绝原因丢失: %q", result.Content)
	}
	if strings.Contains(result.Content, "exit_code") {
		t.Fatalf("拒绝不该冒充命令结果: %q", result.Content)
	}
}

func TestInvokeQueueFullRendering(t *testing.T) {
	tool, _ := newTestToolWith(t, nil, func(escalate.Request) escalate.Response {
		return escalate.Response{Denied: true, ExitCode: escalate.DeniedExitCode, DenyReason: escalate.QueueFullReason}
	})
	result := invoke(t, tool, okArgs)
	if !strings.HasPrefix(result.Content, BusyPrefix) {
		t.Fatalf("队列满必须用 busy 前缀（不是 denied）: %q", result.Content)
	}
	if !strings.Contains(result.Content, "retry") {
		t.Fatalf("busy 文案必须提示可以稍后重试: %q", result.Content)
	}
}

func TestInvokeChannelFailureRendering(t *testing.T) {
	opts := baseOptions(t)
	conn := newFakeConn(nil)
	client := escalate.NewClient(conn)
	tool, err := New(client, okApprover, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 关掉连接：通道故障。
	_ = conn.Close()
	result := invoke(t, tool, okArgs)
	if !strings.HasPrefix(result.Content, FailedPrefix) {
		t.Fatalf("通道故障必须用 failed 前缀: %q", result.Content)
	}
	if !result.IsError {
		t.Fatal("通道故障必须 IsError")
	}
}

func TestInvokeLocalDenySkipsApprovalAndChannel(t *testing.T) {
	approved := 0
	opts := baseOptions(t) // Allow 只有 echo
	conn := newFakeConn(nil)
	tool, err := New(escalate.NewClient(conn), func(context.Context, escalate.Request) (bool, string, error) {
		approved++
		return true, "", nil
	}, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := invoke(t, tool, `{"command":["git","push"],"reason":"deploy"}`)
	if !strings.HasPrefix(result.Content, DeniedPrefix) {
		t.Fatalf("未匹配 Allow 必须本地拒绝: %q", result.Content)
	}
	if approved != 0 {
		t.Fatal("本地拒绝不该进入审批")
	}
	if len(conn.recorded()) != 0 {
		t.Fatal("本地拒绝不该发到通道")
	}
}

func TestInvokeDenyListWinsOverAllow(t *testing.T) {
	tool, conn := newTestTool(t, func(opts *Options) {
		opts.Allow = [][]string{{"git"}}
		opts.Deny = [][]string{{"git", "push"}}
	})
	result := invoke(t, tool, `{"command":["git","push","origin"],"reason":"deploy"}`)
	if !strings.HasPrefix(result.Content, DeniedPrefix) {
		t.Fatalf("Deny 必须优先: %q", result.Content)
	}
	if !strings.Contains(result.Content, "禁止列表") {
		t.Fatalf("拒绝原因应提到禁止列表: %q", result.Content)
	}
	if len(conn.recorded()) != 0 {
		t.Fatal("Deny 命中不该发到通道")
	}
}

func TestInvokeApprovalRejectionMeansDenied(t *testing.T) {
	conn := newFakeConn(nil)
	opts := baseOptions(t)
	tool, err := New(escalate.NewClient(conn), func(context.Context, escalate.Request) (bool, string, error) {
		return false, "operator said no", nil
	}, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := invoke(t, tool, okArgs)
	if !strings.HasPrefix(result.Content, DeniedPrefix) {
		t.Fatalf("审批拒绝必须是 denied: %q", result.Content)
	}
	if !strings.Contains(result.Content, "operator said no") {
		t.Fatalf("审批理由必须透传: %q", result.Content)
	}
	if len(conn.recorded()) != 0 {
		t.Fatal("审批拒绝后命令不得发到通道（更不能执行）")
	}
}

func TestInvokeApprovalErrorMeansDenied(t *testing.T) {
	opts := baseOptions(t)
	tool, err := New(escalate.NewClient(newFakeConn(nil)), func(context.Context, escalate.Request) (bool, string, error) {
		return false, "", errors.New("approval UI offline")
	}, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := invoke(t, tool, okArgs)
	if !strings.HasPrefix(result.Content, DeniedPrefix) {
		t.Fatalf("审批通道出错必须按拒绝处理: %q", result.Content)
	}
	if !strings.Contains(result.Content, "approval UI offline") {
		t.Fatalf("错误原因丢失: %q", result.Content)
	}
}

func TestInvokeIgnoresModelSuppliedDirAndAllow(t *testing.T) {
	instance, conn := newTestTool(t, nil)
	root := instance.(*tool).opts.dir
	tool := instance
	result := invoke(t, tool, `{"command":["echo","hi"],"reason":"why","dir":"/etc","allow":[["sh"]],"id":"model-chosen","timeout_ms":1234}`)
	if result.IsError {
		t.Fatalf("额外字段应被忽略而不是报错: %q", result.Content)
	}
	requests := conn.recorded()
	if len(requests) != 1 {
		t.Fatalf("请求数 = %d", len(requests))
	}
	if requests[0].Dir != root {
		t.Fatalf("Dir = %q, want 宿主根 %q（模型传的 dir 必须被忽略）", requests[0].Dir, root)
	}
	if requests[0].ID == "model-chosen" {
		t.Fatal("模型不能指定请求 ID（防重放依赖 Client 生成）")
	}
	if requests[0].TimeoutMS != 1234 {
		t.Fatalf("TimeoutMS = %d, want 1234", requests[0].TimeoutMS)
	}
}

func TestInvokeArgumentValidation(t *testing.T) {
	cases := map[string]string{
		"空命令":          `{"command":[],"reason":"why"}`,
		"缺命令":          `{"reason":"why"}`,
		"空程序名":         `{"command":["  "],"reason":"why"}`,
		"非字符串元素":       `{"command":[1,2],"reason":"why"}`,
		"空理由":          `{"command":["echo"],"reason":""}`,
		"空白理由":         `{"command":["echo"],"reason":"   "}`,
		"缺理由":          `{"command":["echo"]}`,
		"NUL 字节":       `{"command":["echo","a\u0000b"],"reason":"why"}`,
		"坏 JSON":       `{"command":`,
		"command 是字符串": `{"command":"echo hi","reason":"why"}`,
	}
	for name, arguments := range cases {
		arguments := arguments
		t.Run(name, func(t *testing.T) {
			tool, conn := newTestTool(t, nil)
			result := invoke(t, tool, arguments)
			if !result.IsError {
				t.Fatalf("必须报错: %q", result.Content)
			}
			if len(conn.recorded()) != 0 {
				t.Fatal("参数非法时不得发往通道")
			}
		})
	}
}

func TestInvokeRejectsTooManyTokens(t *testing.T) {
	tool, _ := newTestTool(t, nil)
	command := make([]string, 0, maxArgvTokens+1)
	command = append(command, "echo")
	for len(command) <= maxArgvTokens {
		command = append(command, "x")
	}
	raw, err := json.Marshal(map[string]any{"command": command, "reason": "why"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	result := invoke(t, tool, string(raw))
	if !result.IsError || !strings.Contains(result.Content, "at most") {
		t.Fatalf("超长 argv 必须报错: %q", result.Content)
	}
}

func TestInvokeClampsTimeout(t *testing.T) {
	tool, conn := newTestTool(t, func(opts *Options) {
		opts.MaxTimeout = 2 * time.Second
	})
	cases := map[string]int{
		`{"command":["echo"],"reason":"r","timeout_ms":600000}`: 2000,
		`{"command":["echo"],"reason":"r","timeout_ms":0}`:      0,
		`{"command":["echo"],"reason":"r","timeout_ms":-5}`:     0,
		`{"command":["echo"],"reason":"r","timeout_ms":1500}`:   1500,
	}
	for arguments, want := range cases {
		arguments := arguments
		t.Run(arguments, func(t *testing.T) {
			if result := invoke(t, tool, arguments); result.IsError {
				t.Fatalf("不该失败: %q", result.Content)
			}
			recorded := conn.recorded()
			got := recorded[len(recorded)-1].TimeoutMS
			if got != want {
				t.Fatalf("TimeoutMS = %d, want %d", got, want)
			}
		})
	}
}

func TestInvokeAuditsEveryOutcome(t *testing.T) {
	var mu sync.Mutex
	var audited []escalate.Response
	opts := baseOptions(t)
	// false 也在 Allow 里，这样它才会真的走到（假）Broker 并被远程拒绝。
	opts.Allow = [][]string{{"echo"}, {"false"}}
	opts.Deny = [][]string{{"rm"}}
	opts.Auditor = func(_ escalate.Request, resp escalate.Response) {
		mu.Lock()
		audited = append(audited, resp)
		mu.Unlock()
	}
	conn := newFakeConn(func(req escalate.Request) escalate.Response {
		if req.Argv[0] == "false" {
			return escalate.Response{Denied: true, ExitCode: escalate.DeniedExitCode, DenyReason: "远程策略拒绝"}
		}
		return escalate.Response{ExitCode: 0, Stdout: "ok"}
	})
	tool, err := New(escalate.NewClient(conn), okApprover, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 成功、远程拒绝、本地拒绝（本地 Deny 与未匹配 Allow）各一次。
	invoke(t, tool, `{"command":["echo"],"reason":"r"}`)
	invoke(t, tool, `{"command":["false"],"reason":"r"}`)
	invoke(t, tool, `{"command":["rm","-rf","/"],"reason":"r"}`)
	invoke(t, tool, `{"command":["curl"],"reason":"r"}`)

	mu.Lock()
	defer mu.Unlock()
	if len(audited) != 4 {
		t.Fatalf("审计条数 = %d, want 4（成功与拒绝都要审）", len(audited))
	}
	if !audited[1].Denied || !strings.Contains(audited[1].DenyReason, "远程策略拒绝") {
		t.Fatalf("远程拒绝没有进入审计: %+v", audited[1])
	}
	if !audited[2].Denied || !audited[3].Denied {
		t.Fatalf("本地拒绝没有进入审计: %+v", audited)
	}
	if audited[0].Denied || audited[0].ExitCode != 0 {
		t.Fatalf("成功请求的审计记录不对: %+v", audited[0])
	}
}

func TestInvokeAuditorPanicIsIsolated(t *testing.T) {
	tool, _ := newTestTool(t, func(opts *Options) {
		opts.Auditor = func(escalate.Request, escalate.Response) { panic("auditor exploded") }
	})
	result := invoke(t, tool, okArgs)
	if result.IsError {
		t.Fatalf("审计 panic 不该影响请求结论: %q", result.Content)
	}
}

func TestInvokeTruncatesOutput(t *testing.T) {
	tool, _ := newTestToolWith(t, func(opts *Options) {
		opts.MaxOutputBytes = 16
	}, func(escalate.Request) escalate.Response {
		return escalate.Response{ExitCode: 0, Stdout: strings.Repeat("a", 100), Stderr: strings.Repeat("b", 100)}
	})
	result := invoke(t, tool, okArgs)
	if !strings.Contains(result.Content, truncatedNotice) {
		t.Fatalf("截断必须标注: %q", result.Content)
	}
	if strings.Contains(result.Content, strings.Repeat("a", 20)) {
		t.Fatalf("stdout 没有被截断: %q", result.Content)
	}
	if strings.Contains(result.Content, strings.Repeat("b", 20)) {
		t.Fatalf("stderr 没有被截断: %q", result.Content)
	}
}

// 工具侧的"队列满"判定与限流器语义放在 limiter_test.go 里用确定性信号验证；
// 这里验证的是"Broker 回 QueueFullReason 时文案是 busy 而不是 denied"。

func TestInvokeRespectsContextCancellation(t *testing.T) {
	tool, _ := newTestTool(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := tool.Invoke(ctx, call(okArgs))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !result.IsError || !strings.HasPrefix(result.Content, FailedPrefix) {
		t.Fatalf("取消应返回 failed: %q", result.Content)
	}
}

func TestConcurrentInvokesRespectMaxConcurrent(t *testing.T) {
	var mu sync.Mutex
	active, peak := 0, 0
	opts := baseOptions(t)
	opts.MaxConcurrent = 2
	opts.MaxQueue = 8
	conn := newFakeConn(nil)
	tool, err := New(escalate.NewClient(conn), func(_ context.Context, _ escalate.Request) (bool, string, error) {
		mu.Lock()
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return true, "", nil
	}, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := tool.Invoke(context.Background(), call(okArgs))
			if err != nil || result.IsError {
				t.Errorf("Invoke = %q, %v", result.Content, err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if peak > 2 {
		t.Fatalf("并发峰值 = %d, want <= 2", peak)
	}
	if peak < 2 {
		t.Fatalf("并发峰值 = %d，限流器没有并行起来（测试无意义）", peak)
	}
}

// newTestToolWith 用指定 respond 构造工具，并可改配置。
func newTestToolWith(t *testing.T, mutate func(*Options), respond func(escalate.Request) escalate.Response) (rig.Tool, *fakeConn) {
	t.Helper()
	opts := baseOptions(t)
	opts.Allow = [][]string{{"echo"}}
	if mutate != nil {
		mutate(&opts)
	}
	conn := newFakeConn(respond)
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
