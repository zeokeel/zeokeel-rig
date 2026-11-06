# Changelog

## v0.3.0 — 2026-11-06

Complete agent tool surface (still zero dependencies).

### Added

- `sandbox`: policy layer for agent tools — allowed roots (symlink-safe `os.Root`
  resolution), read-only mode, command allow/deny lists, environment allowlist,
  network switch with private-address URL guard, output/timeout caps, denied globs.
- `tool/builtin`: `NewWithPolicy(*sandbox.Policy)` runs the full built-in toolset under
  the policy (existing `NewFS`/`NewShell`/`NewBuiltins` keep working).
- `tool/web`: `web_fetch` (HTTP(S), redirect/size caps, HTML→text) and `web_search`
  (pluggable `Searcher`, no built-in vendor).
- `tool/mcp`: standard-library MCP client over stdio JSON-RPC 2.0 — initialize
  handshake, paginated tools/list, tools/call (text/image/audio/resource), ping,
  concurrent calls, cancellation, graceful close — plus `Tools()` adapting MCP tools
  into Rig tools (`mcp__<server>__<tool>`).
- `tool/lsp`: standard-library LSP client (Content-Length framing, initialize,
  hover/definition/documentSymbols/diagnostics) and `lsp_*` tools.
- `tool/misc`: `working_directory` (root-checked cwd) and in-memory `todo_write`.
- `docs/TOOLS.md`: full old-agent tool catalog mapped to Rig / product / roadmap.

## v0.2.0 — 2026-11-06

**Breaking: Rig is now a pure toolset with zero dependencies.**

### Removed (moved back to the ZeoKeel product repo)

- `assembly`, `host`, `kernelx`, `keys`, `modules/appinfo` — compile-time module
  assembly, preflight/rollback, headless host and the appinfo module now live in the
  product that uses the kernel; Rig no longer depends on `zeokeel-kernel`.
- `examples/minimal`, `examples/custom-module` (module-assembly examples).

### Added

- `examples/agent` — provider + built-in tools + gate + history + streaming loop.
- `examples/custom-tool` — a third-party `Tool` implementation, registry and invoke.

### Changed

- `go.mod` has no `require` entries; `go list -m all` shows only this module.

## v0.1.0 — 2026-11-06

First public tag of the headless agent SDK.

### Added

- `contracts/v1`: messages/content parts, tool calls, streaming chunks, `Provider`,
  `Tool`, `Gate`, `History`, agent events, `Agent`/`AgentSpec`, sentinel errors.
  Standard library only; the API compatibility generation.
- `agent`: eino-free streaming ReAct loop — model rounds, parallel tool calls,
  approval gate, iteration cap, cancellation, synchronous event stream, history
  persistence.
- `provider/openai`: OpenAI-compatible streaming provider — SSE parsing, tool-call
  deltas, reasoning content, usage, 429/5xx retries, context cancellation.
- `tool` + `tool/builtin`: registry and seven built-in tools (`read_file`,
  `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash`) with root sandbox,
  output caps and timeouts.
- `gate`: `AllowAll`, `DenyAll`, `AllowTools`, `Func`.
- `session`: in-memory `History`.
- `assembly` + `modules/appinfo`: compile-time registry, embedded headless/minimal
  assemblies, kernel preflight/rollback, build-identity module.
- `host`: headless runner with flags, settings file, logger, signals and graceful
  shutdown.
- `kernelx` (public): kernel descriptors/service keys/registry/effect helpers for module authors;
  `keys`: shared service keys (`AppInfoKey`, `SettingsKey`, `LoggerKey`).
- `examples/minimal`, `examples/custom-module`.
