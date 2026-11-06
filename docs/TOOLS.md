# Tool catalog — old agent surface vs. ZeoKeel Rig

This is the migration ledger for "every tool the agent uses". The old ZeoKeel agent
(`/data/mygo/old/internal/tools`, 36 files) is the reference surface; the second column
says where each capability lives after the SDK split.

Legend — **Rig** = in this repo (toolset, zero dependencies); **Product** = belongs to the
product that composes the kernel + host (needs UI, store or process state); **Roadmap** =
planned, not in v0.3.

## Filesystem

| Tool | Status | Where |
| --- | --- | --- |
| `read_file` | ✅ v0.2 | Rig `tool/builtin` |
| `write_file` | ✅ v0.2 | Rig `tool/builtin` |
| `edit_file` | ✅ v0.2 | Rig `tool/builtin` |
| `str_replace_editor` | 🗺️ Roadmap (v0.4) | Rig (`tool/builtin`) — view/create/str_replace/insert with line numbers |
| `list_dir` / `glob` / `grep` | ✅ v0.2 | Rig `tool/builtin` |
| `read_image` | 🗺️ Roadmap (v0.4) | Rig — needs multimodal `ToolResult` (image content part) before it can reach the model |
| `working_directory` | ✅ v0.3 | Rig `tool/misc` (session cwd, root-checked) |

## Shell / processes

| Tool | Status | Where |
| --- | --- | --- |
| `bash` | ✅ v0.2 | Rig `tool/builtin` (`/bin/sh -c`, root sandbox, output cap, timeout) |
| `pwsh` | 🗺️ Roadmap (v0.4) | Rig `tool/builtin` — Windows shell (platform build tags) |
| `terminal_open` / `terminal_send` / `terminal_signal` / `terminal_close` | 🗺️ Roadmap (v0.4) | Rig `tool/terminal` — stateful PTY sessions; product host may also own them |
| `job_start` / `job_output` / `job_kill` / `job_list` | 🗺️ Roadmap (v0.4) | Rig `tool/jobs` — background processes owned by a manager object |
| agent sandbox (policy) | ✅ v0.3 | Rig `sandbox` — roots, read-only, command allow/deny, env allowlist, network, output/timeout caps, denied globs; wired into `tool/builtin` via `NewWithPolicy` |

## Web

| Tool | Status | Where |
| --- | --- | --- |
| `web_fetch` | ✅ v0.3 | Rig `tool/web` — HTTP(S), redirect cap, size cap, HTML→text, URL policy hook |
| `web_search` | ✅ v0.3 | Rig `tool/web` — pluggable `Searcher` interface (`SearchResult`), no built-in vendor |

## MCP / LSP

| Tool | Status | Where |
| --- | --- | --- |
| MCP tools (`mcp__<server>__<tool>`) | ✅ v0.3 | Rig `tool/mcp` — stdlib MCP stdio client (`initialize`, `tools/list`, `tools/call`, `ping`), tools adapted into `v1.Tool` |
| `mcp_list_resources` / `mcp_read_resource` / `mcp_list_prompts` / `mcp_get_prompt` | 🗺️ Roadmap (v0.4) | Rig `tool/mcp` — resources/prompts over the same client |
| `lsp_*` (hover/definition/diagnostics/symbols) | ✅ v0.3 | Rig `tool/lsp` — stdlib LSP stdio client, 1-based line mapping |

## Interaction / host capabilities (Product)

These need a UI, a session store or process state, so they stay product modules; the
SDK supplies the `v1.Tool`/`v1.Gate` interfaces they implement.

| Tool | Where | Why product |
| --- | --- | --- |
| `ask_user` | Product module | blocks on UI/user input |
| `present` (artifacts) | Product module | renders artifacts in the UI |
| `exit_plan_mode` / `plan_review` | Product module | plan-mode state + UI |
| `todo` (persistent) | Product module (Rig has in-memory `todo_write` for standalone agents, v0.3) | session persistence + UI panel |
| `goal` | Product module | goal state/lifecycle |
| `schedule_*` | Product module | scheduler + persistence |
| `memory` / `skills` | Product module | memory store / skill roots |
| `subagent` / `workflow` / `ralph` / `agent_teams` / `team_spawn` / `interrupt_agent` | Product module | subagent runner + team manager |
| `settings_get` / `settings_set` | Product module | settings store |
| `session_event_*` / `inspect` / `report` | Product module | session log/read model |
| `enter_worktree` / `exit_worktree` / `list_worktrees` | Product module | git worktree + workspace state |
| auto-review / code-review helpers | Product module | reviewer policy |

## Composition

A product assembles the rig packages with its kernel modules:

```go
// product module (kernel descriptor) that exposes the rig toolset
policy := sandbox.New(
	sandbox.WithRoots(workspace),
	sandbox.WithCommandAllowlist("sed", "rg", "go", "git"),
	sandbox.WithNetwork(false),
)
tools := tool.NewRegistry(builtin.NewWithPolicy(policy)...)
tools.Register(web.NewFetcher(policy))            // web_fetch
tools.Register(web.NewSearcher(mySearcher))        // web_search
tools.Register(mcpTools...)                        // from tool/mcp
tools.Register(lspTools...)                        // from tool/lsp
tools.Register(misc.NewWorkingDirectoryTool(root)) // working_directory
```

The product owns approval policy (`gate`), session history (`session`), the provider and
the assembly (`kernel`); Rig owns the tools themselves.
