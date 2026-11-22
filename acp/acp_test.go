package acp_test

// The transport owns JSON-RPC framing and dispatch; these tests pin the wire
// behaviour a client sees (response ids, error codes, notifications, concurrent
// prompt turns) without a real agent.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig/acp"
)

type fakeHandler struct {
	mu       sync.Mutex
	seen     []string
	prompted chan struct{}
}

func (h *fakeHandler) Handle(ctx context.Context, method string, params json.RawMessage, emit acp.Emit) (any, error) {
	h.mu.Lock()
	h.seen = append(h.seen, method)
	h.mu.Unlock()
	switch method {
	case acp.MethodSessionNew:
		return map[string]string{"sessionId": "s1", "model": "m"}, nil
	case acp.MethodSessionPrompt:
		emit(acp.MethodSessionUpdate, map[string]any{"sessionId": "s1"})
		if h.prompted != nil {
			close(h.prompted)
		}
		return map[string]string{"stopReason": acp.StopReasonEndTurn}, nil
	case acp.MethodSessionCancel:
		return struct{}{}, nil
	case "host/missing":
		return nil, acp.InvalidParams("host/missing: nope")
	case "host/internal":
		return nil, io.ErrUnexpectedEOF
	case "host/silent":
		return nil, acp.ErrNoResponse
	default:
		return nil, acp.Errorf(acp.CodeMethodNotFound, "method not found: %s", method)
	}
}

// rpc drives one request over a pipe and returns the decoded response.
func rpc(t *testing.T, handler acp.Handler, frame string) map[string]any {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	server := acp.NewServer(acp.Options{Handler: handler, Capabilities: map[string]any{"loadSession": true}})
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background(), inR, outW) }()
	if _, err := io.WriteString(inW, frame+"\n"); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	line, err := bufio.NewReader(outR).ReadString('\n')
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	_ = inW.Close()
	_ = outW.Close()
	<-done
	var decoded map[string]any
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return decoded
}

func TestInitializeAndCapabilities(t *testing.T) {
	response := rpc(t, &fakeHandler{}, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if response["jsonrpc"] != "2.0" || response["id"] != float64(1) {
		t.Fatalf("response = %v", response)
	}
	result := response["result"].(map[string]any)
	if result["protocolVersion"] != float64(acp.ProtocolVersion) {
		t.Fatalf("protocolVersion = %v", result["protocolVersion"])
	}
	caps := result["agentCapabilities"].(map[string]any)
	if caps["loadSession"] != true {
		t.Fatalf("capabilities = %v", caps)
	}
}

func TestHandlerResultAndErrors(t *testing.T) {
	handler := &fakeHandler{}
	newSession := rpc(t, handler, `{"jsonrpc":"2.0","id":2,"method":"session/new"}`)
	if got := newSession["result"].(map[string]any)["sessionId"]; got != "s1" {
		t.Fatalf("session/new = %v", newSession)
	}
	invalid := rpc(t, handler, `{"jsonrpc":"2.0","id":3,"method":"host/missing"}`)
	rpcErr := invalid["error"].(map[string]any)
	if rpcErr["code"] != float64(acp.CodeInvalidParams) {
		t.Fatalf("invalid params error = %v", invalid)
	}
	internal := rpc(t, handler, `{"jsonrpc":"2.0","id":4,"method":"host/internal"}`)
	if internal["error"].(map[string]any)["code"] != float64(acp.CodeInternalError) {
		t.Fatalf("internal error = %v", internal)
	}
	unknown := rpc(t, handler, `{"jsonrpc":"2.0","id":5,"method":"no/such"}`)
	if unknown["error"].(map[string]any)["code"] != float64(acp.CodeMethodNotFound) {
		t.Fatalf("method not found = %v", unknown)
	}
}

func TestProtocolValidation(t *testing.T) {
	handler := &fakeHandler{}
	parse := rpc(t, handler, `{not json`)
	if parse["error"].(map[string]any)["code"] != float64(acp.CodeParseError) {
		t.Fatalf("parse error = %v", parse)
	}
	version := rpc(t, handler, `{"jsonrpc":"1.0","id":1,"method":"session/new"}`)
	if version["error"].(map[string]any)["code"] != float64(acp.CodeInvalidRequest) {
		t.Fatalf("version error = %v", version)
	}
	missing := rpc(t, handler, `{"jsonrpc":"2.0","id":1}`)
	if missing["error"].(map[string]any)["code"] != float64(acp.CodeInvalidRequest) {
		t.Fatalf("missing method = %v", missing)
	}
}

func TestPromptRunsAsyncAndEmitsNotifications(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	handler := &fakeHandler{prompted: make(chan struct{})}
	server := acp.NewServer(acp.Options{Handler: handler})
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background(), inR, outW) }()
	// session/cancel must be answered while the prompt is still in flight.
	if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"session/prompt"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(outR)
	first, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read notification: %v", err)
	}
	var notification map[string]any
	if err := json.Unmarshal([]byte(first), &notification); err != nil {
		t.Fatalf("decode notification %q: %v", first, err)
	}
	if notification["method"] != acp.MethodSessionUpdate {
		t.Fatalf("first frame = %v, want the update notification", notification)
	}
	<-handler.prompted
	if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","id":2,"method":"session/cancel"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	// Read until both responses arrived (prompt result + cancel result).
	ids := map[float64]bool{}
	for len(ids) < 2 {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		if id, ok := frame["id"].(float64); ok {
			ids[id] = true
		}
	}
	if !ids[1] || !ids[2] {
		t.Fatalf("responses = %v, want ids 1 and 2", ids)
	}
	_ = inW.Close()
	_ = outW.Close()
	<-done
}

func TestNotificationWithoutIDGetsNoResponse(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	server := acp.NewServer(acp.Options{Handler: &fakeHandler{}})
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background(), inR, outW) }()
	if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","method":"session/cancel"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	_ = inW.Close()
	_ = outW.Close()
	<-done
	// No frame is expected; the read should fail/hit EOF immediately.
	if line, err := bufio.NewReader(outR).ReadString('\n'); err == nil && strings.TrimSpace(line) != "" {
		t.Fatalf("notification produced a frame: %q", line)
	}
}
