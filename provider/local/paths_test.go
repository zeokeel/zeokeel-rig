package local

// 本文件补齐"分支多但主流程用不到"的路径：多模态内容映射、用量的两种缓存字段、
// Retry-After 的日期形式、传输错误分类、SSE 行格式边界、错误文本渲染、请求构造
// 的失败分支。目的是让 provider/local（三个包共享的实现）在覆盖率地板上留足余量。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

func TestWireContentShapes(t *testing.T) {
	imageURL := "data:image/png;base64,AAA"
	t.Run("空内容", func(t *testing.T) {
		if got := WireContent(rig.Message{Role: rig.RoleUser}); got != "" {
			t.Fatalf("wireContent = %v", got)
		}
	})
	t.Run("纯文本", func(t *testing.T) {
		message := rig.Message{Role: rig.RoleUser, Content: []rig.ContentPart{rig.TextPart("hi")}}
		if got := WireContent(message); got != "hi" {
			t.Fatalf("wireContent = %v", got)
		}
	})
	t.Run("工具角色永不转数组", func(t *testing.T) {
		message := rig.Message{
			Role: rig.RoleTool,
			Content: []rig.ContentPart{
				rig.TextPart("out"),
				rig.ImagePart(imageURL, ""),
			},
		}
		if got := WireContent(message); got != "out" {
			t.Fatalf("wireContent = %v, want 纯文本", got)
		}
	})
	t.Run("图文混合", func(t *testing.T) {
		message := rig.Message{
			Role: rig.RoleUser,
			Content: []rig.ContentPart{
				rig.TextPart("看图"),
				rig.ImagePart(imageURL, "high"),
				{Type: rig.PartText, Text: "尾注"},
			},
		}
		parts, ok := WireContent(message).([]chatContentPart)
		if !ok {
			t.Fatalf("wireContent 应返回分片数组: %T", WireContent(message))
		}
		if len(parts) != 3 {
			t.Fatalf("分片数 = %d", len(parts))
		}
		if parts[0].Type != "text" || parts[0].Text == nil || *parts[0].Text != "看图" {
			t.Fatalf("首个分片 = %+v", parts[0])
		}
		if parts[1].Type != "image_url" || parts[1].ImageURL == nil ||
			parts[1].ImageURL.URL != imageURL || parts[1].ImageURL.Detail != "high" {
			t.Fatalf("图片分片 = %+v", parts[1])
		}
	})
	t.Run("图片 detail 缺省为 auto", func(t *testing.T) {
		message := rig.Message{Role: rig.RoleAssistant, Content: []rig.ContentPart{rig.ImagePart(imageURL, "")}}
		parts := WireContent(message).([]chatContentPart)
		if parts[0].ImageURL == nil || parts[0].ImageURL.Detail != "auto" {
			t.Fatalf("detail = %+v", parts[0].ImageURL)
		}
	})
	t.Run("空图片 URL 被跳过", func(t *testing.T) {
		message := rig.Message{
			Role: rig.RoleUser,
			Content: []rig.ContentPart{
				rig.TextPart("只有文字"),
				{Type: rig.PartImageURL, ImageURL: ""},
			},
		}
		// 唯一的图片分片为空 → 没有可用图片 → 退化成纯文本。
		if got := WireContent(message); got != "只有文字" {
			t.Fatalf("wireContent = %v", got)
		}
	})
	t.Run("未知分片类型被忽略", func(t *testing.T) {
		message := rig.Message{
			Role: rig.RoleUser,
			Content: []rig.ContentPart{
				{Type: "unsupported"},
				rig.ImagePart(imageURL, ""),
			},
		}
		parts, ok := WireContent(message).([]chatContentPart)
		if !ok || len(parts) != 1 || parts[0].Type != "image_url" {
			t.Fatalf("wireContent = %#v", WireContent(message))
		}
	})
}

func TestStreamErrorTextVariants(t *testing.T) {
	cases := map[string]struct {
		err  *streamError
		want string
	}{
		"message+type": {&streamError{Message: "坏了", Type: "server_error"}, "server_error: 坏了"},
		"只有 message":   {&streamError{Message: "坏了"}, "坏了"},
		"只有 type":      {&streamError{Type: "server_error"}, "server_error"},
		"都没有":          {&streamError{}, "stream error"},
	}
	for name, testCase := range cases {
		testCase := testCase
		t.Run(name, func(t *testing.T) {
			if got := testCase.err.text(); got != testCase.want {
				t.Fatalf("text = %q, want %q", got, testCase.want)
			}
		})
	}
	var nilError *streamError
	if got := nilError.text(); got != "" {
		t.Fatalf("nil.text = %q", got)
	}
}

