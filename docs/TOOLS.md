# Tool catalog

Every tool the previous ZeoKeel agent exposed now lives in this module, plus a
few additions. This file is the human-readable half of the coverage gate; the
machine-readable ledger is [`tools/catalog`](../tools/catalog/catalog.go) and
the release check is `go test ./tools/full` (`TestToolCatalogCoverage`).

## Coverage at a glance

| | Count |
| --- | --- |
| Static tools built by `tools/full.All()` | **81** |
| Legacy tool names tracked (`catalog.Legacy`) | **61** |
| Modern tool names tracked (`catalog.Modern`) | **65** |
| Documented gaps (`catalog.DocumentedGaps`) | 0 (structured_output is a subagent capability) |
| Server-bound tools (need an MCP/LSP client) | 6 |
| Dynamic name patterns | `mcp__<server>__<tool>` |

Platform note: the `bash`, `jobs` and `terminal_*` tools require a unix `/bin/sh`
and PTY; on Windows they report a clear configuration error (use `pwsh`). Every
other tool is platform-independent. The shell/job/terminal/worktree tools obey
the policy permission mode: `read-only` disables them, `workspace-write` confines
cwd/arguments to the workspace and the command allowlist, and
`danger-full-access` removes those fences. LSP/MCP server processes are
host-configured and host-trusted.

Status legend: **SDK** = shipped in this module; **server-bound** = shipped but
only constructed when a server/client is configured; **host interface** = the
tool is shipped but a product injects the UI/store implementation via
`rig` package interfaces (memory, scheduler and team-task board have in-memory
fallbacks in `full.All`; other host interfaces are optional but not defaulted).

## Filesystem / editing / images

| Tool | Package | Notes |
| --- | --- | --- |
| `read_file`, `write_file`, `edit_file` | `tools/builtin` | sandbox-policy paths, output caps |
| `list_dir`, `glob`, `grep` | `tools/builtin` | denied globs hidden |
| `str_replace_editor` | `tools/editor` | view/create/str_replace/insert, line numbers |
| `read_image` | `tools/image` | magic-byte MIME, image in `ToolResult.Parts` |
| `skill` | `tools/skill` | list/search/load skill documents (`SKILL.md` dirs or injected `rig.SkillStore`) |

## Shell / processes

| Tool | Package | Notes |
| --- | --- | --- |
| `bash` | `tools/builtin` | policy-checked commands, env allowlist, timeout; DSH `description`/`run_in_background`/promotion + `sandbox_permissions` rejected without an executor |
| `pwsh` | `tools/builtin` | pwsh/powershell detection, `-NoProfile -NonInteractive -Command`; same DSH background/description surface as bash |
| `terminal_open`, `terminal_send`, `terminal_read`, `terminal_signal`, `terminal_close`, `terminal_list` | `tools/terminal` | real unix PTY (pipe fallback), ANSI strip, Ctrl-C; DSH `sessionId`/`text`/`submit`/`offset`/`count`/`type`/`name` aliases, run_in_background sends, SIG* signals |
| `job_start`, `job_output`, `job_kill`, `job_list`, `job_wait` | `tools/jobs` | background processes; DSH `wait`/`timeout_ms`/`reason` args, kinds/labels/statuses, completion notices |

## Web / MCP / LSP

| Tool | Package | Notes |
| --- | --- | --- |
| `web_fetch` | `tools/web` | HTTP(S), redirect/size caps, HTML→text, URL policy |
| `web_search` | `tools/web` | pluggable `Searcher`; nil backend reports a config error |
| `mcp_list_resources`, `mcp_read_resource`, `mcp_list_resource_templates`, `mcp_list_prompts`, `mcp_get_prompt` | `tools/mcp` | **server-bound** (MCP client) |
| `lsp` (legacy action router) | `tools/lsp` | **server-bound** (LSP client) |
| `lsp_hover`, `lsp_definition`, `lsp_diagnostics`, `lsp_symbols` | `tools/lsp` | constructed with an LSP client |
| `mcp__<server>__<tool>` | `tools/mcp` | **dynamic**, generated from each server's `tools/list` |

## Orchestration

