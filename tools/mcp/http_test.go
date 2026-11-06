package mcp

// Streamable HTTP transport tests: httptest servers speak JSON-RPC over POST,
// answering with application/json or text/event-stream. No network or external
// process is involved.

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

	"github.com/zeokeel/zeokeel-rig"
)

const (
	httpTestSession  = "sess-http-1"
	httpTestProtocol = "2025-06-18"
)

// httpRPCRequest is one request decoded by the fake HTTP MCP server.
type httpRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// httpMCPHooks customizes the fake server. Nil hooks fall back to the defaults
// (standard initialize result, accepted initialized notification, and a
// "method not found" error for any other method).
type httpMCPHooks struct {
	onInitialize  func(msg httpRPCRequest, w http.ResponseWriter, r *http.Request)
	onInitialized func(msg httpRPCRequest, w http.ResponseWriter, r *http.Request)
	onRequest     func(msg httpRPCRequest, w http.ResponseWriter, r *http.Request)
}

// newHTTPTestServer starts an httptest server and registers its cleanup.
func newHTTPTestServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

// newHTTPMCPHandler builds the fake MCP HTTP endpoint.
func newHTTPMCPHandler(t *testing.T, hooks httpMCPHooks) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		msg := readHTTPRPCRequest(t, r)
		switch msg.Method {
		case "initialize":
			if hooks.onInitialize != nil {
				hooks.onInitialize(msg, w, r)
				return
			}
			if got := r.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
				t.Errorf("initialize Content-Type = %q", got)
			}
			accept := r.Header.Get("Accept")
			if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
				t.Errorf("initialize Accept = %q", accept)
			}
			w.Header().Set(headerSessionID, httpTestSession)
			writeHTTPRPCResult(t, w, msg.ID, httpInitializeResult())
		case "notifications/initialized":
			if hooks.onInitialized != nil {
				hooks.onInitialized(msg, w, r)
				return
			}
			if got := r.Header.Get(headerSessionID); got != httpTestSession {
				t.Errorf("initialized session header = %q, want %q", got, httpTestSession)
			}
			if got := r.Header.Get(headerProtocolVersion); got != httpTestProtocol {
				t.Errorf("initialized protocol header = %q, want %q", got, httpTestProtocol)
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			if hooks.onRequest == nil {
				t.Errorf("unexpected method %q", msg.Method)
				writeHTTPRPCError(t, w, msg.ID, -32601, "method not found", nil)
				return
			}
			hooks.onRequest(msg, w, r)
		}
	}
}

// readHTTPRPCRequest decodes one request body.
func readHTTPRPCRequest(t *testing.T, r *http.Request) httpRPCRequest {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var msg httpRPCRequest
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("decode request body %q: %v", body, err)
	}
	if msg.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", msg.JSONRPC)
	}
	return msg
}

// writeHTTPRPCResult writes one JSON-RPC success response.
func writeHTTPRPCResult(t *testing.T, w http.ResponseWriter, id json.RawMessage, result any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}); err != nil {
		t.Errorf("encode result: %v", err)
	}
}

// writeHTTPRPCError writes one JSON-RPC error response.
func writeHTTPRPCError(t *testing.T, w http.ResponseWriter, id json.RawMessage, code int, message string, data any) {
	t.Helper()
	body := map[string]any{"code": code, "message": message}
	if data != nil {
		body["data"] = data
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   body,
	}); err != nil {
		t.Errorf("encode error: %v", err)
	}
}

// writeHTTPSSE streams payloads as SSE data events.
func writeHTTPSSE(t *testing.T, w http.ResponseWriter, payloads ...any) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Errorf("ResponseWriter is not a Flusher")
		return
	}
	w.WriteHeader(http.StatusOK)
	for _, payload := range payloads {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Errorf("encode SSE payload: %v", err)
			return
		}
		if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", data); err != nil {
			t.Errorf("write SSE event: %v", err)
			return
		}
		flusher.Flush()
	}
}

// httpInitializeResult is the standard initialize result served by the tests.
func httpInitializeResult() map[string]any {
	return map[string]any{
		"protocolVersion": httpTestProtocol,
		"capabilities":    map[string]any{"tools": map[string]any{}, "resources": map[string]any{}, "prompts": map[string]any{}},
		"serverInfo":      map[string]any{"name": "http-fake", "version": "9.9.9"},
	}
}

