package gate

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// ErrApprovalExpired 表示等待人工审批时调用方的 context 结束或内部审批超时。
// 返回的错误同时包装该哨兵与 ctx.Err()，因此 errors.Is 对两者都成立。
var ErrApprovalExpired = errors.New("gate: approval expired")

// Asker 回答的规范选项（AskGate 会把它们作为 options 传给 rig.Asker）。宿主 UI
// 应把用户选择映射回这些字符串之一；比较时忽略大小写与首尾空白。无法识别的
// 答案按拒绝处理（fail-closed）。
const (
	// AskAllow 表示只放行本次调用。
	AskAllow = "allow"
	// AskAllowAlways 表示放行本次调用，并记住该工具名使后续调用不再询问。
	AskAllowAlways = "allow-always"
	// AskDeny 表示拒绝本次调用。
	AskDeny = "deny"
)

// askOptions 是每次询问时提供给 Asker 的选项（每次返回新切片，避免宿主修改内部
// 状态）。
var askOptions = []string{AskAllow, AskAllowAlways, AskDeny}

// maxAskArgumentRunes 限制默认问题文案里展示的参数长度（UTF-8 安全截断），避免
// 一个巨大的调用参数撑满宿主 UI。
const maxAskArgumentRunes = 4000

// AskOption 配置 AskGate，由 Ask 按顺序应用。
type AskOption func(*AskGate)

// AskGate 是基于 rig.Asker 的交互式审批门禁，由 Ask 构造：每个工具调用都会变成
// 一次人类提问，用户选择 allow（本次）/ allow-always（该工具以后不再询问）/
// deny（拒绝）。
//
// 语义与不变量：
//
//   - allow -> rig.DecisionAllow；allow-always -> rig.DecisionAllowAlways 并记住
//     该工具名（按工具，不按参数）；deny 或无法识别的答案 -> rig.DecisionDeny
//     （agent 会把它作为工具错误回传给模型，而不是中止运行）；
//   - context 被取消或 WithAskTimeout 超时 -> 返回包装 ErrApprovalExpired 与
//     ctx.Err() 的错误（errors.Is 对两者都成立），由 agent 按取消/失败处理；
//   - Asker 返回其它错误 -> 原样包装返回（宿主审批基础设施故障）；
//   - 允许并发调用（多个 Run 可共享一个 AskGate）：只读/写 allow-always 规则时
//     持锁，调用 Asker 时不持锁，因此不会阻塞无关调用。并发时 Asker 也可能被
//     并发调用，宿主实现必须自行保证线程安全（或自行串行化 UI）；
//   - AskGate 在审批超时后不再等待 Asker；若宿主 Asker 忽略 context，该次调用的
//     goroutine 会残留到 Asker 返回为止（结果写入带缓冲的 channel，不会阻塞）。
//
// 典型组装：
//
//	hostGate := gate.Ask(host.Asker, gate.WithAskTimeout(10*time.Minute))
//	a, err := agent.New(rig.AgentSpec{..., Gate: hostGate})
type AskGate struct {
	asker   rig.Asker
	timeout time.Duration
	prompt  func(tool rig.Tool, call rig.ToolCall) string

	mu      sync.Mutex
	allowed map[string]struct{}
}

var _ rig.Gate = (*AskGate)(nil)

// Ask 返回由 asker 驱动的人工审批门禁。asker 为 nil（或 typed nil）时，返回的
// 门禁在每次 Approve 时报错（配置错误）；timeout 默认为 0，表示不设内部超时、
// 一直等待到调用方 context 结束。
func Ask(asker rig.Asker, opts ...AskOption) *AskGate {
	gate := &AskGate{
		asker:   asker,
		allowed: make(map[string]struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(gate)
		}
	}
	return gate
}

// WithAskTimeout 设置单次审批等待的内部超时；<=0 表示不设内部超时、只受调用方
// context 约束（默认）。
func WithAskTimeout(timeout time.Duration) AskOption {
	return func(gate *AskGate) { gate.timeout = timeout }
}

// WithAskPrompt 设置生成提问文案的函数（例如带上工作目录或风险说明）；返回空
// 字符串时回退到默认文案。默认文案包含工具名与（截断后的）参数。fn 为 nil 时
// 忽略。
func WithAskPrompt(fn func(tool rig.Tool, call rig.ToolCall) string) AskOption {
	return func(gate *AskGate) { gate.prompt = fn }
}

