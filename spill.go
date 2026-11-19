package rig

// Spilled tool results: a result too large to put in the model's context is
// stored, and the model sees a short summary plus a handle it can ask for again.
//
// The default store is a directory of files under SpillPolicy.Dir. A host that
// already keeps its state in a database can pass a SpillStore instead, and the
// handle becomes a row id rather than a path — the model-facing sentence keeps
// its shape either way.

import "context"

// SpillRef identifies a stored tool result.
type SpillRef struct {
	// ID is the handle the model sees and can pass back to the tool that reads
	// spilled results.
	ID string
	// Bytes is the stored size.
	Bytes int
}

// SpillStore stores oversized tool results.
//
// Implementations must be safe for concurrent use: several tools can finish at
// once.
type SpillStore interface {
	// Save stores content and returns its handle.
	Save(ctx context.Context, call ToolCall, content string) (SpillRef, error)
	// Load returns the content behind a handle. An unknown handle is an error
	// the caller can branch on.
	Load(ctx context.Context, id string) (string, error)
	// GC keeps the newest keep results and drops the rest, returning how many
	// it dropped.
	GC(ctx context.Context, keep int) (int, error)
}

// ErrSpillNotFound reports a handle that is not stored.
var ErrSpillNotFound = errSpillNotFound{}

type errSpillNotFound struct{}

func (errSpillNotFound) Error() string { return "rig: spilled result not found" }
