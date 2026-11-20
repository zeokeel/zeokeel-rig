package kit

// Shutdown：宿主关窗口时的唯一正确收尾入口。
//
// # 它解决的问题
//
// 旧版本里宿主没有可调用的收尾动作，只能自己去 `st.Close()`。如果此时还有回合在
// 跑，那个回合下一次 Append 就拿到 ErrClosed → 整个 Run 判失败 → 用户看到"回答
// 到一半消失"。这是用户能感知的**数据/体验损失**，不是内部整洁问题。
//
// # 顺序（每一步都不能换位置）
//
//  1. 取消在飞回合：给它们一条 `context.Canceled` 的退出路径，而不是让它们撞上
//     已关闭的存储；
//  2. 等它们退出：Run 的每一次 Append 与事件写入都在返回之前完成（agent 的
//     appendMessages 用 context.WithoutCancel，取消之后仍会把已经产生的消息落盘），
//     所以"排空事件写入"= 等 Run 返回；有缓冲 writer 时再 flush 一次；
//  3. 关后台任务：bash 的后台命令与 PTY 会话不能活过宿主进程（jobs.Manager /
//     terminal.Manager 的 CloseAll）；
//  4. 关存储：`sqlite.Store.Close` 内部先做 `PRAGMA wal_checkpoint(TRUNCATE)`，
//     于是关闭之后的 `.db` 是完整可拷的。
//
// 若第 2 步等不到回合退出（ctx 超时），本方法**不关存储**——那正是 ErrClosed 的
// 来源——但仍然关后台任务并返回错误；宿主可以再调一次 Shutdown 收尾。
//
// # 幂等
//
// 收尾完成的再次调用是 no-op（返回 nil，也不会重复 Close）。Shutdown 之后新的 Run
// 直接返回 ErrShuttingDown，不会去碰一个正在关闭的存储。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ErrShuttingDown 表示 agent 已经进入关停流程，新的 Run 被拒绝。
//
// 它与"存储已关闭"（store.ErrClosed / sqlite.ErrClosed）刻意分开：前者是宿主的
// 主动动作，后者是竞态事故。调用方据此可以静默丢弃用户新输入，而不是弹一个错误。
var ErrShuttingDown = errors.New("kit: agent is shutting down")

// Background 是一个"有后台任务要收尾"的句柄最小面。
//
// tools/jobs.Manager 与 tools/terminal.Manager 都满足它（它们的 CloseAll 会杀进程、
// 回收并清空记录，且可重复调用）。kit 只依赖这个接口，不 import 具体工具包。
type Background interface {
	CloseAll() error
}

// Storage 是 Shutdown 最后收尾的存储。
//
// 现在只需要 Close；`*session/sqlite.Store` 与 `*session/store.Dir` 都满足它。
// `storage.Storage` 聚合契约落地后这个接口会**加宽**成那个契约，
// 名字不变，宿主不需要迁移字段名。
type Storage interface {
	Close() error
}

// lifecycle 是一个 Agent 的运行期收尾状态。
//
// 零值不可用；newLifecycle 建立可取消的根 context。
type lifecycle struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	// background 是需要在 Shutdown 里关掉的后台句柄（可能包含 full.NewRuntime
	// 交出的 jobs.Manager / terminal.Manager，也可能来自 Config.BackgroundJobs）。
	background []Background
	// stores 是需要在 Shutdown 里关掉的存储。kit 只登记**自己打开的** store.Dir，
	// 以及宿主显式放进 Config.Storage 的那一个。
	stores []Storage

	backgroundClosed bool
	storesClosed     bool

	// runs 统计在飞 Run。Add 只在 closed == false 时发生（同一把锁），因此
	// Shutdown 置 closed 之后的 Wait 不会再遇到并发的 Add。
	runs sync.WaitGroup
	// inFlight 是 runs 的原子镜像：让 waitRuns 在 ctx 已取消但 0 在飞回合时
	// 立刻返回成功，而不是和 WaitGroup 的完成通知赛跑。
	inFlight atomic.Int64
}

