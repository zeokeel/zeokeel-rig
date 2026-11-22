package server_test

// TDD tests for the host-agnostic ACP server method mapping. They drive the
// server over in-memory pipes and assert the exact newline-framed JSON-RPC
// frames a client sees: initialize capabilities, session/new, streamed
// session/prompt chunks, image blocks, session/cancel, tool_call /
// tool_call_update updates, unknown methods and malformed frames.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/acp/server"
)

// --- fake chat -------------------------------------------------------------

// fakeChat is a scriptable server.Chat.
type fakeChat struct {
	mu sync.Mutex

	next     int
	sessions map[string]*server.Session
	model    string

	newSessionErr error
	promptErr     error
	promptHook    func(*fakeChat, string, server.PromptRequest, func(server.Session))

	promptRequests []server.PromptRequest
	promptIDs      []string
	cancelCalls    []string

	started      chan struct{}
	startedOnce  sync.Once
	cancelSignal chan struct{}
	cancelOnce   sync.Once
}

func newFakeChat() *fakeChat {
	return &fakeChat{
		sessions:     map[string]*server.Session{},
		started:      make(chan struct{}),
		cancelSignal: make(chan struct{}),
	}
}

// NewSession implements server.Chat.
func (c *fakeChat) NewSession(_ context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.newSessionErr != nil {
		return "", c.newSessionErr
	}
	c.next++
	id := fmt.Sprintf("sess-%d", c.next)
	c.sessions[id] = &server.Session{ID: id, Model: c.model}
	return id, nil
}

// Prompt implements server.Chat.
func (c *fakeChat) Prompt(_ context.Context, sessionID string, request server.PromptRequest, onSnapshot func(server.Session)) error {
	c.mu.Lock()
	c.promptIDs = append(c.promptIDs, sessionID)
	c.promptRequests = append(c.promptRequests, request)
	hook := c.promptHook
	err := c.promptErr
	c.mu.Unlock()
	c.startedOnce.Do(func() { close(c.started) })
	if hook != nil {
		hook(c, sessionID, request, onSnapshot)
	}
	return err
}

// Session implements server.Chat.
func (c *fakeChat) Session(sessionID string) (server.Session, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, ok := c.sessions[sessionID]
	if !ok {
		return server.Session{}, false
	}
	return cloneSession(*session), true
}

// Cancel implements server.Chat.
func (c *fakeChat) Cancel(sessionID string) error {
	c.mu.Lock()
	c.cancelCalls = append(c.cancelCalls, sessionID)
	session, ok := c.sessions[sessionID]
	if ok {
		session.Running = false
	}
	c.mu.Unlock()
	c.cancelOnce.Do(func() { close(c.cancelSignal) })
	return nil
}

// emitAssistant sets the last assistant message text and notifies one snapshot.
func (c *fakeChat) emitAssistant(sessionID, text string, running bool, onSnapshot func(server.Session)) {
	c.mu.Lock()
	session := c.ensureSessionLocked(sessionID)
	message := c.ensureAssistantLocked(session)
	message.Text = text
	session.Running = running
	snapshot := cloneSession(*session)
	c.mu.Unlock()
	onSnapshot(snapshot)
}

// emitTools sets the last assistant message text and tool calls.
func (c *fakeChat) emitTools(sessionID, text string, tools []server.ToolCall, running bool, onSnapshot func(server.Session)) {
	c.mu.Lock()
	session := c.ensureSessionLocked(sessionID)
	message := c.ensureAssistantLocked(session)
	message.Text = text
	message.Tools = append([]server.ToolCall(nil), tools...)
	session.Running = running
	snapshot := cloneSession(*session)
	c.mu.Unlock()
	onSnapshot(snapshot)
}

// seedTools records history without notifying (the server seeds it before the
// prompt starts and must not replay it).
func (c *fakeChat) seedTools(sessionID, text string, tools []server.ToolCall) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session := c.ensureSessionLocked(sessionID)
	message := c.ensureAssistantLocked(session)
	message.Text = text
	message.Tools = append([]server.ToolCall(nil), tools...)
}

