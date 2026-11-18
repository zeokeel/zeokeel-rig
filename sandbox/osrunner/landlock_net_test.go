//go:build linux

package osrunner

// Landlock 网络隔离的回归：同一份 `AllowNetwork: false` 策略下，网络必须真的断。
//
// 审计实测（ABI 4）：bwrap 下 curl 返回 000（连不上），Landlock 下返回 301
// （能连上）——说明当时规则集根本没把网络放进 handled 集合。这里先复现，再修。

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// startTCPListener 起一个只在本机监听的 TCP 服务，返回端口。用例结束后自动关。
func startTCPListener(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			// 回一个最小 HTTP 响应，让 curl 拿到 200（而不是被重置）。
			_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
			_ = conn.Close()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

// httpStatus 用 curl 访问本机监听端口，返回 HTTP 状态码（连不上是 000）。
func httpStatus(t *testing.T, runner *landlockRunner, policy *sandbox.Policy, port int) string {
	t.Helper()
	script := fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' --max-time 3 http://127.0.0.1:%d/ || true", port)
	out, _ := runConfined(t, runner, policy, script)
	return strings.TrimSpace(out)
}

// 核心回归：Landlock 下 AllowNetwork=false 必须真的断网。
func TestLandlockDeniesNetworkWhenDisallowed(t *testing.T) {
	port := startTCPListener(t)
	runner := landlockRunnerForTest(t)
	if landlockNetworkCoverage(runner.abi) != "full" {
		t.Skipf("本机 Landlock ABI %d 不支持网络治理", runner.abi)
	}
	ws := t.TempDir()
	denied := sandbox.New(
		sandbox.WithRoots(ws),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithNetwork(false),
	)
	if got := httpStatus(t, runner, denied, port); got != "000" {
		t.Fatalf("AllowNetwork=false 下仍然连上了本机端口（http_code=%q）——网络域没有生效", got)
	}
	allowed := sandbox.New(
		sandbox.WithRoots(ws),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithNetwork(true),
	)
	if got := httpStatus(t, runner, allowed, port); got == "000" {
		t.Fatalf("AllowNetwork=true 下连不上本机端口（http_code=%q）——网络授权被误伤", got)
	}
}

// RunnerLimits 必须说明"unix socket 不受治理"（docker/podman 逃逸面）。
func TestLandlockLimitsMentionDaemonSockets(t *testing.T) {
	runner := landlockRunnerForTest(t)
	limits := strings.ToLower(runner.RunnerLimits())
	if !strings.Contains(limits, "socket") {
		t.Fatalf("RunnerLimits 没有提到 unix socket 不受治理：%q", runner.RunnerLimits())
	}
	if !strings.Contains(limits, "docker") && !strings.Contains(limits, "daemon") {
		t.Fatalf("RunnerLimits 没有点名宿主守护进程 socket：%q", runner.RunnerLimits())
	}
}