// newLifecycle 建立可取消的根 context：所有 Run 都挂在它下面，Shutdown 用一次
// cancel 就能让每一个在飞回合（包括它们的 provider 流与工具调用）收到取消。
func newLifecycle() *lifecycle {
	ctx, cancel := context.WithCancel(context.Background())
	return &lifecycle{ctx: ctx, cancel: cancel}
}

// begin 把一个调用方的 ctx 接入生命周期：返回的 ctx 在**调用方取消或 Shutdown**
// 时取消。done 必须 defer 调用，它负责把本次 Run 从在飞计数里摘掉。
func (l *lifecycle) begin(ctx context.Context) (context.Context, func(), error) {
	if l == nil {
		// 手工构造的 Agent（嵌入/测试）没有生命周期：行为与旧版本一致。
		return ctx, func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, nil, ErrShuttingDown
	}
	l.runs.Add(1)
	l.inFlight.Add(1)
	base := l.ctx
	l.mu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	// AfterFunc 而不是"另起一个 goroutine select"：没有常驻 goroutine，Run 结束后
	// stop() 就把回调摘掉。
	stop := context.AfterFunc(base, cancel)
	done := func() {
		stop()
		cancel()
		l.runs.Done()
		l.inFlight.Add(-1)
	}
	return runCtx, done, nil
}

// isClosed 报告关停是否已经开始（用于"这次取消是宿主主动关停造成的"判定）。
func (l *lifecycle) isClosed() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// waitRuns 等在飞 Run 全部退出，或 ctx 到期。
func (l *lifecycle) waitRuns(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		l.runs.Wait()
		close(done)
	}()
	if l.inFlight.Load() == 0 {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return &shutdownWaitError{err: ctx.Err()}
	}
}

// shutdownWaitError 标记"等待在飞回合"失败；Shutdown 用它区分"该保留 store"与
// 其它收尾错误（drain/后台 CloseAll）。
type shutdownWaitError struct{ err error }

func (e *shutdownWaitError) Error() string {
	return "kit: shutdown: waiting for in-flight turns: " + e.err.Error()
}
func (e *shutdownWaitError) Unwrap() error { return e.err }

