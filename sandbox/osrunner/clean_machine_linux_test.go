//go:build linux

package osrunner

// 干净机器首次构建的端到端用例：需要真实 cargo/go 与 bwrap，因此只在 Linux 上
// 编译（bwrap 后端也只在 Linux 上存在）。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 端到端：干净机器（空 HOME + 空缓存目录）里首次 cargo build / go build 必须成功，
// 且缓存目录确实被创建出来。
func TestCleanMachineFirstCargoBuildWorks(t *testing.T) {
	cleanMachineSkipUnlessGated(t)
	runner := localBwrap(t)
	home := cleanMachineHome(t)
	ws := cleanMachineHome(t)

	// 记住**真实**的 HOME：rustup 的工具链选择依赖 RUSTUP_HOME 里的 settings.toml，
	// 我们要换掉的是"缓存目录"，不是"工具链安装位置"。
	origHome, _ := os.UserHomeDir()
	origRustup := os.Getenv("RUSTUP_HOME")
	if origRustup == "" && origHome != "" {
		origRustup = filepath.Join(origHome, ".rustup")
	}

	cargoHome := filepath.Join(home, ".cargo")
	goCache := filepath.Join(home, ".cache", "go-build")
	goModCache := filepath.Join(home, "go", "pkg", "mod")
	pipCache := filepath.Join(home, ".cache", "pip")

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CARGO_HOME", cargoHome)
	t.Setenv("GOCACHE", goCache)
	t.Setenv("GOMODCACHE", goModCache)
	t.Setenv("PIP_CACHE_DIR", pipCache)
	// rustup 指向**真实**安装位置：本轮要造的是"cargo 的缓存/配置目录还没建"的
	// 干净状态（CARGO_HOME），不是"没装 Rust"。
	t.Setenv("RUSTUP_HOME", origRustup)

	// 前置断言：此刻确实不存在（否则测不出"干净机器"）。
	for _, path := range []string{cargoHome, goCache, goModCache, pipCache} {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("前置条件不成立：%q 已经存在", path)
		}
	}

	policy := sandbox.New(
		sandbox.WithRoots(ws),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)

	cargoDir := filepath.Join(ws, "crate")
	if err := os.MkdirAll(filepath.Join(cargoDir, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cargoDir, "Cargo.toml"),
		[]byte("[package]\nname = \"probe\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"), 0o644); err != nil {
		t.Fatalf("cargo.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cargoDir, "src", "main.rs"),
		[]byte("fn main() { println!(\"ok\"); }\n"), 0o644); err != nil {
		t.Fatalf("main.rs: %v", err)
	}
	out, err := runBwrap(t, runner, policy, "/bin/sh", "-c",
		"cd "+cargoDir+" && CARGO_TARGET_DIR="+filepath.Join(ws, "target")+" cargo build --offline 2>&1 | tail -3; echo CARGO_DONE")
	if err != nil || !strings.Contains(out, "CARGO_DONE") || strings.Contains(strings.ToLower(out), "error") {
		t.Fatalf("干净机器首次 cargo build 失败：err=%v\n%s", err, out)
	}
	if _, statErr := os.Stat(cargoHome); statErr != nil {
		t.Fatalf("cargo build 之后 %q 仍不存在（沙箱内无法创建缓存目录）: %v", cargoHome, statErr)
	}

	goDir := filepath.Join(ws, "gomod")
	if err := os.MkdirAll(goDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(goDir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(goDir, "main.go"), []byte("package main\n\nfunc main() { println(\"ok\") }\n"), 0o644); err != nil {
		t.Fatalf("main.go: %v", err)
	}
	out, err = runBwrap(t, runner, policy, "/bin/sh", "-c",
		"cd "+goDir+" && go build -o probe ./... && echo BUILD_OK")
	if err != nil || !strings.Contains(out, "BUILD_OK") {
		t.Fatalf("干净机器首次 go build 失败：err=%v\n%s", err, out)
	}
	// GOCACHE 由 go 自己创建；`<home>/go`（我们管理的 GOMODCACHE 父根）由
	// Confine 前的 MkdirAll 建出来。注意 GOMODCACHE 本身（go/pkg/mod）只在真的
	// 需要用模块缓存时才出现，无依赖构建下 go 不一定建它——所以断言父根而不是它。
	for _, path := range []string{goCache, filepath.Join(home, "go")} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("go build 之后 %q 仍不存在: %v", path, statErr)
		}
	}
}

// cleanMachineSkipUnlessGated 让端到端用例默认不跑（依赖真实 cargo/go 与 bwrap）。
//
// 本地定义而不复用 linux_toolchain_test.go 里的同名助手：那个文件带
// `//go:build linux`，在 windows/darwin 上 `go vet` / `go test -c` 会找不到符号。
func cleanMachineSkipUnlessGated(t *testing.T) {
	t.Helper()
	if os.Getenv("RIG_SANDBOX_TOOLCHAIN_TEST") != "1" {
		t.Skip("端到端用例默认跳过；设 RIG_SANDBOX_TOOLCHAIN_TEST=1 启用（需要真实 cargo/go 与 bwrap）")
	}
	if runtime.GOOS != "linux" {
		t.Skip("仅 Linux（bwrap 后端）")
	}
}
