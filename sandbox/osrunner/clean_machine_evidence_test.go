//go:build linux

package osrunner

// 干净机器首次运行的证据用例（打印真实输出，供人工核对）。
//
//	RIG_SANDBOX_EVIDENCE=1 go test ./sandbox/osrunner/ -run EvidenceCleanMachine -v

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func TestEvidenceCleanMachine(t *testing.T) {
	if os.Getenv("RIG_SANDBOX_EVIDENCE") != "1" {
		t.Skip("证据用例默认跳过；设 RIG_SANDBOX_EVIDENCE=1 启用")
	}
	runner := localBwrap(t)
	home := cleanMachineHome(t)
	ws := cleanMachineHome(t)
	origHome, _ := os.UserHomeDir()
	origRustup := os.Getenv("RUSTUP_HOME")
	if origRustup == "" && origHome != "" {
		origRustup = filepath.Join(origHome, ".rustup")
	}
	if err := os.MkdirAll(filepath.Join(home, ".cargo"), 0o700); err != nil {
		t.Fatalf("prep: %v", err)
	}
	// 先用"目录不存在"的状态构建一次 profile，看看诊断怎么说。
	policy := sandbox.New(sandbox.WithRoots(ws), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	before, _ := BuildProfile(policy)
	t.Logf("Confine 之前：ToolchainCaches=%d 项，MissingToolchainCaches=%v", len(before.ToolchainCaches), before.MissingToolchainCaches)

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CARGO_HOME", filepath.Join(home, ".cargo"))
	t.Setenv("GOCACHE", filepath.Join(home, ".cache", "go-build"))
	t.Setenv("GOMODCACHE", filepath.Join(home, "go", "pkg", "mod"))
	t.Setenv("PIP_CACHE_DIR", filepath.Join(home, ".cache", "pip"))
	t.Setenv("RUSTUP_HOME", origRustup)

	cargoDir := filepath.Join(ws, "crate")
	_ = os.MkdirAll(filepath.Join(cargoDir, "src"), 0o755)
	_ = os.WriteFile(filepath.Join(cargoDir, "Cargo.toml"), []byte("[package]\nname = \"probe\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(cargoDir, "src", "main.rs"), []byte("fn main() { println!(\"ok\"); }\n"), 0o644)
	out, err := runBwrap(t, runner, policy, "/bin/sh", "-c",
		"cd "+cargoDir+" && CARGO_TARGET_DIR="+filepath.Join(ws, "target")+" cargo build --offline 2>&1 | tail -2; echo CARGO_DONE")
	t.Logf("首次 cargo build：err=%v\n%s", err, strings.TrimSpace(out))

	goDir := filepath.Join(ws, "gomod")
	_ = os.MkdirAll(goDir, 0o755)
	_ = os.WriteFile(filepath.Join(goDir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o644)
	_ = os.WriteFile(filepath.Join(goDir, "main.go"), []byte("package main\n\nfunc main() { println(\"ok\") }\n"), 0o644)
	out, err = runBwrap(t, runner, policy, "/bin/sh", "-c", "cd "+goDir+" && go build -o probe ./... && echo BUILD_OK")
	t.Logf("首次 go build：err=%v out=%s", err, strings.TrimSpace(out))

	after, _ := BuildProfile(policy)
	t.Logf("Confine 之后：MissingToolchainCaches=%v", after.MissingToolchainCaches)
	for _, root := range after.ToolchainCaches {
		info, statErr := os.Stat(root.Root)
		t.Logf("  %-55s exists=%v mode=%v", root.Root, statErr == nil, func() string {
			if info == nil {
				return "-"
			}
			return info.Mode().Perm().String()
		}())
	}
}