// httpToolJSON builds one tools/list tool entry.
func httpToolJSON(name, description string) map[string]any {
	return map[string]any{
		"name":        name,
		"description": description,
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

// startHTTPClient starts an HTTP-backed client against url.
func startHTTPClient(t *testing.T, url string, tune func(*HTTPConfig)) *Client {
	t.Helper()
	cfg := HTTPConfig{URL: url, Timeout: 5 * time.Second}
	if tune != nil {
		tune(&cfg)
	}
	client, err := StartHTTP(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartHTTP: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestHTTPInitialize verifies the POST handshake, accept/content-type headers,
// the session id header, and the initialized notification.
func TestHTTPInitialize(t *testing.T) {
	var initialized atomic.Int32
	var initRequest httpRPCRequest
	url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{
		onInitialize: func(msg httpRPCRequest, w http.ResponseWriter, r *http.Request) {
			initRequest = msg
			var params struct {
				ProtocolVersion string `json:"protocolVersion"`
				ClientInfo      struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				} `json:"clientInfo"`
			}
			if err := json.Unmarshal(msg.Params, &params); err != nil {
				t.Errorf("initialize params: %v", err)
			}
			if params.ProtocolVersion != httpTestProtocol {
				t.Errorf("requested protocol = %q, want %q", params.ProtocolVersion, httpTestProtocol)
			}
			if params.ClientInfo.Name != DefaultClientName {
				t.Errorf("clientInfo.name = %q, want %q", params.ClientInfo.Name, DefaultClientName)
			}
			w.Header().Set(headerSessionID, httpTestSession)
			writeHTTPRPCResult(t, w, msg.ID, httpInitializeResult())
		},
		onInitialized: func(_ httpRPCRequest, w http.ResponseWriter, r *http.Request) {
			initialized.Add(1)
			if got := r.Header.Get(headerSessionID); got != httpTestSession {
				t.Errorf("initialized session header = %q, want %q", got, httpTestSession)
			}
			if got := r.Header.Get(headerProtocolVersion); got != httpTestProtocol {
				t.Errorf("initialized protocol header = %q, want %q", got, httpTestProtocol)
			}
			w.WriteHeader(http.StatusAccepted)
		},
	}))

	client := startHTTPClient(t, url, nil)
	if initRequest.Method != "initialize" {
		t.Fatalf("first request method = %q", initRequest.Method)
	}
	if got := client.ServerName(); got != "http-fake" {
		t.Fatalf("ServerName() = %q, want http-fake", got)
	}
	if got := client.ServerVersion(); got != "9.9.9" {
		t.Fatalf("ServerVersion() = %q, want 9.9.9", got)
	}
	if got := client.ProtocolVersion(); got != httpTestProtocol {
		t.Fatalf("ProtocolVersion() = %q, want %q", got, httpTestProtocol)
	}
	if got := client.SessionID(); got != httpTestSession {
		t.Fatalf("SessionID() = %q, want %q", got, httpTestSession)
	}
	if got := initialized.Load(); got != 1 {
		t.Fatalf("initialized notifications = %d, want 1", got)
	}
}

// TestHTTPListTools verifies a JSON response and session/protocol headers on
// subsequent requests, plus custom request headers.
func TestHTTPListTools(t *testing.T) {
	var listHeaders http.Header
	url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{
		onRequest: func(msg httpRPCRequest, w http.ResponseWriter, r *http.Request) {
			if msg.Method != "tools/list" {
				t.Errorf("method = %q", msg.Method)
			}
			listHeaders = r.Header.Clone()
			writeHTTPRPCResult(t, w, msg.ID, map[string]any{"tools": []any{
				httpToolJSON("echo", "Echo text back"),
				httpToolJSON("slow", "Slow tool"),
			}})
		},
	}))
	client := startHTTPClient(t, url, func(cfg *HTTPConfig) {
		cfg.Headers = map[string]string{"Authorization": "Bearer test-token", "X-Rig-Test": "1"}
	})
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "echo" || tools[1].Name != "slow" {
		t.Fatalf("tools = %+v", tools)
	}
	if got := listHeaders.Get(headerSessionID); got != httpTestSession {
		t.Fatalf("session header = %q, want %q", got, httpTestSession)
	}
	if got := listHeaders.Get(headerProtocolVersion); got != httpTestProtocol {
		t.Fatalf("protocol header = %q, want %q", got, httpTestProtocol)
	}
	if got := listHeaders.Get("Authorization"); got != "Bearer test-token" {
		t.Fatalf("Authorization = %q, want the configured custom header", got)
	}
	if got := listHeaders.Get("X-Rig-Test"); got != "1" {
		t.Fatalf("X-Rig-Test = %q", got)
	}
	if got := listHeaders.Get("Accept"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Accept = %q, want SSE accepted", got)
	}
}

// TestHTTPCallToolSSE verifies an SSE response containing unrelated frames and
// the matching result frame.
func TestHTTPCallToolSSE(t *testing.T) {
	url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{
		onRequest: func(msg httpRPCRequest, w http.ResponseWriter, _ *http.Request) {
			if msg.Method != "tools/call" {
				t.Errorf("method = %q", msg.Method)
			}
			writeHTTPSSE(t, w,
				// A notification frame and a response for another id must be ignored.
				map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"level": "info"}},
				map[string]any{"jsonrpc": "2.0", "id": 999999, "result": map[string]any{}},
				map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
					"content": []any{map[string]any{"type": "text", "text": "sse hello"}},
				}},
			)
		},
	}))
	client := startHTTPClient(t, url, nil)
	result, err := client.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"x"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError || len(result.Content) != 1 || result.Content[0].Text != "sse hello" {
		t.Fatalf("result = %+v, want one text content sse hello", result)
	}
}

