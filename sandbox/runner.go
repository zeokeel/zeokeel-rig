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
	// EnforcementPartial 表示 runner 只治理其中一部分（例如 Windows ACL 只约束
	// 写/删，不约束读与网络）。
	EnforcementPartial Enforcement = "partial"
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

// ErrSandboxUnavailable 表示本机没有可用的 OS runner 能强制请求的模式，调用方
// 应当拒绝执行而不是退化为未受限执行。
var ErrSandboxUnavailable = errors.New("sandbox: no usable OS sandbox runner")
