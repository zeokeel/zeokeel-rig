package builtin

// Windows 前台超时杀进程树：Linux 上无法真跑 windows 分支，所以把"命令构造"抽成
// 纯函数在这里钉住；windows 分支本身靠 GOOS=windows 交叉构建/vet 保证。

import (
	"slices"
	"testing"
)

func TestWindowsTaskkillArgv(t *testing.T) {
	got := windowsTaskkillArgv(4242)
	want := []string{"/T", "/F", "/PID", "4242"}
	if !slices.Equal(got, want) {
		t.Fatalf("windowsTaskkillArgv(4242) = %v, want %v", got, want)
	}
	// /T 必须在 /F 之前也不会改变语义；这里只钉住参数集合与顺序稳定。
	if got[0] != "/T" || got[1] != "/F" || got[2] != "/PID" {
		t.Fatalf("argv shape = %v, want /T /F /PID <pid>", got)
	}
}

func TestWindowsTaskkillArgvRejectsNonPositivePid(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if got := windowsTaskkillArgv(pid); got != nil {
			t.Fatalf("windowsTaskkillArgv(%d) = %v, want nil (never taskkill an invalid pid)", pid, got)
		}
	}
}
