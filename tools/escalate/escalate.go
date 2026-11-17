package escalate

// 本文件是工具的本体：构造期校验、参数校验、审批、限流、请求投递与结果渲染。
//
// 关键不变量（改动前请先读 doc.go 的"安全纪律"）：
//
//   - Allow/Deny/Approval 与 Roots 只能来自宿主（Options），模型无法影响；
//   - 每次调用都必须经过 approver（nil approver 时构造直接失败）；
//   - 执行目录由工具从 Roots 派生，模型没有入口指定它；
//   - 三类结果（成功/拒绝/通道故障）与"繁忙"用稳定前缀区分。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox/escalate"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

const (
	// ToolName 是模型可见的工具名。
	ToolName = "request_command_escalation"

	// DefaultMaxTimeout 是单条请求的默认时长上限。
	DefaultMaxTimeout = 20 * time.Second
	// DefaultMaxOutputBytes 是 stdout/stderr 各自的默认字节上限（128 KiB）。
	DefaultMaxOutputBytes = 128 << 10
	// DefaultMaxConcurrent 是同时在途执行的默认上限。
	DefaultMaxConcurrent = 2
	// DefaultMaxQueue 是并发额度已满时允许排队的默认上限。
	DefaultMaxQueue = 8
	// maxArgvTokens 是单条命令的参数个数上限（防止把请求撑成协议上限）。
	maxArgvTokens = 256
	// channelGrace 是工具侧比 Broker 超时多等的余量：Broker 先按 MaxTimeout 收尾并
	// 回响应，工具只有在"通道本身卡住"时才需要用这个余量兜底。
	channelGrace = 5 * time.Second
)

// Approver 决定一次提权请求是否可以执行。返回 (true, "", nil) 表示同意；
// (false, reason, nil) 表示拒绝（reason 会展示给模型）；error 表示审批通道出错，
// 按拒绝处理（fail closed）。
type Approver func(ctx context.Context, req escalate.Request) (bool, string, error)

// Options 是工具的宿主侧配置。Allow 与 Roots 必填；其余为可选上界/回调。
type Options struct {
	// Roots 是允许的工作目录根（必需）。工具的**执行目录**取第一个根：模型没有
	// 任何方式指定目录（Schema 里没有 dir，模型传了也会被忽略）。
	// 每个根必须是绝对路径且当前存在（fail closed）。
	Roots []string
	// Allow 是允许执行的 argv 前缀（必需，子命令级）。空列表表示"什么都不允许"，
	// 构造期直接报错。`{"git","fetch"}` 表示 git fetch 及其全部后续参数；
	// 不要写裸程序名（`{"git"}` 等于放开 git 的所有子命令）。
	Allow [][]string
	// Deny 是禁止的 argv 前缀，优先级高于 Allow（必须写整个子命令，
	// 例如 `{"git","push"}`；不要写 flag 级 Deny）。
	Deny [][]string
	// Approval 是"标记为必须审批"的前缀。本工具**每次调用都过审批**，因此它只用于
	// 审计/展示与配置一致性校验：每条 Approval 必须被 Allow 覆盖且不被 Deny 覆盖，
	// 否则构造期报错（那种配置下审批永远无法生效）。
	Approval [][]string
	// MaxTimeout 是单条请求的时长上限；<=0 用 DefaultMaxTimeout。模型可以要求更短，
	// 但不能更长（会被夹到本值）。
	MaxTimeout time.Duration
	// MaxOutputBytes 是 stdout/stderr 各自的字节上限；<=0 用 DefaultMaxOutputBytes。
	MaxOutputBytes int
	// MaxConcurrent 是同时在途执行的并发上限；<=0 用 DefaultMaxConcurrent。
	MaxConcurrent int
	// MaxQueue 是并发满时允许排队的请求数；0 用 DefaultMaxQueue，负数报错
	// （工具不允许无限排队）。
	MaxQueue int
	// Auditor 可选：每次请求（**包括被拒绝与通道失败的**）结束时回调一次，在把
	// 结果返回给模型之前调用。req 是实际发出去的请求（含工具派生出的 Dir），
	// resp 是收到的响应（通道失败时是合成的 -1 响应）。必须自己保证并发安全。
	Auditor func(req escalate.Request, resp escalate.Response)
}

