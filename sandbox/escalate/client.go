package escalate

// 沙箱内的 client：把请求写成一行 JSON，按 ID 收回对应的响应。
//
// client 只做"投递 + 配对"，不做任何策略判断——它运行在不受信的一端，判定权
// 完全在宿主 broker 侧。它必须是并发安全的：同一个连接上可以有多条请求在途，
// 响应靠 ID 路由回各自的调用者。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Client 是提权协议的沙箱内侧。并发安全，可直接被多个工具复用。
type Client struct {
	conn io.ReadWriteCloser

	// wmu 串行化请求写入，保证一条请求整行写出。
	wmu sync.Mutex

	mu      sync.Mutex
	pending map[string]chan Response
	err     error
	done    chan struct{}

	closeOnce sync.Once
	seq       atomic.Uint64
}

// NewClient 在一个已建立的连接上创建 client，并启动读循环。
//
// conn 通常是：子进程（沙箱内的 helper）与宿主 broker 之间的 unix socket、
// 或者 stdin/stdout 管道对。client 拥有 conn：Close 会关闭它。
func NewClient(conn io.ReadWriteCloser) *Client {
	c := &Client{
		conn:    conn,
		pending: make(map[string]chan Response),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Exec 发送一条提权请求并等待配对响应。
//
// 语义：
//   - req.ID 为空时自动分配一个（调用方通常不用关心）；
//   - 同一个 client 内 ID 必须唯一，重复会在本地报错（否则响应会串线）；
//   - 策略拒绝不是 Go 错误：resp.Denied=true、resp.DenyReason 有原因；返回的
//     error 只表示"提权通道本身有问题"（连接断开、本地取消等）；
//   - ctx 取消后本次调用立即返回，但请求可能已经在宿主侧执行——超时应通过
//     req.TimeoutMS 表达，让 broker 自己收尾。
func (c *Client) Exec(ctx context.Context, req Request) (Response, error) {
	if c == nil || c.conn == nil {
		return Response{}, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if req.ID == "" {
		req.ID = c.nextID()
	}

	ch := make(chan Response, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return Response{}, err
	}
	if _, inflight := c.pending[req.ID]; inflight {
		c.mu.Unlock()
		return Response{}, fmt.Errorf("escalate: 请求 ID %q 已有在途请求（同一连接内 ID 必须唯一）", req.ID)
	}
	c.pending[req.ID] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, req.ID)
		c.mu.Unlock()
	}()

	if err := c.write(req); err != nil {
		if errors.Is(err, errRequestTooLarge) {
			return Response{}, err
		}
		c.fail(err)
		return Response{}, wrapClosed(err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return Response{}, ctx.Err()
	case <-c.done:
		c.mu.Lock()
		err := c.err
		c.mu.Unlock()
		if err == nil {
			err = ErrClosed
		}
		return Response{}, err
	}
}

// Close 关闭 client 与底层连接：在途请求立刻得到 ErrClosed，之后所有 Exec 也
// 直接返回 ErrClosed。可重复调用。
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.fail(nil)
	return nil
}

// readLoop 持续把响应行按 ID 投递给等待者，直到连接出错。
func (c *Client) readLoop() {
	reader := bufio.NewReaderSize(c.conn, 64<<10)
	for {
		line, tooLong, err := readBoundedLine(reader, MaxLineBytes)
		if tooLong {
			// 超长响应无法归属到任何请求，丢弃后继续读。
			continue
		}
		if err != nil {
			c.fail(err)
			return
		}
		if isBlankLine(line) {
			continue
		}
		var resp Response
		if json.Unmarshal(line, &resp) != nil {
			continue
		}
		if resp.ID == "" && resp.Denied {
			// broker 回了拒绝但没有 ID（例如超长行/坏 JSON）：这条响应无法归属到
			// 任何请求，流已经失步。把在途请求全部失败掉，好过让每个调用方各自挂死。
			c.fail(fmt.Errorf("escalate: 收到无法配对的拒绝响应: %s", resp.DenyReason))
			return
		}
		c.mu.Lock()
		ch := c.pending[resp.ID]
		delete(c.pending, resp.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
}

// write 把一条请求整行写出（写入串行化）。
//
// 超出 MaxLineBytes 的请求在本地就拒绝：broker 对超长行只能回一条**没有 ID** 的
// 拒绝，这种响应无法配对，调用方会一直等到 ctx 超时。提前失败让错误可归因。
func (c *Client) write(req Request) error {
	line, err := marshalLine(req)
	if err != nil {
		return err
	}
	if len(line) > MaxLineBytes {
		return fmt.Errorf("%w: 请求编码后 %d 字节，超过单行上限 %d", errRequestTooLarge, len(line), MaxLineBytes)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.conn.Write(line)
	return err
}

// errRequestTooLarge 表示请求本身超出协议单行上限（与连接状态无关）。
var errRequestTooLarge = errors.New("escalate: 请求过大")

// fail 记录关闭原因并（只做一次地）关闭 done 与底层连接。
func (c *Client) fail(reason error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = wrapClosed(reason)
	}
	c.mu.Unlock()
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

// nextID 生成一个连接内唯一的请求 ID。
func (c *Client) nextID() string {
	return fmt.Sprintf("req-%d-%d", time.Now().UnixNano(), c.seq.Add(1))
}

// wrapClosed 把任意原因包装成"可 errors.Is(err, ErrClosed)"的错误。
func wrapClosed(reason error) error {
	if reason == nil {
		return ErrClosed
	}
	if errors.Is(reason, ErrClosed) {
		return reason
	}
	return fmt.Errorf("%w: %v", ErrClosed, reason)
}