// shutdown 执行收尾。drain 在关存储之前调用（默认是 Agent.drainEvents）。
func (l *lifecycle) shutdown(ctx context.Context, drain func() error) error {
	if l == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	l.mu.Lock()
	if l.closed && l.backgroundClosed && l.storesClosed {
		// 已经完整收尾：幂等的 no-op（不重复 Close，也不重复 report 错误）。
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	background := append([]Background(nil), l.background...)
	backgroundClosed := l.backgroundClosed
	stores := append([]Storage(nil), l.stores...)
	storesClosed := l.storesClosed
	l.mu.Unlock()

	var errs []error

	// 1. 取消在飞回合。
	l.cancel()

	// 2. 等它们退出（= 排空回合驱动的 Append 与事件写入）。
	waitErr := l.waitRuns(ctx)
	if waitErr != nil {
		errs = append(errs, waitErr)
	}

	// 3. 关后台任务。它在关存储之前：回合已经退出，不会再有工具去等一个刚被杀的
	//    后台命令；而后台任务本身不写存储，先关它不会影响第 5 步。
	if !backgroundClosed {
		closedCleanly := true
		for _, handle := range background {
			if handle == nil {
				continue
			}
			if err := handle.CloseAll(); err != nil {
				closedCleanly = false
				errs = append(errs, fmt.Errorf("kit: shutdown background jobs: %w", err))
			}
		}
		// 有失败就不标记完成：宿主再调一次 Shutdown 会重试（CloseAll 幂等）。
		if closedCleanly {
			l.mu.Lock()
			l.backgroundClosed = true
			l.mu.Unlock()
		}
	}

	// 4. 排空事件写入：同步 emitter 的每一行都在 Run 返回前写完，这里只 flush
	//    缓冲 writer（bufio.Writer 之类）并汇报失败。
	if drain != nil {
		if err := drain(); err != nil {
			errs = append(errs, err)
		}
	}

	// 5. 关存储。回合没退干净时**不关**：那正是"回答到一半消失"的路径。
	if !storesClosed && waitErr == nil {
		closedCleanly := true
		for _, store := range stores {
			if store == nil {
				continue
			}
			if err := store.Close(); err != nil {
				closedCleanly = false
				errs = append(errs, fmt.Errorf("kit: shutdown storage: %w", err))
			}
		}
		// 同上：失败的存储留给下一次调用重试，不把错误吞成"已完成"。
		if closedCleanly {
			l.mu.Lock()
			l.storesClosed = true
			l.mu.Unlock()
		}
	}
	return errors.Join(errs...)
}

// drainEvents 把事件流收尾：flush 缓冲 writer，并把粘性写错误（EventError）留给
// 宿主单独查询——事件写失败按设计不该改变 Run 的结果，也不该把 Shutdown 判失败。
func (a *Agent) drainEvents() error {
	if a == nil {
		return nil
	}
	if a.eventWriter == nil {
		return nil
	}
	flusher, ok := a.eventWriter.(interface{ Flush() error })
	if !ok {
		// 非缓冲 writer（文件、bytes.Buffer、os.Stdout）：写入已经是同步的。
		return nil
	}
	if err := flusher.Flush(); err != nil {
		return fmt.Errorf("kit: shutdown: flush events: %w", err)
	}
	return nil
}

// Shutdown 收尾本 agent：取消在飞回合 → 等它们退出并排空事件 → 关后台任务 →
// 关存储（存储自己的 Close 负责 wal_checkpoint）。
//
// 可重复调用；ctx 用来限制"等在飞回合退出"的时间。nil Agent 上调用是安全的
// （宿主清理代码常写在 defer 里）。
func (a *Agent) Shutdown(ctx context.Context) error {
	if a == nil || a.life == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// 先取消并等待在飞回合退出（与其它存储同一条规则：等不到就不关存储）。
	shutdownErr := a.life.shutdown(ctx, a.drainEvents)
	var waitErr *shutdownWaitError
	if shutdownErr != nil && errors.As(shutdownErr, &waitErr) {
		// 只有"等待在飞回合超时/取消"才保留 memory store；drain/后台关闭失败
		// 仍应继续收尾记忆，否则 pending 永远不落盘。
		return shutdownErr
	}
	// 回合都退出后再冲刷 pending（否则冲刷的是取消之前的快照），最后关 store。
	// 调用方常用"已取消的回合 ctx"来关机：此时用独立短超时上下文完成落盘。
	flushCtx := ctx
	var cancelFlush context.CancelFunc
	if ctx.Err() != nil {
		flushCtx, cancelFlush = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelFlush()
	}
	memoryErr := a.flushMemory(flushCtx)
	closeErr := a.closeMemory()
	return errors.Join(shutdownErr, memoryErr, closeErr)
}

// shutdownCancellation 报告"这次 Run 的错误是 Shutdown 取消造成的"。
//
// 用途：关停不是失败。事件流里若把 context.Canceled 原样写进 run.end 的 err，
// 宿主 UI 会把一次正常退出显示成"回合失败"——正是 关停路径要止住的那件事。
func (a *Agent) shutdownCancellation(err error) bool {
	if err == nil || a == nil || a.life == nil {
		return false
	}
	// Runtime.Close 取消在飞回合时，Run 已归一成 ErrRuntimeClosed：对 UI 而言
	// 同样是"宿主主动收尾"，不是回合失败。
	if errors.Is(err, ErrRuntimeClosed) && a.runtime != nil {
		return true
	}
	if !errors.Is(err, context.Canceled) {
		return false
	}
	return a.life.isClosed() || (a.runtime != nil && a.runtime.isClosed())
}