// tool 是 rig.Tool 的实现。
type tool struct {
	client   *escalate.Client
	approver Approver
	opts     options
	limiter  *limiter
	auditor  func(escalate.Request, escalate.Response)

	closeOnce sync.Once
	closeErr  error
}

var _ rig.Tool = (*tool)(nil)

// options 是归一化之后的配置。
type options struct {
	roots          []string
	dir            string
	allow          [][]string
	deny           [][]string
	approval       [][]string
	maxTimeout     time.Duration
	maxOutputBytes int
	maxConcurrent  int
	maxQueue       int
}

// New 在一条已经建立的提权连接上构造工具。
//
// 使用场景：宿主（或提权 helper）已经建好 escalate.Client（例如通过 unix socket
// 连回宿主 Broker）。工具会先用 Options 里的 Allow/Deny 做一次本地判定（让模型
// 立刻看到清晰拒绝），再把请求交给 Broker —— Broker 侧仍然会按它自己的策略再判
// 一次（纵深防御）。
//
// approver 为 nil、client 为 nil、或 Options 非法时返回错误（fail closed）。
func New(client *escalate.Client, approver Approver, opts Options) (rig.Tool, error) {
	if client == nil {
		return nil, errors.New("request_command_escalation: 需要非空的 escalate.Client")
	}
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	if approver == nil {
		return nil, errors.New("request_command_escalation: 需要审批回调（nil approver 会被当成一律拒绝，因此拒绝构造）")
	}
	return &tool{
		client:   client,
		approver: approver,
		opts:     normalized,
		limiter:  newLimiter(normalized.maxConcurrent, normalized.maxQueue),
		auditor:  opts.Auditor,
	}, nil
}

// TransportPair 是"一对双向连接"的可选接口。实现它的 conn 交给 NewWithBroker
// 时，Broker 跑在 Endpoints 返回的第一端、Client 跑在第二端。
//
// 为什么需要它：单端连接（例如 net.Pipe() 只取一端）写出去的字节不会自动回流到
// 自己的读端，Broker 与 Client 共用同一个对象会互相等死。真实管道必须成对使用。
type TransportPair interface {
	io.ReadWriteCloser
	// Endpoints 返回 (Broker 端, Client 端)。
	Endpoints() (broker io.ReadWriteCloser, client io.ReadWriteCloser)
}

// NewWithBroker 在一条连接上自建 Broker + Client，并让 Broker 在后台 Serve。
//
// 这是宿主侧"一次装配"的推荐入口。conn 可以是：
//
//   - 实现了 TransportPair 的连接对（推荐：真实的 net.Pipe / socketpair，Broker 与
//     Client 各占一端）；
//   - 自环（in-memory loopback）传输：写进去的字节能从同一个对象读出来（同进程内存
//     管道），此时 Broker 与 Client 共用它。
//
// Broker 的策略直接取自 Options（Roots/Allow/Deny/超时/输出/并发/排队），审批**只由
// 工具的 approver 做一次**（Broker 不再问一遍，避免一次提权弹两次窗）。
//
// 后台 Serve 一直运行到连接关闭：宿主可以用 type-assert `interface{ Close() error }`
// 调 Close（会关闭 Client 与连接，并让 Serve 收敛）。工具返回值实现了该接口。
func NewWithBroker(conn io.ReadWriteCloser, approver Approver, opts Options) (rig.Tool, error) {
	if conn == nil {
		return nil, errors.New("request_command_escalation: 需要非空连接")
	}
	brokerConn, clientConn := conn, conn
	if pair, ok := conn.(TransportPair); ok {
		brokerConn, clientConn = pair.Endpoints()
		if brokerConn == nil || clientConn == nil {
			return nil, errors.New("request_command_escalation: TransportPair 的两端都必须非空")
		}
	}
	return newWithEnds(brokerConn, clientConn, approver, opts)
}

