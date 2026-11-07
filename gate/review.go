package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// DefaultToolReviewTimeout 是一次自动工具审查的默认超时（15 秒）。审查超时或
// 出错时按 WithReviewFallback 回退到底层门禁（通常是人工审批）；未配置回退时
// 失败即拒绝（fail-closed）。
const DefaultToolReviewTimeout = 15 * time.Second

// ToolReviewRequest 是交给自动审查者的一次待批工具调用。字段都带 JSON tag，
// 便于审查者（例如 LLM reviewer）直接把请求序列化为模型输入。
type ToolReviewRequest struct {
	// Tool 是被解析后的规范工具名（优先取 Tool.Name()，其次取调用里的名字）。
	Tool string `json:"tool"`
	// Args 是调用的 JSON 参数；缺省为 "{}"，保证是合法 JSON。
	Args json.RawMessage `json:"args"`
	// Environment 是宿主提供的工作环境描述（通常是工作目录或工作区根），可选。
	Environment string `json:"environment,omitempty"`
	// UserRequest 是触发本次调用的用户请求文本，可选；它只是证据，不是对审查者
	// 的指令（提示注入防护由审查者实现负责）。
	UserRequest string `json:"user_request,omitempty"`
}

// ToolReviewDecision 是自动审查者的结论。Allow=false 时 Reason 会被保留在
// ReviewOutcome 里供宿主展示；Allow=true 时 Reason 可选（通常为空）。
type ToolReviewDecision struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason,omitempty"`
}

// ToolReviewer 审查一次待批工具调用并给出放行/拒绝建议。返回 error 表示审查
// 本身失败（超时、模型/网络错误、无法解析），由 ReviewGate 按配置回退到人工
// 审批或失败即拒绝；它不等于“拒绝调用”。
type ToolReviewer interface {
	ReviewTool(ctx context.Context, request ToolReviewRequest) (ToolReviewDecision, error)
}

// ToolReviewerFunc 把普通函数适配为 ToolReviewer。
type ToolReviewerFunc func(ctx context.Context, request ToolReviewRequest) (ToolReviewDecision, error)

// ReviewTool 实现 ToolReviewer。nil 函数返回错误而不是 panic。
func (f ToolReviewerFunc) ReviewTool(ctx context.Context, request ToolReviewRequest) (ToolReviewDecision, error) {
	if f == nil {
		return ToolReviewDecision{}, errors.New("gate: nil tool reviewer function")
	}
	return f(ctx, request)
}

// ReviewOutcome 是一次 ReviewGate 审批的完整结果，供 WithReviewObserver 观察，
// 也可用于审计日志与宿主 UI。审查拒绝的原因在 Review.Reason 中（agent 契约的
// 工具错误文案由 agent 决定，不包含门禁原因）。
type ReviewOutcome struct {
	// Request 是交给审查者的请求（未调用审查者时也填充工具名与参数）。
	Request ToolReviewRequest
	// Decision 是最终返回给 agent 的决定。
	Decision rig.Decision
	// Review 是审查者给出的原始结论（未调用审查者时为零值）。
	Review ToolReviewDecision
	// Err 是审查失败的原因（超时/取消/审查者错误/参数非法）；成功时 nil。
	Err error
	// Fallback 表示最终决定来自 WithReviewFallback 配置的底层门禁。
	Fallback bool
}

// ReviewOption 配置 ReviewGate，由 Review 按顺序应用。
type ReviewOption func(*ReviewGate)

