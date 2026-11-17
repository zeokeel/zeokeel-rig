package escalate

// 本文件是端到端用例：真实的 Broker + 真实的 os/exec（在 net.Pipe 上跑线协议）。
// 覆盖“命令真跑起来”“策略拒绝不执行”“审批拒绝更不执行”“超时”“截断”“审计”
// 以及并发峰值。
//
// 只依赖 POSIX 上常见的 coreutils（echo/touch/sleep/seq），Windows 上跳过——构建
// 仍然必须在 Windows 通过，所以这里用 runtime.GOOS 跳过而不是 build tag。

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox/escalate"
)

// pipePair 把 net.Pipe() 的两端包成 TransportPair，交给 NewWithBroker。
type pipePair struct {
	net.Conn
	brokerEnd, clientEnd net.Conn
}

func newPipePair() *pipePair {
	brokerEnd, clientEnd := net.Pipe()
	return &pipePair{Conn: clientEnd, brokerEnd: brokerEnd, clientEnd: clientEnd}
}

func (p *pipePair) Endpoints() (io.ReadWriteCloser, io.ReadWriteCloser) {
	return p.brokerEnd, p.clientEnd
}

func (p *pipePair) Close() error {
	_ = p.brokerEnd.Close()
	return p.clientEnd.Close()
}

// requirePOSIX 在没有 coreutils 的平台上跳过端到端用例。
func requirePOSIX(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("端到端用例依赖 POSIX coreutils（echo/touch/sleep/seq）")
	}
}

// startE2E 用 net.Pipe 装配 Broker + 工具，并在测试结束时关闭通道。
func startE2E(t *testing.T, opts Options, approver Approver) rig.Tool {
	t.Helper()
	pair := newPipePair()
	tool, err := NewWithBroker(pair, approver, opts)
	if err != nil {
		t.Fatalf("NewWithBroker: %v", err)
	}
	closer, ok := tool.(interface{ Close() error })
	if !ok {
		t.Fatal("工具必须实现 Close（否则后台 Serve 无法收敛）")
	}
	t.Cleanup(func() { _ = closer.Close() })
	return tool
}

// e2eOptions 返回一份“允许 echo/touch/sleep/seq”的配置。
func e2eOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Roots:          []string{t.TempDir()},
		Allow:          [][]string{{"echo"}, {"touch"}, {"sleep"}, {"seq"}, {"git", "fetch"}},
		Deny:           [][]string{{"git", "push"}},
		MaxTimeout:     2 * time.Second,
		MaxOutputBytes: 64 << 10,
	}
}

func TestEndToEndEchoSucceeds(t *testing.T) {
	requirePOSIX(t)
	tool := startE2E(t, e2eOptions(t), okApprover)
	result := invoke(t, tool, `{"command":["echo","hi"],"reason":"smoke test"}`)
	if result.IsError {
		t.Fatalf("echo 应该成功: %q", result.Content)
	}
	if !strings.HasPrefix(result.Content, "exit_code: 0\n") {
		t.Fatalf("首行必须是 exit_code: 0: %q", result.Content)
	}
	if !strings.Contains(result.Content, "hi") {
		t.Fatalf("stdout 丢了: %q", result.Content)
	}
}

func TestEndToEndDeniedCommandIsNotExecuted(t *testing.T) {
	requirePOSIX(t)
	opts := e2eOptions(t)
	tool := startE2E(t, opts, okApprover)
	result := invoke(t, tool, `{"command":["git","push","origin","main"],"reason":"deploy"}`)
	if !strings.HasPrefix(result.Content, DeniedPrefix) {
		t.Fatalf("git push 必须被拒绝: %q", result.Content)
	}
	if strings.Contains(result.Content, "exit_code") {
		t.Fatalf("拒绝不该有退出码（命令没跑）: %q", result.Content)
	}
}

func TestEndToEndApprovalRejectionMeansNoExecution(t *testing.T) {
	requirePOSIX(t)
	opts := e2eOptions(t)
	marker := filepath.Join(opts.Roots[0], "should-not-exist")
	tool := startE2E(t, opts, func(context.Context, escalate.Request) (bool, string, error) {
		return false, "operator declined", nil
	})
	result := invoke(t, tool, `{"command":["touch","`+marker+`"],"reason":"marker"}`)
	if !strings.HasPrefix(result.Content, DeniedPrefix) || !strings.Contains(result.Content, "operator declined") {
		t.Fatalf("审批拒绝文案不对: %q", result.Content)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("审批拒绝之后命令仍然被执行了（marker 存在）: %v", err)
	}
}

func TestEndToEndApprovedCommandExecutes(t *testing.T) {
	requirePOSIX(t)
	opts := e2eOptions(t)
	marker := filepath.Join(opts.Roots[0], "created")
	tool := startE2E(t, opts, okApprover)
	result := invoke(t, tool, `{"command":["touch","`+marker+`"],"reason":"marker"}`)
	if result.IsError {
		t.Fatalf("touch 应该成功: %q", result.Content)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("命令没有真的执行: %v", err)
	}
}