// NewWithPair 是 NewWithBroker 的显式形式：Broker 跑在 brokerConn 上，工具的 Client
// 跑在 clientConn 上。两者通常是 net.Pipe() / socketpair 的两端，或者子进程的
// stdin/stdout 管道对。
func NewWithPair(brokerConn, clientConn io.ReadWriteCloser, approver Approver, opts Options) (rig.Tool, error) {
	if brokerConn == nil || clientConn == nil {
		return nil, errors.New("request_command_escalation: 需要非空的 broker 连接与 client 连接")
	}
	return newWithEnds(brokerConn, clientConn, approver, opts)
}

// newWithEnds 是 NewWithBroker/NewWithPair 的共同实现。
func newWithEnds(brokerConn, clientConn io.ReadWriteCloser, approver Approver, opts Options) (rig.Tool, error) {
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	if approver == nil {
		return nil, errors.New("request_command_escalation: 需要审批回调（nil approver 会被当成一律拒绝，因此拒绝构造）")
	}
	broker, err := escalate.NewBroker(escalate.Policy{
		Roots:          normalized.roots,
		Allow:          normalized.allow,
		Deny:           normalized.deny,
		MaxTimeout:     normalized.maxTimeout,
		MaxOutputBytes: normalized.maxOutputBytes,
		MaxConcurrent:  normalized.maxConcurrent,
		MaxQueue:       normalized.maxQueue,
		// Approval 不放进 Broker：工具已经对每次调用问过 approver，Broker 再问一次
		// 会变成双份审批；Broker 的 approver 保持 nil，也就不会因为"命中审批列表但
		// 没有 approver"而误拒。
	})
	if err != nil {
		return nil, fmt.Errorf("request_command_escalation: 构造 Broker: %w", err)
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	t := &tool{
		client:   escalate.NewClient(clientConn),
		approver: approver,
		opts:     normalized,
		limiter:  newLimiter(normalized.maxConcurrent, normalized.maxQueue),
		auditor:  opts.Auditor,
	}
	go func() {
		// Serve 的错误在 Close 时通过 closeErr 露出；正常生命周期里它只在对端断开
		// （工具被 Close 或对端进程退出）时返回。
		err := broker.Serve(serveCtx, brokerConn)
		cancel()
		if err != nil && !errors.Is(err, escalate.ErrClosed) && !errors.Is(err, context.Canceled) {
			t.closeOnce.Do(func() { t.closeErr = err })
		}
	}()
	return t, nil
}

// Close 关闭提权通道（实现 io.Closer 的等价契约，供宿主回收资源）。
//
// 可重复调用；关闭后 Invoke 会返回 `[escalation failed: ...]`。
func (t *tool) Close() error {
	if t == nil {
		return nil
	}
	t.closeOnce.Do(func() {
		if t.client != nil {
			_ = t.client.Close()
		}
	})
	return t.closeErr
}

// Name 实现 rig.Tool。
func (t *tool) Name() string { return ToolName }

// Description 实现 rig.Tool。
func (t *tool) Description() string {
	return "Ask the host to execute one command outside the sandbox (as the host user), for commands that cannot work inside it " +
		"(network access, host credentials, cloud CLIs). You provide an argv array — no shell syntax, no pipes, no redirection — " +
		"plus a clear reason; the host decides. Only sub-commands the operator pre-approved can run, every call needs operator approval, " +
		"and the working directory is fixed by the host, so do not pass paths that assume another directory. " +
		"Results start with `exit_code: N` when the command actually ran; `[escalation denied: ...]` means policy refused it and nothing " +
		"was executed; `[escalation failed: ...]` means the escalation channel itself failed; `[escalation busy: ...]` is a capacity limit " +
		"you can retry later. Never use this tool to work around a file-permission error you could fix inside the sandbox. " +
		"Only command, reason and timeout_ms are read: any other field you send (dir, allow, id, env, ...) is ignored — the host decides the working directory, " +
		"the allowed commands and the request id."
}

// Schema 实现 rig.Tool。注意：不暴露 id（Client 自动生成），也不暴露 dir/allow/env。
func (t *tool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"command": toolutil.StringArrayProp(
			"The command as an argv array, e.g. [\"git\",\"fetch\",\"origin\"]. " +
				"It is executed directly (no shell), so pipes, globs, redirection and quoting are not interpreted."),
		"reason": toolutil.StringProp(
			"One sentence explaining why this command must run on the host (it goes into the approval prompt and the audit log)."),
		"timeout_ms": toolutil.IntegerProp(
			"Optional timeout in milliseconds; the host clamps it to its own maximum."),
	}, "command", "reason")
}

