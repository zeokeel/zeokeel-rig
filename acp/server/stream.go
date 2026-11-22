package server

// Streaming mapping of one prompt turn onto ACP session/update notifications.
// Assistant text becomes agent_message_chunk deltas; tool-call transitions in
// the snapshot's assistant messages become tool_call (start) and
// tool_call_update (completion/failure) notifications. History that already
// existed when the prompt started is seeded and never replayed.

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	rigacp "github.com/zeokeel/zeokeel-rig/acp"
)

// promptState tracks the streamed state of one prompt turn.
type promptState struct {
	mu        sync.Mutex
	emit      rigacp.Emit
	sessionID string
	lastText  string
	finished  bool

	// Tool-call streaming state: the last status per tool call (keyed by the
	// stable id or read-model position), the emitted id for empty ids and the
	// generated-id counter.
	toolStates map[string]string
	toolIDs    map[string]string
	generated  int
}

// newPromptState builds the streaming state of one prompt.
func newPromptState(emit rigacp.Emit, sessionID string) *promptState {
	return &promptState{
		emit:       emit,
		sessionID:  sessionID,
		toolStates: make(map[string]string),
		toolIDs:    make(map[string]string),
	}
}

// seed records the assistant text and tool calls already present in a session
// so a new prompt neither replays old text nor old tool history.
func (state *promptState) seed(snapshot Session) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.lastText = assistantText(snapshot)
	for messageIndex, message := range snapshot.Messages {
		if message.Role != RoleAssistant {
			continue
		}
		for toolIndex, tool := range message.Tools {
			key := toolStateKey(tool, messageIndex, toolIndex)
			id := strings.TrimSpace(tool.ID)
			if id == "" {
				// Keep the generated-id namespace stable with later snapshots.
				id = state.toolIDs[key]
			}
			state.toolIDs[key] = id
			state.toolStates[key] = strings.TrimSpace(tool.Status)
		}
	}
}

// observe processes one snapshot: it emits the new assistant text delta first,
// then the new/changed tool-call events, in snapshot order. Emissions happen
// inside the state lock so two concurrent snapshot callbacks cannot reorder
// frames (the emit callback only takes the transport writer lock).
func (state *promptState) observe(snapshot Session) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.finished {
		return
	}
	text := assistantText(snapshot)
	delta := ""
	if len(text) > len(state.lastText) {
		delta = text[len(state.lastText):]
		state.lastText = text
	}
	events := state.observeToolEventsLocked(snapshot)
	if delta != "" {
		state.writeText(delta)
	}
	for _, event := range events {
		state.writeToolEvent(event)
	}
	if !snapshot.Running {
		state.finished = true
	}
}

// finish stops all later emissions once the turn has settled.
func (state *promptState) finish() {
	state.mu.Lock()
	state.finished = true
	state.mu.Unlock()
}

// writeText emits one agent_message_chunk notification. The caller holds state.mu.
func (state *promptState) writeText(delta string) {
	state.emit(MethodSessionUpdate, map[string]any{
		"sessionId": state.sessionID,
		"update": map[string]any{
			"sessionUpdate": UpdateAgentMessageChunk,
			"content":       map[string]any{"type": "text", "text": delta},
		},
	})
}

// assistantText concatenates the assistant message text of a session snapshot.
func assistantText(session Session) string {
	var builder strings.Builder
	for _, message := range session.Messages {
		if message.Role == RoleAssistant {
			builder.WriteString(message.Text)
		}
	}
	return builder.String()
}

// toolEvent is one derived tool-call notification (start and/or result).
type toolEvent struct {
	ID      string
	Title   string
	Input   string
	Started bool
	Final   bool
	Failed  bool
	Content string
}

