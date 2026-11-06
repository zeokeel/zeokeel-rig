# Changelog

## Unreleased

### Fixed

- CI: the "contracts/v1 must stay standard-library only" step compared the
  module's own import path against `grep '\.'` and therefore failed on every run
  since v0.1.0. It now lists `go list -deps` entries with `Standard=false` and
  excludes the package itself, so it actually looks for non-stdlib imports.
- Coverage gate (`tools/full` + `tools/catalog`): `TestToolCatalogCoverage` now
  also fails when `full.All` builds a tool that is not registered in
  `catalog.Static`. Nine tools added in v0.7/v0.8 (`skill`, `code_review`,
  `auto_review`, `team_task_*`, `job_wait`, `todo_write`) had been silently
  outside the ledger; they are registered now (74 static names = 74 built).

### Changed

- README quickstart is now the compilable `examples/quickstart` program, and a
  CI step diffs the README block against that file so it cannot rot again.
  Repository layout, package tables and roadmap were regenerated against v0.8.
- `docs/ARCHITECTURE.md`: fix the stale `tool` package name and document the two
  sandbox limits honestly (best-effort shell allowlist, hostname-resolving URLs).
- `go.mod` declares `go 1.27` instead of `1.27.3`; the patch-level minimum forced
  consumers to upgrade their toolchain for no API reason.
- CI runs gofmt, the modern-surface audit, cross-compilation (windows, darwin,
  freebsd) and the README quickstart drift check; the job gained `permissions:
  contents: read`, a `concurrency` group and a timeout.

### Removed

- Local `.probe/` development scripts are untracked (and ignored), and
  `scripts/audit-legacy-tools.sh` now requires the old-repo path instead of
  defaulting to the maintainer's local `/data/mygo/old`.

## v0.8.0 — 2026-11-06

Audit-gap close: remote MCP templates, subagent fork and structured output.

### Added

- `tools/mcp`: `ListResourceTemplates` / `mcp_list_resource_templates`
  (resources/templates/list with cursor pagination, uriTemplate dedupe;
  server-bound like the other MCP tools).
- `agent.NewRunner`: `RunnerOptions.ParentSessionID` (+ optional ParentHistory)
  for fork; `SubagentSpec.Fork` seeds a child with a copy of the parent session
  history, `SubagentSpec.OutputSchema` gives the child a `structured_output`
  tool validated against a JSON-Schema subset and returns
  `SubagentResult.Structured`.
- `tools/subagent`: `fork` and `output_schema` parameters; new `subagent_fork`
  tool (fork forced on).
- `contracts/v1`: `SubagentSpec.Fork`, `SubagentSpec.OutputSchema`,
  `SubagentResult.Structured` (additive).
- `tools/audit` + `docs/AUDIT-REPORT.md`: three gaps closed; the remaining 6 are
  reclassified as host/kernel-bound (cordis inspection, plugin manager/market,
  workspace dependencies, PTC run_code).
- `tools/catalog` + `tools/full.All`: 74 static tools, 62 modern names, no
  catalog gaps.

## v0.7.0 — 2026-11-06

Modern-surface gap close.

### Added

- `tools/skill`: `skill` tool (list/search/load) with a filesystem `SkillStore`
  (`*/SKILL.md`, top-level `*.md`, `$ZEOKEEL_SKILLS`,
  `<root>/.zeokeel/skills`, `~/.zeokeel/skills`).
- `tools/team` (`tasks.go`): shared task board - `team_task_create`,
  `team_task_list`, `team_task_get`, `team_task_update` over
  `v1.TeamTaskManager` with pending/in_progress/completed states, owner claim,
  blocked-by dependency cycles and an in-memory default manager.
- `tools/hosttools`: `goal action=update` with complete/blocked/pause/resume/edit
  via the optional `v1.GoalUpdater`; the in-memory Host implements the state
  machine (blocked reason retained).
- `tools/review`: `code_review` and `auto_review` over `v1.Reviewer`, plus
  `SubagentReviewer` (child-agent reviewer with `VERDICT:`/`FILE:LINE` parsing).