func (c *fakeChat) ensureSessionLocked(sessionID string) *server.Session {
	session, ok := c.sessions[sessionID]
	if !ok {
		session = &server.Session{ID: sessionID}
		c.sessions[sessionID] = session
	}
	return session
}

func (c *fakeChat) ensureAssistantLocked(session *server.Session) *server.Message {
	if len(session.Messages) == 0 || session.Messages[len(session.Messages)-1].Role != server.RoleAssistant {
		session.Messages = append(session.Messages, server.Message{Role: server.RoleAssistant})
	}
	return &session.Messages[len(session.Messages)-1]
}

func (c *fakeChat) calls() ([]server.PromptRequest, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]server.PromptRequest(nil), c.promptRequests...), append([]string(nil), c.promptIDs...)
}

func cloneSession(session server.Session) server.Session {
	out := session
	out.Messages = make([]server.Message, len(session.Messages))
	for index, message := range session.Messages {
		message.Tools = append([]server.ToolCall(nil), message.Tools...)
		out.Messages[index] = message
	}
	return out
}

// --- wire harness ----------------------------------------------------------

// frame is one decoded output frame.
type frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// harness drives one server over two io.Pipe pairs.
type harness struct {
	t      *testing.T
	chat   *fakeChat
	inW    *io.PipeWriter
	outW   *io.PipeWriter
	lines  chan string
	closed chan struct{}
	done   chan error
}

func newHarness(t *testing.T, chat *fakeChat) *harness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &harness{t: t, chat: chat, inW: inW, outW: outW, lines: make(chan string, 128), closed: make(chan struct{}), done: make(chan error, 1)}
	service := server.New(chat)
	go func() { h.done <- service.Serve(context.Background(), inR, outW) }()
	go func() {
		defer close(h.closed)
		scanner := bufio.NewScanner(outR)
		for scanner.Scan() {
			h.lines <- scanner.Text()
		}
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		select {
		case err := <-h.done:
			if err != nil {
				t.Errorf("Serve returned %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("Serve did not stop after EOF")
		}
		_ = outW.Close()
		_ = outR.Close()
	})
	return h
}

// sendRaw writes one raw line (no framing help) to the server.
func (h *harness) sendRaw(line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.inW, line+"\n"); err != nil {
		h.t.Fatalf("write frame %q: %v", line, err)
	}
}

// sendRequest writes one request frame without waiting.
func (h *harness) sendRequest(id any, method string, params any) {
	h.t.Helper()
	request := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		request["params"] = params
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		h.t.Fatalf("marshal request: %v", err)
	}
	h.sendRaw(string(encoded))
}

// readFrame returns the next output frame, failing after a timeout.
func (h *harness) readFrame() frame {
	h.t.Helper()
	select {
	case line, ok := <-h.lines:
		if !ok {
			h.t.Fatalf("server closed its output stream")
		}
		var decoded frame
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			h.t.Fatalf("decode output frame %q: %v", line, err)
		}
		return decoded
	case <-time.After(3 * time.Second):
		h.t.Fatalf("timed out waiting for an output frame")
		return frame{}
	}
}

// call sends one request and returns every frame up to and including its
// response (notifications included, in order).
func (h *harness) call(id any, method string, params any) []frame {
	h.t.Helper()
	h.sendRequest(id, method, params)
	return h.waitResponse(id)
}

// waitResponse reads frames until the response for id arrives.
func (h *harness) waitResponse(id any) []frame {
	h.t.Helper()
	want := jsonID(id)
	frames := make([]frame, 0, 4)
	for len(frames) < 32 {
		next := h.readFrame()
		frames = append(frames, next)
		if next.Method == "" && string(next.ID) == want {
			return frames
		}
	}
	h.t.Fatalf("no response for id %s", want)
	return nil
}

