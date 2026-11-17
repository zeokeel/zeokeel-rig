package escalate

// 宿主侧 broker：把"被批准的提权请求"真正执行掉，并把结果交回沙箱内。
//
// broker 是整个设计的信任边界：它读到的每一个字段都可能来自被攻破的沙箱，
// 因此它在执行前重新校验一切，并且只在自己这一侧做策略判定、审批、环境过滤、
// 目录规范化与资源限制。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig/internal/procattr"
)

// Broker 在宿主侧执行被批准的提权请求。并发安全：同一个 Broker 可以服务多条
// 连接，所有请求共享一份策略、防重放集合与并发额度。
type Broker struct {
	policy   Policy
	approver func(context.Context, Request) (bool, string, error)
	runner   func(context.Context, Request) (Response, error)
	auditor  func(Request, Response)
	// sem 是并发额度：容量即"同时最多几个在途执行"。
	sem chan struct{}
	// queue 是排队名额（容量 = Policy.MaxQueue）；nil 表示不限。
	queue chan struct{}

	// 防重放集合（FIFO 淘汰，内存有界）。
	mu      sync.Mutex
	seen    map[string]struct{}
	order   []string
	maxSeen int
}

// BrokerOption 配置 Broker。
type BrokerOption func(*Broker)

// WithApprover 设置人工审批回调。fn 返回 (true, "", nil) 表示同意执行，
// (false, reason, nil) 表示拒绝，error 表示审批通道本身出错——按拒绝处理
// （fail closed）。**nil approver 表示"一律拒绝需要审批的请求"**，不是"无需审批"。
func WithApprover(fn func(context.Context, Request) (bool, string, error)) BrokerOption {
	return func(b *Broker) {
		if fn != nil {
			b.approver = fn
		}
	}
}

// WithRunner 替换默认的执行器（os/exec）。传入的 Request 已经过策略判定、
// 目录规范化与环境过滤，runner 可以直接采信这些字段。
func WithRunner(fn func(context.Context, Request) (Response, error)) BrokerOption {
	return func(b *Broker) {
		if fn != nil {
			b.runner = fn
		}
	}
}

// WithAuditor 设置审计回调：每个请求（**包括被拒绝的**）都会调用一次，且在
// 响应写出之前调用，因此审计不会因为连接先断而漏记。auditor 必须自己保证
// 并发安全（broker 不串行化它）。
func WithAuditor(fn func(Request, Response)) BrokerOption {
	return func(b *Broker) {
		if fn != nil {
			b.auditor = fn
		}
	}
}

// NewBroker 构造一个 broker。配置非法（Roots 为空/相对/不存在、空前缀条目、
// 负数上限）时返回错误，而不是等第一条请求才失败。
func NewBroker(policy Policy, opts ...BrokerOption) (*Broker, error) {
	if err := policy.validate(); err != nil {
		return nil, err
	}
	b := &Broker{
		policy:  policy,
		sem:     make(chan struct{}, policy.maxConcurrent()),
		seen:    make(map[string]struct{}),
		maxSeen: DefaultMaxSeenIDs,
	}
	if maxQueue := policy.maxQueue(); maxQueue > 0 {
		b.queue = make(chan struct{}, maxQueue)
	}
	b.runner = b.execRunner
	for _, opt := range opts {
		if opt != nil {
			opt(b)
		}
	}
	if b.runner == nil {
		b.runner = b.execRunner
	}
	return b, nil
}

// Policy 返回 broker 使用的策略（只读用途，例如工具描述里列出允许的命令）。
func (b *Broker) Policy() Policy { return b.policy }