- `tools/audit`: modern tool-surface audit (`Modern`/`DocumentedGaps`,
  `Missing`) and `docs/AUDIT-REPORT.md`.
- `contracts/v1`: `GoalUpdater`/`GoalUpdateAction`, `TeamTask`,
  `TeamTaskFilter`, `TeamTaskUpdate`, `TeamTaskManager`.
- `tools/catalog`: `Modern`, `DocumentedGaps`, `AuditModern`; the coverage test
  now checks static + legacy + modern names.

### Changed

- `tools/full.All`: new `Skill`, `TeamTasks`, `Reviewer` options; the full
  registry is now 73 static tools.

## Unreleased

### Changed

- Relicensed from Apache-2.0 to MIT (`LICENSE`, `README.md`).

## v0.6.0 — 2026-11-06

The legacy agent tool surface is complete, with a coverage gate that keeps it
complete.

### Added

- `tools/hosttools`: `ask_user`, `present`, `plan_review`, `exit_plan_mode`,
  `goal` built on the new `contracts/v1` host interfaces (Asker/Presenter/
  PlanController/GoalStore) with an in-memory default `Host`.
- `tools/settings`: `settings_get` / `settings_set` over `v1.Settings`.
- `tools/memory`: `memory` tool (add/list/search/remove/clear) with an
  in-memory `v1.MemoryStore` default.
- `tools/schedule`: `schedule_create/list/update/delete` with an in-memory
  `v1.Scheduler` default (cron evaluation stays in the host).
- `tools/worktree`: `enter_worktree` / `exit_worktree` / `list_worktrees`
  (git worktree, sandbox-checked, `git worktree list --porcelain` parsing).
- `tools/session`: `session_search`, `session_trace`, `session_event_read`,
  `session_event_search`, `session_event_trace` over `v1.SessionLog`, plus an
  in-memory log implementation.
- `tools/inspect` (`inspect`) and `tools/report` (markdown session report
  written inside the sandbox root).
- `tools/ralph`: iterate a child agent via `v1.SubagentRunner` until DONE or
  the round limit.
- `tools/team`: legacy tool names (`spawn_team`, `list_teams`, `team_status`,
  `close_team`, `wait_team`, `send_message`, `interrupt_agent`,
  `list_subagent_models`) mapped onto the existing manager.
- `tools/terminal`: `terminal_list` (sessions with transport/cwd/state).
- `tools/lsp`: legacy single `lsp` tool with action routing.
- `tools/misc`: legacy `todo` alias sharing the `todo_write` store.
- `tools/full`: `All(options)` aggregating every static tool.
- `tools/catalog`: coverage ledger (`Static`, `Legacy`, `ServerBound`,
  `Dynamic`), `Check` and `AuditLegacy`; `tools/full` runs them as
  `TestToolCatalogCoverage` so a missing legacy tool fails CI.
- `contracts/v1/extensions.go`: host interfaces and store contracts
  (Asker, Presenter, PlanController, GoalStore, MemoryStore, SkillStore,
  Scheduler, SessionLog, Reviewer + types).

### Notes

- 66 static tools, 61 legacy names, 5 server-bound (MCP resources/prompts and
  the legacy `lsp` tool), dynamic `mcp__<server>__<tool>` names.
- Zero dependencies, standard library only.

## v0.5.0 — 2026-11-06

Real terminal, remote MCP and in-process orchestration. Still zero dependencies.

### Added

- `tools/terminal`: real unix PTY transport (`/dev/ptmx` + ioctl, setsid/setctty,
  interactive shell); `terminal_send` uses a quiet period instead of a marker, ANSI
  is stripped by default, Ctrl-C is delivered as `0x03`; pipe transport remains as a
  fallback (Windows/other platforms) and keeps its previous semantics.
- `tools/mcp`: Streamable HTTP transport — POST JSON-RPC, `Mcp-Session-Id`,
  `MCP-Protocol-Version`, JSON or SSE responses, session-expiry errors, concurrency
  and cancellation; `Config{Transport,URL,Headers,HTTPClient}` and `StartHTTP`.
  The existing stdio transport is unchanged.
