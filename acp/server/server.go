// Package server maps the host-agnostic ACP method surface onto a Chat backend:
// initialize/capabilities, session/new, streamed session/prompt turns (including
// ACP image blocks), session/cancel, and the agent_message_chunk / tool_call /
// tool_call_update session updates. Newline-framed JSON-RPC 2.0 is provided by
// the rig acp transport; this package is its host-method mapping.
//
// The host owns what a session is and how a prompt reaches its agent: it
// implements Chat (NewSession, Prompt with a snapshot callback, Session,
// Cancel). The server owns validation, streaming diffing (assistant text and
// tool-call transitions), attachment decoding and the ACP wire shapes.
package server

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	rigacp "github.com/zeokeel/zeokeel-rig/acp"
)

const (
	// JSONRPCVersion is the only JSON-RPC version the transport speaks.
	JSONRPCVersion = rigacp.JSONRPCVersion
	// ProtocolVersion is the ACP protocol version advertised by initialize.
	ProtocolVersion = rigacp.ProtocolVersion

	// JSON-RPC error codes.
	CodeParseError     = rigacp.CodeParseError
	CodeInvalidRequest = rigacp.CodeInvalidRequest
	CodeMethodNotFound = rigacp.CodeMethodNotFound
	CodeInvalidParams  = rigacp.CodeInvalidParams
	CodeInternalError  = rigacp.CodeInternalError

	// ACP methods served here (initialize is answered by the transport).
	MethodInitialize    = rigacp.MethodInitialize
	MethodSessionNew    = rigacp.MethodSessionNew
	MethodSessionPrompt = rigacp.MethodSessionPrompt
	MethodSessionCancel = rigacp.MethodSessionCancel
	MethodSessionUpdate = rigacp.MethodSessionUpdate

	// ACP stop reasons.
	StopReasonEndTurn   = rigacp.StopReasonEndTurn
	StopReasonCancelled = rigacp.StopReasonCancelled

	// session/update kinds emitted by this server.
	UpdateAgentMessageChunk = "agent_message_chunk"
	UpdateToolCall          = "tool_call"
	UpdateToolCallUpdate    = "tool_call_update"

	// MaxPromptImageBytes bounds the decoded size of one prompt image (10 MiB).
	MaxPromptImageBytes = 10 << 20

	// toolKindOther is the only tool kind reported (the read model does not
	// classify tools).
	toolKindOther = "other"
	// maxToolContentBytes bounds the result text of one update (4 KiB).
	maxToolContentBytes = 4 << 10
)

// Message roles a Chat snapshot may contain.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
	RoleTool      = "tool"
)

// Message is one conversation message of a session snapshot.
type Message struct {
	Role string
	Text string
	// Tools is the tool-call read model of an assistant message, in call order.
	Tools []ToolCall
}

// ToolCall is one tool call of a session snapshot. Status is the backend's
// status string ("running", "ok", "error", ...); Summary is the result text.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
	Status    string
	Summary   string
}

// Session is the read-model snapshot of one conversation.
type Session struct {
	ID       string
	Title    string
	Model    string
	Messages []Message
	Running  bool
	Error    string
}

// Attachment is one decoded prompt image forwarded to the chat backend.
type Attachment struct {
	ID   string
	Name string
	MIME string
	Data []byte
}

// PromptRequest is one user turn: its text and its decoded image attachments.
type PromptRequest struct {
	Text        string
	Attachments []Attachment
}

// Chat is the host chat backend the server drives. Implementations must be
// safe for concurrent use (a session/prompt turn and session/cancel run on
// different goroutines).
type Chat interface {
	// NewSession creates a conversation and returns its ID.
	NewSession(ctx context.Context) (string, error)
	// Prompt starts one user turn. It returns when the turn settles (success
	// or failure). onSnapshot is called with each read-model snapshot the host
	// wants streamed; it may be called from another goroutine. Cancelling ctx
	// or calling Cancel should make the turn settle early.
	Prompt(ctx context.Context, sessionID string, request PromptRequest, onSnapshot func(Session)) error
	// Session returns the current snapshot of a session.
	Session(sessionID string) (Session, bool)
	// Cancel stops the in-flight turn of a session (idempotent).
	Cancel(sessionID string) error
}

// Option configures a Server.
type Option func(*Server)

// WithTimeout bounds one session/prompt turn. Zero (the default) means no
// server-side timeout: the client's cancel or the connection lifetime controls
// the turn.
func WithTimeout(timeout time.Duration) Option {
	return func(server *Server) {
		if timeout > 0 {
			server.timeout = timeout
		}
	}
}

// WithCapabilities replaces the agentCapabilities object returned by
// initialize (nil keeps the default: image prompts supported, no loadSession).
func WithCapabilities(capabilities map[string]any) Option {
	return func(server *Server) {
		if capabilities != nil {
			server.capabilities = capabilities
		}
	}
}

// Server is one ACP connection over a Chat backend.
type Server struct {
	chat         Chat
	timeout      time.Duration
	capabilities map[string]any

	mu              sync.Mutex
	cancelRequested map[string]bool
}

// New builds a server; it panics when chat is nil.
func New(chat Chat, opts ...Option) *Server {
	if chat == nil {
		panic("server: New requires a Chat")
	}
	service := &Server{
		chat: chat,
		capabilities: map[string]any{
			"promptCapabilities": map[string]any{
				"image":           true,
				"audio":           false,
				"embeddedContext": false,
			},
		},
		cancelRequested: make(map[string]bool),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(service)
		}
	}
	return service
}

