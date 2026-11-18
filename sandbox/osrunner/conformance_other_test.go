//go:build !linux && !darwin && !windows

package osrunner

import "testing"

// conformancePlatform 在"没有 OS 沙箱后端"的平台上返回空后端列表，真机层随之跳过。
//
// 为什么需要这个文件：conformance_test.go 没有 build tag（它是"同一张表"，应当与平台
// 无关），但它引用的 conformancePlatform 只有 linux/darwin/windows 三份实现。缺了这一份，
// `GOOS=freebsd go vet ./...` 会报 undefined——而我们的交叉 vet 门禁正会跑 freebsd。
func conformancePlatform(t *testing.T) (conformanceEnv, []conformanceBackend) {
	t.Helper()
	t.Skip("conformance: no OS sandbox backend on this platform")
	return conformanceEnv{}, nil
}