// arguments 是工具接受的参数。刻意没有 dir/allow/env/id：它们只能由宿主决定。
type arguments struct {
	Command   []string `json:"command"`
	Reason    string   `json:"reason"`
	TimeoutMS int      `json:"timeout_ms"`
}

// Invoke 实现 rig.Tool。
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args arguments
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return toolutil.ErrorResult(err), nil
	}
	argv, reason, err := t.validateArguments(args)
	if err != nil {
		return toolutil.ErrorResult(err), nil
	}
	// 本地策略判定（纵深防御）：让模型立刻看到清晰拒绝，不必等一个往返。
	if entry, denied := matchPrefix(t.opts.deny, argv); denied {
		response := deniedResponse("命中禁止列表: " + strings.Join(entry, " "))
		t.audit(escalate.Request{Argv: argv, Dir: t.opts.dir, Reason: reason}, response)
		return toolutil.ErrorResultf("%s", renderDenied(response.DenyReason)), nil
	}
	if _, allowed := matchPrefix(t.opts.allow, argv); !allowed {
		response := deniedResponse("未匹配任何允许前缀（allow list）: " + strings.Join(argv, " "))
		t.audit(escalate.Request{Argv: argv, Dir: t.opts.dir, Reason: reason}, response)
		return toolutil.ErrorResultf("%s", renderDenied(response.DenyReason)), nil
	}
	// 与 Broker 共用的内置危险参数表：长选项缩写（--upload-pac）与短选项附加值
	// （-O/bin/echo）都在这里被拦住，而不是靠 Allow 前缀去数参数。
	if token, flag, dangerous := ValidateArgv(argv); dangerous {
		response := deniedResponse(fmt.Sprintf("命中内置危险参数 %q（等价于 %q，会把命令变成执行器或越界读写）", token, flag))
		t.audit(escalate.Request{Argv: argv, Dir: t.opts.dir, Reason: reason}, response)
		return toolutil.ErrorResultf("%s", renderDenied(response.DenyReason)), nil
	}

	// 并发额度在审批之前获取：审批 UI 与宿主资源都不该被并发工具调用打爆。
	acquired, busy := t.limiter.acquire(ctx)
	if busy {
		response := deniedResponse(escalate.QueueFullReason)
		t.audit(escalate.Request{Argv: argv, Dir: t.opts.dir, Reason: reason}, response)
		return toolutil.ErrorResultf("%s", renderBusy(response.DenyReason)), nil
	}
	if !acquired {
		err := ctx.Err()
		if err == nil {
			err = errors.New("请求已取消")
		}
		return toolutil.ErrorResultf("%s", renderFailed(err)), nil
	}
	defer t.limiter.release()

	request := escalate.Request{
		Argv:      argv,
		Dir:       t.opts.dir,
		Reason:    reason,
		TimeoutMS: t.timeoutMS(args.TimeoutMS),
	}
	approved, denyReason, approveErr := t.callApprover(ctx, request)
	if approveErr != nil {
		response := deniedResponse("审批通道失败（按拒绝处理）: " + approveErr.Error())
		t.audit(request, response)
		return toolutil.ErrorResultf("%s", renderDenied(response.DenyReason)), nil
	}
	if !approved {
		response := deniedResponse(denyReason)
		t.audit(request, response)
		return toolutil.ErrorResultf("%s", renderDenied(response.DenyReason)), nil
	}

	// 通道兜底超时：Broker 会先按 MaxTimeout 收尾并回响应，这里只在通道本身卡死
	// 时才生效。
	execCtx, cancel := context.WithTimeout(ctx, t.opts.maxTimeout+channelGrace)
	defer cancel()
	response, err := t.client.Exec(execCtx, request)
	if err != nil {
		t.audit(request, escalate.Response{ExitCode: -1, Stderr: err.Error()})
		return toolutil.ErrorResultf("%s", renderFailed(err)), nil
	}
	t.audit(request, response)
	if response.Denied {
		// 队列满是容量问题，不是策略拒绝：文案必须让模型知道可以重试。
		if response.DenyReason == escalate.QueueFullReason {
			return toolutil.ErrorResultf("%s", renderBusy(response.DenyReason)), nil
		}
		return toolutil.ErrorResultf("%s", renderDenied(response.DenyReason)), nil
	}
	if response.ExitCode < 0 {
		// 协议约定：-1 表示"命令没有正常退出"（超时、取消、被信号杀掉、执行失败）。
		// 这与"跑完但退出码非零"是两种不同结论，因此单独走 failed 通道。
		detail := strings.TrimSpace(response.Stderr)
		if detail == "" {
			detail = "the command did not exit normally"
		}
		return toolutil.ErrorResultf("%s", renderFailed(errors.New(detail))), nil
	}
	return toolutil.TextResult(renderSuccess(response.ExitCode, response.Stdout, response.Stderr, response.Truncated, t.opts.maxOutputBytes)), nil
}

