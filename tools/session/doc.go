// Package session provides read-only session-history tools backed by
// contracts/v1.SessionLog: session_search, session_trace, session_event_read,
// session_event_search and session_event_trace.
//
// The tools never write session state. A host provides its own SessionLog
// implementation; NewMemoryLog is a small in-memory implementation for tests,
// examples and embedding Rig without a persistent store.
package session
