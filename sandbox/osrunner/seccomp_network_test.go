//go:build linux

package osrunner

// seccomp 网络过滤器的能力用例：AllowNetwork=false 时 connect(2) 必须失败
// （这同时根治了 docker/podman/ssh-agent 这类"宿主守护进程 socket 逃逸"），
// AllowNetwork=true 时一切照常（不能把本地开发流程打死）。

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/internal/seccomp"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// startHTTPStub 起一个回环 HTTP 服务，返回端口。
func startHTTPStub(t *testing.T) int {
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
			_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
			_ = conn.Close()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

// curlStub 在沙箱里访问回环端口，返回 curl 的 stderr+状态码。
func curlStub(port int) []string {
	return []string{"/bin/sh", "-c", fmt.Sprintf(
		"curl -sS -o /dev/null -w 'http=%%{http_code}' --max-time 3 http://127.0.0.1:%d/ 2>&1; echo", port)}
}

// 过滤器本身的结构：非空、指令数为 8 的倍数、以 RET ALLOW 结尾的那条存在。
func TestSeccompProgramShape(t *testing.T) {
	program, err := seccomp.NetworkDeny()
	if err != nil {
		t.Skipf("当前架构没有内置系统调用号表：%v", err)
	}
	if program.Empty() {
		t.Fatal("NetworkDeny 返回了空程序")
	}
	raw := program.Bytes()
	if len(raw)%8 != 0 || program.Len() < 10 {
		t.Fatalf("程序长度异常：%d 字节 / %d 条指令", len(raw), program.Len())
	}
	// 最后一条指令是 RET EPERM（socket 块的兜底拒绝），倒数第二条是 RET ALLOW。
	last := raw[len(raw)-8:]
	if last[0] != 0x06 || last[1] != 0x00 {
		t.Fatalf("最后一条不是 RET 指令：% x", last)
	}
}

// 允许网络时不装过滤器（AllowNetwork=true → 空程序）。
func TestSeccompProgramSkippedWhenNetworkAllowed(t *testing.T) {
	program, err := seccompProgram(true)
	if err != nil {
		t.Fatalf("seccompProgram(true): %v", err)
	}
	if !program.Empty() {
		t.Fatal("AllowNetwork=true 时不应生成过滤器（会断掉本地 dev server）")
	}
}

// bwrap：断网时回环连不上；允许网络时连得上。
func TestBwrapSeccompBlocksLoopback(t *testing.T) {
	runner := localBwrap(t)
	port := startHTTPStub(t)
	ws := t.TempDir()

	denied := sandbox.New(sandbox.WithRoots(ws), sandbox.WithMode(sandbox.ModeWorkspaceWrite), sandbox.WithNetwork(false))
	out, _ := runBwrap(t, runner, denied, curlStub(port)...)
	if !strings.Contains(out, "http=000") {
		t.Fatalf("AllowNetwork=false 下 bwrap 仍连上回环：%s", strings.TrimSpace(out))
	}

	allowed := sandbox.New(sandbox.WithRoots(ws), sandbox.WithMode(sandbox.ModeWorkspaceWrite), sandbox.WithNetwork(true))
	out, _ = runBwrap(t, runner, allowed, curlStub(port)...)
	if !strings.Contains(out, "http=200") {
		t.Fatalf("AllowNetwork=true 下 bwrap 连不上回环（开发流程被打死）：%s", strings.TrimSpace(out))
	}
}

// bwrap：断网时 docker 客户端连不上守护进程 socket（这是 seccomp 根治的逃逸面）。
func TestBwrapSeccompBlocksDockerClient(t *testing.T) {
	if len(existingDaemonSockets()) == 0 {
		t.Skip("宿主没有 docker/podman/containerd socket")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("宿主没有 docker 客户端")
	}
	runner := localBwrap(t)
	denied := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithMode(sandbox.ModeWorkspaceWrite), sandbox.WithNetwork(false))
	out, _ := runBwrap(t, runner, denied, "/bin/sh", "-c", "timeout 5 docker ps 2>&1 | head -3; echo END")
	lower := strings.ToLower(out)
	if !strings.Contains(lower, "permission denied") && !strings.Contains(lower, "connection refused") &&
		!strings.Contains(lower, "cannot connect") && !strings.Contains(lower, "permission") {
		t.Fatalf("docker 客户端在断网沙箱内没有被拒：%s", strings.TrimSpace(out))
	}
}

// Landlock：helper 内装 seccomp，效果必须与 bwrap 一致。
func TestLandlockSeccompBlocksLoopback(t *testing.T) {
	runner := landlockRunnerForTest(t)
	port := startHTTPStub(t)
	ws := t.TempDir()
	denied := sandbox.New(sandbox.WithRoots(ws), sandbox.WithMode(sandbox.ModeWorkspaceWrite), sandbox.WithNetwork(false))
	out, _ := runConfined(t, runner, denied, "curl -sS -o /dev/null -w 'http=%{http_code}' --max-time 3 http://127.0.0.1:"+fmt.Sprint(port)+"/ 2>&1; echo")
	if !strings.Contains(out, "http=000") {
		t.Fatalf("AllowNetwork=false 下 Landlock 仍连上回环：%s", strings.TrimSpace(out))
	}
	allowed := sandbox.New(sandbox.WithRoots(ws), sandbox.WithMode(sandbox.ModeWorkspaceWrite), sandbox.WithNetwork(true))
	out, _ = runConfined(t, runner, allowed, "curl -sS -o /dev/null -w 'http=%{http_code}' --max-time 3 http://127.0.0.1:"+fmt.Sprint(port)+"/ 2>&1; echo")
	if !strings.Contains(out, "http=200") {
		t.Fatalf("AllowNetwork=true 下 Landlock 连不上回环：%s", strings.TrimSpace(out))
	}
}

// seccomp 不能影响正常工具链：沙箱内 go build 必须成功。
func TestSeccompDoesNotBreakGoBuild(t *testing.T) {
	runner := localBwrap(t)
	ws := t.TempDir()
	modDir := filepath.Join(ws, "mod")
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "main.go"), []byte("package main\n\nfunc main() { println(\"ok\") }\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(ws), sandbox.WithMode(sandbox.ModeWorkspaceWrite), sandbox.WithNetwork(false))
	out, err := runBwrap(t, runner, policy, "/bin/sh", "-c", "cd "+modDir+" && go build -o "+filepath.Join(modDir, "probe")+" ./... && echo BUILD_OK")
	if err != nil || !strings.Contains(out, "BUILD_OK") {
		t.Fatalf("seccomp 下 go build 失败：err=%v\n%s", err, out)
	}
}