// validateArguments 校验并规范化模型给的参数。
func (t *tool) validateArguments(args arguments) (argv []string, reason string, err error) {
	if len(args.Command) == 0 {
		return nil, "", errors.New("request_command_escalation: command is required (a non-empty argv array; no shell)")
	}
	if len(args.Command) > maxArgvTokens {
		return nil, "", fmt.Errorf("request_command_escalation: command has %d elements, at most %d are allowed", len(args.Command), maxArgvTokens)
	}
	argv = make([]string, len(args.Command))
	copy(argv, args.Command)
	if strings.TrimSpace(argv[0]) == "" {
		return nil, "", errors.New("request_command_escalation: command[0] must name the program to execute")
	}
	for i, token := range argv {
		if strings.ContainsRune(token, 0) {
			return nil, "", fmt.Errorf("request_command_escalation: command[%d] contains a NUL byte", i)
		}
	}
	reason = strings.TrimSpace(args.Reason)
	if reason == "" {
		return nil, "", errors.New("request_command_escalation: reason is required (a one-sentence justification for the approval prompt and the audit log)")
	}
	return argv, reason, nil
}

// timeoutMS 把模型要求的超时夹到宿主上限内（0 表示用 Broker 默认值）。
func (t *tool) timeoutMS(requested int) int {
	if requested <= 0 {
		return 0
	}
	limit := int(t.opts.maxTimeout / time.Millisecond)
	if requested > limit {
		return limit
	}
	return requested
}

// callApprover 调用宿主审批回调，并把它的 panic 变成"按拒绝处理"。
//
// 审批回调是宿主代码（可能连着 UI）；它的 bug 不应该把 panic 抛进 agent 循环，更
// 不能因为 panic 让一次提权"静默通过"——recover 之后一律按拒绝（fail closed），与
// 审计回调的隔离策略一致。
func (t *tool) callApprover(ctx context.Context, req escalate.Request) (approved bool, reason string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			approved = false
			reason = ""
			err = fmt.Errorf("审批回调 panic: %v", recovered)
		}
	}()
	return t.approver(ctx, req)
}