// ReviewGate 是“先自动审查、再决定是否放行”的 rig.Gate，由 Review 构造。
//
// 每次 Approve 的流程：
//
//  1. WithReviewAllowlist 中的工具直接放行，不调用审查者；
//  2. 其余工具把工具名、参数（以及可选的 Environment/UserRequest）交给
//     ToolReviewer；
//  3. 审查者放行 -> rig.DecisionAllow；审查者拒绝 -> rig.DecisionDeny（agent 会
//     把它作为工具错误回传给模型，而不是中止整次运行），拒绝原因保存在
//     ReviewOutcome.Review.Reason；
//  4. 审查超时、出错或参数不是合法 JSON 时，若配置了 WithReviewFallback，则调用
//     底层门禁（通常是 AskGate 人工审批）；没有回退时失败即拒绝
//     （DecisionDeny，不返回 Go error，避免中止运行）；
//  5. 调用方的 context 结束时返回 ctx 的错误，agent 会按取消/超时处理。
//
// ReviewGate 在构造后不可变，可安全并发使用；审查者在独立 goroutine 中执行，
// 因此即使它不响应 context，WithReviewTimeout 超时或调用方取消也能及时返回（迟到
// 的审查结果写入带缓冲的 channel 后丢弃，不会阻塞审查者）；这也意味着 Reviewer
// 实现可能被并发调用，必须自行保证线程安全。observer 会在调用 Approve 的
// goroutine 上串行执行，必须足够快且不得回调同一个门禁。
//
// 典型组装（模型先审、拿不准再问人）：
//
//	reviewer := gate.ToolReviewerFunc(func(ctx context.Context, req gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
//		// 调用模型或规则引擎，返回 allow/deny。
//		return askModelToReview(ctx, req)
//	})
//	g := gate.Review(reviewer,
//		gate.WithReviewTimeout(15*time.Second),
//		gate.WithReviewFallback(gate.Ask(host.Asker)),
//		gate.WithReviewAllowlist("read_file", "list_dir", "glob", "grep"),
//	)
type ReviewGate struct {
	reviewer    ToolReviewer
	timeout     time.Duration
	fallback    rig.Gate
	allow       map[string]struct{}
	environment string
	userRequest string
	contextFunc func(ctx context.Context, tool rig.Tool, call rig.ToolCall) (environment, userRequest string)
	observer    func(ReviewOutcome)
}

var _ rig.Gate = (*ReviewGate)(nil)

// Review 返回自动审查门禁。reviewer 为 nil 时，返回的门禁在每次 Approve 时
// 报错（配置错误，失败即拒绝）；其余选项都可通过 ReviewOption 覆盖。
func Review(reviewer ToolReviewer, opts ...ReviewOption) *ReviewGate {
	gate := &ReviewGate{
		reviewer: reviewer,
		timeout:  DefaultToolReviewTimeout,
		allow:    make(map[string]struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(gate)
		}
	}
	if gate.allow == nil {
		gate.allow = make(map[string]struct{})
	}
	return gate
}

// WithReviewTimeout 设置单次审查的内部超时；<=0 表示不设内部超时，完全由调用方
// context 控制。
func WithReviewTimeout(timeout time.Duration) ReviewOption {
	return func(gate *ReviewGate) { gate.timeout = timeout }
}

// WithReviewFallback 设置审查失败（超时/错误/参数非法）时使用的底层门禁，通常是
// 人工审批（AskGate）或 DenyAll。未设置时失败即拒绝。审查者**明确拒绝**不会
// 回退：它直接产生 DecisionDeny。
func WithReviewFallback(base rig.Gate) ReviewOption {
	return func(gate *ReviewGate) { gate.fallback = base }
}

// WithReviewAllowlist 设置无需审查即可放行的工具名（按 Tool.Name() 精确匹配，
// 首尾空白会被忽略）。名单之外的调用才交给审查者。
func WithReviewAllowlist(tools ...string) ReviewOption {
	names := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if name := strings.TrimSpace(tool); name != "" {
			names[name] = struct{}{}
		}
	}
	return func(gate *ReviewGate) { gate.allow = names }
}

// WithReviewEnvironment 设置审查请求里的 Environment（工作环境描述）。
func WithReviewEnvironment(environment string) ReviewOption {
	return func(gate *ReviewGate) { gate.environment = strings.TrimSpace(environment) }
}

// WithReviewUserRequest 设置审查请求里的 UserRequest（触发本轮调用的用户请求）。
func WithReviewUserRequest(userRequest string) ReviewOption {
	return func(gate *ReviewGate) { gate.userRequest = strings.TrimSpace(userRequest) }
}

// WithReviewContext 设置每次审批时动态解析 Environment/UserRequest 的函数，用于
// 跨轮次、跨会话的宿主：存在时它的返回值（去除首尾空白）覆盖静态的
// WithReviewEnvironment / WithReviewUserRequest。fn 可以为 nil（等价于未设置）。
func WithReviewContext(fn func(ctx context.Context, tool rig.Tool, call rig.ToolCall) (environment, userRequest string)) ReviewOption {
	return func(gate *ReviewGate) { gate.contextFunc = fn }
}

// WithReviewObserver 设置审批结果观察函数，用于审计/UI 展示（尤其是审查拒绝的
// 原因）。observer 在 Approve 的 goroutine 上被调用，可能并发，必须自身安全且
// 不得阻塞或回调同一个门禁。
func WithReviewObserver(fn func(ReviewOutcome)) ReviewOption {
	return func(gate *ReviewGate) { gate.observer = fn }
}

