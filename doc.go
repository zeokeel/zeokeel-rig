// Package rig is ZeoKeel Rig's stable extension surface and the module root.
//
// Everything a third-party module, provider, tool, host or durable store needs
// lives here: messages and streaming chunks, the model provider interface, the
// tool interface, the approval gate, session history, agent events, the agent
// interface and the host/store contracts. The package imports only the standard
// library on purpose: it is a leaf of the import graph, so a headless Rig binary
// never drags a UI toolkit, a database driver or an agent framework into its
// contracts (CI enforces both properties). Most packages in this module import
// it; sandbox, tools/catalog and tools/audit are leaves that deliberately do not.
//
// Versioning: the module follows semantic import versioning. The root package
// is the current contract generation and stays additive within a module major
// version; a breaking contract change after v1.0.0 gets a new module major
// version at the import path (for example
// github.com/zeokeel/zeokeel-rig/v2), never a versioned subdirectory inside this
// module. Before v1.0.0 the API may still change between minor releases. See
// docs/ARCHITECTURE.md and COMPATIBILITY.md.
package rig
