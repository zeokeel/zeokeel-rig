// Package acp is a reusable Agent Client Protocol (ACP) JSON-RPC 2.0 transport.
//
// It owns the host-agnostic half of an ACP server: newline framing, request
// validation, initialize/capabilities, method dispatch, concurrent prompt
// turns, cancel dispatch, notification emission and JSON-RPC error mapping. The
// host implements Handler: what a session is, how a prompt reaches its agent and
// which notifications the agent produces are host decisions.
package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

const (
	// JSONRPCVersion is the only JSON-RPC version the transport speaks.
	JSONRPCVersion = "2.0"
	// ProtocolVersion is the ACP protocol version advertised by initialize.
	ProtocolVersion = 1
	// DefaultMaxFrameBytes bounds one JSON-RPC frame (16 MiB).
	DefaultMaxFrameBytes = 16 << 20

	// JSON-RPC 2.0 error codes.
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	// ACP method names.
	MethodInitialize    = "initialize"
	MethodSessionNew    = "session/new"
	MethodSessionList   = "session/list"
	MethodSessionLoad   = "session/load"
	MethodSessionPrompt = "session/prompt"
	MethodSessionCancel = "session/cancel"
	MethodSessionUpdate = "session/update"

	// ACP stop reasons.
	StopReasonEndTurn   = "end_turn"
	StopReasonCancelled = "cancelled"
)

// Error is a JSON-RPC error returned by a Handler.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error implements error.
func (e *Error) Error() string { return e.Message }

// Errorf builds a JSON-RPC Error.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// InvalidParams is the common handler error for bad parameters.
func InvalidParams(format string, args ...any) *Error {
	return Errorf(CodeInvalidParams, format, args...)
}

// ErrNoResponse tells the server to write no frame for this request (the
// connection is closing, or another path already answered it).
var ErrNoResponse = errors.New("acp: no response")

// Emit sends one notification frame to the client. It is safe to call from any
// goroutine and never blocks on other frames.
type Emit func(method string, params any)

// Handler serves the host-specific methods. Returning an *Error writes that
// JSON-RPC error; returning ErrNoResponse writes nothing; any other error is
// reported as an internal error.
type Handler interface {
	Handle(ctx context.Context, method string, params json.RawMessage, emit Emit) (any, error)
}

// Options configures one server.
type Options struct {
	// Handler is required.
	Handler Handler
	// Capabilities is the agentCapabilities object returned by initialize
	// (nil advertises no capabilities).
	Capabilities map[string]any
	// AsyncMethods lists request methods that run concurrently (default:
	// session/prompt).
	AsyncMethods []string
	// MaxFrameBytes bounds one frame (0 = DefaultMaxFrameBytes).
	MaxFrameBytes int
}

// Server is one ACP connection.
type Server struct {
	handler  Handler
	caps     map[string]any
	async    map[string]bool
	maxFrame int
}

// NewServer builds a server; it panics when Handler is nil.
func NewServer(opts Options) *Server {
	if opts.Handler == nil {
		panic("acp: NewServer requires a Handler")
	}
	async := make(map[string]bool, len(opts.AsyncMethods)+1)
	if len(opts.AsyncMethods) == 0 {
		async[MethodSessionPrompt] = true
	} else {
		for _, method := range opts.AsyncMethods {
			async[method] = true
		}
	}
	maxFrame := opts.MaxFrameBytes
	if maxFrame <= 0 {
		maxFrame = DefaultMaxFrameBytes
	}
	return &Server{handler: opts.Handler, caps: opts.Capabilities, async: async, maxFrame: maxFrame}
}

// request is one decoded JSON-RPC frame.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func hasID(req request) bool {
	id := bytes.TrimSpace(req.ID)
	return len(id) > 0 && !bytes.Equal(id, []byte("null"))
}