// waitTwoResponses reads until both ids got a response.
func (h *harness) waitTwoResponses(first, second any) []frame {
	h.t.Helper()
	wantFirst, wantSecond := jsonID(first), jsonID(second)
	seenFirst, seenSecond := false, false
	frames := make([]frame, 0, 8)
	for len(frames) < 64 && !(seenFirst && seenSecond) {
		next := h.readFrame()
		frames = append(frames, next)
		if next.Method == "" {
			switch string(next.ID) {
			case wantFirst:
				seenFirst = true
			case wantSecond:
				seenSecond = true
			}
		}
	}
	if !seenFirst || !seenSecond {
		h.t.Fatalf("missing responses for %s/%s in %+v", wantFirst, wantSecond, frames)
	}
	return frames
}

// response returns the response frame for id.
func response(t *testing.T, frames []frame, id any) frame {
	t.Helper()
	want := jsonID(id)
	for _, candidate := range frames {
		if candidate.Method == "" && string(candidate.ID) == want {
			return candidate
		}
	}
	t.Fatalf("no response for id %s in %+v", want, frames)
	return frame{}
}

// updates returns the session/update payloads in frame order.
func updates(t *testing.T, frames []frame) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, candidate := range frames {
		if candidate.Method != "session/update" {
			continue
		}
		var params struct {
			SessionID string         `json:"sessionId"`
			Update    map[string]any `json:"update"`
		}
		if err := json.Unmarshal(candidate.Params, &params); err != nil {
			t.Fatalf("decode session/update params %s: %v", candidate.Params, err)
		}
		if params.Update == nil {
			t.Fatalf("session/update without update payload: %s", candidate.Params)
		}
		out = append(out, params.Update)
	}
	return out
}

// resultMap decodes one response result into a generic map.
func resultMap(t *testing.T, responseFrame frame) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(responseFrame.Result, &result); err != nil {
		t.Fatalf("decode result %s: %v", responseFrame.Result, err)
	}
	return result
}

// errorCode returns the JSON-RPC error code of a response.
func errorCode(t *testing.T, responseFrame frame) int {
	t.Helper()
	if responseFrame.Error == nil {
		t.Fatalf("expected an error response, got %+v", responseFrame)
	}
	return responseFrame.Error.Code
}

func jsonID(id any) string {
	encoded, err := json.Marshal(id)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// promptParams builds a session/prompt params object.
func promptParams(sessionID string, blocks ...map[string]any) map[string]any {
	return map[string]any{"sessionId": sessionID, "prompt": blocks}
}

func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func imageBlock(data, mimeType string) map[string]any {
	return map[string]any{"type": "image", "data": data, "mimeType": mimeType}
}

var testImage = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x01, 0x02, 0xff}

// --- tests -----------------------------------------------------------------

func TestInitializeAdvertisesImagePrompts(t *testing.T) {
	h := newHarness(t, newFakeChat())
	frames := h.call(1, "initialize", nil)
	if len(frames) != 1 {
		t.Fatalf("frames = %+v, want one response", frames)
	}
	result := resultMap(t, frames[0])
	if result["protocolVersion"] != float64(1) {
		t.Fatalf("protocolVersion = %v", result["protocolVersion"])
	}
	caps, ok := result["agentCapabilities"].(map[string]any)
	if !ok {
		t.Fatalf("agentCapabilities = %v", result["agentCapabilities"])
	}
	prompts, _ := caps["promptCapabilities"].(map[string]any)
	if prompts["image"] != true || prompts["audio"] != false {
		t.Fatalf("promptCapabilities = %v", prompts)
	}
	if caps["loadSession"] == true {
		t.Fatalf("server advertises loadSession without implementing session/load: %v", caps)
	}
}

func TestSessionNewReturnsSessionAndModel(t *testing.T) {
	chat := newFakeChat()
	chat.model = "fake-model-7"
	h := newHarness(t, chat)
	frames := h.call("new", "session/new", map[string]any{})
	result := resultMap(t, response(t, frames, "new"))
	if result["sessionId"] != "sess-1" || result["model"] != "fake-model-7" {
		t.Fatalf("session/new = %v, want sess-1/fake-model-7", result)
	}
}

