package web_test

// Tests for web_fetch: httptest servers cover plain text, HTML extraction,
// redirects, HTTP errors, timeouts, response/output caps, policy and context
// cancellation.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/web"
)

// invoke runs a tool call and requires no Go error.
func invoke(t *testing.T, tool rig.Tool, arguments string) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "call-1", Name: tool.Name(), Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", tool.Name(), err)
	}
	return result
}

// invokeError requires an IsError result and returns its text.
func invokeError(t *testing.T, tool rig.Tool, arguments string) string {
	t.Helper()
	result := invoke(t, tool, arguments)
	if !result.IsError {
		t.Fatalf("%s: result = %q, want IsError", tool.Name(), result.Content)
	}
	return result.Content
}

func requireContains(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Fatalf("text does not contain %q:\n%s", fragment, text)
		}
	}
}

func requireNotContains(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if strings.Contains(text, fragment) {
			t.Fatalf("text unexpectedly contains %q:\n%s", fragment, text)
		}
	}
}

func urlArgument(t *testing.T, rawURL string) string {
	t.Helper()
	return fmt.Sprintf(`{"url":%q}`, rawURL)
}

func TestFetchPlainTextAndUserAgent(t *testing.T) {
	var userAgent, method atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		userAgent.Store(request.UserAgent())
		method.Store(request.Method)
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(writer, "hello plain\nworld")
	}))
	defer server.Close()

	result := invoke(t, web.NewFetcher(nil), urlArgument(t, server.URL))
	if result.IsError {
		t.Fatalf("fetch = %q", result.Content)
	}
	if result.Content != "hello plain\nworld" {
		t.Fatalf("fetch content = %q, want the body as text", result.Content)
	}
	if got, _ := userAgent.Load().(string); got != web.DefaultUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, web.DefaultUserAgent)
	}
	if got, _ := method.Load().(string); got != http.MethodGet {
		t.Fatalf("method = %q, want GET", got)
	}
}

