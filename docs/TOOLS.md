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
| `read_file` | ✅ v0.2 | Rig `tools/builtin` |
| `write_file` | ✅ v0.2 | Rig `tools/builtin` |
| `edit_file` | ✅ v0.2 | Rig `tools/builtin` |
| `str_replace_editor` | ✅ v0.4 | Rig `tools/editor` — view/create/str_replace/insert with line numbers |
| `list_dir` / `glob` / `grep` | ✅ v0.2 | Rig `tools/builtin` |
| `read_image` | ✅ v0.4 | Rig `tools/image` — magic-byte MIME detection, base64 data URL in `ToolResult.Parts` |
| `working_directory` | ✅ v0.3 | Rig `tools/misc` (session cwd, root-checked) |

## Shell / processes

| Tool | Status | Where |
| --- | --- | --- |
| `bash` | ✅ v0.2 | Rig `tools/builtin` (`/bin/sh -c`, root sandbox, output cap, timeout) |
| `pwsh` | ✅ v0.4 | Rig `tools/builtin` — pwsh/powershell detection, `-NoProfile -NonInteractive -Command`, policy-aware |
| `terminal_open` / `terminal_send` / `terminal_read` / `terminal_signal` / `terminal_close` | ✅ v0.5 | Rig `tools/terminal` — real unix PTY (interactive shells, Ctrl-C, ANSI strip), pipe fallback; Windows ConPTY is v0.6 |
| `job_start` / `job_output` / `job_kill` / `job_list` / `job_wait` | ✅ v0.4 | Rig `tools/jobs` — background processes with ring-buffered output, exit codes, signals |
| agent sandbox (policy) | ✅ v0.3 | Rig `sandbox` — roots, read-only, command allow/deny, env allowlist, network, output/timeout caps, denied globs; wired into `tools/builtin` via `NewWithPolicy` |

## Web

| Tool | Status | Where |
| --- | --- | --- |
| `web_fetch` | ✅ v0.3 | Rig `tools/web` — HTTP(S), redirect cap, size cap, HTML→text, URL policy hook |
| `web_search` | ✅ v0.3 | Rig `tools/web` — pluggable `Searcher` interface (`SearchResult`), no built-in vendor |

## MCP / LSP

| Tool | Status | Where |
| --- | --- | --- |
| MCP tools (`mcp__<server>__<tool>`) | ✅ v0.3/0.5 | Rig `tools/mcp` — stdio and **Streamable HTTP** transports (session id, protocol version, JSON/SSE), tools adapted into `v1.Tool` |
| `mcp_list_resources` / `mcp_read_resource` / `mcp_list_prompts` / `mcp_get_prompt` | ✅ v0.4 | Rig `tools/mcp` — resources/prompts over the same stdio client |
| `lsp_*` (hover/definition/diagnostics/symbols) | ✅ v0.3 | Rig `tools/lsp` — stdlib LSP stdio client, 1-based line mapping |

## Interaction / host capabilities (Product)

These need a UI, a session store or process state, so they stay product modules; the
SDK supplies the `v1.Tool`/`v1.Gate` interfaces they implement.

| Tool | Where | Why product |
| --- | --- | --- |
| `ask_user` | Product module (v0.4 memory implementation) | blocks on UI/user input; `Asker` interface |
| `present` (artifacts) | Product module (v0.4 memory implementation) | renders artifacts in the UI; `Presenter` interface |
| `exit_plan_mode` / `plan_review` | Product module (v0.4 state machine) | plan-mode state + UI |
| `todo` (persistent) | Product module (Rig has in-memory `todo_write`, v0.3) | session persistence + UI panel |
| `goal` | Product module (v0.4 memory store) | goal state/lifecycle |
| `schedule_*` | Product module | scheduler + persistence |
| `memory` / `skills` | Product module | memory store / skill roots |
| `subagent` / `workflow` / `team_*` | ✅ v0.5 | Rig `tools/subagent`, `tools/workflow`, `tools/team` + `agent.NewRunner` (depth/concurrency limits, orchestration tools excluded from children) |
| `ralph` / cross-process agents | 🗺️ Roadmap (v0.6) | Rig — iterative loop tool and out-of-process subagents |
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
