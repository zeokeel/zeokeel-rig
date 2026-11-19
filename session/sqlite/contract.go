package sqlite

// The store implements the directory store's contract, so a host switches by
// changing the constructor it calls and nothing else. These aliases let callers
// name the types from either package.

import (
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// Meta is one session's durable list-item view (weights and meaning documented
// on store.Meta).
type Meta = store.Meta

// Filter narrows a List.
type Filter = store.Filter

// The errors a host branches on, identical to the directory store's so that
// `errors.Is` works across an implementation switch.
var (
	// ErrNotFound reports a session the store does not hold.
	ErrNotFound = store.ErrNotFound
	// ErrClosed reports use of a store after Close.
	ErrClosed = store.ErrClosed
	// ErrCorrupt reports a record that cannot be decoded. A database rejects a
	// malformed write at insert time, so this is returned only by the importer
	// when a source record is unreadable.
	ErrCorrupt = store.ErrCorrupt
)

// MaxTitleRunes is the longest title Append derives from a first message.
const MaxTitleRunes = store.MaxTitleRunes

// compile-time proof that the database satisfies the directory store's contract.
var _ store.Store = (*Store)(nil)