// TestHTTPErrorStatus verifies non-2xx responses quote the status and body.
func TestHTTPErrorStatus(t *testing.T) {
	url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{
		onRequest: func(_ httpRPCRequest, w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "upstream exploded")
		},
	}))
	client := startHTTPClient(t, url, nil)
	_, err := client.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools returned nil error for HTTP 500")
	}
	for _, want := range []string{"500", "upstream exploded"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

// TestHTTPJSONRPCError verifies server errors are surfaced as *RPCError both in
// JSON and SSE responses.
func TestHTTPJSONRPCError(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{
			onRequest: func(msg httpRPCRequest, w http.ResponseWriter, _ *http.Request) {
				writeHTTPRPCError(t, w, msg.ID, -32000, "remote exploded", map[string]any{"detail": "test"})
			},
		}))
		client := startHTTPClient(t, url, nil)
		_, err := client.CallTool(context.Background(), "echo", nil)
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != -32000 || rpcErr.Message != "remote exploded" {
			t.Fatalf("CallTool error = %v, want RPCError -32000", err)
		}
		if !strings.Contains(err.Error(), "test") {
			t.Fatalf("RPCError text = %q, want the data payload", err)
		}
	})

	t.Run("sse", func(t *testing.T) {
		url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{
			onRequest: func(msg httpRPCRequest, w http.ResponseWriter, _ *http.Request) {
				writeHTTPSSE(t, w, map[string]any{
					"jsonrpc": "2.0",
					"id":      msg.ID,
					"error":   map[string]any{"code": -32001, "message": "stream exploded"},
				})
			},
		}))
		client := startHTTPClient(t, url, nil)
		_, err := client.CallTool(context.Background(), "echo", nil)
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != -32001 || rpcErr.Message != "stream exploded" {
			t.Fatalf("CallTool error = %v, want RPCError -32001", err)
		}
	})
}

// TestHTTPSessionExpired verifies 404/410 map to ErrSessionExpired.
func TestHTTPSessionExpired(t *testing.T) {
	url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{
		onRequest: func(_ httpRPCRequest, w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "session not found")
		},
	}))
	client := startHTTPClient(t, url, nil)
	_, err := client.ListTools(context.Background())
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("ListTools error = %v, want ErrSessionExpired", err)
	}
	for _, want := range []string{"404", "session not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

// TestHTTPContextCancel verifies a blocked request is cancelled through ctx.
func TestHTTPContextCancel(t *testing.T) {
	url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{
		onRequest: func(_ httpRPCRequest, _ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		},
	}))
	client := startHTTPClient(t, url, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := client.ListTools(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ListTools error = %v, want context.DeadlineExceeded", err)
	}
}

// TestHTTPClientTimeout verifies Config.Timeout bounds one request even when
// the caller context has no deadline.
func TestHTTPClientTimeout(t *testing.T) {
	url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{
		onRequest: func(_ httpRPCRequest, _ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		},
	}))
	client := startHTTPClient(t, url, func(cfg *HTTPConfig) { cfg.Timeout = 100 * time.Millisecond })
	_, err := client.ListTools(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ListTools error = %v, want context.DeadlineExceeded", err)
	}
}

// TestHTTPCloseIdempotent verifies Close is idempotent and later calls fail.
func TestHTTPCloseIdempotent(t *testing.T) {
	url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{}))
	client := startHTTPClient(t, url, nil)
	for i := 0; i < 3; i++ {
		if err := client.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
	if _, err := client.ListTools(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("ListTools after Close = %v, want ErrClosed", err)
	}
	if err := client.Ping(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Ping after Close = %v, want ErrClosed", err)
	}
	if got := client.SessionID(); got != httpTestSession {
		t.Fatalf("SessionID after Close = %q, want it preserved", got)
	}
}

