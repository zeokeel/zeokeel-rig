// Package v1 is ZeoKeel-Rig's stable extension surface.
//
// Everything a third-party module, provider or tool needs lives here: messages
// and streaming chunks, the model provider interface, the tool interface, the
// approval gate, session history, agent events and the agent interface. The
// package imports only the standard library on purpose: a headless Rig binary
// must not drag in a UI toolkit, a database driver or an agent framework.
//
// Stability: this package is versioned by its import path (`contracts/v1`).
// Within v1, additive changes only; any breaking change gets a new `v2`
// package and a deprecation window. See docs/ARCHITECTURE.md.
package v1

// APIVersion is the compatibility generation of this package.
const APIVersion = "v1"
