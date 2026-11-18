//go:build linux

package osrunner

// 运行时证据用例：把"沙箱里实际看到什么"打印出来，供人工核对与审计复现。
//
// 门控：RIG_SANDBOX_EVIDENCE=1（它会真的执行 curl / ls / 写文件）。
//
//	RIG_SANDBOX_EVIDENCE=1 go test ./sandbox/osrunner/ -run Evidence -v
//
// 它不是断言型的（断言在 landlock_net_test.go / bwrap_hardening_test.go），
// 存在的理由是"报告要贴真实输出"。

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func evidenceRun(t *testing.T, argv []string) string {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	out, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(out))
}

func TestEvidenceRuntimeProbe(t *testing.T) {
	if os.Getenv("RIG_SANDBOX_EVIDENCE") != "1" {
		t.Skip("证据用例默认跳过；设 RIG_SANDBOX_EVIDENCE=1 启用")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
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
	port := listener.Addr().(*net.TCPAddr).Port
	ws := t.TempDir()
	tcpCmd := []string{"/bin/sh", "-c", fmt.Sprintf(
		"curl -sS -o /dev/null -w 'http=%%{http_code}' --max-time 3 http://127.0.0.1:%d/ 2>&1; echo", port)}
	denied := sandbox.New(sandbox.WithRoots(ws), sandbox.WithMode(sandbox.ModeWorkspaceWrite), sandbox.WithNetwork(false))
	allowed := sandbox.New(sandbox.WithRoots(ws), sandbox.WithMode(sandbox.ModeWorkspaceWrite), sandbox.WithNetwork(true))

	t.Logf("sockets=%v", existingDaemonSockets())
	if path, reason := probeBwrapDetailed(DefaultProbeTimeout); reason == "" {
		bwrap := &bwrapRunner{path: path}
		if sockets := existingDaemonSockets(); len(sockets) > 0 {
			confined, _ := bwrap.Confine([]string{"/bin/sh", "-c",
				"ls -l " + sockets[0] + "; stat -c '%F' " + sockets[0]}, denied)
			t.Logf("bwrap socket in sandbox: %s", evidenceRun(t, confined.Argv))
			host, _ := exec.Command("sh", "-c", "ls -l "+sockets[0]+"; stat -c '%F' "+sockets[0]).CombinedOutput()
			t.Logf("host socket: %s", strings.TrimSpace(string(host)))
		}
		c1, _ := bwrap.Confine(tcpCmd, denied)
		t.Logf("bwrap net denied: %s", evidenceRun(t, c1.Argv))
		c2, _ := bwrap.Confine(tcpCmd, allowed)
		t.Logf("bwrap net allowed: %s", evidenceRun(t, c2.Argv))
		profile, _ := BuildProfile(denied)
		t.Logf("privateTmp=%v reason=%q ws=%s", profile.PrivateTmp, profile.TmpDisabledReason, ws)
		c3, _ := bwrap.Confine([]string{"/bin/sh", "-c",
			"echo x > " + filepath.Join(ws, "w.txt") + " && cat " + filepath.Join(ws, "w.txt") + " && echo WROTE_OK"}, denied)
		t.Logf("bwrap write into /tmp workspace: %s", evidenceRun(t, c3.Argv))
	} else {
		t.Logf("bwrap unavailable: %s", reason)
	}
	if abi, ok := probeLandlock(DefaultProbeTimeout); ok {
		exe, _ := os.Executable()
		ll := &landlockRunner{exe: exe, abi: abi}
		c1, _ := ll.Confine(tcpCmd, denied)
		t.Logf("landlock(abi=%d) net denied: %s", abi, evidenceRun(t, c1.Argv))
		c2, _ := ll.Confine(tcpCmd, allowed)
		t.Logf("landlock(abi=%d) net allowed: %s", abi, evidenceRun(t, c2.Argv))
		t.Logf("landlock limits: %s", ll.RunnerLimits())
	}
	t.Logf("bwrap limits: %s", (&bwrapRunner{}).RunnerLimits())
	t.Logf("seatbelt limits: %s", (&seatbeltRunner{}).RunnerLimits())
}
