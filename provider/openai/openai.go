// Package openai implements the Rig model Provider for any
// OpenAI-compatible chat-completions endpoint: OpenAI, DeepSeek, vLLM,
// llama.cpp server, Ollama's OpenAI shim, OpenRouter, and similar.
//
// The provider speaks the streaming chat-completions protocol: it POSTs
// {BaseURL}/chat/completions with "stream": true, then decodes the SSE
// response into rig.Chunk values (content, reasoning, tool-call fragments,
// usage, finish reason). It is standard-library only and safe for concurrent
// use: a Provider holds no per-stream state.
//
//	p, err := openai.New(openai.Config{
//		BaseURL: "https://api.deepseek.com/v1",
//		APIKeyEnv: "DEEPSEEK_API_KEY",
//		DefaultModel: "deepseek-chat",
//	})
//	stream, err := p.Stream(ctx, rig.Request{...})
//
// Transport failures, non-2xx statuses and request-encoding errors surface
// synchronously from Stream. HTTP 429/5xx responses are retried with a bounded
// exponential backoff that honours Retry-After. Once Stream has returned the
// caller owns the Stream; Recv returns io.EOF after the final chunk and closes
// the response body.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

const (
	// DefaultBaseURL is used when Config.BaseURL is empty.
	DefaultBaseURL = "https://api.openai.com/v1"
	// DefaultAPIKeyEnv is the environment variable read when Config.APIKey and
	// Config.APIKeyEnv are both empty.
	DefaultAPIKeyEnv = "OPENAI_API_KEY"
	// DefaultMaxRetries is the retry budget applied when Config.MaxRetries is
	// the zero value (0). A negative value disables retries.
	DefaultMaxRetries = 2
	// DefaultRetryBackoff is the base delay applied when Config.RetryBackoff is
	// the zero value. Backoff doubles per attempt; a negative value means retry
	// immediately.
	DefaultRetryBackoff = 500 * time.Millisecond

	// userAgent identifies the SDK to the endpoint.
	userAgent = "zeokeel-rig"
	// errorBodyMax caps how much of a failed response body is quoted in the
	// returned error.
	errorBodyMax = 1024
	// maxRetryDelay caps one backoff sleep (including Retry-After). Long waits
	// are the caller's decision; a provider retry never sleeps for minutes.
	maxRetryDelay = 30 * time.Second
	// defaultHeaderTimeout bounds the wait for response headers. The streamed
	// body itself is bounded by the caller's context (long generations are
	// legitimate), so no overall http.Client timeout is set.
	defaultHeaderTimeout = 60 * time.Second
)

// Config configures a Provider. The zero value is usable: BaseURL defaults to
// DefaultBaseURL, the API key is read from OPENAI_API_KEY, up to two retries
// are attempted with a 500ms base backoff, and streamed usage accounting is
// requested.
type Config struct {
	// BaseURL is the API root without the /chat/completions suffix, e.g.
	// "https://api.openai.com/v1". Trailing slashes are trimmed.
	BaseURL string
	// APIKey is sent as a bearer token. When empty, APIKeyEnv (or
	// OPENAI_API_KEY) is read from the environment. Local servers that need no
	// key are supported: an empty resolved key simply omits the header.
	APIKey string
	// APIKeyEnv names the environment variable that holds the API key. Empty
	// means OPENAI_API_KEY.
	APIKeyEnv string
	// DefaultModel is used when a Request does not name a model.
	DefaultModel string
	// Headers are extra request headers (for example attribution headers for a
	// gateway). They are applied after the SDK headers and therefore may
	// override them.
	Headers map[string]string
	// HTTPClient overrides the HTTP client. Nil means a client whose transport
	// bounds the response-header wait at 60s; the request context bounds the
	// streamed body.
	HTTPClient *http.Client
	// MaxRetries is the number of extra attempts after a retryable failure
	// (429, 408, 425, 5xx, transport error). Zero means DefaultMaxRetries; a
	// negative value disables retries.
	MaxRetries int
	// RetryBackoff is the base delay before the first retry; it doubles per
	// attempt. Zero means DefaultRetryBackoff; a negative value retries with no
	// delay.
	RetryBackoff time.Duration
	// IncludeUsage is kept for configuration compatibility: Rig always asks
	// the endpoint for streamed usage accounting (stream_options.include_usage),
	// so the zero value mirrors the SDK default.
	IncludeUsage bool
}

// Provider is an OpenAI-compatible rig.Provider. It holds no per-stream state
// and is safe for concurrent use.
type Provider struct {
	baseURL      string
	apiKey       string
	defaultModel string
	headers      map[string]string
	client       *http.Client
	maxRetries   int
	backoff      time.Duration
}

var _ rig.Provider = (*Provider)(nil)

