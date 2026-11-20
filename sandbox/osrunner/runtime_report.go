package osrunner

import (
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// RuntimeReport 是"沙箱实际生效了什么"的只读诊断快照，供 sandbox_status /
// rigdoctor 直接展示。
//
// 它存在的理由是审计者自己踩过的坑：缓存白名单到底生效没有，只能靠 `go build`
// 试错；网络到底断没断，只能靠 curl 看状态码；宿主守护进程 socket 有没有被遮蔽，只能
// 靠 `ls -l` 猜。把这些变成可读字段，宿主与模型都不必再猜。
type RuntimeReport struct {
	// ToolchainCaches 是**当前存在且已授权**的工具链缓存根（不是声明清单）。
	ToolchainCaches []string
	// MissingToolchainCaches 是声明了但当前不存在的缓存根（首次使用前的常态）。
	MissingToolchainCaches []string
	// MaskedSockets 是被显式屏蔽（/dev/null 覆盖）的宿主守护进程 socket。
	MaskedSockets []string
	// NetworkCoverage 是 runner 对 `AllowNetwork: false` 的实际覆盖度
	// （full/partial/none）。没有 runner 时是 "none"。
	NetworkCoverage string
	// PrivateTmpDisabledReason 说明为什么没启用私有 /tmp（空表示已启用）。
	PrivateTmpDisabledReason string
}

// DescribeRuntime 汇总一份诊断快照。runner 可以为 nil（宿主没挂 OS 沙箱）。
//
// 它不改变策略、不施加任何限制，只读。
func DescribeRuntime(policy *sandbox.Policy, runner sandbox.Runner) RuntimeReport {
	var report RuntimeReport
	if policy != nil {
		profile, err := BuildProfile(policy)
		if err == nil && profile != nil {
			for _, cache := range profile.ToolchainCaches {
				report.ToolchainCaches = append(report.ToolchainCaches, cache.Root)
			}
			report.MissingToolchainCaches = append(report.MissingToolchainCaches, profile.MissingToolchainCaches...)
			report.MaskedSockets = append(report.MaskedSockets, profile.MaskedSockets...)
			report.PrivateTmpDisabledReason = profile.TmpDisabledReason
		}
	}
	report.NetworkCoverage = sandbox.RunnerNetworkCoverage(runner)
	return report
}
