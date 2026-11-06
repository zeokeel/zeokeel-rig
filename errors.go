package rig

import "errors"

// Sentinel errors that callers can test with errors.Is. Providers and tools may
// wrap them with context; they must not replace them.
var (
	// ErrProviderRequired is returned when an agent has no provider configured.
	ErrProviderRequired = errors.New("rig: provider is required")
	// ErrModelRequired is returned when no model name is available.
	ErrModelRequired = errors.New("rig: model is required")
	// ErrToolNotFound is returned when a tool call names an unknown tool.
	ErrToolNotFound = errors.New("rig: tool not found")
	// ErrDenied is returned when an approval gate denies a call and the agent is
	// configured to fail instead of reporting the denial to the model.
	ErrDenied = errors.New("rig: tool call denied")
	// ErrMaxIterations is returned (or wrapped in RunResult) when the loop hits
	// its round limit while the model still requests tools.
	ErrMaxIterations = errors.New("rig: max iterations reached")
	// ErrNoStream is returned by a provider that cannot stream.
	ErrNoStream = errors.New("rig: provider does not support streaming")
)