- `contracts/v1/subagent.go`: `SubagentSpec`, `SubagentResult`, `Subagent`,
  `SubagentRunner` (additive).
- `agent.NewRunner`: `v1.SubagentRunner` with depth (`MaxDepth`) and concurrency
  (`MaxConcurrent`) limits, tool exclusion (`ExcludedTools`, defaults include the
  orchestration tools) and per-child isolated history.
- `tools/subagent`: `subagent` tool (one child agent, blocking result).
- `tools/workflow`: `workflow` tool — sequential/parallel subagent steps with
  `{{prev}}` / `{{steps.<name>}}` templating.
- `tools/team`: managed subagents — `team_spawn/team_list/team_send/team_wait/
  team_last/team_close/team_interrupt` with id/name resolution, member limits and
  lifecycle state machine.

### Changed

- `agent.DefaultExcludedTools` now lists the shipped orchestration tool names
  (`subagent`, `workflow`, `team*`) so child agents cannot recurse by default.

## v0.4.0 — 2026-11-06

Complete agent tool surface, still zero dependencies.

### Added

- `tools/editor`: `str_replace_editor` — view/create/str_replace/insert with line
  numbers, sandbox-root checks, read-only enforcement and output caps.
- `tools/image`: `read_image` — magic-byte MIME detection (PNG/JPEG/GIF/WEBP/BMP)
  returning the image in the new `contracts/v1.ToolResult.Parts` (the agent delivers
  attachments as a follow-up user message).
- `tools/terminal`: stateful shell sessions — `terminal_open/send/read/signal/close`
  over a persistent pipe-based shell (marker-terminated commands; not a PTY yet).
- `tools/jobs`: background processes — `job_start/output/kill/list/wait` with ring
  buffered output, exit codes, signals and cleanup.
- `tools/mcp`: resources/prompts support — `ListResources/ReadResource/ListPrompts/
  GetPrompt` and the `mcp_list_resources` / `mcp_read_resource` / `mcp_list_prompts` /
  `mcp_get_prompt` tools.
- `tools/builtin`: `pwsh` tool (pwsh/powershell detection, `-NoProfile
  -NonInteractive -Command`, policy-aware).
- `contracts/v1.ToolResult.Parts` for multimodal tool attachments (additive).

### Changed

- Repository layout: tool packages moved from `tool/` to `tools/` (package `tools`),
  with implementations in `tools/{builtin,editor,image,terminal,jobs,web,mcp,lsp,misc}`.
- `tools.Registry.Unregister(name)` for dynamically composed tool sets.

## v0.3.0 — 2026-11-06

Complete agent tool surface (still zero dependencies).

### Added

- `sandbox`: policy layer for agent tools — allowed roots (symlink-safe `os.Root`
  resolution), read-only mode, command allow/deny lists, environment allowlist,
  network switch with private-address URL guard, output/timeout caps, denied globs.
- `tools/builtin`: `NewWithPolicy(*sandbox.Policy)` runs the full built-in toolset under
  the policy (existing `NewFS`/`NewShell`/`NewBuiltins` keep working).
- `tools/web`: `web_fetch` (HTTP(S), redirect/size caps, HTML→text) and `web_search`
  (pluggable `Searcher`, no built-in vendor).
- `tools/mcp`: standard-library MCP client over stdio JSON-RPC 2.0 — initialize
  handshake, paginated tools/list, tools/call (text/image/audio/resource), ping,
  concurrent calls, cancellation, graceful close — plus `Tools()` adapting MCP tools
  into Rig tools (`mcp__<server>__<tool>`).
- `tools/lsp`: standard-library LSP client (Content-Length framing, initialize,
  hover/definition/documentSymbols/diagnostics) and `lsp_*` tools.
- `tools/misc`: `working_directory` (root-checked cwd) and in-memory `todo_write`.
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
- `tools` + `tools/builtin`: registry and seven built-in tools (`read_file`,
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
