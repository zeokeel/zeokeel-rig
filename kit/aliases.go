package kit

import "github.com/zeokeel/zeokeel-rig/sandbox"

import "github.com/zeokeel/zeokeel-rig"

// All aliases below name the exact root-package types, so values built with kit
// interoperate with github.com/zeokeel/zeokeel-rig, agent, tools and every other
// package without conversion. The alias block is the one-import contract
// surface: everything a basic consumer needs to define messages, implement a
// Provider/Tool/Gate/History and observe events.

// Agent contracts.
type (
	// Provider is a model backend; implement it to plug in a custom model.
	Provider = rig.Provider
	// Request is one model completion request (the Provider-side contract).
	Request = rig.Request
	// Stream is a model response stream (the Provider-side contract).
	Stream = rig.Stream
	// Chunk is one streamed piece of a completion (the Provider-side contract).
	Chunk = rig.Chunk
	// ToolSpec is the schema advertised to the model for one tool.
	ToolSpec = rig.ToolSpec
	// ToolCallDelta is one partial tool call in a stream.
	ToolCallDelta = rig.ToolCallDelta
	// Tool is a capability the model can call.
	Tool = rig.Tool
	// ToolCall is one tool invocation requested by the model.
	ToolCall = rig.ToolCall
	// ToolResult is what a tool returns.
	ToolResult = rig.ToolResult
	// Gate decides whether a tool call may run.
	Gate = rig.Gate
	// Decision is one approval outcome for a pending tool call.
	Decision = rig.Decision
	// History stores conversation messages per session.
	History = rig.History
	// RunRequest is one agent turn (or continuation) for a session.
	RunRequest = rig.RunRequest
	// RunResult is the outcome of one agent run.
	RunResult = rig.RunResult
	// Event is one observable step of an agent run.
	Event = rig.Event
	// EventType identifies one agent event.
	EventType = rig.EventType
	// Message is one conversation message.
	Message = rig.Message
	// Role identifies who produced a message.
	Role = rig.Role
	// ContentPart is one piece of a message's content.
	ContentPart = rig.ContentPart
	// PartType is the kind of one content part.
	PartType = rig.PartType
	// Usage is token accounting for one model call.
	Usage = rig.Usage
)

// Message roles. They match the OpenAI-compatible wire format.
const (
	RoleSystem    = rig.RoleSystem
	RoleUser      = rig.RoleUser
	RoleAssistant = rig.RoleAssistant
	RoleTool      = rig.RoleTool
)

// Content part kinds.
const (
	PartText     = rig.PartText
	PartImageURL = rig.PartImageURL
)

// Approval decisions returned by a Gate.
const (
	DecisionAllow       = rig.DecisionAllow
	DecisionAllowAlways = rig.DecisionAllowAlways
	DecisionDeny        = rig.DecisionDeny
)

// Agent event types observed through RunRequest.Emit / Agent.RunText's emit.
const (
	EventTurnStart      = rig.EventTurnStart
	EventTextDelta      = rig.EventTextDelta
	EventReasoningDelta = rig.EventReasoningDelta
	EventToolCall       = rig.EventToolCall
	EventToolResult     = rig.EventToolResult
	EventUsage          = rig.EventUsage
	EventMessage        = rig.EventMessage
	EventTurnEnd        = rig.EventTurnEnd
	EventError          = rig.EventError
)

// Version is the ZeoKeel Rig module version (the same value as rig.Version, so
// the facade reports the module version without a second import).
const Version = rig.Version

// NewMessage builds a single-text-part message.
func NewMessage(role Role, text string) Message { return rig.NewMessage(role, text) }

// TextPart builds a text content part.
func TextPart(text string) ContentPart { return rig.TextPart(text) }

// ImagePart builds an image content part from a URL (http(s) or data URL).
func ImagePart(url, detail string) ContentPart { return rig.ImagePart(url, detail) }

// Sentinel errors callers can test with errors.Is. They are aliases of the root
// package values; providers and tools may wrap them with context.
var (
	// ErrProviderRequired is returned when an agent has no provider configured.
	ErrProviderRequired = rig.ErrProviderRequired
	// ErrModelRequired is returned when no model name is available.
	ErrModelRequired = rig.ErrModelRequired
	// ErrToolNotFound is returned by the tool registry when a call names an
	// unregistered tool.
	ErrToolNotFound = rig.ErrToolNotFound
	// ErrMaxIterations is returned when the loop hits its round limit while the
	// model still requests tools.
	ErrMaxIterations = rig.ErrMaxIterations
)

// Permission modes are aliases for the sandbox modes shipped by this module.
type Mode = sandbox.Mode

const (
	ModeWorkspaceWrite   = sandbox.ModeWorkspaceWrite
	ModeReadOnly         = sandbox.ModeReadOnly
	ModeDangerFullAccess = sandbox.ModeDangerFullAccess
)
