package osrunner

import (
	"os"
	"testing"
)

// TestMain 让测试二进制本身可以充当内置 helper 子进程：宿主（这里是测试二进制）
// 启动时先调用 MaybeRunHelper，命中隐藏 helper 参数时执行 helper 并以 helper 的
// 退出码退出，否则继续正常运行测试。
//
// 这是 Linux Landlock 运行期测试的前提：landlockRunner 用 os.Executable() 作为
// helper 的 argv[0]，测试进程必须能响应 HelperArgLandlock。
func TestMain(m *testing.M) {
	if MaybeRunHelper(os.Args) {
		return
	}
	os.Exit(m.Run())
}
