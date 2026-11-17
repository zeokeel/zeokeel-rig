package escalate_test

// 测试共用的接线与断言工具。
//
// 所有接线都用 net.Pipe()（同步、无缓冲）构造一对 io.ReadWriteCloser：
// broker 在 Serve 里读，client 在 readLoop 里读，天然并发，不会互相阻塞。

import (
	"bufio"
	"context"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox/escalate"
)

// pipePair 返回一对内存连接（同步、无缓冲）；测试结束时自动关闭。
func pipePair(t *testing.T) (server, client net.Conn) {
	t.Helper()
	server, client = net.Pipe()
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	return server, client
}

// mkdir 是 os.MkdirAll 的测试简写。
func mkdir(path string) error { return os.MkdirAll(path, 0o755) }

// requireUnix 跳过依赖 POSIX 命令（echo/sh/sleep）的用例；本包在 windows 上只需
// 编译通过，执行类用例留给 Linux/macOS 跑。
func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("执行类用例依赖 POSIX 命令")
	}
}

// testPolicy 返回一个"工作区可写、允许 echo/true/sh -c"的基线策略及其根目录。
func testPolicy(t *testing.T) (escalate.Policy, string) {
	t.Helper()
	root := t.TempDir()
	return escalate.Policy{
		Allow: [][]string{{"echo"}, {"true"}, {"sh", "-c"}},
		Roots: []string{root},
	}, root
}

// startBroker 起一个 broker 并把另一端包装成 client（常规路径）。
func startBroker(t *testing.T, policy escalate.Policy, opts ...escalate.BrokerOption) *escalate.Client {
	t.Helper()
	conn, _ := startRawServer(t, policy, opts...)
	return escalate.NewClient(conn)
}

// startRawServer 起一个 broker，返回测试自己持有的那端连接（用于直接说 NDJSON）。
func startRawServer(t *testing.T, policy escalate.Policy, opts ...escalate.BrokerOption) (net.Conn, <-chan error) {
	t.Helper()
	broker, err := escalate.NewBroker(policy, opts...)
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	serverConn, clientConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- broker.Serve(ctx, serverConn) }()
	t.Cleanup(func() {
		cancel()
		_ = clientConn.Close()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Errorf("Serve 未在 3s 内返回（疑似泄漏）")
		}
	})
	return clientConn, errCh
}

// rawRoundTrip 写一行、读一行。
func rawRoundTrip(t *testing.T, conn net.Conn, r *bufio.Reader, line string) string {
	t.Helper()
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("写请求失败: %v", err)
	}
	resp, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("读响应失败: %v", err)
	}
	return resp
}

// mustContain 断言错误/文本里含某个关键片段。
func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("文本 %q 未包含 %q", got, want)
	}
}

// execCtx 返回带超时的 context，避免用例挂死。
func execCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// waitGoroutines 在容差内等待 goroutine 数回落到基线，用于"无泄漏"断言。
func waitGoroutines(t *testing.T, baseline, tolerance int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last int
	for {
		last = runtime.NumGoroutine()
		if last <= baseline+tolerance {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine 泄漏: baseline=%d, now=%d (容差 %d)", baseline, last, tolerance)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
