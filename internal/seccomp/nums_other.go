//go:build !(linux && amd64) && !(linux && arm64)

package seccomp

// networkBlockedSyscalls 在没有内置系统调用号表的平台（非 Linux，或 Linux 上的
// 其它架构）上返回 ok=false：调用方据此 fail closed（不装过滤器就不以"已隔离
// 网络"的姿态运行），并在 RunnerLimits 里如实说明。
func networkBlockedSyscalls() ([]uint32, uint32, uint32, bool) { return nil, 0, 0, false }