func TestPromptStreamsAssistantTextChunks(t *testing.T) {
	chat := newFakeChat()
	chat.promptHook = func(c *fakeChat, sessionID string, _ server.PromptRequest, onSnapshot func(server.Session)) {
		c.emitAssistant(sessionID, "Hello", true, onSnapshot)
		c.emitAssistant(sessionID, "Hello world", true, onSnapshot)
		c.emitAssistant(sessionID, "Hello world", false, onSnapshot)
	}
	h := newHarness(t, chat)
	h.call("new", "session/new", map[string]any{})
	frames := h.call(2, "session/prompt", promptParams("sess-1", textBlock("hi")))

	chunks := updates(t, frames)
	if len(chunks) != 2 {
		t.Fatalf("updates = %+v, want two text chunks", chunks)
	}
	for index, want := range []string{"Hello", " world"} {
		if chunks[index]["sessionUpdate"] != "agent_message_chunk" {
			t.Fatalf("update[%d] = %+v", index, chunks[index])
		}
		content, _ := chunks[index]["content"].(map[string]any)
		if content["type"] != "text" || content["text"] != want {
			t.Fatalf("chunk[%d] content = %v, want %q", index, content, want)
		}
	}
	stop := resultMap(t, response(t, frames, 2))
	if stop["stopReason"] != "end_turn" {
		t.Fatalf("stopReason = %v", stop["stopReason"])
	}
	requests, ids := chat.calls()
	if len(requests) != 1 || requests[0].Text != "hi" || ids[0] != "sess-1" {
		t.Fatalf("prompt calls = %v / %v", requests, ids)
	}
}

func TestPromptForwardsImageBlocks(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(testImage)
	chat := newFakeChat()
	h := newHarness(t, chat)
	h.call("new", "session/new", map[string]any{})
	frames := h.call(3, "session/prompt", promptParams("sess-1",
		textBlock("describe this image"), imageBlock(encoded, "image/png")))
	requests, _ := chat.calls()
	if len(requests) != 1 {
		t.Fatalf("prompt calls = %d, want 1", len(requests))
	}
	if requests[0].Text != "describe this image" {
		t.Fatalf("text = %q", requests[0].Text)
	}
	if len(requests[0].Attachments) != 1 {
		t.Fatalf("attachments = %+v", requests[0].Attachments)
	}
	attachment := requests[0].Attachments[0]
	if attachment.MIME != "image/png" || !bytes.Equal(attachment.Data, testImage) {
		t.Fatalf("attachment = %+v", attachment)
	}
	if attachment.Name != "image-1.png" {
		t.Fatalf("attachment name = %q", attachment.Name)
	}
	if stop := resultMap(t, response(t, frames, 3)); stop["stopReason"] != "end_turn" {
		t.Fatalf("stopReason = %v", stop["stopReason"])
	}
}

func TestPromptAcceptsImageOnlyAndContentFallback(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(testImage)
	t.Run("image only prompt", func(t *testing.T) {
		chat := newFakeChat()
		h := newHarness(t, chat)
		h.call("new", "session/new", map[string]any{})
		frames := h.call(4, "session/prompt", promptParams("sess-1", imageBlock(encoded, "image/jpeg")))
		requests, _ := chat.calls()
		if len(requests) != 1 || requests[0].Text != "" || len(requests[0].Attachments) != 1 {
			t.Fatalf("image-only request = %+v", requests)
		}
		if response(t, frames, 4).Error != nil {
			t.Fatalf("image-only prompt failed: %+v", frames)
		}
	})
	t.Run("content fallback", func(t *testing.T) {
		chat := newFakeChat()
		h := newHarness(t, chat)
		h.call("new", "session/new", map[string]any{})
		h.call(5, "session/prompt", map[string]any{
			"sessionId": "sess-1",
			"content":   []map[string]any{textBlock("legacy client")},
		})
		requests, _ := chat.calls()
		if len(requests) != 1 || requests[0].Text != "legacy client" {
			t.Fatalf("content fallback request = %+v", requests)
		}
	})
}

