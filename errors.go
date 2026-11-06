package rig

import "errors"

// Sentinel errors that callers can test with errors.Is. Providers and tools may
// wrap them with context; they must not replace them.
var (
	// ErrProviderRequired is returned when an agent has no provider configured.
	ErrProviderRequired = errors.New("rig: provider is required")
	// ErrModelRequired is returned when no model name is available.
	ErrModelRequired = errors.New("rig: model is required")
	// ErrToolNotFound is returned by the tool registry when a call names a tool
	// that is not registered. The agent reports unknown tool calls to the model
	// as tool errors rather than failing the run.
	ErrToolNotFound = errors.New("rig: tool not found")
	// ErrMaxIterations is returned (wrapped) by Run when the model<->tool loop
	// hits its round limit while the model still requests tools.
	ErrMaxIterations = errors.New("rig: max iterations reached")
)