// Serve 在 conn 上服务请求，直到 ctx 取消或对端断开。
//
// 保证：返回时所有在途请求都已经"响应完毕或已取消"——不会留下还在写连接的
// goroutine。ctx 取消返回 ctx.Err()，对端断开返回包装 ErrClosed 的错误。
//
// 对端断开（EOF）**不会**取消已经收到的请求：它们会执行完再返回（各自受
// MaxTimeout 与 Policy.MaxQueue 约束），这样"客户端写完请求就关掉写端"不会让已经
// 批准的命令静默丢失。代价是长命令可能让 Serve 在 EOF 之后再等一会儿；需要立刻
// 收敛（例如会话结束）时请取消 ctx——那会走 CommandContext 杀掉直接子进程。
//
// 连接所有权：Serve 不负责关闭 conn（正常返回时保持原样），只有 ctx 取消时才
// 用 Close 打断阻塞中的读——否则一次取消会卡在 Read 上。需要回收连接的是调用方。
func (b *Broker) Serve(ctx context.Context, conn io.ReadWriteCloser) error {
	if conn == nil {
		return errors.New("escalate: Serve 需要非空连接")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 写入串行化：一条响应必须整行写出，不能与另一条交错。
	var writeMu sync.Mutex
	var writeErr error
	write := func(resp Response) {
		writeMu.Lock()
		defer writeMu.Unlock()
		if writeErr != nil {
			return
		}
		if err := writeJSONLine(conn, resp); err != nil {
			writeErr = err
			cancel()
		}
	}

	// ctx 取消时关掉连接，让阻塞中的读立即返回。
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-sctx.Done():
			_ = conn.Close()
		case <-stopWatch:
		}
	}()

	var handlers sync.WaitGroup
	reader := bufio.NewReaderSize(conn, 64<<10)
	for {
		line, tooLong, err := readBoundedLine(reader, MaxLineBytes)
		if tooLong {
			resp := Response{
				ExitCode:   DeniedExitCode,
				Denied:     true,
				DenyReason: fmt.Sprintf("协议错误: 单行超过 1MiB 上限（%d 字节）", MaxLineBytes),
			}
			// 这一行没能解析出 ID，响应无法与请求配对；审计仍必须留痕。
			b.safeAudit(Request{Reason: resp.DenyReason}, resp)
			write(resp)
			continue
		}
		if err != nil {
			// 对端"正常关闭写端"（EOF）与"调用方取消"要区分：
			//   - EOF：已经收到的请求仍然要执行完、响应写完（半行请求也属于这种情况：
			//     客户端 CloseWrite 之后不该让已经在跑的请求变成"未获得执行槽位"）；
			//   - 其它读错误：协议已不可信，取消在途请求尽快收敛。
			peerGone := errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
			if !peerGone {
				cancel()
			}
			handlers.Wait()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if peerGone {
				return fmt.Errorf("%w: 对端已断开", ErrClosed)
			}
			return fmt.Errorf("%w: 读取失败: %v", ErrClosed, err)
		}
		if isBlankLine(line) {
			continue
		}
		var req Request
		if uerr := json.Unmarshal(line, &req); uerr != nil {
			resp := Response{
				ExitCode:   DeniedExitCode,
				Denied:     true,
				DenyReason: "协议错误: 请求不是合法 JSON: " + uerr.Error(),
			}
			b.safeAudit(Request{Reason: resp.DenyReason}, resp)
			write(resp)
			continue
		}
		handlers.Add(1)
		go func(req Request) {
			defer handlers.Done()
			// 宿主回调（runner/auditor）与策略代码都可能 panic。一次 panic 不能
			// 拖垮整个 Serve（否则连接静默失效、甚至杀掉宿主进程），也不能让调用方
			// 收不到响应。
			defer func() {
				if recovered := recover(); recovered != nil {
					write(denied(Response{ID: req.ID},
						fmt.Sprintf("broker 内部错误（已隔离，命令可能未执行）: %v", recovered)))
				}
			}()
			resp := b.process(sctx, req)
			b.safeAudit(req, resp)
			write(resp)
		}(req)
	}
}

