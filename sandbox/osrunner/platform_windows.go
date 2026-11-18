//go:build windows

package osrunner

import (
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// # 本后端的边界（与 Linux/macOS 不等价，别让后人误以为三平台等价）
//
//   - **不隔离网络**：Linux 的 bwrap 用 --unshare-net、macOS 的 Seatbelt 用
//     (deny network*)，Windows 侧**没有对应物**。同一个 Policy{Network:false}
//     在 Windows 上意味着"网络完全不被约束"，而不是"没有网络能力"。
//   - **读完全不受限**：只治理写/删（这一点与另外两平台一致：bwrap 的
//     --ro-bind / / 也只是只读挂载，读同样开放）。因此 ~/.ssh、~/.aws 等文件
//     在沙箱内可读——需要保密请用容器/VM。
//   - **设备命名空间不受限**：\\.\ 与 \\?\GLOBALROOT 不经过文件 ACL 围栏，
//     Low 完整性级别也不拦设备访问。
//   - **注册表不在模型里**：HKCU 写入被拒绝是 Low 完整性令牌的**副作用**
//     （no-write-up），不是策略；不可配置、无诊断文案，读注册表不受影响。
//
// newPlatformRunner 选择 Windows 的 runner：受限令牌 + 目录 ACL 的隐藏 helper。
func newPlatformRunner(cfg Config) (sandbox.Runner, error) {
	return newWindowsACLRunner(cfg)
}

// runHelper 执行 Windows 受限令牌 helper。
func runHelper(args []string) (handled bool, code int) {
	return runWindowsACLHelper(args)
}