// Approve 实现 rig.Gate。
func (g *AskGate) Approve(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
	if g == nil {
		return "", errors.New("gate: nil ask gate")
	}
	if isNilAsker(g.asker) {
		return "", errors.New("gate: ask gate has no rig.Asker implementation")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", approvalExpiredError(err)
	}
	name := toolCallName(tool, call)
	if name == "" {
		return "", errors.New("gate: ask gate received a call without a tool name")
	}
	// 快速路径：该工具已被 allow-always 规则放行。规则只在锁内读写，提问/等待
	// 期间不持锁。
	if g.IsAllowAlways(name) {
		return rig.DecisionAllow, nil
	}
	answer, err := g.ask(ctx, g.questionFor(tool, call), append([]string(nil), askOptions...))
	if err != nil {
		return "", err
	}
	switch parseAskAnswer(answer) {
	case rig.DecisionAllowAlways:
		g.AllowAlways(name)
		return rig.DecisionAllowAlways, nil
	case rig.DecisionAllow:
		return rig.DecisionAllow, nil
	default:
		return rig.DecisionDeny, nil
	}
}

// ask 调用 Asker 并在当前 goroutine 上等待答案、内部超时或 context 结束。Asker
// 在独立 goroutine 中执行，因此即使它忽略 context，超时/取消也总能及时返回；
// 结果 channel 带缓冲，迟到的答案不会阻塞 Asker goroutine。
func (g *AskGate) ask(ctx context.Context, question string, options []string) (string, error) {
	askCtx := ctx
	if g.timeout > 0 {
		var cancel context.CancelFunc
		askCtx, cancel = context.WithTimeout(ctx, g.timeout)
		defer cancel()
	}
	type result struct {
		answer string
		err    error
	}
	results := make(chan result, 1)
	asker := g.asker
	go func() {
		answer, err := asker.Ask(askCtx, question, options)
		results <- result{answer: answer, err: err}
	}()
	select {
	case outcome := <-results:
		if outcome.err != nil {
			// context 结束优先按“审批过期”语义返回；否则视为宿主审批设施故障。
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", approvalExpiredError(ctxErr)
			}
			if askErr := askCtx.Err(); askErr != nil {
				return "", approvalExpiredError(askErr)
			}
			return "", fmt.Errorf("gate: ask approval: %w", outcome.err)
		}
		return outcome.answer, nil
	case <-askCtx.Done():
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", approvalExpiredError(ctxErr)
		}
		return "", approvalExpiredError(askCtx.Err())
	}
}

// approvalExpiredError 返回同时包装 ErrApprovalExpired 与 cause 的错误。
func approvalExpiredError(cause error) error {
	if cause == nil {
		cause = context.Canceled
	}
	return fmt.Errorf("%w: %w", ErrApprovalExpired, cause)
}

// parseAskAnswer 把 Asker 的答案映射为审批决定。无法识别的答案（包括空答案、用户
// 直接关掉对话框）按拒绝处理。
func parseAskAnswer(answer string) rig.Decision {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case AskAllow, "allow-once", "once", "approve", "approved", "yes", "y":
		return rig.DecisionAllow
	case AskAllowAlways, "always", "allow always":
		return rig.DecisionAllowAlways
	default:
		return rig.DecisionDeny
	}
}

// questionFor 返回本次提问的文案：优先使用 WithAskPrompt 的自定义函数，否则使用
// 默认文案（工具名 + 截断后的参数）。
func (g *AskGate) questionFor(tool rig.Tool, call rig.ToolCall) string {
	if g.prompt != nil {
		if question := strings.TrimSpace(g.prompt(tool, call)); question != "" {
			return question
		}
	}
	name := toolCallName(tool, call)
	args := strings.TrimSpace(call.Arguments)
	if args == "" {
		args = "{}"
	}
	if runes := []rune(args); len(runes) > maxAskArgumentRunes {
		args = string(runes[:maxAskArgumentRunes]) + "…"
	}
	return fmt.Sprintf("The agent wants to run the tool %q.\n\nArguments:\n%s\n\nAllow this tool call?", name, args)
}

// AllowAlways 记录工具名，使该工具的后续调用不再提问。并发安全；空名字被忽略。
func (g *AskGate) AllowAlways(toolName string) {
	if g == nil {
		return
	}
	name := strings.TrimSpace(toolName)
	if name == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.allowed == nil {
		g.allowed = make(map[string]struct{})
	}
	g.allowed[name] = struct{}{}
}

// ClearAllowAlways 清除所有 allow-always 规则，恢复逐次提问。
func (g *AskGate) ClearAllowAlways() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	clear(g.allowed)
}

// AllowAlwaysTools 返回已记录 allow-always 的工具名（排序后的副本）。
func (g *AskGate) AllowAlwaysTools() []string {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.allowed) == 0 {
		return nil
	}
	names := make([]string, 0, len(g.allowed))
	for name := range g.allowed {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsAllowAlways 报告工具是否已有 allow-always 规则。
func (g *AskGate) IsAllowAlways(toolName string) bool {
	if g == nil {
		return false
	}
	name := strings.TrimSpace(toolName)
	if name == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.allowed[name]
	return ok
}

// isNilAsker 报告 asker 是否为 nil 或 typed nil（例如 (*myAsker)(nil)），避免在
// 调用其方法时 panic。
func isNilAsker(asker rig.Asker) bool {
	if asker == nil {
		return true
	}
	value := reflect.ValueOf(asker)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