func TestFetchHTMLExtraction(t *testing.T) {
	page := `<!doctype html>
<html><head><title>Example</title><style>.hidden{color:red}</style></head>
<body>
  <h1>Hello &amp; welcome</h1>
  <p>First   paragraph with a
     <a href="/link">link text</a>.</p>
  <script>alert('bad')</script>
  <div>Second paragraph &lt;tag&gt; &#39;quoted&#39;.</div>
</body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(writer, page)
	}))
	defer server.Close()

	result := invoke(t, web.NewFetcher(nil), urlArgument(t, server.URL))
	if result.IsError {
		t.Fatalf("fetch = %q", result.Content)
	}
	requireContains(t, result.Content, "Hello & welcome", "First paragraph", "link text", "Second paragraph", "<tag>", "'quoted'")
	requireNotContains(t, result.Content, "alert(", ".hidden{", "<p>", "<h1>")

	// format=html returns the raw markup.
	result = invoke(t, web.NewFetcher(nil), fmt.Sprintf(`{"url":%q,"format":"html"}`, server.URL))
	if result.IsError {
		t.Fatalf("fetch(html) = %q", result.Content)
	}
	requireContains(t, result.Content, "<h1>Hello &amp; welcome</h1>", "<script>")
}

func TestFetchFollowsRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		io.WriteString(writer, "redirected destination")
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hits.Add(1)
		switch request.URL.Path {
		case "/one":
			http.Redirect(writer, request, "/two", http.StatusFound)
		case "/two":
			http.Redirect(writer, request, target.URL, http.StatusMovedPermanently)
		default:
			io.WriteString(writer, "unexpected")
		}
	}))
	defer server.Close()

	result := invoke(t, web.NewFetcher(nil), urlArgument(t, server.URL+"/one"))
	if result.IsError {
		t.Fatalf("fetch(redirect) = %q", result.Content)
	}
	requireContains(t, result.Content, "redirected destination")
	if hits.Load() != 2 {
		t.Fatalf("redirect handler hits = %d, want 2", hits.Load())
	}
}

// redirectChain serves /r/<n>, redirecting until n equals the final hop count.
func redirectChain(t *testing.T, maxHops int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var hop int
		if _, err := fmt.Sscanf(request.URL.Path, "/r/%d", &hop); err != nil {
			http.Error(writer, "bad path", http.StatusBadRequest)
			return
		}
		if hop >= maxHops {
			io.WriteString(writer, "done")
			return
		}
		http.Redirect(writer, request, fmt.Sprintf("/r/%d", hop+1), http.StatusFound)
	}))
}

func TestFetchFollowsFiveRedirects(t *testing.T) {
	server := redirectChain(t, 5) // 5 redirect hops, then "done"
	defer server.Close()
	result := invoke(t, web.NewFetcher(nil), urlArgument(t, server.URL+"/r/0"))
	if result.IsError || result.Content != "done" {
		t.Fatalf("5-redirect fetch = %q (IsError=%v), want done", result.Content, result.IsError)
	}
}

func TestFetchRedirectLimit(t *testing.T) {
	server := redirectChain(t, 10) // more hops than allowed
	defer server.Close()
	text := invokeError(t, web.NewFetcher(nil), urlArgument(t, server.URL+"/r/0"))
	requireContains(t, text, "redirect")
}

func TestFetchHTTPErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html")
		writer.WriteHeader(http.StatusNotFound)
		io.WriteString(writer, "<html><body><p>no such page</p></body></html>")
	}))
	defer server.Close()

	text := invokeError(t, web.NewFetcher(nil), urlArgument(t, server.URL))
	requireContains(t, text, "404", "no such page")
}

func TestFetchTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		<-release
		io.WriteString(writer, "late")
	}))
	defer server.Close()
	defer close(release)

	started := time.Now()
	text := invokeError(t, web.NewHTTPFetcher(web.WithTimeout(50*time.Millisecond)), urlArgument(t, server.URL))
	elapsed := time.Since(started)
	requireContains(t, text, "timed out")
	if elapsed > 2*time.Second {
		t.Fatalf("timeout took %s, want it near 50ms", elapsed)
	}
}

func TestFetchResponseBodyTruncation(t *testing.T) {
	large := strings.Repeat("a", 5000)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		io.WriteString(writer, large)
	}))
	defer server.Close()

	result := invoke(t, web.NewHTTPFetcher(web.WithMaxBytes(1024)), urlArgument(t, server.URL))
	if result.IsError {
		t.Fatalf("fetch = %q", result.Content)
	}
	requireContains(t, result.Content, "truncated")
	if len(result.Content) > 1024+200 {
		t.Fatalf("truncated body length = %d, want bounded", len(result.Content))
	}
}

func TestFetchOutputCharacterCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		io.WriteString(writer, strings.Repeat("x", 1000))
	}))
	defer server.Close()

	result := invoke(t, web.NewFetcher(nil), fmt.Sprintf(`{"url":%q,"maxChars":50}`, server.URL))
	if result.IsError {
		t.Fatalf("fetch = %q", result.Content)
	}
	requireContains(t, result.Content, "truncated")
	beforeNotice := strings.SplitN(result.Content, "\n[truncated:", 2)[0]
	if len(beforeNotice) != 50 {
		t.Fatalf("output before notice = %d chars, want 50", len(beforeNotice))
	}
}

func TestFetchRejectsInvalidURLs(t *testing.T) {
	tool := web.NewFetcher(nil)
	cases := map[string]string{
		"missing": `{}`,
		"empty":   `{"url":""}`,
		"scheme":  `{"url":"ftp://example.com/file"}`,
		"file":    `{"url":"file:///etc/passwd"}`,
		"bare":    `{"url":"example.com"}`,
		"no host": `{"url":"http://"}`,
		"format":  `{"url":"https://example.com","format":"markdown"}`,
	}
	for name, arguments := range cases {
		t.Run(name, func(t *testing.T) {
			text := invokeError(t, tool, arguments)
			if name == "format" {
				requireContains(t, text, "format")
			} else {
				requireContains(t, text, "web_fetch:")
			}
		})
	}
}

func TestFetchPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect" {
			http.Redirect(writer, request, "/blocked", http.StatusFound)
			return
		}
		io.WriteString(writer, "allowed")
	}))
	defer server.Close()

	// The initial URL is checked before the request.
	denyAll := web.PolicyFunc(func(*url.URL) error { return errors.New("denied by test policy") })
	text := invokeError(t, web.NewFetcher(denyAll), urlArgument(t, server.URL))
	requireContains(t, text, "blocked by policy", "denied by test policy")

	// Redirect hops are checked too.
	privateRedirect := web.PolicyFunc(func(u *url.URL) error {
		if strings.Contains(u.Path, "/blocked") {
			return errors.New("redirect target is private")
		}
		return nil
	})
	text = invokeError(t, web.NewHTTPFetcher(web.WithPolicy(privateRedirect), web.WithAllowPrivateHosts()), urlArgument(t, server.URL+"/redirect"))
	requireContains(t, text, "policy")

	// A policy that allows the URL leaves the fetch working.
	allowLocal := web.PolicyFunc(func(u *url.URL) error {
		if u.Host != strings.TrimPrefix(server.URL, "http://") {
			return errors.New("not the test server")
		}
		return nil
	})
	result := invoke(t, web.NewHTTPFetcher(web.WithPolicy(allowLocal), web.WithAllowPrivateHosts()), urlArgument(t, server.URL))
	if result.IsError || result.Content != "allowed" {
		t.Fatalf("policy-allowed fetch = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestFetchContextCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		<-release
		io.WriteString(writer, "late")
	}))
	defer server.Close()
	defer close(release)

	// Already-cancelled context: error, not a model tool result.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := web.NewFetcher(nil).Invoke(ctx, rig.ToolCall{Arguments: urlArgument(t, server.URL)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled fetch error = %v, want context.Canceled", err)
	}

	// Cancel while the request is in flight.
	ctx, cancel = context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err = web.NewFetcher(nil).Invoke(ctx, rig.ToolCall{Arguments: urlArgument(t, server.URL)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight cancelled fetch error = %v, want context.Canceled", err)
	}
}

func TestFetchOptionsAndSchema(t *testing.T) {
	var userAgent atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		userAgent.Store(request.UserAgent())
		io.WriteString(writer, "ok")
	}))
	defer server.Close()

	tool := web.NewHTTPFetcher(web.WithUserAgent("custom-agent/1.0"), web.WithMaxChars(10))
	if tool.Name() != "web_fetch" {
		t.Fatalf("Name = %q, want web_fetch", tool.Name())
	}
	result := invoke(t, tool, urlArgument(t, server.URL))
	if result.IsError {
		t.Fatalf("fetch = %q", result.Content)
	}
	if got, _ := userAgent.Load().(string); got != "custom-agent/1.0" {
		t.Fatalf("User-Agent = %q, want custom-agent/1.0", got)
	}
	schema := tool.Schema()
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v", schema["type"])
	}
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "url" {
		t.Fatalf("schema required = %v, want [url]", required)
	}
	properties, _ := schema["properties"].(map[string]any)
	format, _ := properties["format"].(map[string]any)
	if fmt.Sprint(format["enum"]) != "[text html]" {
		t.Fatalf("format enum = %v, want [text html]", format["enum"])
	}
}

func TestFetchConcurrent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		io.WriteString(writer, "concurrent")
	}))
	defer server.Close()
	tool := web.NewFetcher(nil)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: urlArgument(t, server.URL)})
			if err != nil || result.IsError || result.Content != "concurrent" {
				t.Errorf("concurrent fetch: result=%q err=%v", result.Content, err)
			}
		}()
	}
	wg.Wait()
}
