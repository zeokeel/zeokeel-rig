package rig

import (
	"context"
	"time"
)

// 本文件是"审批记忆"的最小契约：用户在某次审批里选了 allow-always 之后，那条
// 回答要能被记住、能跨进程存活，下次遇到**同一个目标**时不必再问。
//
// 它刻意只描述"记住了什么"，不描述"怎么判断该不该放行"：围栏（沙箱策略、可写
// 根、只读模式）永远由工具与策略层独立执行，记忆只是"免掉一次提问"，不是权限
// 提升。宿主可以实现这个接口落文件/数据库；kit 自带一个进程内实现
// （kit.NewMemoryApprovals）。

// 审批记忆的作用域取值（rig.ApprovalRule.Scope）。
const (
	// ApprovalScopeSession 表示规则只对写入它的那个会话生效（默认）。
	ApprovalScopeSession = "session"
	// ApprovalScopeGlobal 表示规则对所有会话生效。
	ApprovalScopeGlobal = "global"
)

// ApprovalRule 是一条"问过的答案"：审批人选择 allow-always 之后被记住的具体目标。
//
//   - Kind 是目标的类别（tool / command / path，见 gate 的 ApprovalKind* 常量）；
//   - Key 是被记住的具体目标（命令文本、路径，或"工具 + 规范化参数"）；
//   - Scope 是作用域：rig.ApprovalScopeSession 或 rig.ApprovalScopeGlobal；
//   - SessionID 只在 Scope 为 session 时有意义，global 规则留空；
//   - DecidedAt 是这条规则被写下的时间（UTC），供宿主做展示/过期清理。
//
// 匹配语义（相同 Kind + 相同 Key + 作用域命中）由门禁实现（gate.WithApprovalMemory）
// 负责，本类型只是数据结构。记忆**不做**版本迁移：宿主自己的 store 若字段不兼容，
// 自行丢弃旧规则即可（丢规则的代价只是再问一次）。
type ApprovalRule struct {
	Kind      string
	Key       string
	Scope     string
	SessionID string
	DecidedAt time.Time
}

// ApprovalStore 持久化审批记忆。
//
// Load 返回当前全部规则（未知/空 store 返回 nil, nil）；Save **整体替换**规则集合。
// 实现必须并发安全（agent 可能在多个 goroutine 上写规则），并且不得修改调用方传入
// 的切片。写失败由门禁记日志并继续运行，不会打断回合——记忆是便利，不是正确性。
type ApprovalStore interface {
	Load(ctx context.Context) ([]ApprovalRule, error)
	Save(ctx context.Context, rules []ApprovalRule) error
}