// observeToolEventsLocked derives the tool events of one snapshot. The caller
// holds state.mu.
func (state *promptState) observeToolEventsLocked(snapshot Session) []toolEvent {
	var events []toolEvent
	for messageIndex, message := range snapshot.Messages {
		if message.Role != RoleAssistant {
			continue
		}
		for toolIndex, tool := range message.Tools {
			key := toolStateKey(tool, messageIndex, toolIndex)
			id := state.toolIDs[key]
			if id == "" {
				id = strings.TrimSpace(tool.ID)
				if id == "" {
					state.generated++
					id = fmt.Sprintf("call-%d", state.generated)
				}
				state.toolIDs[key] = id
			}
			status := strings.TrimSpace(tool.Status)
			previous, seen := state.toolStates[key]
			final, failed := toolFinalStatus(status)
			event := toolEvent{ID: id, Title: strings.TrimSpace(tool.Name), Input: strings.TrimSpace(tool.Arguments)}
			if !seen {
				event.Started = true
				state.toolStates[key] = status
				if final {
					event.Final = true
					event.Failed = failed
					event.Content = truncateToolContent(tool.Summary)
					state.toolStates[key] = status
				}
			} else if final && !toolStatusIsFinal(previous) {
				event.Final = true
				event.Failed = failed
				event.Content = truncateToolContent(tool.Summary)
				state.toolStates[key] = status
			}
			if event.Started || event.Final {
				events = append(events, event)
			}
		}
	}
	return events
}

// toolStateKey identifies one tool call across snapshots: the stable tool id
// when present, else its position in the read model.
func toolStateKey(tool ToolCall, messageIndex, toolIndex int) string {
	if id := strings.TrimSpace(tool.ID); id != "" {
		return "id:" + id
	}
	return fmt.Sprintf("index:%d:%d", messageIndex, toolIndex)
}

// toolFinalStatus classifies a tool status: final reports a finished result and
// failed reports an error result.
func toolFinalStatus(status string) (final, failed bool) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "running", "in_progress", "pending", "started":
		return false, false
	case "error", "failed", "failure":
		return true, true
	default:
		return true, false
	}
}

// toolStatusIsFinal reports whether a recorded status is a final one.
func toolStatusIsFinal(status string) bool {
	final, _ := toolFinalStatus(status)
	return final
}

// writeToolEvent emits the tool_call and/or tool_call_update notifications of
// one derived event. The caller holds state.mu.
func (state *promptState) writeToolEvent(event toolEvent) {
	if event.Started {
		state.emit(MethodSessionUpdate, map[string]any{
			"sessionId": state.sessionID,
			"update": map[string]any{
				"sessionUpdate": UpdateToolCall,
				"toolCall": map[string]any{
					"toolCallId": event.ID,
					"title":      event.Title,
					"kind":       toolKindOther,
					"status":     "in_progress",
					"rawInput":   toolRawInput(event.Input),
				},
			},
		})
	}
	if !event.Final {
		return
	}
	update := map[string]any{
		"sessionUpdate": UpdateToolCallUpdate,
		"toolCallId":    event.ID,
		"status":        "completed",
	}
	if event.Failed {
		update["status"] = "failed"
	}
	if event.Title != "" {
		update["title"] = event.Title
	}
	if event.Input != "" {
		update["rawInput"] = toolRawInput(event.Input)
	}
	if content := strings.TrimSpace(event.Content); content != "" {
		update["content"] = []any{map[string]any{
			"type":    "content",
			"content": map[string]any{"type": "text", "text": content},
		}}
	}
	state.emit(MethodSessionUpdate, map[string]any{
		"sessionId": state.sessionID,
		"update":    update,
	})
}

// toolRawInput renders a tool's raw JSON arguments as an ACP rawInput value:
// the decoded JSON value when the string is valid JSON, the original string
// otherwise, and "" when the backend reported no arguments.
func toolRawInput(input string) any {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	var value any
	if err := json.Unmarshal([]byte(input), &value); err != nil {
		return input
	}
	return value
}

// truncateToolContent bounds result text to maxToolContentBytes bytes, keeping
// valid UTF-8 and reserving room for the ellipsis when truncation happens.
func truncateToolContent(content string) string {
	content = strings.TrimSpace(content)
	if len([]byte(content)) <= maxToolContentBytes {
		return content
	}
	limit := maxToolContentBytes - len("…")
	cut := content[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