func TestUsageFromWireCachedTokens(t *testing.T) {
	t.Run("OpenAI 嵌套 cached_tokens", func(t *testing.T) {
		usage := usageFromWire(&streamUsage{
			PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
			PromptTokensDetails: &promptTokensDetails{CachedTokens: 7},
		})
		if usage == nil || usage.CachedTokens != 7 || usage.TotalTokens != 15 {
			t.Fatalf("usage = %+v", usage)
		}
	})
	t.Run("DeepSeek 扁平 prompt_cache_hit_tokens", func(t *testing.T) {
		usage := usageFromWire(&streamUsage{PromptTokens: 4, CompletionTokens: 1, PromptCacheHitTokens: 3})
		if usage == nil || usage.CachedTokens != 3 {
			t.Fatalf("usage = %+v", usage)
		}
		if usage.TotalTokens != 5 {
			t.Fatalf("total 缺失时应补齐: %+v", usage)
		}
	})
	t.Run("nil 用量", func(t *testing.T) {
		if usage := usageFromWire(nil); usage != nil {
			t.Fatalf("usage = %+v", usage)
		}
	})
}

func TestParseRetryAfterForms(t *testing.T) {
	cases := []struct {
		value   string
		want    time.Duration
		wantOK  bool
		clamped bool
	}{
		{"", 0, false, false},
		{"3", 3 * time.Second, true, false},
		{"-5", 0, true, false},
		{"0", 0, true, false},
		{"999999", MaxRetryDelay, true, true},
		{"not-a-number", 0, false, false},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.value, func(t *testing.T) {
			delay, ok := ParseRetryAfter(testCase.value)
			if ok != testCase.wantOK {
				t.Fatalf("ok = %v, want %v", ok, testCase.wantOK)
			}
			if ok && !testCase.clamped && delay != testCase.want {
				t.Fatalf("delay = %s, want %s", delay, testCase.want)
			}
			if testCase.clamped && delay != MaxRetryDelay {
				t.Fatalf("delay = %s, want 上限 %s", delay, MaxRetryDelay)
			}
		})
	}
	// HTTP 日期形式（未来 → 正延迟；过去 → 0）。
	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	if delay, ok := ParseRetryAfter(future); !ok || delay <= 0 {
		t.Fatalf("未来日期 delay = %s, ok=%v", delay, ok)
	}
	past := time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)
	if delay, ok := ParseRetryAfter(past); !ok || delay != 0 {
		t.Fatalf("过去日期 delay = %s, ok=%v", delay, ok)
	}
}

func TestRetryableTransportError(t *testing.T) {
	urlErr := &url.Error{Op: "Post", URL: "http://x", Err: errors.New("connection refused")}
	if !retryableTransportError(urlErr) {
		t.Fatal("url.Error 应可重试")
	}
	if retryableTransportError(context.Canceled) {
		t.Fatal("context.Canceled 不该重试")
	}
	if retryableTransportError(context.DeadlineExceeded) {
		t.Fatal("DeadlineExceeded 不该重试")
	}
	if retryableTransportError(errors.New("boom")) {
		t.Fatal("普通错误不该重试")
	}
}

func TestTransportNameDefaultsToLocal(t *testing.T) {
	transport := &Transport{Client: &http.Client{}}
	if transport.name() != DefaultName {
		t.Fatalf("name = %q, want %q", transport.name(), DefaultName)
	}
	if transport.retryDelay(0, "") != 0 {
		t.Fatal("Backoff<=0 时延迟应为 0")
	}
	transport.Backoff = time.Second
	if got := transport.retryDelay(3, ""); got <= 0 {
		t.Fatalf("退避延迟 = %s", got)
	}
	if got := transport.retryDelay(100, ""); got != MaxRetryDelay {
		t.Fatalf("长退避应被夹到上限: %s", got)
	}
}