// New validates cfg, applies defaults and returns a ready Provider.
func New(cfg Config) (*Provider, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("openai: invalid base URL %q: %w", cfg.BaseURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("openai: invalid base URL %q: scheme must be http or https", cfg.BaseURL)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("openai: invalid base URL %q: missing host", cfg.BaseURL)
	}

	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		env := strings.TrimSpace(cfg.APIKeyEnv)
		if env == "" {
			env = DefaultAPIKeyEnv
		}
		apiKey = strings.TrimSpace(os.Getenv(env))
	}

	headers := make(map[string]string, len(cfg.Headers))
	for name, value := range cfg.Headers {
		headers[name] = value
	}

	client := cfg.HTTPClient
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if transport.ResponseHeaderTimeout == 0 {
			transport.ResponseHeaderTimeout = defaultHeaderTimeout
		}
		client = &http.Client{Transport: transport}
	}

	maxRetries := cfg.MaxRetries
	switch {
	case maxRetries == 0:
		maxRetries = DefaultMaxRetries
	case maxRetries < 0:
		maxRetries = 0
	}
	backoff := cfg.RetryBackoff
	switch {
	case backoff == 0:
		backoff = DefaultRetryBackoff
	case backoff < 0:
		backoff = 0
	}

	return &Provider{
		baseURL:      base,
		apiKey:       apiKey,
		defaultModel: strings.TrimSpace(cfg.DefaultModel),
		headers:      headers,
		client:       client,
		maxRetries:   maxRetries,
		backoff:      backoff,
	}, nil
}

// Name implements rig.Provider.
func (p *Provider) Name() string { return "openai" }

// Stream starts one streaming completion. It returns once the endpoint has
// accepted the request and responded with an event stream; parsing then happens
// lazily in Recv.
//
// Retryable request failures (HTTP 429/408/425/5xx and transport errors) are
// retried up to MaxRetries times with exponential backoff, honouring a
// Retry-After header. Failures after the stream is established are not retried:
// a partially emitted answer cannot be retracted.
func (p *Provider) Stream(ctx context.Context, req rig.Request) (rig.Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := p.buildRequest(req)
	if err != nil {
		return nil, err
	}

	attempts := p.maxRetries + 1
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		resp, err := p.post(ctx, payload)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, canceledError(ctxErr)
			}
			lastErr = err
			if attempt+1 >= attempts || !retryableTransportError(err) {
				return nil, err
			}
			if waitErr := sleepCtx(ctx, p.retryDelay(attempt, "")); waitErr != nil {
				return nil, canceledError(waitErr)
			}
			continue
		}

		if resp.StatusCode >= http.StatusOK && resp.StatusCode <= 299 {
			if err := checkStreamingResponse(resp); err != nil {
				return nil, err
			}
			return newSSEStream(ctx, resp), nil
		}

		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyMax))
		_ = resp.Body.Close()
		statusErr := statusError(resp, snippet)
		lastErr = statusErr
		if attempt+1 >= attempts || !retryableStatus(resp.StatusCode) {
			return nil, statusErr
		}
		delay := p.retryDelay(attempt, resp.Header.Get("Retry-After"))
		if waitErr := sleepCtx(ctx, delay); waitErr != nil {
			return nil, canceledError(waitErr)
		}
	}
	return nil, lastErr
}

// post performs one HTTP request for payload.
func (p *Provider) post(ctx context.Context, payload []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("User-Agent", userAgent)
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for name, value := range p.headers {
		httpReq.Header.Set(name, value)
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: chat completions request: %w", err)
	}
	return resp, nil
}

// checkStreamingResponse rejects 2xx responses that are not an SSE stream: this
// provider does not implement the non-streaming fallback.
func checkStreamingResponse(resp *http.Response) error {
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" || strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyMax))
	_ = resp.Body.Close()
	body := strings.TrimSpace(string(snippet))
	if body == "" {
		return fmt.Errorf("openai: expected a text/event-stream response, got Content-Type %q; non-streaming responses are not supported", contentType)
	}
	return fmt.Errorf("openai: expected a text/event-stream response, got Content-Type %q; non-streaming responses are not supported: %s", contentType, body)
}

// statusError renders a non-2xx response, quoting a short body snippet (many
// compatible servers put the real cause there).
func statusError(resp *http.Response, snippet []byte) error {
	body := strings.TrimSpace(string(snippet))
	if body == "" {
		return fmt.Errorf("openai: chat completions failed: %s", resp.Status)
	}
	return fmt.Errorf("openai: chat completions failed: %s: %s", resp.Status, body)
}

// canceledError reports ctx cancellation in a way errors.Is matches.
func canceledError(err error) error {
	return fmt.Errorf("openai: request canceled: %w", err)
}

// retryableStatus reports the statuses worth retrying: rate limiting, request
// timeout/too-early and server-side failures.
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

// retryableTransportError reports whether the request failed before receiving
// any response, so replaying it is safe. Context errors are handled by the
// caller and never retried here.
func retryableTransportError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