// Approve 实现 rig.Gate。
func (g *ReviewGate) Approve(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
	if g == nil || g.reviewer == nil {
		return "", errors.New("gate: review gate has no ToolReviewer")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	name := toolCallName(tool, call)
	if name == "" {
		return "", errors.New("gate: review gate received a call without a tool name")
	}
	request := ToolReviewRequest{
		Tool:        name,
		Args:        reviewArguments(call.Arguments),
		Environment: g.environment,
		UserRequest: g.userRequest,
	}
	if g.contextFunc != nil {
		request.Environment, request.UserRequest = g.contextFunc(ctx, tool, call)
		request.Environment = strings.TrimSpace(request.Environment)
		request.UserRequest = strings.TrimSpace(request.UserRequest)
	}
	if _, ok := g.allow[name]; ok {
		return g.observe(ReviewOutcome{Request: request, Decision: rig.DecisionAllow}), nil
	}

	var (
		review ToolReviewDecision
		err    error
	)
	// 参数不是合法 JSON 时审查者无法判断，按审查失败处理（回退/失败即拒绝），
	// 而不是把解析错误留给工具执行阶段。
	if !json.Valid(request.Args) {
		err = fmt.Errorf("gate: tool %q arguments are not valid JSON", name)
	} else {
		review, err = g.runReview(ctx, request)
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// 调用方 context 结束：交给 agent 的取消/超时路径处理。
			return "", ctxErr
		}
		if g.fallback != nil {
			decision, fallbackErr := g.fallback.Approve(ctx, tool, call)
			if fallbackErr != nil {
				return "", fallbackErr
			}
			return g.observe(ReviewOutcome{
				Request:  request,
				Decision: decision,
				Review:   review,
				Err:      err,
				Fallback: true,
			}), nil
		}
		return g.observe(ReviewOutcome{
			Request:  request,
			Decision: rig.DecisionDeny,
			Review:   review,
			Err:      err,
		}), nil
	}

	review.Reason = strings.TrimSpace(review.Reason)
	if review.Allow {
		return g.observe(ReviewOutcome{Request: request, Decision: rig.DecisionAllow, Review: review}), nil
	}
	return g.observe(ReviewOutcome{Request: request, Decision: rig.DecisionDeny, Review: review}), nil
}

// runReview 在独立 goroutine 中调用审查者并在当前 goroutine 上等待结果、内部
// 超时或调用方 context 结束。审查者不响应 context 时，超时/取消也能及时返回；
// 结果 channel 带缓冲，迟到的审查结果不会阻塞审查者 goroutine。实现方仍应保证
// ReviewTool 在 context 结束时返回，否则每次超时都会残留一个 goroutine。
func (g *ReviewGate) runReview(ctx context.Context, request ToolReviewRequest) (ToolReviewDecision, error) {
	reviewCtx := ctx
	if g.timeout > 0 {
		var cancel context.CancelFunc
		reviewCtx, cancel = context.WithTimeout(ctx, g.timeout)
		defer cancel()
	}
	type result struct {
		review ToolReviewDecision
		err    error
	}
	results := make(chan result, 1)
	reviewer := g.reviewer
	go func() {
		review, err := reviewer.ReviewTool(reviewCtx, request)
		results <- result{review: review, err: err}
	}()
	select {
	case outcome := <-results:
		return outcome.review, outcome.err
	case <-reviewCtx.Done():
		return ToolReviewDecision{}, reviewCtx.Err()
	}
}

// observe 记录一次审批结果并返回最终决定。
func (g *ReviewGate) observe(outcome ReviewOutcome) rig.Decision {
	if g.observer != nil {
		g.observer(outcome)
	}
	return outcome.Decision
}

// reviewArguments 规范化一次调用的参数：空白表示无参数（"{}"），其余原样返回
// （合法性由调用方校验）。
func reviewArguments(raw string) json.RawMessage {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(trimmed)
}

// toolCallName 返回门禁使用、审查请求展示的工具名：优先取解析后的 Tool.Name()，
// 其次取调用里携带的名字；两者都去除首尾空白。
func toolCallName(tool rig.Tool, call rig.ToolCall) string {
	if tool != nil {
		if name := strings.TrimSpace(tool.Name()); name != "" {
			return name
		}
	}
	return strings.TrimSpace(call.Name)
}