func TestPromptImageValidationErrors(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(testImage)
	oversized := base64.StdEncoding.EncodeToString(make([]byte, 10<<20+1))
	cases := []struct {
		name  string
		block map[string]any
	}{
		{name: "missing mimeType", block: map[string]any{"type": "image", "data": encoded}},
		{name: "non-image mimeType", block: imageBlock(encoded, "text/plain")},
		{name: "missing data", block: imageBlock("", "image/png")},
		{name: "bad base64", block: imageBlock("!!!not-base64!!!", "image/png")},
		{name: "oversized", block: imageBlock(oversized, "image/png")},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			chat := newFakeChat()
			h := newHarness(t, chat)
			h.call("new", "session/new", map[string]any{})
			frames := h.call(6, "session/prompt", promptParams("sess-1", testCase.block))
			if code := errorCode(t, response(t, frames, 6)); code != -32602 {
				t.Fatalf("error code = %d, want -32602", code)
			}
			if requests, _ := chat.calls(); len(requests) != 0 {
				t.Fatalf("invalid image reached the chat backend: %+v", requests)
			}
		})
	}
}

func TestPromptRejectsMissingSessionAndEmptyPrompt(t *testing.T) {
	chat := newFakeChat()
	h := newHarness(t, chat)
	missing := h.call(7, "session/prompt", map[string]any{"prompt": []map[string]any{textBlock("hi")}})
	if code := errorCode(t, response(t, missing, 7)); code != -32602 {
		t.Fatalf("missing sessionId code = %d", code)
	}
	empty := h.call(8, "session/prompt", promptParams("sess-1"))
	if code := errorCode(t, response(t, empty, 8)); code != -32602 {
		t.Fatalf("empty prompt code = %d", code)
	}
	if requests, _ := chat.calls(); len(requests) != 0 {
		t.Fatalf("invalid prompts reached the chat backend: %+v", requests)
	}
}

func TestSessionCancelSettlesCancelled(t *testing.T) {
	chat := newFakeChat()
	chat.promptHook = func(_ *fakeChat, _ string, _ server.PromptRequest, _ func(server.Session)) {
		<-chat.cancelSignal
	}
	h := newHarness(t, chat)
	h.call("new", "session/new", map[string]any{})
	h.sendRequest(9, "session/prompt", promptParams("sess-1", textBlock("long task")))
	select {
	case <-chat.started:
	case <-time.After(3 * time.Second):
		t.Fatalf("prompt never started")
	}
	h.sendRequest("cancel", "session/cancel", map[string]any{"sessionId": "sess-1"})
	frames := h.waitTwoResponses(9, "cancel")

	cancelResponse := response(t, frames, "cancel")
	if cancelResponse.Error != nil {
		t.Fatalf("session/cancel failed: %+v", cancelResponse)
	}
	stop := resultMap(t, response(t, frames, 9))
	if stop["stopReason"] != "cancelled" {
		t.Fatalf("stopReason = %v, want cancelled", stop["stopReason"])
	}
	chat.mu.Lock()
	cancelled := append([]string(nil), chat.cancelCalls...)
	chat.mu.Unlock()
	if len(cancelled) != 1 || cancelled[0] != "sess-1" {
		t.Fatalf("Cancel calls = %v", cancelled)
	}
}