// retryDelay returns the delay before retry attempt (0-based). A valid
// Retry-After header (delta seconds or HTTP date) wins over the exponential
// backoff; both are capped at maxRetryDelay.
func (p *Provider) retryDelay(attempt int, retryAfter string) time.Duration {
	if delay, ok := parseRetryAfter(retryAfter); ok {
		return delay
	}
	if p.backoff <= 0 {
		return 0
	}
	delay := p.backoff
	for i := 0; i < attempt; i++ {
		if delay >= maxRetryDelay {
			return maxRetryDelay
		}
		delay *= 2
	}
	delay = min(delay, maxRetryDelay)
	return delay
}

// parseRetryAfter parses the Retry-After header (delta seconds or HTTP-date).
func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			seconds = 0
		}
		delay := time.Duration(seconds) * time.Second
		delay = min(delay, maxRetryDelay)
		return delay, true
	}
	if when, err := http.ParseTime(value); err == nil {
		delay := time.Until(when)
		if delay < 0 {
			delay = 0
		}
		delay = min(delay, maxRetryDelay)
		return delay, true
	}
	return 0, false
}

// sleepCtx waits for delay or ctx cancellation, whichever comes first.
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

// buildRequest maps a rig.Request to the OpenAI-compatible JSON body.
func (p *Provider) buildRequest(req rig.Request) ([]byte, error) {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = p.defaultModel
	}
	if model == "" {
		return nil, fmt.Errorf("openai: no model configured: set Config.DefaultModel or Request.Model: %w", rig.ErrModelRequired)
	}

	messages := make([]chatMessage, 0, len(req.Messages))
	for _, message := range req.Messages {
		wire := chatMessage{
			Role:       string(message.Role),
			Content:    wireContent(message),
			ToolCallID: message.ToolCallID,
			Name:       message.Name,
		}
		for _, call := range message.ToolCalls {
			arguments := strings.TrimSpace(call.Arguments)
			if arguments == "" {
				arguments = "{}" // the canonical "no arguments" object
			}
			wire.ToolCalls = append(wire.ToolCalls, chatToolCall{
				ID:   call.ID,
				Type: "function",
				Function: chatFunctionCall{
					Name:      call.Name,
					Arguments: arguments,
				},
			})
		}
		messages = append(messages, wire)
	}

	tools := make([]chatTool, 0, len(req.Tools))
	for _, spec := range req.Tools {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			return nil, fmt.Errorf("openai: tool with empty name")
		}
		tools = append(tools, chatTool{
			Type: "function",
			Function: chatFunction{
				Name:        name,
				Description: spec.Description,
				Parameters:  spec.Parameters,
			},
		})
	}

	body := chatRequest{
		Model:         model,
		Messages:      messages,
		Tools:         tools,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
		Temperature:   cloneFloat(req.Temperature),
		TopP:          cloneFloat(req.TopP),
		MaxTokens:     req.MaxTokens,
	}
	raw, err := encodeJSON(body)
	if err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}
	if len(req.Extra) == 0 {
		return raw, nil
	}
	return mergeExtra(raw, req.Extra)
}

// reservedRequestFields are owned by the provider; Extra cannot override them.
var reservedRequestFields = map[string]bool{
	"model": true, "messages": true, "tools": true, "stream": true,
	"stream_options": true, "temperature": true, "top_p": true, "max_tokens": true,
}

// mergeExtra merges provider-specific top-level fields (for example
// reasoning_effort) into the encoded request without letting them override the
// fields the provider owns.
func mergeExtra(raw []byte, extra map[string]any) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}
	for name, value := range extra {
		if reservedRequestFields[name] {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("openai: encode extra field %q: %w", name, err)
		}
		fields[name] = encoded
	}
	merged, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}
	return merged, nil
}

// wireContent maps a message's content. Text-only messages and non-user/
// assistant roles keep the plain string form; a user/assistant message with at
// least one usable image becomes an OpenAI multimodal content array preserving
// the original part order.
func wireContent(message rig.Message) any {
	if len(message.Content) == 0 {
		return ""
	}
	if message.Role != rig.RoleUser && message.Role != rig.RoleAssistant {
		return message.Text()
	}
	hasImage := false
	for _, part := range message.Content {
		if part.Type == rig.PartImageURL && part.ImageURL != "" {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return message.Text()
	}
	parts := make([]chatContentPart, 0, len(message.Content))
	for _, part := range message.Content {
		switch part.Type {
		case rig.PartText:
			text := part.Text
			parts = append(parts, chatContentPart{Type: "text", Text: &text})
		case rig.PartImageURL:
			if part.ImageURL == "" {
				continue
			}
			detail := part.Detail
			if detail == "" {
				detail = "auto"
			}
			parts = append(parts, chatContentPart{
				Type:     "image_url",
				ImageURL: &chatImageURL{URL: part.ImageURL, Detail: detail},
			})
		}
	}
	if len(parts) == 0 {
		return message.Text()
	}
	return parts
}

// cloneFloat copies a pointer so later caller mutation cannot change a request.
func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