// audit 调用宿主审计回调，并隔离它自身的 panic（审计是旁路，不能改变结论）。
func (t *tool) audit(req escalate.Request, resp escalate.Response) {
	if t.auditor == nil {
		return
	}
	defer func() { _ = recover() }()
	t.auditor(req, resp)
}

// deniedResponse 合成一条"策略拒绝、命令未执行"的响应（供本地拒绝与审计使用）。
func deniedResponse(reason string) escalate.Response {
	return escalate.Response{
		ExitCode:   escalate.DeniedExitCode,
		Denied:     true,
		DenyReason: reason,
	}
}

// normalizeOptions 校验并归一化宿主配置；任何可疑配置都在这里失败（fail closed）。
func normalizeOptions(opts Options) (options, error) {
	if len(opts.Roots) == 0 {
		return options{}, errors.New("request_command_escalation: Options.Roots 不能为空（工具需要宿主指定工作目录根）")
	}
	roots := make([]string, 0, len(opts.Roots))
	for i, root := range opts.Roots {
		trimmed := strings.TrimSpace(root)
		if trimmed == "" {
			return options{}, fmt.Errorf("request_command_escalation: Options.Roots[%d] 为空", i)
		}
		if !filepath.IsAbs(trimmed) {
			return options{}, fmt.Errorf("request_command_escalation: Options.Roots[%d] 必须是绝对路径: %q", i, trimmed)
		}
		resolved, err := filepath.EvalSymlinks(trimmed)
		if err != nil {
			return options{}, fmt.Errorf("request_command_escalation: Options.Roots[%d] 无法解析（必须存在）: %q: %w", i, trimmed, err)
		}
		roots = append(roots, resolved)
	}
	if len(opts.Allow) == 0 {
		return options{}, errors.New("request_command_escalation: Options.Allow 不能为空（空 Allow 等于什么都不允许，工具没有意义）")
	}
	for _, candidate := range []struct {
		name           string
		list           [][]string
		checkDangerous bool
	}{
		{"Allow", opts.Allow, true},
		{"Deny", opts.Deny, false},
		{"Approval", opts.Approval, true},
	} {
		if err := validatePrefixLists(candidate.name, candidate.list, candidate.checkDangerous); err != nil {
			return options{}, fmt.Errorf("request_command_escalation: %w", err)
		}
	}
	if err := validateApprovalCoverage(opts.Allow, opts.Deny, opts.Approval); err != nil {
		return options{}, fmt.Errorf("request_command_escalation: %w", err)
	}
	normalized := options{
		roots:          roots,
		dir:            roots[0],
		allow:          clonePrefixList(opts.Allow),
		deny:           clonePrefixList(opts.Deny),
		approval:       clonePrefixList(opts.Approval),
		maxTimeout:     opts.MaxTimeout,
		maxOutputBytes: opts.MaxOutputBytes,
		maxConcurrent:  opts.MaxConcurrent,
		maxQueue:       opts.MaxQueue,
	}
	if normalized.maxTimeout <= 0 {
		normalized.maxTimeout = DefaultMaxTimeout
	}
	if normalized.maxOutputBytes <= 0 {
		normalized.maxOutputBytes = DefaultMaxOutputBytes
	}
	if normalized.maxConcurrent <= 0 {
		normalized.maxConcurrent = DefaultMaxConcurrent
	}
	if normalized.maxQueue < 0 {
		return options{}, fmt.Errorf("request_command_escalation: Options.MaxQueue 不能为负（不允许无限排队）: %d", opts.MaxQueue)
	}
	if normalized.maxQueue == 0 {
		normalized.maxQueue = DefaultMaxQueue
	}
	return normalized, nil
}

// clonePrefixList 深拷贝一份前缀列表：工具之后的读路径与宿主切片彻底解耦。
func clonePrefixList(list [][]string) [][]string {
	if list == nil {
		return nil
	}
	out := make([][]string, 0, len(list))
	for _, entry := range list {
		out = append(out, append([]string(nil), entry...))
	}
	return out
}