func TestToolCallAndToolCallUpdate(t *testing.T) {
	chat := newFakeChat()
	call := server.ToolCall{ID: "call-1", Name: "read_file", Arguments: `{"path":"a.txt"}`, Status: "running"}
	chat.promptHook = func(c *fakeChat, sessionID string, _ server.PromptRequest, onSnapshot func(server.Session)) {
		c.emitAssistant(sessionID, "Let me check.", true, onSnapshot)
		c.emitTools(sessionID, "Let me check.", []server.ToolCall{call}, true, onSnapshot)
		call.Status, call.Summary = "ok", "file contents"
		c.emitTools(sessionID, "Let me check.", []server.ToolCall{call}, true, onSnapshot)
		c.emitTools(sessionID, "Let me check. Done.", []server.ToolCall{call}, false, onSnapshot)
	}
	h := newHarness(t, chat)
	h.call("new", "session/new", map[string]any{})
	frames := h.call(10, "session/prompt", promptParams("sess-1", textBlock("read it")))

	chunks := updates(t, frames)
	if len(chunks) != 4 {
		t.Fatalf("updates = %+v, want text/tool_call/tool_call_update/text", chunks)
	}
	if chunks[0]["sessionUpdate"] != "agent_message_chunk" {
		t.Fatalf("update[0] = %+v", chunks[0])
	}
	if chunks[1]["sessionUpdate"] != "tool_call" {
		t.Fatalf("update[1] = %+v", chunks[1])
	}
	toolCall, _ := chunks[1]["toolCall"].(map[string]any)
	if toolCall["toolCallId"] != "call-1" || toolCall["title"] != "read_file" ||
		toolCall["kind"] != "other" || toolCall["status"] != "in_progress" {
		t.Fatalf("tool_call = %+v", toolCall)
	}
	rawInput, _ := toolCall["rawInput"].(map[string]any)
	if rawInput["path"] != "a.txt" {
		t.Fatalf("tool_call rawInput = %v", toolCall["rawInput"])
	}
	if chunks[2]["sessionUpdate"] != "tool_call_update" {
		t.Fatalf("update[2] = %+v", chunks[2])
	}
	if chunks[2]["toolCallId"] != "call-1" || chunks[2]["status"] != "completed" {
		t.Fatalf("tool_call_update = %+v", chunks[2])
	}
	content, _ := chunks[2]["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "content" {
		t.Fatalf("tool content = %+v", chunks[2]["content"])
	}
	text := content[0].(map[string]any)["content"].(map[string]any)["text"]
	if text != "file contents" {
		t.Fatalf("tool content text = %v", text)
	}
	if chunks[3]["sessionUpdate"] != "agent_message_chunk" {
		t.Fatalf("update[3] = %+v", chunks[3])
	}
	if stop := resultMap(t, response(t, frames, 10)); stop["stopReason"] != "end_turn" {
		t.Fatalf("stopReason = %v", stop["stopReason"])
	}
}

func TestToolFailureAndGeneratedID(t *testing.T) {
	chat := newFakeChat()
	call := server.ToolCall{Name: "bash", Status: "running", Arguments: "not json"}
	chat.promptHook = func(c *fakeChat, sessionID string, _ server.PromptRequest, onSnapshot func(server.Session)) {
		c.emitTools(sessionID, "", []server.ToolCall{call}, true, onSnapshot)
		call.Status, call.Summary = "error", "boom"
		c.emitTools(sessionID, "", []server.ToolCall{call}, false, onSnapshot)
	}
	h := newHarness(t, chat)
	h.call("new", "session/new", map[string]any{})
	frames := h.call(11, "session/prompt", promptParams("sess-1", textBlock("run")))

	chunks := updates(t, frames)
	if len(chunks) != 2 {
		t.Fatalf("updates = %+v, want tool_call + tool_call_update", chunks)
	}
	toolCall, _ := chunks[0]["toolCall"].(map[string]any)
	if toolCall["toolCallId"] != "call-1" {
		t.Fatalf("generated id = %v", toolCall["toolCallId"])
	}
	if toolCall["rawInput"] != "not json" {
		t.Fatalf("non-JSON rawInput = %v, want the original string", toolCall["rawInput"])
	}
	if chunks[1]["status"] != "failed" {
		t.Fatalf("failed status = %v", chunks[1]["status"])
	}
}