| Tool | Package | Notes |
| --- | --- | --- |
| `subagent`, `subagent_fork` | `tools/team` (DSH surface) | async managed child: returns target immediately, completion notices via `rig.NoticeSource`, replay unchanged |
| `subagent_fork` | `tools/subagent` | same delegation with `Fork=true`: child inherits the parent session history (`RunnerOptions.ParentSessionID`) |
| `structured_output` | `agent` (child-context tool) | injected into structured children; arguments validated against `SubagentSpec.OutputSchema` |
| `workflow` | `tools/workflow` | Go-native declarative orchestration (sequential/parallel, `{{prev}}`/`{{steps.<name>}}`); no JavaScript engine |
| `ralph` | `tools/ralph` | iterate a child agent until `DONE`/round limit |
| `team_spawn`, `team_list`, `team_send`, `team_wait`, `team_last`, `team_close`, `team_interrupt` | `tools/team` | managed subagents |
| `spawn_teammate`, `list_agents`, `wait_agent` | `tools/team` | DSH names added; send_message is non-blocking, wait_agent waits for the next team change |
| `get_goal`, `create_goal`, `update_goal` | `tools/hosttools` | DSH revision-CAS goal tools (legacy `goal` retained) |
| `spawn_team`, `list_teams`, `team_status`, `close_team`, `wait_team`, `send_message`, `interrupt_agent`, `list_subagent_models` | `tools/team` (`legacy.go`) | legacy tool names mapped onto the manager |
| `team_task_create`, `team_task_list`, `team_task_get`, `team_task_update` | `tools/team` (`tasks.go`) | shared task board over `rig.TeamTaskManager` (in-memory default) |
| `code_review`, `auto_review` | `tools/review` | `rig.Reviewer` (SubagentReviewer adapter) with verdict/findings/blocking |

## Session / diagnostics / reporting

| Tool | Package | Notes |
| --- | --- | --- |
| `session_search`, `session_trace`, `session_event_read`, `session_event_search`, `session_event_trace` | `tools/session` | over `rig.SessionLog`; in-memory log included |
| `inspect` | `tools/inspect` | sessions/events/workspace/stats |
| `report` | `tools/report` | markdown report written inside the sandbox root |

## Host interaction (interfaces)

| Tool | Package | Interface |
| --- | --- | --- |
| `ask_user` | `tools/hosttools` | `rig.Asker` (`Interactive=false` → "no interactive UI") |
| `get_goal`, `create_goal`, `update_goal` | `tools/hosttools` | DSH three-tool goal surface with revision CAS via optional `hosttools.GoalRevisionStore`; `goal` remains as a legacy extension |
| `present` | `tools/hosttools` | DSH-compatible: declares existing files as deliverables (`rig.FilePresenter`) |
| `present_artifact` | `tools/hosttools` | renders an artifact (`rig.Presenter`) |
| `plan_review`, `exit_plan_mode` | `tools/hosttools` | `rig.PlanController` |
| `goal` | `tools/hosttools` | `rig.GoalStore` (+ optional `rig.GoalUpdater` for complete/blocked/pause/resume/edit) |
| `settings_get`, `settings_set` | `tools/settings` | `rig.Settings` |
| `run_code` (opt-in) | `tools/ptc` via `kit.Config.PTC` | model-authored Python 3/Node.js program in a system runtime; inner calls go through the Gate |

These tools need the host to supply the corresponding implementation: without
one they report a readable "host did not configure" tool error (memory, scheduler
and team-task board have in-memory fallbacks; host-interface, settings, session,
runner, reviewer and searcher tools do not). Use `hosttools.NewMemoryHost()`,
`settings.Tools(nil)`, `session.NewMemory()`, a `rig.SubagentRunner`, a
`rig.Reviewer` and a `web.Searcher` when a standalone agent should work out of
the box. A product can swap in its UI/store implementations without changing
tool names or schemas.

## Memory / scheduling / worktrees / misc

| Tool | Package | Notes |
| --- | --- | --- |
| `memory` | `tools/memory` | add/list/search/remove/clear over `rig.MemoryStore` |
| `schedule_create`, `schedule_list`, `schedule_update`, `schedule_delete` | `tools/schedule` | CRUD over `rig.Scheduler`; cron evaluation is the host's job |
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