// Serve runs one ACP connection: newline-framed JSON-RPC frames are read from
// in and written to out until EOF or ctx ends.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	transport := rigacp.NewServer(rigacp.Options{
		Handler:      s,
		Capabilities: s.capabilities,
	})
	return transport.Serve(ctx, in, out)
}

// Handle implements the transport's acp.Handler: it maps one ACP method onto
// the Chat backend. initialize is answered by the transport.
func (s *Server) Handle(ctx context.Context, method string, params json.RawMessage, emit rigacp.Emit) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	switch method {
	case MethodSessionNew:
		return s.handleSessionNew(ctx)
	case MethodSessionPrompt:
		return s.handlePrompt(ctx, params, emit)
	case MethodSessionCancel:
		return s.handleCancel(params)
	default:
		return nil, rigacp.Errorf(CodeMethodNotFound, "method not found: %s", method)
	}
}

// handleSessionNew creates a session and returns its ID (plus the session's
// model when the snapshot exposes one).
func (s *Server) handleSessionNew(ctx context.Context) (any, error) {
	sessionID, err := s.chat.NewSession(ctx)
	if err != nil {
		return nil, internalError("session/new: " + err.Error())
	}
	result := map[string]any{"sessionId": sessionID}
	if snapshot, ok := s.chat.Session(sessionID); ok {
		if model := strings.TrimSpace(snapshot.Model); model != "" {
			result["model"] = model
		}
	}
	return result, nil
}

// promptParams is the params object of session/prompt. Content is accepted as a
// fallback for clients that use it instead of prompt.
type promptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
	Content   []contentBlock `json:"content"`
}

// cancelParams is the params object of session/cancel.
type cancelParams struct {
	SessionID string `json:"sessionId"`
}

// handlePrompt implements session/prompt: it validates and decodes the request,
// streams assistant text and tool-call updates while the turn runs, and settles
// with a stop reason (end_turn/cancelled) or a JSON-RPC error.
func (s *Server) handlePrompt(ctx context.Context, raw json.RawMessage, emit rigacp.Emit) (any, error) {
	var params promptParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams("session/prompt: " + err.Error())
	}
	sessionID := strings.TrimSpace(params.SessionID)
	if sessionID == "" {
		return nil, invalidParams("session/prompt: sessionId is required")
	}
	request, rpcErr := buildPromptRequest(params)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if request.Text == "" && len(request.Attachments) == 0 {
		return nil, invalidParams("session/prompt: prompt text or image content is required")
	}

	s.mu.Lock()
	s.cancelRequested[sessionID] = false
	s.mu.Unlock()

	state := newPromptState(emit, sessionID)
	if snapshot, ok := s.chat.Session(sessionID); ok {
		state.seed(snapshot)
	}

	done := make(chan error, 1)
	go func() {
		done <- s.chat.Prompt(ctx, sessionID, request, state.observe)
	}()

	var timeout <-chan time.Time
	if s.timeout > 0 {
		timer := time.NewTimer(s.timeout)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case err := <-done:
		if err != nil {
			state.finish()
			if ctx.Err() != nil {
				_ = s.chat.Cancel(sessionID)
				return nil, rigacp.ErrNoResponse
			}
			return nil, internalError("session/prompt: " + err.Error())
		}
	case <-ctx.Done():
		state.finish()
		_ = s.chat.Cancel(sessionID)
		return nil, rigacp.ErrNoResponse
	case <-timeout:
		state.finish()
		_ = s.chat.Cancel(sessionID)
		return nil, internalError("session/prompt: timeout")
	}
	// The host may have completed without a final notification: process the
	// settled snapshot once more so late text/tool updates are not lost.
	if snapshot, ok := s.chat.Session(sessionID); ok {
		state.observe(snapshot)
	}
	state.finish()

	s.mu.Lock()
	cancelled := s.cancelRequested[sessionID]
	s.mu.Unlock()
	if cancelled {
		return map[string]string{"stopReason": StopReasonCancelled}, nil
	}
	// A run failure must not look like a successful empty turn to an ACP
	// client: the snapshot carries the terminal run error.
	if snapshot, ok := s.chat.Session(sessionID); ok {
		if errText := strings.TrimSpace(snapshot.Error); errText != "" {
			return nil, internalError("session/prompt: " + errText)
		}
	}
	return map[string]string{"stopReason": StopReasonEndTurn}, nil
}

// handleCancel implements session/cancel: it marks the turn cancelled and asks
// the chat backend to stop it.
func (s *Server) handleCancel(raw json.RawMessage) (any, error) {
	var params cancelParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams("session/cancel: " + err.Error())
	}
	sessionID := strings.TrimSpace(params.SessionID)
	if sessionID == "" {
		return nil, invalidParams("session/cancel: sessionId is required")
	}
	s.mu.Lock()
	s.cancelRequested[sessionID] = true
	s.mu.Unlock()
	_ = s.chat.Cancel(sessionID)
	return struct{}{}, nil
}

// invalidParams builds a -32602 error.
func invalidParams(message string) *rigacp.Error {
	return &rigacp.Error{Code: CodeInvalidParams, Message: message}
}

// internalError builds a -32603 error.
func internalError(message string) *rigacp.Error {
	return &rigacp.Error{Code: CodeInternalError, Message: message}
}
