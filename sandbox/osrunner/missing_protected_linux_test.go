//go:build linux

package osrunner

// F3 的 Linux 真机断言：保护路径缺失时 bwrap 必须拒绝执行（而不是放行）。

import (
	"strings"
	"testing"
)

// 真机（linux）：同一场景走 bwrap 必须报错，而不是放行。
func TestBwrapRejectsMissingProtected(t *testing.T) {
	runner := localBwrap(t)
	root := t.TempDir()
	policy, missing := missingProtectedPolicy(t, root)
	_, err := runner.Confine([]string{"/bin/sh", "-c", "true"}, policy)
	if err == nil {
		t.Fatalf("保护路径 %q 不存在时 bwrap 应当拒绝执行", missing)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("错误里没有列出缺失路径 %q：%v", missing, err)
	}
}
