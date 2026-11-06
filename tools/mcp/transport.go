package mcp

// This file defines the transport seam: the Client speaks JSON-RPC through a
// transport, and stdio (child process) / Streamable HTTP (remote endpoint) are
// two implementations. The Client owns framing-independent behaviour (handshake,
// ids, timeouts, retries of the session) and the transport owns wire framing.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrNotInitialized is returned when a transport is asked to send a request
// before the initialize handshake completed. The HTTP transport enforces this:
// only initialize is allowed on a fresh transport.
var ErrNotInitialized = errors.New("mcp: transport is not initialized")

// ErrSessionExpired is returned by the Streamable HTTP transport when the server
// answers 404 or 410 (the session is unknown or expired). Callers can restart a
// client to establish a new session.
var ErrSessionExpired = errors.New("mcp: session expired")

// rpcResponse is the transported outcome of one request: the raw result JSON or
// a JSON-RPC error.
type rpcResponse struct {
	result json.RawMessage
	err    error
}

// transport sends JSON-RPC requests/notifications and returns the matching
// response. Implementations must be safe for concurrent Request/Notify calls;
// Close is idempotent.
type transport interface {
	// Request sends one request and returns the response matching msg.ID. A
	// JSON-RPC error comes back as rpcResponse.err (typically *RPCError);
	// protocol/transport failures are returned as the error value.
	Request(ctx context.Context, msg requestMessage) (rpcResponse, error)
	// Notify sends one notification (no response expected).
	Notify(ctx context.Context, msg notificationMessage) error
	// Close releases the transport resources.
	Close() error
}

// responseFromEnvelope converts one decoded JSON-RPC envelope into a transport
// response for id.
func responseFromEnvelope(envelope *responseEnvelope, id uint64) rpcResponse {
	response := rpcResponse{result: envelope.Result}
	if envelope.Error != nil {
		response.err = envelope.Error
	} else if len(bytes.TrimSpace(envelope.Result)) == 0 {
		response.err = fmt.Errorf("mcp: response %d carries neither result nor error", id)
	}
	return response
}

// decodeResponseID parses a JSON-RPC id echoed by the server. The client sends
// numeric ids; a string form of the same number is tolerated as well.
func decodeResponseID(raw json.RawMessage) (uint64, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, false
	}
	text := string(trimmed)
	if strings.HasPrefix(text, `"`) {
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return 0, false
		}
	}
	id, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}