// process 对一个请求走完整条判定 → 审批 → 执行流水线，返回该请求的响应。
//
// 它不写连接（写由 Serve 串行化），也不 panic：任何内部错误都变成 Denied 或
// ExitCode=-1 的响应，让沙箱内得到明确结论而不是"连接莫名断了"。
func (b *Broker) process(ctx context.Context, req Request) Response {
	resp := Response{ID: req.ID}

	// 协议级校验（不属于 Policy：它们是"能否配对与是否重放"的事实）。
	if strings.TrimSpace(req.ID) == "" {
		return denied(resp, "id 为空：请求无法与响应配对，也无法防重放")
	}
	if !b.claimID(req.ID) {
		return denied(resp, "重复的请求 ID（防重放）: "+req.ID)
	}

	decision := b.policy.Decide(req)
	if !decision.Allow {
		return denied(resp, decision.Reason)
	}
	if decision.NeedApproval {
		approved, reason, err := b.approve(ctx, req)
		if err != nil {
			return denied(resp, "审批失败（按拒绝处理）: "+err.Error())
		}
		if !approved {
			if strings.TrimSpace(reason) == "" {
				reason = "审批被拒绝（approver 未给出理由）"
			}
			return denied(resp, reason)
		}
	}

	kept, dropped := b.policy.FilterEnv(req.Env)
	req.Env = kept

	// 并发额度：满了就排队等待（不是立刻拒绝），但排队数有上限——超过
	// Policy.MaxQueue 立刻回"繁忙"，让客户端退避重试，而不是被无限排队拖到超时。
	if !b.tryAcquireSlot() {
		if !b.tryWaitQueueSlot() {
			// 走到这里说明既有执行在途、排队也已满：这是容量问题，不是策略拒绝，
			// 但仍然走 Denied + 审计，客户端可以据此退避重试。
			return denied(resp, QueueFullReason)
		}
		defer b.releaseQueueSlot()
		select {
		case b.sem <- struct{}{}:
		case <-ctx.Done():
			resp.ExitCode = -1
			resp.Stderr = appendDiag(resp.Stderr, "请求已取消：未获得执行槽位")
			return resp
		}
	}
	// 到这里一定拿到了执行槽位。
	defer func() { <-b.sem }()

	// 不信客户端：目录必须在"立刻要 spawn"的这一刻再解析一次。放在槽位之后是
	// 有意的——排队期间目录可能被沙箱换成符号链接（TOCTOU），排队越久窗口越大。
	dir, err := b.policy.ResolveDir(req.Dir)
	if err != nil {
		return denied(resp, err.Error())
	}
	req.Dir = dir

	timeout := b.policy.maxTimeout()
	if req.TimeoutMS > 0 {
		requested := time.Duration(req.TimeoutMS) * time.Millisecond
		// TimeoutMS 来自不受信的一端：超大值会让 duration 乘法溢出成负数，
		// 于是"请求更长超时"变成"立刻超时"。溢出时按策略上限处理。
		if requested <= 0 {
			requested = timeout
		}
		if requested < timeout {
			timeout = requested
		}
	}
	execCtx, cancelExec := context.WithTimeout(ctx, timeout)
	defer cancelExec()

	out, runErr := b.runner(execCtx, req)
	out.ID = req.ID
	switch {
	case errors.Is(execCtx.Err(), context.DeadlineExceeded):
		// 默认 runner 被 CommandContext 杀掉时只会得到 *ExitError（-1），
		// 这里补上"为什么"，否则调用方看到的是一个无声的 -1。
		out.ExitCode = -1
		out.Stderr = appendDiag(out.Stderr, fmt.Sprintf("执行超时（上限 %s），已终止", timeout))
	case runErr != nil:
		out.ExitCode = -1
		out.Stderr = appendDiag(out.Stderr, "执行失败: "+runErr.Error())
	case ctx.Err() != nil:
		out.ExitCode = -1
		out.Stderr = appendDiag(out.Stderr, "执行被取消: "+ctx.Err().Error())
	}
	if len(dropped) > 0 {
		out.Stderr = appendDiag(out.Stderr, "已丢弃非白名单环境变量: "+strings.Join(dropped, ", "))
	}
	// 复核：把"解析时看到的目录"和"现在解析出来的目录"再比一次。相等不代表一定
	// 安全（窗口仍然存在），不相等则说明目录在执行期间被替换过，本次结果不可信。
	if after, afterErr := b.policy.ResolveDir(req.Dir); afterErr != nil || after != dir {
		out.Stderr = appendDiag(out.Stderr,
			"目录在执行期间发生变化（疑似 TOCTOU 攻击）：本次结果不可信")
		out.ExitCode = -1
	}
	return b.truncate(out)
}

// safeAudit 调用审计回调，并把回调自身的 panic 隔离掉。
//
// 审计是"旁路"：它写错了不能改变请求的结论，也不能让调用方拿不到响应。因此
// panic 被 recover，响应照常写出（宿主应在自己的 auditor 里写日志，而不是 panic）。
func (b *Broker) safeAudit(req Request, resp Response) {
	if b.auditor == nil {
		return
	}
	defer func() { _ = recover() }()
	b.auditor(req, resp)
}

