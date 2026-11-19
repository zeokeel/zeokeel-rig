//go:build linux

package builtin_test

// 审计 P1 回归：前台 shell 超时必须杀掉**整个进程组**，而不只是直接子进程。
//
// 现象（修复前）：runForeground 用 exec.CommandContext 且不设进程组，超时只 Kill
// 直接子进程 `sh`；`sh` 派生出来的孙进程（后台的 `(sleep 1; echo late > f) &`）
// 继续运行，工具已经返回"超时"之后仍然写入工作区 —— 一条命令既能"超时失败"又能
// 在之后产生副作用，模型和用户都无法据此判断状态。

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBashTimeoutKillsGrandchildren(t *testing.T) {
	root, bash := newBashForTest(t, 5*time.Second)
	marker := filepath.Join(root, "late-marker.txt")
	// 后台子 shell 在 1s 后写文件；前台 sleep 30 让工具先超时（300ms）。
	command := `(sleep 1; echo late > ` + marker + `) & sleep 30`

	started := time.Now()
	result := runBash(t, bash, `{"command":`+strconv.Quote(command)+`,"timeout_ms":300}`)
	elapsed := time.Since(started)
	if !result.IsError || !strings.Contains(result.Content, "timeout") {
		t.Fatalf("bash(grandchild) = %q (IsError=%v), want a timeout error", result.Content, result.IsError)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("timeout took %s, want it to fire near 300ms", elapsed)
	}

	// 工具返回后等过孙进程本应写文件的时刻：它必须已经被一起杀掉。
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("grandchild survived the foreground timeout and wrote %s after the tool returned", marker)
	}
}

// TestBashTimeoutStillReturnsTimeoutError 保证进程组改造没有改坏正常超时语义。
func TestBashTimeoutStillReturnsTimeoutError(t *testing.T) {
	root, bash := newBashForTest(t, 5*time.Second)
	marker := filepath.Join(root, "never.txt")
	result := runBash(t, bash, `{"command":"sleep 5","timeout_ms":200}`)
	if !result.IsError || !strings.Contains(result.Content, "timeout") {
		t.Fatalf("bash = %q (IsError=%v), want timeout", result.Content, result.IsError)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("unexpected marker %s", marker)
	}
}

// TestBashNormalCommandUnaffected 保证独立进程组不改变正常命令的输出/退出码。
func TestBashNormalCommandUnaffected(t *testing.T) {
	_, bash := newBashForTest(t, 5*time.Second)
	result := runBash(t, bash, `{"command":"echo grouped; exit 4"}`)
	if result.IsError {
		t.Fatalf("bash = %q, want a non-error result", result.Content)
	}
	if !strings.Contains(result.Content, "grouped") || !strings.Contains(result.Content, "[exit code: 4]") {
		t.Fatalf("bash = %q, want output plus exit code 4", result.Content)
	}
}
