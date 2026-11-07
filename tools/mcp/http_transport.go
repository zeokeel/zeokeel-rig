package mcp

// httpTransport is the MCP Streamable HTTP transport: JSON-RPC messages are
// POSTed to one endpoint, a response is either a single application/json body or
// an SSE stream carrying JSON-RPC messages, and session/protocol state is
// carried in headers (Mcp-Session-Id, MCP-Protocol-Version).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const (
	// headerSessionID is the MCP session header set by the server on initialize
	// and echoed by the client on subsequent requests.
	headerSessionID = "Mcp-Session-Id"
	// headerProtocolVersion carries the negotiated MCP protocol version after
	// initialization.
	headerProtocolVersion = "MCP-Protocol-Version"
	// httpErrorBodyMax caps how much of a non-2xx body is quoted in errors.
	httpErrorBodyMax = 1024
	// httpNotificationDrainMax bounds how much of an ignored notification
	// response body is drained so the connection can be reused.
	httpNotificationDrainMax = 64 << 10
)

// httpTransport is one remote MCP endpoint.
type httpTransport struct {
	url     string
	headers map[string]string
	client  *http.Client

	mu              sync.Mutex
	sessionID       string
	protocolVersion string
	initialized     bool
	closed          bool
}

var _ transport = (*httpTransport)(nil)

// newHTTPTransport validates cfg.URL and builds the HTTP transport.
func newHTTPTransport(cfg Config) (*httpTransport, error) {
	rawURL := strings.TrimSpace(cfg.URL)
	if rawURL == "" {
		return nil, errors.New("mcp: Config.URL is required for the http transport")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("mcp: invalid URL %q: %w", cfg.URL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("mcp: invalid URL %q: scheme must be http or https", cfg.URL)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("mcp: invalid URL %q: missing host", cfg.URL)
	}
	headers := make(map[string]string, len(cfg.Headers))
	for name, value := range cfg.Headers {
		headers[name] = value
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	return &httpTransport{url: rawURL, headers: headers, client: client}, nil
}

// SetProtocolVersion records the negotiated protocol version (called by the
// Client after the initialize result is validated) and marks the transport as
// initialized. Subsequent requests carry the MCP-Protocol-Version header.
func (t *httpTransport) SetProtocolVersion(version string) {
	t.mu.Lock()
	t.protocolVersion = version
	t.initialized = true
	t.mu.Unlock()
}

// SessionID returns the server session id captured from the initialize
// response, or "" when the server is stateless.
func (t *httpTransport) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// setSessionID records a server-provided session id.
func (t *httpTransport) setSessionID(sessionID string) {
	t.mu.Lock()
	t.sessionID = sessionID
	t.mu.Unlock()
}

// Request POSTs one request and returns its JSON-RPC response.
func (t *httpTransport) Request(ctx context.Context, msg requestMessage) (rpcResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if t.isClosed() {
		return rpcResponse{}, ErrClosed
	}
	// Only initialize is legal before the handshake completes; this makes an
	// accidentally uninitialized transport fail loudly instead of POSTing
	// session-less requests.
	if msg.Method != "initialize" && !t.isInitialized() {
		return rpcResponse{}, ErrNotInitialized
	}

	resp, err := t.post(ctx, msg)
	if err != nil {
		return rpcResponse{}, err
	}
	defer resp.Body.Close()
	if err := checkHTTPStatus(resp); err != nil {
		return rpcResponse{}, err
	}

	contentType := resp.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch strings.ToLower(mediaType) {
	case "application/json":
		envelope, err := decodeJSONEnvelope(resp.Body)
		if err != nil {
			return rpcResponse{}, err
		}
		id, ok := decodeResponseID(envelope.ID)
		if !ok {
			return rpcResponse{}, fmt.Errorf("mcp: server returned a notification instead of a response to %q", msg.Method)
		}
		if id != msg.ID {
			return rpcResponse{}, fmt.Errorf("mcp: response id %d does not match request id %d", id, msg.ID)
		}
		return responseFromEnvelope(envelope, msg.ID), nil
	case "text/event-stream":
		return t.readSSEResponse(ctx, resp.Body, msg.ID)
	default:
		return rpcResponse{}, fmt.Errorf("mcp: server returned Content-Type %q for %q; expected application/json or text/event-stream",
			contentType, msg.Method)
	}
}

// Notify POSTs one notification. The server normally answers 202 with no body;
// any 2xx is accepted (a bounded amount of body is drained for connection
// reuse).
func (t *httpTransport) Notify(ctx context.Context, msg notificationMessage) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if t.isClosed() {
		return ErrClosed
	}
	if !t.isInitialized() {
		return ErrNotInitialized
	}
	resp, err := t.post(ctx, msg)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkHTTPStatus(resp); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, httpNotificationDrainMax))
	return nil
}