func TestEndToEndTimeoutIsFailedState(t *testing.T) {
	requirePOSIX(t)
	opts := e2eOptions(t)
	opts.MaxTimeout = 100 * time.Millisecond
	tool := startE2E(t, opts, okApprover)
	start := time.Now()
	result := invoke(t, tool, `{"command":["sleep","5"],"reason":"should be killed","timeout_ms":2000}`)
	elapsed := time.Since(start)
	if !strings.HasPrefix(result.Content, FailedPrefix) {
		t.Fatalf("超时必须走 failed 通道: %q", result.Content)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("超时没有生效，耗时 %s", elapsed)
	}
}

func TestEndToEndTruncatesLargeOutput(t *testing.T) {
	requirePOSIX(t)
	opts := e2eOptions(t)
	opts.MaxOutputBytes = 64
	tool := startE2E(t, opts, okApprover)
	result := invoke(t, tool, `{"command":["seq","1","10000"],"reason":"flood"}`)
	if !strings.Contains(result.Content, truncatedNotice) {
		t.Fatalf("大输出必须标注截断: %q", result.Content)
	}
	if len(result.Content) > 4096 {
		t.Fatalf("输出没有被截断（%d 字节）", len(result.Content))
	}
}

func TestEndToEndApproverSeesDerivedRequest(t *testing.T) {
	requirePOSIX(t)
	opts := e2eOptions(t)
	var seen escalate.Request
	tool := startE2E(t, opts, func(_ context.Context, req escalate.Request) (bool, string, error) {
		seen = req
		return true, "", nil
	})
	result := invoke(t, tool, `{"command":["echo","hi"],"reason":"because","dir":"/etc"}`)
	if result.IsError {
		t.Fatalf("echo 应该成功: %q", result.Content)
	}
	if len(seen.Argv) != 2 || seen.Argv[0] != "echo" || seen.Argv[1] != "hi" {
		t.Fatalf("审批看到的 argv = %v", seen.Argv)
	}
	if seen.Reason != "because" {
		t.Fatalf("审批看到的 reason = %q", seen.Reason)
	}
	if seen.Dir != filepath.Clean(opts.Roots[0]) && !strings.HasPrefix(seen.Dir, filepath.Clean(opts.Roots[0])) {
		t.Fatalf("审批看到的 Dir = %q, want 宿主根 %q（模型传的 /etc 必须被忽略）", seen.Dir, opts.Roots[0])
	}
}

func TestEndToEndAuditorSeesDenials(t *testing.T) {
	requirePOSIX(t)
	opts := e2eOptions(t)
	var mu sync.Mutex
	var audited []escalate.Response
	opts.Auditor = func(_ escalate.Request, resp escalate.Response) {
		mu.Lock()
		audited = append(audited, resp)
		mu.Unlock()
	}
	tool := startE2E(t, opts, okApprover)
	invoke(t, tool, `{"command":["echo","hi"],"reason":"ok"}`)
	invoke(t, tool, `{"command":["git","push"],"reason":"denied"}`)
	mu.Lock()
	defer mu.Unlock()
	if len(audited) != 2 {
		t.Fatalf("审计条数 = %d, want 2", len(audited))
	}
	if !audited[1].Denied || !strings.Contains(audited[1].DenyReason, "禁止列表") {
		t.Fatalf("拒绝没有进入审计: %+v", audited[1])
	}
}

func TestEndToEndConcurrentPeakRespectsLimit(t *testing.T) {
	requirePOSIX(t)
	opts := e2eOptions(t)
	opts.MaxConcurrent = 2
	opts.MaxQueue = 8
	var mu sync.Mutex
	active, peak := 0, 0
	tool := startE2E(t, opts, func(_ context.Context, _ escalate.Request) (bool, string, error) {
		mu.Lock()
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return true, "", nil
	})
	var wg sync.WaitGroup
	errs := make(chan string, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := tool.Invoke(context.Background(), call(`{"command":["echo","hi"],"reason":"concurrent"}`))
			if err != nil || result.IsError {
				errs <- result.Content
			}
		}()
	}
	wg.Wait()
	close(errs)
	for content := range errs {
		t.Fatalf("并发调用失败: %q", content)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > 2 {
		t.Fatalf("并发峰值 = %d, want <= 2", peak)
	}
}

func TestEndToEndCloseStopsChannel(t *testing.T) {
	requirePOSIX(t)
	pair := newPipePair()
	tool, err := NewWithBroker(pair, okApprover, e2eOptions(t))
	if err != nil {
		t.Fatalf("NewWithBroker: %v", err)
	}
	if err := tool.(interface{ Close() error }).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	result := invoke(t, tool, `{"command":["echo","hi"],"reason":"after close"}`)
	if !strings.HasPrefix(result.Content, FailedPrefix) {
		t.Fatalf("Close 之后必须 failed: %q", result.Content)
	}
}

func TestNewWithPairWorksWithRealPipe(t *testing.T) {
	requirePOSIX(t)
	brokerEnd, clientEnd := net.Pipe()
	tool, err := NewWithPair(brokerEnd, clientEnd, okApprover, e2eOptions(t))
	if err != nil {
		t.Fatalf("NewWithPair: %v", err)
	}
	t.Cleanup(func() { _ = tool.(interface{ Close() error }).Close() })
	result := invoke(t, tool, `{"command":["echo","pair"],"reason":"pipe"}`)
	if result.IsError || !strings.Contains(result.Content, "pair") {
		t.Fatalf("NewWithPair 失败: %q", result.Content)
	}
}
