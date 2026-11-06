# Tool catalog

Every tool the previous ZeoKeel agent exposed now lives in this module, plus a
few additions. This file is the human-readable half of the coverage gate; the
machine-readable ledger is [`tools/catalog`](../tools/catalog/catalog.go) and
the release check is `go test ./tools/full` (`TestToolCatalogCoverage`).

## Coverage at a glance

| | Count |
| --- | --- |
| Static tools built by `tools/full.All()` | **73** |
| Legacy tool names tracked (`catalog.Legacy`) | **61** |
| Modern tool names tracked (`catalog.Modern`) | **61** |
| Documented gaps (`catalog.DocumentedGaps`) | 1 (`subagent_fork`) |
| Server-bound tools (need an MCP/LSP client) | 5 |
| Dynamic name patterns | `mcp__<server>__<tool>` |

Status legend: **SDK** = shipped in this module; **server-bound** = shipped but
only constructed when a server/client is configured; **host interface** = the
tool is shipped but a product injects the UI/store implementation via
`contracts/v1` interfaces (in-memory defaults included).

## Filesystem / editing / images

| Tool | Package | Notes |
| --- | --- | --- |
| `read_file`, `write_file`, `edit_file` | `tools/builtin` | sandbox-policy paths, output caps |
| `list_dir`, `glob`, `grep` | `tools/builtin` | denied globs hidden |
| `str_replace_editor` | `tools/editor` | view/create/str_replace/insert, line numbers |
| `read_image` | `tools/image` | magic-byte MIME, image in `ToolResult.Parts` |
| `skill` | `tools/skill` | list/search/load skill documents (`SKILL.md` dirs or injected `v1.SkillStore`) |

## Shell / processes

| Tool | Package | Notes |
| --- | --- | --- |
| `bash` | `tools/builtin` | policy-checked commands, env allowlist, timeout |
| `pwsh` | `tools/builtin` | pwsh/powershell detection, `-NoProfile -NonInteractive -Command` |
| `terminal_open`, `terminal_send`, `terminal_read`, `terminal_signal`, `terminal_close`, `terminal_list` | `tools/terminal` | real unix PTY (pipe fallback), ANSI strip, Ctrl-C |
| `job_start`, `job_output`, `job_kill`, `job_list` | `tools/jobs` | background processes, ring buffer, exit codes |

## Web / MCP / LSP

| Tool | Package | Notes |
| --- | --- | --- |
| `web_fetch` | `tools/web` | HTTP(S), redirect/size caps, HTML→text, URL policy |
| `web_search` | `tools/web` | pluggable `Searcher`; nil backend reports a config error |
| `mcp_list_resources`, `mcp_read_resource`, `mcp_list_prompts`, `mcp_get_prompt` | `tools/mcp` | **server-bound** (MCP client) |
| `lsp` (legacy action router) | `tools/lsp` | **server-bound** (LSP client) |
| `lsp_hover`, `lsp_definition`, `lsp_diagnostics`, `lsp_symbols` | `tools/lsp` | constructed with an LSP client |
| `mcp__<server>__<tool>` | `tools/mcp` | **dynamic**, generated from each server's `tools/list` |

## Orchestration

| Tool | Package | Notes |
| --- | --- | --- |
| `subagent` | `tools/subagent` | one in-process child agent (`agent.NewRunner`) |
| `workflow` | `tools/workflow` | sequential/parallel steps + `{{prev}}`/`{{steps.<name>}}` |
| `ralph` | `tools/ralph` | iterate a child agent until `DONE`/round limit |
| `team_spawn`, `team_list`, `team_send`, `team_wait`, `team_last`, `team_close`, `team_interrupt` | `tools/team` | managed subagents |
| `spawn_team`, `list_teams`, `team_status`, `close_team`, `wait_team`, `send_message`, `interrupt_agent`, `list_subagent_models` | `tools/team` (`legacy.go`) | legacy tool names mapped onto the manager |
| `team_task_create`, `team_task_list`, `team_task_get`, `team_task_update` | `tools/team` (`tasks.go`) | shared task board over `v1.TeamTaskManager` (in-memory default) |
| `code_review`, `auto_review` | `tools/review` | `v1.Reviewer` (SubagentReviewer adapter) with verdict/findings/blocking |

## Session / diagnostics / reporting

| Tool | Package | Notes |
| --- | --- | --- |
| `session_search`, `session_trace`, `session_event_read`, `session_event_search`, `session_event_trace` | `tools/session` | over `v1.SessionLog`; in-memory log included |
| `inspect` | `tools/inspect` | sessions/events/workspace/stats |
| `report` | `tools/report` | markdown report written inside the sandbox root |

## Host interaction (interfaces)

| Tool | Package | Interface |
| --- | --- | --- |
| `ask_user` | `tools/hosttools` | `v1.Asker` (`Interactive=false` → "no interactive UI") |
| `present` | `tools/hosttools` | `v1.Presenter` |
| `plan_review`, `exit_plan_mode` | `tools/hosttools` | `v1.PlanController` |
| `goal` | `tools/hosttools` | `v1.GoalStore` (+ optional `v1.GoalUpdater` for complete/blocked/pause/resume/edit) |
| `settings_get`, `settings_set` | `tools/settings` | `v1.Settings` |

All of these ship with in-memory defaults (`hosttools.NewMemoryHost()`,
`settings.Tools(nil)`, …), so a standalone agent works out of the box and a
product can swap in its UI/store implementations without changing tool names or
schemas.

## Memory / scheduling / worktrees / misc

| Tool | Package | Notes |
| --- | --- | --- |
| `memory` | `tools/memory` | add/list/search/remove/clear over `v1.MemoryStore` |
| `schedule_create`, `schedule_list`, `schedule_update`, `schedule_delete` | `tools/schedule` | CRUD over `v1.Scheduler`; cron evaluation is the host's job |
| `enter_worktree`, `exit_worktree`, `list_worktrees` | `tools/worktree` | git worktree, sandbox-checked, porcelain parsing |
| `working_directory` | `tools/misc` | root-checked cwd |
| `todo_write`, `todo` | `tools/misc` | shared store; `todo` is the legacy name |

## Building a registry

```go
policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithCommandAllowlist("ls", "git", "go"))
devices := full.All(full.AllOptions{
	Policy:  policy,
	Root:    root,
	Host:    hosttools.Options{Interactive: true}, // or drive a real UI
	Runner:  agentRunner,                          // agent.NewRunner(...)
	Models:  []string{"gpt-4o-mini"},
})
reg := tools.NewRegistry(devices...)
```

MCP and LSP tools are added by passing `MCPClients` / `LSP` in the options (or
by calling their packages directly).

## Capability gaps (not missing tools)

These are roadmap items, not absent tools: MCP SSE subscriptions/OAuth, PTY
resize, Windows ConPTY, workflow conditions/loops, cross-process subagents,
session-log persistence (host-owned), and skill stores. The coverage gate only
tracks the tool surface.

See [COVERAGE.md](COVERAGE.md) for the gate mechanics and the add-a-tool
checklist.