func TestToolHistoryIsNotReplayed(t *testing.T) {
	chat := newFakeChat()
	existing := server.ToolCall{ID: "old-call", Name: "read_file", Status: "ok", Summary: "old"}
	chat.promptHook = func(c *fakeChat, sessionID string, _ server.PromptRequest, onSnapshot func(server.Session)) {
		c.emitTools(sessionID, "old answer new answer", []server.ToolCall{existing}, false, onSnapshot)
	}
	h := newHarness(t, chat)
	h.call("new", "session/new", map[string]any{})
	chat.seedTools("sess-1", "old answer ", []server.ToolCall{existing})
	frames := h.call(12, "session/prompt", promptParams("sess-1", textBlock("again")))
	chunks := updates(t, frames)
	if len(chunks) != 1 || chunks[0]["sessionUpdate"] != "agent_message_chunk" {
		t.Fatalf("history replayed: %+v", chunks)
	}
	content, _ := chunks[0]["content"].(map[string]any)
	if content["text"] != "new answer" {
		t.Fatalf("delta = %v, want %q", content["text"], "new answer")
	}
}

func TestPromptErrorsSurfaceAsJSONRPCErrors(t *testing.T) {
	t.Run("prompt error", func(t *testing.T) {
		chat := newFakeChat()
		chat.promptErr = errors.New("send boom")
		h := newHarness(t, chat)
		h.call("new", "session/new", map[string]any{})
		frames := h.call(13, "session/prompt", promptParams("sess-1", textBlock("hi")))
		if code := errorCode(t, response(t, frames, 13)); code != -32603 {
			t.Fatalf("error code = %d", code)
		}
	})
	t.Run("terminal run error", func(t *testing.T) {
		chat := newFakeChat()
		chat.promptHook = func(c *fakeChat, sessionID string, _ server.PromptRequest, onSnapshot func(server.Session)) {
			c.mu.Lock()
			c.ensureSessionLocked(sessionID).Error = "provider exploded"
			snapshot := cloneSession(*c.sessions[sessionID])
			c.mu.Unlock()
			onSnapshot(snapshot)
		}
		h := newHarness(t, chat)
		h.call("new", "session/new", map[string]any{})
		frames := h.call(14, "session/prompt", promptParams("sess-1", textBlock("hi")))
		if code := errorCode(t, response(t, frames, 14)); code != -32603 {
			t.Fatalf("error code = %d", code)
		}
	})
}

func TestUnknownMethodAndMalformedFrame(t *testing.T) {
	chat := newFakeChat()
	h := newHarness(t, chat)
	unknown := h.call(15, "session/bogus", nil)
	if code := errorCode(t, response(t, unknown, 15)); code != -32601 {
		t.Fatalf("unknown method code = %d, want -32601", code)
	}
	h.sendRaw("{not json")
	parseError := h.readFrame()
	if parseError.Error == nil || parseError.Error.Code != -32700 {
		t.Fatalf("malformed frame response = %+v", parseError)
	}
	if string(parseError.ID) != "null" {
		t.Fatalf("parse error id = %s, want null", parseError.ID)
	}
	// The connection survives a malformed frame.
	after := h.call(16, "initialize", nil)
	if resultMap(t, response(t, after, 16))["protocolVersion"] != float64(1) {
		t.Fatalf("server did not recover after a malformed frame")
	}
}

func TestPromptFramesAreNewlineDelimitedJSON(t *testing.T) {
	chat := newFakeChat()
	h := newHarness(t, chat)
	// A request with a trailing newline sequence inside the payload must not
	// break framing: JSON escapes the newline.
	h.call(17, "session/prompt", map[string]any{
		"sessionId": "sess-1",
		"prompt":    []map[string]any{textBlock("line one\nline two")},
	})
	requests, _ := chat.calls()
	if len(requests) != 1 || requests[0].Text != "line one\nline two" {
		t.Fatalf("multiline text = %+v", requests)
	}
	if !strings.Contains(string(requests[0].Text), "\n") {
		t.Fatalf("newline was not preserved in the prompt text")
	}
}
