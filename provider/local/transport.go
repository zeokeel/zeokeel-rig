package local

// 本文件是共享的 HTTP 传输层：重试、退避（尊重 Retry-After）、错误分类与"连不上
// 服务"的可执行提示。ollama / lmstudio 的模型列表探测复用它，因此三个包只有一份
// 重试实现。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// maxRetryDelay 限制单次退避等待（含 Retry-After）：provider 的重试不会睡上
	// 好几分钟，长等待应由调用方决定。
	maxRetryDelay = 30 * time.Second
	// maxResponseBody 限制模型列表等非流式响应体的读取上限（8 MiB）。
	maxResponseBody = 8 << 20
)

// Transport 是带重试的 HTTP 传输层，可被多个 provider 共享（不可变，并发安全）。
type Transport struct {
	// Name 是错误前缀（例如 "ollama"）；空表示 "local"。厂商封装设置它，好让
	// 用户看到的错误文本指向真正出问题的服务。
	Name string
	// Client 是底层 HTTP 客户端。
	Client *http.Client
	// MaxRetries 是可重试失败的额外尝试次数。
	MaxRetries int
	// Backoff 是首次重试前的基础延迟，每次翻倍；<=0 表示立即重试。
	Backoff time.Duration
	// Hint 会附加到"连不上服务"的错误上（例如 "ollama 未启动？试试 `ollama serve`"）。
	Hint string
}

// Do 执行一次请求，并对可重试失败（429/408/425/5xx 与传输错误）按 MaxRetries 重试。
//
// 返回值语义：只对"传输层失败"（连接被拒、超时、ctx 取消等）返回 error；HTTP 状态
// 由调用方检查 resp.StatusCode。重试耗尽后返回最后一次响应（或最后一次传输错误），
// 因此调用方能在错误信息里带上服务端给的响应体。
func (t *Transport) Do(ctx context.Context, method, url string, payload []byte, headers http.Header) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// 至少尝试一次：MaxRetries 为负（"禁用重试"）时不能变成"一次都不发"。
	attempts := max(t.MaxRetries, 0) + 1
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("%s: build request: %w", t.name(), err)
		}
		for name, values := range headers {
			for _, value := range values {
				req.Header.Add(name, value)
			}
		}
		resp, err := t.Client.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, canceledError(t.name(), ctxErr)
			}
			lastErr = t.transportError(err)
			if attempt+1 >= attempts || !retryableTransportError(err) {
				return nil, lastErr
			}
			if waitErr := sleepCtx(ctx, t.retryDelay(attempt, "")); waitErr != nil {
				return nil, canceledError(t.name(), waitErr)
			}
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			return resp, nil
		}
		if attempt+1 >= attempts || !retryableStatus(resp.StatusCode) {
			return resp, nil
		}
		// 重试前把失败的响应体读掉并关闭，避免连接泄漏。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errorBodyMax))
		_ = resp.Body.Close()
		if waitErr := sleepCtx(ctx, t.retryDelay(attempt, resp.Header.Get("Retry-After"))); waitErr != nil {
			return nil, canceledError(t.name(), waitErr)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%s: request failed", t.name())
	}
	return nil, lastErr
}

// GetBody 发一次 GET 并返回响应体（有上限）。非 2xx 时返回带状态码与响应体的错误。
func (t *Transport) GetBody(ctx context.Context, url string, headers http.Header) ([]byte, error) {
	resp, err := t.Do(ctx, http.MethodGet, url, nil, headers)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyMax))
		return nil, statusError(t.name(), resp, snippet)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("%s: read response: %w", t.name(), err)
	}
	return body, nil
}

// transportError 把传输错误包装上宿主的提示（如果有）。
//
// 这里只对"连不上"这一大类加提示：本机服务没起来是本地模型最常见的故障，用户看到
// "connection refused" 往往不知道该做什么。
func (t *Transport) transportError(err error) error {
	wrapped := fmt.Errorf("%s: request failed: %w", t.name(), err)
	if strings.TrimSpace(t.Hint) == "" {
		return wrapped
	}
	if isUnreachable(err) {
		return fmt.Errorf("%s: %s", wrapped.Error(), t.Hint)
	}
	return wrapped
}

// name 返回错误前缀。
func (t *Transport) name() string {
	if strings.TrimSpace(t.Name) != "" {
		return t.Name
	}
	return DefaultName
}

// isUnreachable 报告错误是否属于"服务没起来/连不上"。
func isUnreachable(err error) bool {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		text := strings.ToLower(urlErr.Err.Error())
		return strings.Contains(text, "connection refused") ||
			strings.Contains(text, "connect: connection refused") ||
			strings.Contains(text, "no such host") ||
			strings.Contains(text, "connection reset") ||
			strings.Contains(text, "actively refused")
	}
	return false
}

// retryableStatus 报告值得重试的状态码。
func retryableStatus(code int) bool {
	switch {
	case code == http.StatusRequestTimeout, code == http.StatusTooEarly, code == http.StatusTooManyRequests:
		return true
	case code >= 500 && code <= 599:
		return true
	default:
		return false
	}
}

// retryableTransportError 报告请求是否在收到响应之前就失败（因此重放是安全的）。
func retryableTransportError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

// retryDelay 返回第 attempt 次（从 0 开始）重试前的延迟：Retry-After 优先，其次指数
// 退避，两者都受 maxRetryDelay 约束。
func (t *Transport) retryDelay(attempt int, retryAfter string) time.Duration {
	if delay, ok := parseRetryAfter(retryAfter); ok {
		return delay
	}
	if t.Backoff <= 0 {
		return 0
	}
	delay := t.Backoff
	for i := 0; i < attempt; i++ {
		if delay >= maxRetryDelay {
			return maxRetryDelay
		}
		delay *= 2
	}
	return min(delay, maxRetryDelay)
}

// parseRetryAfter 解析 Retry-After（delta 秒或 HTTP 日期）。
func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			seconds = 0
		}
		return min(time.Duration(seconds)*time.Second, maxRetryDelay), true
	}
	if when, err := http.ParseTime(value); err == nil {
		delay := time.Until(when)
		if delay < 0 {
			delay = 0
		}
		return min(delay, maxRetryDelay), true
	}
	return 0, false
}

// sleepCtx 等待 delay 或 ctx 取消，先到者为准。
func sleepCtx(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
