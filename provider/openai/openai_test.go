package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// newTestProvider starts a test server and returns a provider pointed at it
// with fast retry settings.
func newTestProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := New(Config{
		BaseURL:      server.URL,
		APIKey:       "test-key",
		DefaultModel: "test-model",
		HTTPClient:   server.Client(),
		RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return provider
}

// writeSSE streams events as SSE data lines followed by [DONE].
func writeSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	for _, event := range events {
		fmt.Fprintf(w, "data: %s\n\n", event)
		if flusher != nil {
			flusher.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// collect drains a stream until io.EOF and fails the test on any error.
func collect(t *testing.T, stream v1.Stream) []v1.Chunk {
	t.Helper()
	var chunks []v1.Chunk
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return chunks
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		chunks = append(chunks, chunk)
	}
}

func joinText(chunks []v1.Chunk) string {
	var builder strings.Builder
	for _, chunk := range chunks {
		builder.WriteString(chunk.Delta)
	}
	return builder.String()
}

func finishReason(chunks []v1.Chunk) string {
	for _, chunk := range chunks {
		if chunk.FinishReason != "" {
			return chunk.FinishReason
		}
	}
	return ""
}

// ① Pure text stream.
func TestStreamText(t *testing.T) {
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept header = %q, want text/event-stream", got)
		}
		writeSSE(w,
			`{"choices":[{"delta":{"role":"assistant"}}]}`,
			`{"choices":[{"delta":{"content":"Hello"}}]}`,
			`{"choices":[{"delta":{"content":" world"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		)
	})

	stream, err := provider.Stream(context.Background(), v1.Request{
		Messages: []v1.Message{v1.NewMessage(v1.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := collect(t, stream)
	if got := joinText(chunks); got != "Hello world" {
		t.Fatalf("text = %q, want %q", got, "Hello world")
	}
	if got := finishReason(chunks); got != "stop" {
		t.Fatalf("finish reason = %q, want stop", got)
	}
}

// ② Tool-call arguments accumulate across chunks, keyed by index.
func TestStreamToolCallDeltas(t *testing.T) {
	provider := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(w,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"Shenzhen\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		)
	})

	stream, err := provider.Stream(context.Background(), v1.Request{
		Messages: []v1.Message{v1.NewMessage(v1.RoleUser, "weather?")},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := collect(t, stream)

	type assembled struct {
		id, name, args string
	}
	calls := make(map[int]*assembled)
	for _, chunk := range chunks {
		for _, delta := range chunk.ToolCalls {
			call, ok := calls[delta.Index]
			if !ok {
				call = &assembled{}
				calls[delta.Index] = call
			}
			if delta.ID != "" {
				call.id = delta.ID
			}
			if delta.Name != "" {
				call.name = delta.Name
			}
			call.args += delta.Arguments
		}
	}
	call, ok := calls[0]
	if !ok {
		t.Fatalf("no tool call deltas received: %+v", chunks)
	}
	if call.id != "call_1" || call.name != "weather" {
		t.Fatalf("tool call = %+v, want call_1/weather", call)
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(call.args), &arguments); err != nil {
		t.Fatalf("assembled arguments %q are not JSON: %v", call.args, err)
	}
	if arguments["city"] != "Shenzhen" {
		t.Fatalf("arguments = %v, want city Shenzhen", arguments)
	}
	if got := finishReason(chunks); got != "tool_calls" {
		t.Fatalf("finish reason = %q, want tool_calls", got)
	}
}

// ③ Reasoning and usage mapping.
func TestStreamReasoningAndUsage(t *testing.T) {
	provider := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(w,
			`{"choices":[{"delta":{"reasoning_content":"I should think"}}]}`,
			`{"choices":[{"delta":{"content":"42"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3}}}`,
		)
	})

	stream, err := provider.Stream(context.Background(), v1.Request{
		Messages: []v1.Message{v1.NewMessage(v1.RoleUser, "meaning of life?")},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := collect(t, stream)

	var reasoning strings.Builder
	var usage *v1.Usage
	for _, chunk := range chunks {
		reasoning.WriteString(chunk.Reasoning)
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
	if got := reasoning.String(); got != "I should think" {
		t.Fatalf("reasoning = %q, want %q", got, "I should think")
	}
	if got := joinText(chunks); got != "42" {
		t.Fatalf("text = %q, want 42", got)
	}
	if usage == nil {
		t.Fatal("usage chunk missing")
	}
	want := v1.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18, CachedTokens: 3}
	if *usage != want {
		t.Fatalf("usage = %+v, want %+v", *usage, want)
	}
}

// ④ finish_reason propagation.
func TestStreamFinishReason(t *testing.T) {
	provider := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(w,
			`{"choices":[{"delta":{"content":"partial"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
		)
	})

	stream, err := provider.Stream(context.Background(), v1.Request{
		Messages: []v1.Message{v1.NewMessage(v1.RoleUser, "write a lot")},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := collect(t, stream)
	if got := finishReason(chunks); got != "length" {
		t.Fatalf("finish reason = %q, want length", got)
	}
	// The finish reason must be the last data chunk: nothing after it except io.EOF.
	last := chunks[len(chunks)-1]
	if last.FinishReason != "length" {
		t.Fatalf("last chunk = %+v, want the finish reason", last)
	}
}

// ⑤ Non-2xx responses are returned with the body snippet and are not retried
// when the status is not retryable.
func TestStreamNon2xxError(t *testing.T) {
	var requests atomic.Int32
	provider := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"invalid api key","type":"auth_error"}}`)
	})

	_, err := provider.Stream(context.Background(), v1.Request{
		Messages: []v1.Message{v1.NewMessage(v1.RoleUser, "hi")},
	})
	if err == nil {
		t.Fatal("Stream returned nil error for HTTP 400")
	}
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("error %q does not quote the response body", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("error %q does not carry the status", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1 (400 must not be retried)", got)
	}
}

// ⑥ A retryable 5xx is retried and a later success is returned.
func TestStreamRetryAfterServerError(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempt := attempts.Add(1); attempt == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "temporary failure")
			return
		}
		writeSSE(w, `{"choices":[{"delta":{"content":"recovered"}}]}`)
	}))
	t.Cleanup(server.Close)
	provider, err := New(Config{
		BaseURL:      server.URL,
		APIKey:       "test-key",
		DefaultModel: "test-model",
		HTTPClient:   server.Client(),
		MaxRetries:   2,
		RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stream, err := provider.Stream(context.Background(), v1.Request{
		Messages: []v1.Message{v1.NewMessage(v1.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := collect(t, stream)
	if got := joinText(chunks); got != "recovered" {
		t.Fatalf("text = %q, want recovered", got)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2 (one failure + one retry)", got)
	}
}

// ⑦ Cancelling the context aborts a live stream promptly.
func TestStreamContextCancel(t *testing.T) {
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done() // block until the client cancels
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := provider.Stream(ctx, v1.Request{
		Messages: []v1.Message{v1.NewMessage(v1.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if first.Delta != "first" {
		t.Fatalf("first chunk = %+v, want text first", first)
	}
	cancel()
	if _, err := stream.Recv(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Recv after cancel = %v, want context.Canceled", err)
	}
}

// ⑧ Request mapping: messages (text parts, image parts, tool calls, tool role),
// tools, sampling fields, stream_options and the Authorization header.
func TestStreamRequestMapping(t *testing.T) {
	var (
		mu   sync.Mutex
		auth string
		body map[string]any
	)
	done := make(chan struct{})
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		mu.Lock()
		auth = r.Header.Get("Authorization")
		body = nil
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		mu.Unlock()
		writeSSE(w) // no content; just [DONE]
		close(done)
	})

	temperature := 0.3
	topP := 0.9
	stream, err := provider.Stream(context.Background(), v1.Request{
		Model: "override-model",
		Messages: []v1.Message{
			v1.NewMessage(v1.RoleSystem, "be nice"),
			{Role: v1.RoleUser, Content: []v1.ContentPart{
				v1.TextPart("describe"),
				v1.ImagePart("https://example.com/a.png", "high"),
			}},
			{Role: v1.RoleAssistant, ToolCalls: []v1.ToolCall{
				{ID: "call_9", Name: "weather", Arguments: `{"city":"x"}`},
			}},
			{Role: v1.RoleTool, Content: []v1.ContentPart{v1.TextPart("sunny")}, ToolCallID: "call_9", Name: "weather"},
		},
		Tools: []v1.ToolSpec{{
			Name:        "weather",
			Description: "lookup",
			Parameters:  map[string]any{"type": "object"},
		}},
		Temperature: &temperature,
		TopP:        &topP,
		MaxTokens:   128,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, stream)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if auth != "Bearer test-key" {
		t.Fatalf("Authorization = %q, want Bearer test-key", auth)
	}
	if got := body["model"]; got != "override-model" {
		t.Fatalf("model = %v, want override-model", got)
	}
	if got := body["stream"]; got != true {
		t.Fatalf("stream = %v, want true", got)
	}
	options, ok := body["stream_options"].(map[string]any)
	if !ok || options["include_usage"] != true {
		t.Fatalf("stream_options = %v, want include_usage true", body["stream_options"])
	}
	if got := body["temperature"]; got != 0.3 {
		t.Fatalf("temperature = %v, want 0.3", got)
	}
	if got := body["top_p"]; got != 0.9 {
		t.Fatalf("top_p = %v, want 0.9", got)
	}
	if got := body["max_tokens"]; got != float64(128) {
		t.Fatalf("max_tokens = %v, want 128", got)
	}

	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 4 {
		t.Fatalf("messages = %v, want 4 entries", body["messages"])
	}
	// system
	system, _ := messages[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "be nice" {
		t.Fatalf("system message = %v", system)
	}
	// user with image part
	user, _ := messages[1].(map[string]any)
	parts, ok := user["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("user content = %v, want two parts", user["content"])
	}
	textPart, _ := parts[0].(map[string]any)
	if textPart["type"] != "text" || textPart["text"] != "describe" {
		t.Fatalf("text part = %v", textPart)
	}
	imagePart, _ := parts[1].(map[string]any)
	imageURL, _ := imagePart["image_url"].(map[string]any)
	if imagePart["type"] != "image_url" || imageURL["url"] != "https://example.com/a.png" || imageURL["detail"] != "high" {
		t.Fatalf("image part = %v", imagePart)
	}
	// assistant tool call
	assistant, _ := messages[2].(map[string]any)
	calls, ok := assistant["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("assistant tool_calls = %v", assistant["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	function, _ := call["function"].(map[string]any)
	if call["id"] != "call_9" || call["type"] != "function" || function["name"] != "weather" || function["arguments"] != `{"city":"x"}` {
		t.Fatalf("tool call = %v", call)
	}
	// tool result
	toolMessage, _ := messages[3].(map[string]any)
	if toolMessage["role"] != "tool" || toolMessage["tool_call_id"] != "call_9" || toolMessage["content"] != "sunny" {
		t.Fatalf("tool message = %v", toolMessage)
	}

	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v", body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	toolFunction, _ := tool["function"].(map[string]any)
	if tool["type"] != "function" || toolFunction["name"] != "weather" || toolFunction["description"] != "lookup" {
		t.Fatalf("tool spec = %v", tool)
	}
}

// ⑨ Malformed SSE lines are tolerated: comments, unknown fields, invalid JSON
// and empty data lines must not abort a response that also carries valid data.
func TestStreamMalformedSSELines(t *testing.T) {
	provider := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": keep-alive comment\n\n")
		fmt.Fprint(w, "not-a-field-line\n\n")
		fmt.Fprint(w, "event: ping\nid: 7\nretry: 1000\n")
		fmt.Fprint(w, "data: {this is not json}\n\n")
		fmt.Fprint(w, "data: \n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})

	stream, err := provider.Stream(context.Background(), v1.Request{
		Messages: []v1.Message{v1.NewMessage(v1.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := collect(t, stream)
	if got := joinText(chunks); got != "ok" {
		t.Fatalf("text = %q, want ok (malformed lines must be skipped)", got)
	}
}

// Constructor validation, defaults and the missing-model error.
func TestNewDefaultsAndValidation(t *testing.T) {
	if _, err := New(Config{BaseURL: "://bad"}); err == nil {
		t.Fatal("New accepted a malformed base URL")
	}
	if _, err := New(Config{BaseURL: "ftp://example.com"}); err == nil {
		t.Fatal("New accepted a non-http base URL")
	}

	t.Setenv("OPENAI_API_KEY", "env-key")
	provider, err := New(Config{})
	if err != nil {
		t.Fatalf("New(Config{}): %v", err)
	}
	if provider.baseURL != DefaultBaseURL {
		t.Fatalf("baseURL = %q, want %q", provider.baseURL, DefaultBaseURL)
	}
	if provider.apiKey != "env-key" {
		t.Fatalf("apiKey = %q, want the environment fallback", provider.apiKey)
	}
	if provider.maxRetries != DefaultMaxRetries {
		t.Fatalf("maxRetries = %d, want %d", provider.maxRetries, DefaultMaxRetries)
	}
	if provider.backoff != DefaultRetryBackoff {
		t.Fatalf("backoff = %v, want %v", provider.backoff, DefaultRetryBackoff)
	}
	if provider.Name() != "openai" {
		t.Fatalf("Name() = %q, want openai", provider.Name())
	}

	t.Setenv("RIG_TEST_KEY", "custom-env-key")
	custom, err := New(Config{APIKeyEnv: "RIG_TEST_KEY"})
	if err != nil {
		t.Fatalf("New with APIKeyEnv: %v", err)
	}
	if custom.apiKey != "custom-env-key" {
		t.Fatalf("apiKey = %q, want custom-env-key", custom.apiKey)
	}

	// No model anywhere is rejected before any network I/O.
	if _, err := provider.Stream(context.Background(), v1.Request{}); !errors.Is(err, v1.ErrModelRequired) {
		t.Fatalf("Stream without model = %v, want ErrModelRequired", err)
	}
}

// Retry-After parsing and exponential backoff bounds (unit-level; exercised end
// to end by TestStreamRetryAfterServerError for the plain-backoff path).
func TestRetryAfterAndBackoff(t *testing.T) {
	provider, err := New(Config{RetryBackoff: time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := provider.retryDelay(3, "0"); got != 0 {
		t.Fatalf("Retry-After: 0 -> %v, want 0", got)
	}
	if got := provider.retryDelay(3, "2"); got != 2*time.Second {
		t.Fatalf("Retry-After: 2 -> %v, want 2s", got)
	}
	past := time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)
	if got := provider.retryDelay(3, past); got != 0 {
		t.Fatalf("past HTTP-date Retry-After -> %v, want 0", got)
	}
	if _, ok := parseRetryAfter("not-a-date"); ok {
		t.Fatal("parseRetryAfter accepted garbage")
	}

	defaults, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := defaults.retryDelay(0, ""); got != DefaultRetryBackoff {
		t.Fatalf("attempt 0 backoff = %v, want %v", got, DefaultRetryBackoff)
	}
	if got := defaults.retryDelay(1, ""); got != 2*DefaultRetryBackoff {
		t.Fatalf("attempt 1 backoff = %v, want %v", got, 2*DefaultRetryBackoff)
	}
	if got := defaults.retryDelay(100, ""); got != maxRetryDelay {
		t.Fatalf("capped backoff = %v, want %v", got, maxRetryDelay)
	}
}

// Retries are bounded: a persistently failing endpoint returns the last error
// after MaxRetries+1 attempts.
func TestStreamRetryExhausted(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "still overloaded")
	}))
	t.Cleanup(server.Close)
	provider, err := New(Config{
		BaseURL:      server.URL,
		APIKey:       "test-key",
		DefaultModel: "test-model",
		HTTPClient:   server.Client(),
		MaxRetries:   1,
		RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := provider.Stream(context.Background(), v1.Request{
		Messages: []v1.Message{v1.NewMessage(v1.RoleUser, "hi")},
	}); err == nil || !strings.Contains(err.Error(), "still overloaded") {
		t.Fatalf("Stream error = %v, want the 503 body", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2 (1 initial + 1 retry)", got)
	}
}

// A 2xx response that is not an SSE stream is rejected explicitly: non-streaming
// fallback is not implemented.
func TestStreamNonStreamingRejected(t *testing.T) {
	provider := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"whole answer"}}]}`)
	})
	_, err := provider.Stream(context.Background(), v1.Request{
		Messages: []v1.Message{v1.NewMessage(v1.RoleUser, "hi")},
	})
	if err == nil || !strings.Contains(err.Error(), "non-streaming") {
		t.Fatalf("Stream error = %v, want explicit non-streaming rejection", err)
	}
}

// In-band stream errors surface as Recv errors; a clean connection close without
// [DONE] ends the stream normally.
func TestStreamInBandErrorAndCleanEOF(t *testing.T) {
	t.Run("in-band error", func(t *testing.T) {
		provider := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"part\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"error\":{\"message\":\"quota exceeded\",\"type\":\"rate_limit\"}}\n\n")
		})
		stream, err := provider.Stream(context.Background(), v1.Request{
			Messages: []v1.Message{v1.NewMessage(v1.RoleUser, "hi")},
		})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		if _, err := stream.Recv(); err != nil {
			t.Fatalf("first Recv: %v", err)
		}
		if _, err := stream.Recv(); err == nil || !strings.Contains(err.Error(), "quota exceeded") {
			t.Fatalf("second Recv = %v, want the in-band stream error", err)
		}
	})

	t.Run("clean EOF without done marker", func(t *testing.T) {
		provider := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
		})
		stream, err := provider.Stream(context.Background(), v1.Request{
			Messages: []v1.Message{v1.NewMessage(v1.RoleUser, "hi")},
		})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		if got := joinText(collect(t, stream)); got != "done" {
			t.Fatalf("text = %q, want done", got)
		}
	})
}