// tryAcquireSlot 非阻塞地占用一个执行槽位。
func (b *Broker) tryAcquireSlot() bool {
	select {
	case b.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// tryWaitQueueSlot 非阻塞地占用一个排队名额。返回 false 表示排队已满
// （b.queue 为 nil 表示"不限"，永远成功）。
func (b *Broker) tryWaitQueueSlot() bool {
	if b.queue == nil {
		return true
	}
	select {
	case b.queue <- struct{}{}:
		return true
	default:
		return false
	}
}

// releaseQueueSlot 归还排队名额。
func (b *Broker) releaseQueueSlot() {
	if b.queue == nil {
		return
	}
	<-b.queue
}

// approve 走人工审批；未配置 approver 时 fail closed。
func (b *Broker) approve(ctx context.Context, req Request) (bool, string, error) {
	if b.approver == nil {
		return false, "该命令需要人工审批，但宿主未配置 approver（fail closed）", nil
	}
	return b.approver(ctx, req)
}

// truncate 按策略上限截断输出，并在发生截断时置 Truncated。
//
// 截断点会回退到 UTF-8 字符边界，避免把中文/emoji 劈成半个字节。
func (b *Broker) truncate(resp Response) Response {
	limit := b.policy.maxOutputBytes()
	if cut, truncated := cutUTF8(resp.Stdout, limit); truncated {
		resp.Stdout = cut
		resp.Truncated = true
	}
	if cut, truncated := cutUTF8(resp.Stderr, limit); truncated {
		resp.Stderr = cut
		resp.Truncated = true
	}
	return resp
}

// claimID 登记一个请求 ID；重复返回 false（防重放）。集合按 FIFO 淘汰保持有界。
func (b *Broker) claimID(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.seen[id]; exists {
		return false
	}
	b.seen[id] = struct{}{}
	b.order = append(b.order, id)
	if len(b.order) > b.maxSeen {
		evict := b.order[0]
		b.order = b.order[1:]
		delete(b.seen, evict)
	}
	return true
}

// execRunner 是默认执行器：用 os/exec 跑 argv，输出写入有界缓冲。
func (b *Broker) execRunner(ctx context.Context, req Request) (Response, error) {
	if len(req.Argv) == 0 {
		return Response{}, errors.New("argv 为空")
	}
	if strings.TrimSpace(req.Argv[0]) == "" {
		return Response{}, errors.New("argv[0] 为空")
	}
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	// 仓库约定：每个 exec.Command/exec.CommandContext 都必须调用 HideWindow
	// （Windows 上避免每条命令弹出控制台窗口；其它平台是空操作）。由
	// internal/procattr 的守护测试强制。
	procattr.HideWindow(cmd)
	cmd.Dir = req.Dir
	cmd.Env = b.childEnv(req.Env)
	cmd.Stdin = nil
	// WaitDelay 是"有界"的关键：只杀直接子进程时，它派生的孙进程仍占着输出
	// 管道，Wait 会一直等 I/O 结束——一次 50ms 的超时能拖成整条命令的时长。
	// 给它一个上界，让 Wait 到点关管道返回。
	cmd.WaitDelay = 200 * time.Millisecond

	limit := b.policy.maxOutputBytes()
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = limit, limit
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	resp := Response{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Truncated: stdout.truncated || stderr.truncated,
	}
	if err == nil {
		return resp, nil
	}
	// 子进程已退出、只是还有孙进程占着管道：这是"命令完成"，不是失败。
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil {
		resp.ExitCode = cmd.ProcessState.ExitCode()
		return resp, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// 非零退出是"命令跑完但失败"，不是提权通道的错误：原样上报退出码。
		resp.ExitCode = exitErr.ExitCode()
		return resp, nil
	}
	resp.ExitCode = -1
	return resp, err
}

// childEnv 合成子进程环境：宿主基线 + 已过滤的客户端变量（客户端同名键覆盖基线）。
func (b *Broker) childEnv(extra map[string]string) []string {
	merged := make(map[string]string, len(extra)+16)
	for _, item := range b.policy.baseEnv() {
		if key, value, ok := strings.Cut(item, "="); ok {
			merged[key] = value
		}
	}
	for key, value := range extra {
		merged[key] = value
	}
	out := make([]string, 0, len(merged))
	for key, value := range merged {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}

// denied 把响应标记为"策略拒绝、命令未执行"。
func denied(resp Response, reason string) Response {
	resp.Denied = true
	resp.ExitCode = DeniedExitCode
	resp.DenyReason = reason
	resp.Stdout = ""
	return resp
}

// appendDiag 往 stderr 追加一行 broker 诊断（不覆盖命令自己的输出）。
func appendDiag(existing, line string) string {
	if existing == "" {
		return line
	}
	if strings.HasSuffix(existing, "\n") {
		return existing + line
	}
	return existing + "\n" + line
}

// cutUTF8 把 s 截到最多 limit 字节，且不切断多字节字符。
func cutUTF8(s string, limit int) (string, bool) {
	if limit < 0 || len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// limitedBuffer 是"有上限、永不短写"的输出缓冲。
//
// 永远返回 len(p) 是必须的：os/exec 用 io.Copy 把子进程的管道搬进来，短写会让
// 子进程收到 EPIPE 或让拷贝提前结束，等于用截断把命令弄失败。
type limitedBuffer struct {
	buf       strings.Builder
	limit     int
	truncated bool
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		w.limit = DefaultMaxOutputBytes
	}
	room := w.limit - w.buf.Len()
	switch {
	case room <= 0:
		w.truncated = true
	case len(p) > room:
		// 截断点回退到 UTF-8 字符边界：多字节字符被劈开会在响应里留下半个 rune
		// （JSON 序列化后变成 U+FFFD，属于数据损坏）。
		cut := room
		for cut > 0 && !utf8.RuneStart(p[cut]) {
			cut--
		}
		w.buf.Write(p[:cut])
		w.truncated = true
	default:
		w.buf.Write(p)
	}
	return len(p), nil
}

func (w *limitedBuffer) String() string { return w.buf.String() }