// TestHTTPResourcesAndPrompts verifies the resources/ and prompts/ subset over
// HTTP.
func TestHTTPResourcesAndPrompts(t *testing.T) {
	url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{
		onRequest: func(msg httpRPCRequest, w http.ResponseWriter, _ *http.Request) {
			switch msg.Method {
			case "resources/list":
				writeHTTPRPCResult(t, w, msg.ID, map[string]any{"resources": []any{
					map[string]any{"uri": "file:///a.txt", "name": "a", "description": "A file", "mimeType": "text/plain"},
				}})
			case "prompts/get":
				writeHTTPRPCResult(t, w, msg.ID, map[string]any{"messages": []any{
					map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": "hello over http"}},
				}})
			default:
				t.Errorf("unexpected method %q", msg.Method)
				writeHTTPRPCError(t, w, msg.ID, -32601, "method not found", nil)
			}
		},
	}))
	client := startHTTPClient(t, url, nil)
	resources, err := client.ListResources(context.Background())
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(resources) != 1 || resources[0].URI != "file:///a.txt" {
		t.Fatalf("resources = %+v", resources)
	}
	messages, err := client.GetPrompt(context.Background(), "greet", nil)
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if len(messages) != 1 || messages[0].Text != "hello over http" {
		t.Fatalf("prompt messages = %+v", messages)
	}
}

// TestHTTPResourceTemplates verifies resources/templates/list over the HTTP
// transport, both through the client and through the adapted tool.
func TestHTTPResourceTemplates(t *testing.T) {
	url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{
		onRequest: func(msg httpRPCRequest, w http.ResponseWriter, _ *http.Request) {
			if msg.Method != "resources/templates/list" {
				t.Errorf("method = %q, want resources/templates/list", msg.Method)
				writeHTTPRPCError(t, w, msg.ID, -32601, "method not found", nil)
				return
			}
			writeHTTPRPCResult(t, w, msg.ID, map[string]any{"resourceTemplates": []any{
				map[string]any{"uriTemplate": "mem://doc/{id}", "name": "document", "description": "A document by id", "mimeType": "text/plain"},
				map[string]any{"uriTemplate": "mem://repo/{name}", "name": "repository", "mimeType": "application/json"},
			}})
		},
	}))
	client := startHTTPClient(t, url, nil)
	templates, err := client.ListResourceTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListResourceTemplates: %v", err)
	}
	if len(templates) != 2 || templates[0].URITemplate != "mem://doc/{id}" ||
		templates[0].Name != "document" || templates[0].MimeType != "text/plain" {
		t.Fatalf("templates = %+v", templates)
	}
	result, err := ResourceTools(client, "")[2].Invoke(context.Background(), rig.ToolCall{Arguments: `{}`})
	if err != nil {
		t.Fatalf("mcp_list_resource_templates: %v", err)
	}
	requireContains(t, result.Content, "mem://doc/{id}", "(document)", "[text/plain]", "A document by id",
		"mem://repo/{name}")
}

// TestHTTPConcurrentCalls verifies parallel tool calls over independent POSTs.
func TestHTTPConcurrentCalls(t *testing.T) {
	url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{
		onRequest: func(msg httpRPCRequest, w http.ResponseWriter, _ *http.Request) {
			var params struct {
				Arguments struct {
					Text string `json:"text"`
				} `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			writeHTTPRPCResult(t, w, msg.ID, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "echo: " + params.Arguments.Text}},
			})
		},
	}))
	client := startHTTPClient(t, url, nil)

	const calls = 8
	var waitGroup sync.WaitGroup
	errs := make(chan error, calls)
	for i := 0; i < calls; i++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			result, err := client.CallTool(context.Background(), "echo",
				json.RawMessage(fmt.Sprintf(`{"text":"%d"}`, index)))
			if err != nil {
				errs <- fmt.Errorf("call %d: %w", index, err)
				return
			}
			want := fmt.Sprintf("echo: %d", index)
			if len(result.Content) != 1 || result.Content[0].Text != want {
				errs <- fmt.Errorf("call %d result = %+v, want %q", index, result, want)
			}
		}(i)
	}
	waitGroup.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestHTTPTransportNotInitialized verifies a transport rejects non-initialize
// requests before the handshake and validates configuration.
func TestHTTPTransportNotInitialized(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	transport, err := newHTTPTransport(Config{Transport: TransportHTTP, URL: server.URL})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	_, err = transport.Request(context.Background(), requestMessage{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	if !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Request before initialize = %v, want ErrNotInitialized", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("HTTP requests before initialize = %d, want 0", got)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestHTTPConfigValidation verifies transport selection and URL validation.
func TestHTTPConfigValidation(t *testing.T) {
	if _, err := Start(context.Background(), Config{Transport: "bogus"}); err == nil {
		t.Fatal("Start accepted an unknown transport")
	}
	if _, err := StartHTTP(context.Background(), HTTPConfig{}); err == nil ||
		!strings.Contains(err.Error(), "Config.URL is required") {
		t.Fatalf("StartHTTP with empty URL = %v, want Config.URL error", err)
	}
	for _, rawURL := range []string{"ftp://example.com/mcp", "://bad", "https:///mcp"} {
		if _, err := StartHTTP(context.Background(), HTTPConfig{URL: rawURL}); err == nil {
			t.Fatalf("StartHTTP accepted invalid URL %q", rawURL)
		}
	}
}