// Custom headers are sent; DeepSeek's flat prompt_cache_hit_tokens fallback maps
// to Usage.CachedTokens.
func TestStreamHeadersAndFlatCachedTokens(t *testing.T) {
	var sawAttribution atomic.Bool
	var usage *v1.Usage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Attribution"); got == "zeokeel-tests" {
			sawAttribution.Store(true)
		}
		writeSSE(w,
			`{"choices":[{"delta":{"content":"x"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6,"prompt_cache_hit_tokens":4}}`,
		)
	}))
	t.Cleanup(server.Close)
	provider, err := New(Config{
		BaseURL:      server.URL,
		APIKey:       "test-key",
		DefaultModel: "test-model",
		HTTPClient:   server.Client(),
		Headers:      map[string]string{"X-Attribution": "zeokeel-tests"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := provider.Stream(context.Background(), v1.Request{
		Messages: []v1.Message{v1.NewMessage(v1.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for _, chunk := range collect(t, stream) {
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
	if !sawAttribution.Load() {
		t.Fatal("custom X-Attribution header was not sent")
	}
	if usage == nil || usage.CachedTokens != 4 || usage.TotalTokens != 6 {
		t.Fatalf("usage = %+v, want cached 4 / total 6", usage)
	}
}

// Transport failures before any response are retried too.
func TestStreamTransportRetry(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("ResponseWriter is not a Hijacker")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("Hijack: %v", err)
				return
			}
			_ = conn.Close() // drop the connection before sending headers
			return
		}
		writeSSE(w, `{"choices":[{"delta":{"content":"retried"}}]}`)
	}))
	t.Cleanup(server.Close)
	provider, err := New(Config{
		BaseURL:      server.URL,
		APIKey:       "test-key",
		DefaultModel: "test-model",
		HTTPClient:   server.Client(),
		MaxRetries:   2,
		RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := provider.Stream(context.Background(), v1.Request{
		Messages: []v1.Message{v1.NewMessage(v1.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := joinText(collect(t, stream)); got != "retried" {
		t.Fatalf("text = %q, want retried", got)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

// Extra carries provider-specific top-level fields; reserved fields cannot be
// overridden by Extra.
func TestStreamExtraFields(t *testing.T) {
	var body map[string]any
	done := make(chan struct{})
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeSSE(w)
		close(done)
	})
	if _, err := provider.Stream(context.Background(), v1.Request{
		Model:    "request-model",
		Messages: []v1.Message{v1.NewMessage(v1.RoleUser, "hi")},
		Extra: map[string]any{
			"reasoning_effort": "high",
			"custom_option":    7,
			"model":            "must-not-override",
		},
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	<-done
	if body["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high", body["reasoning_effort"])
	}
	if body["custom_option"] != float64(7) {
		t.Fatalf("custom_option = %v, want 7", body["custom_option"])
	}
	if body["model"] != "request-model" {
		t.Fatalf("model = %v, want request-model (Extra must not override reserved fields)", body["model"])
	}
}

// Cancelling during the retry backoff returns promptly with context.Canceled.
func TestStreamCancelDuringBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "slow down")
	}))
	t.Cleanup(server.Close)
	provider, err := New(Config{
		BaseURL:      server.URL,
		APIKey:       "test-key",
		DefaultModel: "test-model",
		HTTPClient:   server.Client(),
		MaxRetries:   3,
		RetryBackoff: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = provider.Stream(ctx, v1.Request{
		Messages: []v1.Message{v1.NewMessage(v1.RoleUser, "hi")},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancelled Stream took %v, want prompt return", elapsed)
	}
}