func TestStatusErrorWithoutBody(t *testing.T) {
	resp := &http.Response{StatusCode: 500, Status: "500 Internal Server Error"}
	err := statusError("local", resp, nil)
	if !strings.Contains(err.Error(), "500 Internal Server Error") {
		t.Fatalf("err = %v", err)
	}
	if strings.HasSuffix(err.Error(), ": ") {
		t.Fatalf("空响应体不该留下悬空冒号: %v", err)
	}
	// 带响应体时引用一小段。
	err = statusError("local", resp, []byte("  {\"error\":\"x\"}  "))
	if !strings.Contains(err.Error(), `{"error":"x"}`) {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildRequestRejectsNamelessTool(t *testing.T) {
	provider, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.buildRequest(rig.Request{
		Model:    "m",
		Messages: []rig.Message{rig.NewMessage(rig.RoleUser, "hi")},
		Tools:    []rig.ToolSpec{{Name: "  "}},
	})
	if err == nil || !strings.Contains(err.Error(), "tool with empty name") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildRequestDefaultsAndEmptyToolArguments(t *testing.T) {
	provider, err := New(Config{DefaultModel: "default-model"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	raw, err := provider.buildRequest(rig.Request{
		Messages: []rig.Message{{
			Role:      rig.RoleAssistant,
			ToolCalls: []rig.ToolCall{{ID: "c1", Name: "echo", Arguments: "   "}},
		}},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if body["model"] != "default-model" {
		t.Fatalf("model = %v（应回退到 DefaultModel）", body["model"])
	}
	messages := body["messages"].([]any)
	first := messages[0].(map[string]any)
	calls := first["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	function := call["function"].(map[string]any)
	if function["arguments"] != "{}" {
		t.Fatalf("空参数应补成 {}: %v", function["arguments"])
	}
}

func TestMergeExtraEncodingError(t *testing.T) {
	provider, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 通道类型无法编码成 JSON → 必须报错，而不是静默丢弃。
	_, err = provider.buildRequest(rig.Request{
		Model:    "m",
		Messages: []rig.Message{rig.NewMessage(rig.RoleUser, "hi")},
		Extra:    map[string]any{"bad": make(chan int)},
	})
	if err == nil || !strings.Contains(err.Error(), "encode extra field") {
		t.Fatalf("err = %v", err)
	}
}

func TestNormalizePathVariants(t *testing.T) {
	cases := map[string]string{
		"":             DefaultChatPath,
		"/chat":        "/chat",
		"chat":         "/chat",
		"  /trimmed  ": "/trimmed",
	}
	for input, want := range cases {
		if got := normalizePath(input, DefaultChatPath); got != want {
			t.Fatalf("normalizePath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDoGivesUpAfterRetries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "always down", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	transport := &Transport{Client: server.Client(), MaxRetries: 2, Backoff: time.Millisecond}
	resp, err := transport.Do(context.Background(), http.MethodGet, server.URL, nil, nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d（重试耗尽后应返回最后一次响应）", resp.StatusCode)
	}
}

// TestDoRetriesTransportFailures 覆盖"连接层失败后重试、耗尽后返回错误"。
func TestDoRetriesTransportFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close() // 端口已关闭：连接被拒
	transport := &Transport{Client: &http.Client{}, MaxRetries: 2, Backoff: time.Millisecond, Name: "probe"}
	_, err := transport.Do(context.Background(), http.MethodGet, url, nil, nil)
	if err == nil {
		t.Fatal("连接被拒必须返回错误")
	}
	if !strings.HasPrefix(err.Error(), "probe:") {
		t.Fatalf("错误前缀 = %v", err)
	}
}

func TestGetBodyRejectsHugeAndBadStatus(t *testing.T) {
	t.Run("非 2xx", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusForbidden)
		}))
		t.Cleanup(server.Close)
		transport := &Transport{Client: server.Client(), MaxRetries: -1}
		if _, err := transport.GetBody(context.Background(), server.URL, nil); err == nil ||
			!strings.Contains(err.Error(), "403") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("读取失败", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, "short") // 少写 → 客户端读到 unexpected EOF
		}))
		t.Cleanup(server.Close)
		transport := &Transport{Client: server.Client(), MaxRetries: -1}
		if _, err := transport.GetBody(context.Background(), server.URL, nil); err == nil ||
			!strings.Contains(err.Error(), "read response") {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestStreamReadErrorMidway 覆盖读流出错（连接被截断）的分支。
func TestStreamReadErrorMidway(t *testing.T) {
	provider := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var lastErr error
	for {
		_, recvErr := stream.Recv()
		if recvErr != nil {
			lastErr = recvErr
			break
		}
	}
	if lastErr == nil || !strings.Contains(lastErr.Error(), "read stream") {
		t.Fatalf("err = %v, want 读流错误", lastErr)
	}
}

func TestStreamCloseIsIdempotent(t *testing.T) {
	stream := streamFrom(t, nil, true, `{"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`)
	collect(t, stream) // 读到 EOF 时会 close 一次
	closer, ok := stream.(io.Closer)
	if !ok {
		t.Fatal("stream 必须实现 io.Closer")
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("再次 Close: %v", err)
	}
}

func TestSSEDataEdgeCases(t *testing.T) {
	cases := map[string]struct {
		line string
		want string
		ok   bool
	}{
		"标准":   {"data: x", "x", true},
		"无空格":  {"data:x", "x", true},
		"空值":   {"data:", "", false},
		"只有空白": {"data:   ", "", false},
		"注释":   {": ping", "", false},
		"其它字段": {"event: message", "", false},
		"空行":   {"", "", false},
		"CRLF": {"data: y\r", "y", true},
		"无冒号":  {"data", "", false},
	}
	for name, testCase := range cases {
		testCase := testCase
		t.Run(name, func(t *testing.T) {
			got, ok := SSEData(testCase.line)
			if ok != testCase.ok || got != testCase.want {
				t.Fatalf("SSEData(%q) = (%q,%v), want (%q,%v)", testCase.line, got, ok, testCase.want, testCase.ok)
			}
		})
	}
}

func TestStreamInBandErrorVariants(t *testing.T) {
	cases := map[string]string{
		"只有 message": `{"error":{"message":"模型未加载"}}`,
		"只有 type":    `{"error":{"type":"server_error"}}`,
	}
	for name, event := range cases {
		event := event
		t.Run(name, func(t *testing.T) {
			provider := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+event+"\n\n")
			})
			stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			_, recvErr := stream.Recv()
			if recvErr == nil || !strings.Contains(recvErr.Error(), "stream error") {
				t.Fatalf("err = %v", recvErr)
			}
		})
	}
}

// TestStreamRequireDoneWithTerminatorOnly 覆盖"只有 [DONE]、没有 finish_reason"的正常结束。
func TestStreamRequireDoneWithTerminatorOnly(t *testing.T) {
	stream := streamFrom(t, func(cfg *Config) { cfg.Quirks.RequireDone = true }, true,
		`{"choices":[{"delta":{"content":"x"}}]}`,
	)
	if chunks := collect(t, stream); len(chunks) != 1 || chunks[0].Delta != "x" {
		t.Fatalf("chunks = %+v", chunks)
	}
}

// TestStreamEmptyChoicesAndNilFinish 覆盖 choices 为空、finish_reason 为 null 的事件。
func TestStreamEmptyChoicesAndNilFinish(t *testing.T) {
	stream := streamFrom(t, nil, true,
		`{"choices":[]}`,
		`{"choices":[{"delta":{"content":"a"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"b"},"finish_reason":"  stop  "}]}`,
	)
	chunks := collect(t, stream)
	if joinText(chunks) != "ab" {
		t.Fatalf("text = %q", joinText(chunks))
	}
	if got := finishOf(chunks); got != "stop" {
		t.Fatalf("finish = %q（首尾空白应被归一化）", got)
	}
}

// TestRequestHeadersOverride 覆盖用户头部覆盖 SDK 默认头。
func TestRequestHeadersOverride(t *testing.T) {
	var sawUA atomic1
	provider := newTestProvider(t, func(cfg *Config) {
		cfg.Headers = map[string]string{"User-Agent": "custom-agent", "X-Tenant": "acme"}
	}, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "custom-agent" && r.Header.Get("X-Tenant") == "acme" {
			sawUA.ok()
		}
		writeSSE(w, true, `{"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, stream)
	if !sawUA.seen {
		t.Fatal("用户头部没有覆盖 SDK 默认头")
	}
}

// atomic1 是一个最小的"是否被设置"标记（避免为一行断言引入原子类型噪音）。
type atomic1 struct{ seen bool }

func (a *atomic1) ok() { a.seen = true }

var _ = bytes.MinRead