// Serve reads frames from in and writes frames to out until EOF or ctx ends.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if in == nil || out == nil {
		return errors.New("acp: Serve requires non-nil input and output streams")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	writer := NewWriter(out)
	defer writer.Close()

	lines := make(chan []byte, 16)
	readErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 64*1024), s.maxFrame)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- line:
			case <-serveCtx.Done():
				readErr <- nil
				close(lines)
				return
			}
		}
		readErr <- scanner.Err()
		close(lines)
	}()

	var prompts sync.WaitGroup
	var scanErr error
readLoop:
	for {
		select {
		case <-serveCtx.Done():
			cancel()
			prompts.Wait()
			return nil
		case line, ok := <-lines:
			if !ok {
				scanErr = <-readErr
				break readLoop
			}
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				continue
			}
			var req request
			if err := json.Unmarshal(trimmed, &req); err != nil {
				writer.writeError(json.RawMessage("null"),
					&Error{Code: CodeParseError, Message: "parse error: " + err.Error()})
				continue
			}
			if req.JSONRPC != JSONRPCVersion {
				if hasID(req) {
					writer.writeError(req.ID, &Error{Code: CodeInvalidRequest, Message: `jsonrpc must be "2.0"`})
				}
				continue
			}
			if strings.TrimSpace(req.Method) == "" {
				if hasID(req) {
					writer.writeError(req.ID, &Error{Code: CodeInvalidRequest, Message: "missing method"})
				}
				continue
			}
			if req.Method == MethodInitialize {
				if hasID(req) {
					result := map[string]any{"protocolVersion": ProtocolVersion}
					if s.caps != nil {
						result["agentCapabilities"] = s.caps
					}
					writer.writeResult(req.ID, result)
				}
				continue
			}
			dispatch := func(req request) {
				result, err := s.handler.Handle(serveCtx, req.Method, req.Params, writer.Notify)
				if err == nil {
					if hasID(req) {
						writer.writeResult(req.ID, result)
					}
					return
				}
				if errors.Is(err, ErrNoResponse) {
					return
				}
				if !hasID(req) {
					return
				}
				var rpcErr *Error
				if errors.As(err, &rpcErr) {
					writer.writeError(req.ID, rpcErr)
					return
				}
				writer.writeError(req.ID, Errorf(CodeInternalError, "%s: %v", req.Method, err))
			}
			if s.async[req.Method] {
				prompts.Add(1)
				go func(req request) {
					defer prompts.Done()
					dispatch(req)
				}(req)
				continue
			}
			dispatch(req)
		}
	}
	// EOF: let in-flight prompts settle, then stop the connection.
	prompts.Wait()
	cancel()
	if scanErr != nil {
		return fmt.Errorf("acp: read protocol stream: %w", scanErr)
	}
	return nil
}

// Writer serializes frames (prompt goroutines and cancel share the stream).
// Hosts that emit notifications outside a Handler call (protocol pings, extra
// updates) may use one directly; Serve uses its own.
type Writer struct {
	mu  sync.Mutex
	out io.Writer
}

// NewWriter wraps out. Frames are written one line at a time.
func NewWriter(out io.Writer) *Writer { return &Writer{out: out} }

// Notify writes one notification frame.
func (w *Writer) Notify(method string, params any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeLocked(map[string]any{"jsonrpc": JSONRPCVersion, "method": method, "params": params})
}

func (w *Writer) writeResult(id json.RawMessage, result any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeLocked(map[string]any{"jsonrpc": JSONRPCVersion, "id": id, "result": result})
}

func (w *Writer) writeError(id json.RawMessage, rpcErr *Error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeLocked(map[string]any{"jsonrpc": JSONRPCVersion, "id": id, "error": rpcErr})
}

// writeLocked writes one frame; the caller holds w.mu.
func (w *Writer) writeLocked(frame map[string]any) {
	data, err := json.Marshal(frame)
	if err != nil {
		return
	}
	data = append(data, '\n')
	_, _ = w.out.Write(data)
}

// Close is a no-op hook for symmetry (stdio stays open).
func (w *Writer) Close() {}
