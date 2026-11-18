package sandbox

import "errors"

// Enforcement 表示一个 OS runner 对策略承诺的文件效果的实际覆盖程度。
//
// 它是运行时事实而不是承诺：调用方可以据此决定是否把该次执行当作“完整受限”
// 使用。Windows ACL 后端与较老的 Landlock ABI 是目前已知的 partial 情形。
type Enforcement string

const (
	// EnforcementFull 表示 runner 治理策略承诺的全部文件效果。
	EnforcementFull Enforcement = "full"
	// EnforcementPartial 表示 runner 只治理策略承诺的**一部分**文件效果。
	//
	// "partial" 的具体含义按后端展开（调用方据此判断能不能把它当成"完整受限"）：
	//
	//   - **读不受限**（Windows ACL、bwrap 的 --ro-bind / /、Seatbelt 的
	//     allow-default 姿态都一样）：~/.ssh、~/.aws 在沙箱内可读；
	//   - **网络不隔离**：Windows 后端完全没有网络围栏；Landlock 未接
	//     LANDLOCK_ACCESS_NET_*（只有 bwrap 的 --unshare-net 与 Seatbelt 的
	//     (deny network*) 真正生效）；
	//   - **设备命名空间**：Windows 不拦 \\.\ 与 \\?\GLOBALROOT；Linux 由
	//     --dev/--remount-ro 收窄，macOS 只放行 /dev/null、/dev/ptmx、/dev/*tty*；
	//   - **别名对象**：NTFS 硬链接与目录 junction 是文件对象别名，路径前缀检查
	//     管不了它（Windows 的写限制靠对象 ACL，因此仍然拦住写；但读面无差别）；
	//   - **保活组**：Windows 受限令牌的 restricting 列表保留 Everyone（否则早期
	//     DLL 初始化会 0xC0000142），因此"对 Everyone 有写授权的位置"仍可写；
	//   - **元数据操作**：Landlock（较老 ABI）不能治理 chmod/chown 等。
	EnforcementPartial Enforcement = "partial"
	// EnforcementNone 表示本次执行没有被任何 runner 包装（danger-full-access
	// 透传）。调用方不能把它的 argv 当作受限执行。
	EnforcementNone Enforcement = "none"
)

// ConfinedCommand 是一次 Confine 的结果。
type ConfinedCommand struct {
	// Argv 是最终应当 spawn 的 argv。对 argv 前缀式 runner（bwrap、sandbox-exec、
	// 内置 helper）来说，它已经包含 runner 自身与 profile 参数。
	Argv []string
	// Enforcement 是该 runner 对本次策略的实际覆盖程度。
	Enforcement Enforcement
	// DenialSignatures 是子进程 stderr 中表示“策略拒绝”的大小写不敏感子串，
	// 例如 bwrap 的 "read-only file system" 或 Seatbelt 的
	// "operation not permitted"。空表示未知方言。
	DenialSignatures []string
}

// Runner 把一条命令包装成 OS 层受限的形式。
//
// 实现必须 fail closed：当本机无法强制请求的模式时返回
// ErrSandboxUnavailable（或包装它的错误），绝不能返回未受限的 argv 让调用方
// 静默裸跑。Runner 是纯 Go 的：它只构造 argv 或调用平台系统调用，不引入 cgo。
//
// *Policy 为 nil 时的行为由实现决定，但推荐返回 ErrSandboxUnavailable——没有
// 策略就没有可强制的效果集。
type Runner interface {
	Confine(argv []string, policy *Policy) (ConfinedCommand, error)
}

// Reporter 是 runner 的**可选**接口：报告自己的名字与实际覆盖程度。
//
// 它存在的原因是"强制完整度必须对模型与用户可见"：Windows ACL 只治理写/删（不
// 治理读与网络），Landlock 不治理 chmod/chown 这类元数据变更——把这些差隐含在
// 文档里不够，sandbox_status 与拒绝归因要能当场读到。没有实现本接口的 runner
// （例如自定义 argv 前缀包装器）视为"未知"，调用方应按 partial 处理。
type Reporter interface {
	// RunnerName 是稳定的 runner 标识（"bwrap"、"landlock"、"seatbelt"、"windows-acl"）。
	RunnerName() string
	// RunnerEnforcement 是该 runner 对策略承诺的文件效果的实际覆盖程度。
	RunnerEnforcement() Enforcement
	// RunnerLimits 用一句话说明"没被治理的部分"，供 sandbox_status 与拒绝归因
	// 直接展示（例如 Windows 的"硬链接别名 + 读不受限"）。空串表示无额外限制说明。
	RunnerLimits() string
}

// NetworkReporter 是 runner 的**可选**接口：报告它对 `AllowNetwork: false` 的实际
// 覆盖程度（"full"/"partial"/"none"）。没实现它的 runner 按 "none" 处理——把
// "不知道" 报成能隔离网络，比不报更危险。
type NetworkReporter interface {
	NetworkCoverage() string
}

// ErrSandboxUnavailable 表示本机没有可用的 OS runner 能强制请求的模式，调用方
// 应当拒绝执行而不是退化为未受限执行。
var ErrSandboxUnavailable = errors.New("sandbox: no usable OS sandbox runner")

// RunnerStatus 返回 runner 的自报信息（名字、覆盖程度、"没被治理的部分"）。
// 未实现 Reporter 的 runner 返回 ("", EnforcementPartial, "")：调用方应按
// "未知后端 -> 只能假设 partial" 处理，而不是乐观地当成 full。
func RunnerStatus(runner Runner) (name string, enforcement Enforcement, limits string) {
	if reporter, ok := runner.(Reporter); ok {
		return reporter.RunnerName(), reporter.RunnerEnforcement(), reporter.RunnerLimits()
	}
	return "", EnforcementPartial, ""
}

// RunnerNetworkCoverage 返回 runner 对网络隔离的实际覆盖程度；未实现
// NetworkReporter 的 runner 返回 "none"（fail honest：不假设它能挡）。
func RunnerNetworkCoverage(runner Runner) string {
	if reporter, ok := runner.(NetworkReporter); ok {
		if coverage := reporter.NetworkCoverage(); coverage != "" {
			return coverage
		}
	}
	return "none"
}
