// Package lsp is a standard-library-only Language Server Protocol client and a
// set of model-facing tools (lsp_hover, lsp_definition, lsp_diagnostics,
// lsp_symbols) built on it.
//
// # Transport
//
// The client speaks LSP's stdio transport: a language server is started as a
// child process and JSON-RPC messages are framed with a Content-Length header:
//
//	Content-Length: 42\r\n
//	\r\n
//	{"jsonrpc":"2.0",...}
//
// One goroutine reads frames and resolves pending requests by numeric ID; server
// notifications (only textDocument/publishDiagnostics is consumed) are stored,
// and server-initiated requests get a generic JSON-RPC reply. Inbound messages
// are capped at 16 MiB. Message headers are read with a 64 KiB byte budget
// before allocation (a single unterminated header line is rejected instead of
// buffered), so a hostile server cannot exhaust the host's memory with an
// oversized header.
//
// # Lifecycle
//
//	client, err := lsp.Start(ctx, lsp.Config{Command: "gopls"})
//	defer client.Close()
//	if err := client.Initialize(ctx); err != nil { ... }
//	if err := client.DidOpen(ctx, uri, text); err != nil { ... }
//	hover, err := client.Hover(ctx, uri, line, character) // 0-based LSP position
//
// Initialize performs the initialize request/response handshake and then sends
// the initialized notification. Shutdown, Exit and Close are idempotent:
// Close first makes a bounded graceful attempt (shutdown, exit, process exit)
// and only then kills the child. Cancelling the context passed to Start
// terminates the server and fails pending requests.
//
// # Diagnostics
//
// Diagnostics are published by servers asynchronously. Client.Diagnostics
// returns the newest set published for the URI, or waits briefly
// (DiagnosticsWait) for the next publish before returning "none known". It is
// best effort: a server that never publishes, or publishes for a different URI
// (e.g. for files it was never asked about), yields an empty result rather than
// an error.
//
// # Coverage and limits
//
// Implemented: initialize/initialized, hover, definition (Location,
// Location[], LocationLink), documentSymbols (hierarchical DocumentSymbol and
// flat SymbolInformation), publishDiagnostics, didOpen/didChange (full-text
// sync), shutdown/exit.
//
// Not implemented (out of scope for the read-only tool set): completion,
// rename, code actions, references, implementation, formatting, workspace
// folders or configuration, dynamic capability registration, other transports
// (TCP, pipes), and retries.
//
// Positions are LSP-native: zero-based lines and UTF-16 code-unit columns.
// The client does not convert UTF-16 offsets; the model-facing tools take
// 1-based path/line/character arguments and only shift them by one, so callers
// must express columns in UTF-16 code units.
package lsp
