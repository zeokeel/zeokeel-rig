package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/mcp"
)

// reconnectTool is one exposed MCP tool. It dispatches to the server's current
// implementation and, when the transport died, rebuilds the connection and
// retries the call once.
type reconnectTool struct {
	manager *Manager
	server  *managedServer
	name    string
	meta    rig.Tool
}

var _ rig.Tool = (*reconnectTool)(nil)

// Name implements rig.Tool.
func (t *reconnectTool) Name() string { return t.meta.Name() }

// Description implements rig.Tool.
func (t *reconnectTool) Description() string { return t.meta.Description() }

// Schema implements rig.Tool.
func (t *reconnectTool) Schema() map[string]any { return t.meta.Schema() }

// Invoke implements rig.Tool.
func (t *reconnectTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	inner := t.server.currentTool(t.name)
	if inner == nil {
		return rig.ToolResult{}, fmt.Errorf("mcp/manager: tool %q is no longer available", t.name)
	}
	observed := t.server.currentClient()
	result, err := inner.Invoke(ctx, call)
	if err == nil || !isRetryableMCPError(err) {
		return result, err
	}
	if reconnectErr := t.manager.reconnectServer(ctx, t.server, observed); reconnectErr != nil {
		return result, err
	}
	inner = t.server.currentTool(t.name)
	if inner == nil {
		return result, err
	}
	return inner.Invoke(ctx, call)
}

// isRetryableMCPError reports whether err means "the connection died and a
// reconnect can fix it": a lost HTTP session, a closed client, or a stdio child
// process that exited / stopped reading.
func isRetryableMCPError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, mcp.ErrSessionExpired) || errors.Is(err, mcp.ErrClosed) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"server process exited",
		"server closed its stdout",
		"write to server",
		"broken pipe",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// limitedTool bounds one tool result to the configured byte budget.
type limitedTool struct {
	inner rig.Tool
	limit int
}

var _ rig.Tool = (*limitedTool)(nil)

// Name implements rig.Tool.
func (t *limitedTool) Name() string { return t.inner.Name() }

// Description implements rig.Tool.
func (t *limitedTool) Description() string { return t.inner.Description() }

// Schema implements rig.Tool.
func (t *limitedTool) Schema() map[string]any { return t.inner.Schema() }

// Invoke implements rig.Tool.
func (t *limitedTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	result, err := t.inner.Invoke(ctx, call)
	if err != nil {
		return result, err
	}
	result.Content, _ = truncateMCPContent(result.Content, t.limit)
	return result, nil
}

// truncateMCPContent bounds one MCP tool result. The returned bool reports
// whether anything was dropped (the caller keeps the result's IsError field).
// The head and tail of the payload are both preserved and a
// "[truncated: N bytes]" marker replaces the dropped middle, so the model can
// tell the result is incomplete (a silent cut would read as a complete answer).
func truncateMCPContent(content string, limit int) (string, bool) {
	if limit <= 0 || len(content) <= limit {
		return content, false
	}
	dropped := 0
	for attempt := 0; attempt < 4; attempt++ {
		marker := fmt.Sprintf("\n[truncated: %d bytes]\n", dropped)
		remaining := limit - len(marker)
		if remaining < 2 {
			// Tiny budget: a hard cut is the only option, but keep it valid UTF-8.
			return validUTF8Prefix(content, limit), true
		}
		headLen := remaining * 3 / 4
		tailLen := remaining - headLen
		if headLen+tailLen > len(content) {
			tailLen = len(content) - headLen
		}
		head := validUTF8Prefix(content, headLen)
		tail := validUTF8Suffix(content, tailLen)
		if len(head)+len(tail) > len(content) {
			tail = ""
		}
		dropped = len(content) - len(head) - len(tail)
		candidate := head + fmt.Sprintf("\n[truncated: %d bytes]\n", dropped) + tail
		if len(candidate) <= limit {
			return candidate, true
		}
	}
	return validUTF8Prefix(content, limit), true
}

// validUTF8Prefix returns content[:limit] backed off to a rune boundary.
func validUTF8Prefix(content string, limit int) string {
	if limit >= len(content) {
		return content
	}
	if limit < 0 {
		return ""
	}
	for limit > 0 && !utf8.RuneStart(content[limit]) {
		limit--
	}
	return content[:limit]
}

// validUTF8Suffix returns the last limit bytes of content advanced to a rune
// boundary.
func validUTF8Suffix(content string, limit int) string {
	if limit >= len(content) {
		return content
	}
	if limit < 0 {
		return ""
	}
	start := len(content) - limit
	for start < len(content) && !utf8.RuneStart(content[start]) {
		start++
	}
	return content[start:]
}
