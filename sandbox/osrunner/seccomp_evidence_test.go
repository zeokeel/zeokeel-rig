//go:build linux

package osrunner

// seccomp 的**证据**用例：把"允许/禁止两态"的真实输出打出来，供人工核对。
//
//	RIG_SANDBOX_EVIDENCE=1 go test ./sandbox/osrunner/ -run EvidenceSeccomp -v
//
// 断言在 seccomp_network_test.go；这里只负责可复现的输出。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func TestEvidenceSeccomp(t *testing.T) {
	if os.Getenv("RIG_SANDBOX_EVIDENCE") != "1" {
		t.Skip("证据用例默认跳过；设 RIG_SANDBOX_EVIDENCE=1 启用")
	}
	runner := localBwrap(t)
	port := startHTTPStub(t)
	ws := t.TempDir()
	denied := sandbox.New(sandbox.WithRoots(ws), sandbox.WithMode(sandbox.ModeWorkspaceWrite), sandbox.WithNetwork(false))
	allowed := sandbox.New(sandbox.WithRoots(ws), sandbox.WithMode(sandbox.ModeWorkspaceWrite), sandbox.WithNetwork(true))

	run := func(tag string, policy *sandbox.Policy, script string) {
		out, err := runBwrap(t, runner, policy, "/bin/sh", "-c", script)
		trimmed := strings.ReplaceAll(strings.TrimSpace(out), "\n", " | ")
		t.Logf("%-28s err=%-14v %s", tag, fmt.Sprint(err), trimmed)
	}

	curl := fmt.Sprintf("curl -sS -o /dev/null -w 'http=%%{http_code}' --max-time 3 http://127.0.0.1:%d/ 2>&1", port)
	dockerCmd := "timeout 5 docker ps 2>&1 | head -2"

	if _, err := exec.LookPath("docker"); err == nil && len(existingDaemonSockets()) > 0 {
		run("docker ps / network=true", allowed, dockerCmd)
		run("docker ps / network=false", denied, dockerCmd)
	}
	run("curl loopback / true", allowed, curl)
	run("curl loopback / false", denied, curl)

	// 工具链：go build / cargo / npm 在断网沙箱里是否正常。
	modDir := filepath.Join(ws, "gomod")
	_ = os.MkdirAll(modDir, 0o755)
	_ = os.WriteFile(filepath.Join(modDir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o644)
	_ = os.WriteFile(filepath.Join(modDir, "main.go"), []byte("package main\n\nfunc main() { println(\"ok\") }\n"), 0o644)
	// 注意：本容器的宿主根文件系统是只读的（findmnt / → ro），所以缓存与
	// CARGO_HOME 一律指到工作区里，避免把"环境限制"误读成"seccomp 打死了工具链"。
	goCache := filepath.Join(ws, "gocache")
	_ = os.MkdirAll(goCache, 0o755)
	run("go build / network=false", denied, "cd "+modDir+" && GOCACHE="+goCache+" go build -o probe ./... && echo BUILD_OK")

	if _, err := exec.LookPath("cargo"); err == nil {
		cargoDir := filepath.Join(ws, "cargoproj")
		_ = os.MkdirAll(filepath.Join(cargoDir, "src"), 0o755)
		_ = os.WriteFile(filepath.Join(cargoDir, "Cargo.toml"), []byte("[package]\nname = \"probe\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(cargoDir, "src", "main.rs"), []byte("fn main() { println!(\"ok\"); }\n"), 0o644)
		cargoHome := filepath.Join(ws, "cargohome")
		_ = os.MkdirAll(cargoHome, 0o755)
		run("cargo build / network=false", denied,
			"cd "+cargoDir+" && CARGO_HOME="+cargoHome+" CARGO_TARGET_DIR="+filepath.Join(ws, "cargo-target")+" cargo build --offline 2>&1 | tail -3; echo CARGO_DONE")
	}
	if _, err := exec.LookPath("npm"); err == nil {
		npmDir := filepath.Join(ws, "npmproj")
		_ = os.MkdirAll(npmDir, 0o755)
		run("npm install(离线) / false", denied, "cd "+npmDir+" && npm install --offline --no-audit --no-fund 2>&1 | tail -2; echo NPM_DONE")
	}
	t.Logf("bwrap limits: %s", runner.RunnerLimits())
	if abi, ok := probeLandlock(DefaultProbeTimeout); ok {
		exe, _ := os.Executable()
		ll := &landlockRunner{exe: exe, abi: abi}
		t.Logf("landlock limits: %s", ll.RunnerLimits())
	}
}