// Close marks the transport closed. Idempotent. The MCP DELETE-session request
// is intentionally not sent: servers may not support it, and closing a client
// must not fail because of a best-effort cleanup.
func (t *httpTransport) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return nil
}

// post sends one JSON-RPC message and returns the raw HTTP response. The caller
// owns and must close the body.
func (t *httpTransport) post(ctx context.Context, message any) (*http.Response, error) {
	payload, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("mcp: encode message: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("mcp: build http request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")

	t.mu.Lock()
	sessionID, protocolVersion, closed := t.sessionID, t.protocolVersion, t.closed
	overrides := t.headers
	t.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if sessionID != "" {
		request.Header.Set(headerSessionID, sessionID)
	}
	if protocolVersion != "" {
		request.Header.Set(headerProtocolVersion, protocolVersion)
	}
	// Extra headers win, so hosts can add authentication or override the
	// defaults for a non-conforming gateway.
	for name, value := range overrides {
		request.Header.Set(name, value)
	}

	resp, err := t.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("mcp: http request: %w", err)
	}
	if serverSession := resp.Header.Get(headerSessionID); serverSession != "" {
		t.setSessionID(serverSession)
	}
	return resp, nil
}

// readSSEResponse reads JSON-RPC messages from one SSE response until the
// message matching id arrives. Notifications, server requests and responses for
// other ids are ignored.
func (t *httpTransport) readSSEResponse(ctx context.Context, body io.Reader, id uint64) (rpcResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxMessageBytes)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found || field != "data" {
			continue
		}
		payload, _ := strings.CutPrefix(value, " ")
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		var envelope responseEnvelope
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
			continue // tolerate a malformed event; the next one may match
		}
		responseID, ok := decodeResponseID(envelope.ID)
		if !ok || responseID != id {
			continue
		}
		return responseFromEnvelope(&envelope, id), nil
	}
	if err := scanner.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return rpcResponse{}, ctxErr
		}
		return rpcResponse{}, fmt.Errorf("mcp: read SSE response: %w", err)
	}
	return rpcResponse{}, fmt.Errorf("mcp: SSE response ended without a response for request %d", id)
}

// decodeJSONEnvelope decodes one JSON-RPC envelope, bounded to MaxMessageBytes.
func decodeJSONEnvelope(reader io.Reader) (*responseEnvelope, error) {
	var envelope responseEnvelope
	decoder := json.NewDecoder(io.LimitReader(reader, MaxMessageBytes))
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("mcp: decode json response: %w", err)
	}
	return &envelope, nil
}

// checkHTTPStatus turns non-2xx responses into errors. 404/410 are reported as
// ErrSessionExpired because MCP servers use them for unknown/expired sessions.
func checkHTTPStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return nil
	}
	snippet := readErrorSnippet(resp.Body)
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		if snippet != "" {
			return fmt.Errorf("%w: %s: %s", ErrSessionExpired, resp.Status, snippet)
		}
		return fmt.Errorf("%w: %s", ErrSessionExpired, resp.Status)
	}
	if snippet != "" {
		return fmt.Errorf("mcp: http request failed: %s: %s", resp.Status, snippet)
	}
	return fmt.Errorf("mcp: http request failed: %s", resp.Status)
}

// readErrorSnippet reads a bounded body snippet for diagnostics.
func readErrorSnippet(body io.Reader) string {
	if body == nil {
		return ""
	}
	data, _ := io.ReadAll(io.LimitReader(body, httpErrorBodyMax))
	return strings.TrimSpace(string(data))
}

// isClosed reports whether Close was called.
func (t *httpTransport) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// isInitialized reports whether the handshake completed on this transport.
func (t *httpTransport) isInitialized() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.initialized
}
