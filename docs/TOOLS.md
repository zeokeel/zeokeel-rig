# Tool catalog

> 中文文档以 [README.md](../README.md) 与 [docs/zh/](zh/README.md) 为主；本文件保留英文原文作为参考，对应中文版见 [docs/zh/TOOLS.md](zh/TOOLS.md)。

Every tool the previous ZeoKeel agent exposed now lives in this module, plus a
few additions. This file is the human-readable half of the coverage gate; the
machine-readable ledger is [`tools/catalog`](../tools/catalog/catalog.go) and
the release check is `go test ./tools/full` (`TestToolCatalogCoverage`).

## Coverage at a glance

Three counts that are easy to confuse — they answer different questions:

| | Count |
| --- | --- |
| Tools `kit.New(kit.Config{})` **registers** with zero config | **37** |
| Tools `kit.New(kit.Config{})` keeps **active** with zero config (dynamic tool loading) | **16** (17 with `Presenter`) |
| Static tools built by `tools/full.All(AllOptions{Root: …})` | **81** |
| Same, with `SandboxGrants: true` | **83** |
| Ledger total (`catalog.Static` / `presets["all"]`) | **83** |
| Opt-in sandbox tools (`AllOptions.SandboxGrants`) | **2** (`sandbox_status`, `request_sandbox_access`) |
| Legacy tool names tracked (`catalog.Legacy`) | **58** |
| Modern tool names tracked (`catalog.Modern`) | **62** |
| Documented gaps (`catalog.DocumentedGaps`) | 0 (structured_output is a subagent capability) |
| Server-bound tools (need an MCP/LSP client) | 6 |
| Dynamic name patterns | `mcp__<server>__<tool>` |

How the 37/16/81/83 relate: the `personal` preset is a name list of 55; `kit.New`
subtracts the two sandbox-grant tools until `SandboxGrants: true`, `undo` until a
tracker exists (`GitUndo: true` in a git work tree), and Zero-config health check (an easy-to-miss default in `kit.New`): any `personal`-preset tool that can never succeed without a host dependency is **dropped from the default set** — `web_search` without `Searcher`, five `session_*` tools without `SessionLog`, and `get_goal`/`create_goal`/`update_goal`/`plan_review`/`exit_plan_mode`/`subagent`/`subagent_fork` always (kit has no injection point for them), plus `ask_user` without `Asker`, `present` without `Presenter` (15 in total). The rule is "every tool in the default set must actually work". That
leaves 37 with zero config; host wiring adds them back one by one (`Asker` +1,
`Searcher` +1, `SessionLog` +5, `SandboxGrants` +2, `PTC` +1, and `GitUndo` in a
git work tree +1). `full.All` applies no preset filter (81, 83 with grants);
`catalog.Static` is the ledger and counts the two grant tools (83).

The **registered vs active** split is dynamic tool loading (`kit.Config.ToolDiscovery`,
on by default): 38 tools are registered but only 16 are active — the core 14 plus
`tool_search` (the discovery entry point) and `memory`. The rest are registered but
not sent in `Request.Tools`; the model loads them on demand with `tool_search`.
The 16 are the core 13 plus `tool_search`, `memory` and `skill`; `present` is gated on
`Config.Presenter` (no presenter → the tool is not registered at all).
`Agent.RegisteredTools()` lists all 38, `Agent.Tools()` lists the 16 currently
active. Re-check with
`go test ./tools/full -run TestToolCatalogCoverage -v`, which prints the canonical
`catalog complete: …` line.

⚠️ **Zero-value trap**: `full.All(full.AllOptions{})` — no `Root` and no
`Policy` — builds only **77** tools. `apply_patch`, `search` and `undo` are
workspace-scoped and are silently *not constructed*, and every file tool answers
`sandbox: denied: no roots configured`. Always pass `Root` or a `Policy` that has
roots.

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
| `apply_patch` | `tools/patch` | atomic multi-file patches (unified diff or `*** Begin Patch`), 3-level context matching, ambiguity refused, dry-run; limits: patch ≤ 1 MiB, per-file ≤ 16 MiB, ≤ 200 files; options `WithPolicy` (required for read-only/deny-glob enforcement), `WithMaxPatchBytes`, `WithMaxFileBytes`, `WithMaxFiles`, `WithDryRun`. Updates write *through* symlinks; delete/move act on the link itself; insert-only hunks are not idempotent |
| `search` | `tools/search` | fuzzy filename ranking + BM25 content search + `@path` mention resolution; ignores VCS/binary/large files |
| `undo` | `tools/undo` | **user-facing undo button**: revert one turn's file changes from a git snapshot (hidden commit, HEAD untouched); git-only (non-repos degrade to "no undo"); `.gitignore` respected; new untracked files removed, untracked-before files restored; submodules reported, not reverted |
| `read_image` | `tools/image` | magic-byte MIME, image in `ToolResult.Parts` |
| `skill` | `tools/skill` | list/search/load skill documents (`SKILL.md` dirs or injected `rig.SkillStore`) |

## Shell / processes

| Tool | Package | Notes |
| --- | --- | --- |
| `bash` | `tools/builtin` | policy-checked commands, env allowlist, timeout; DSH `description`/`run_in_background`/promotion + `sandbox_permissions` rejected without an executor; description follows runner injection; sandbox-denial attribution on failed results |
| `pwsh` | `tools/builtin` | pwsh/powershell detection, `-NoProfile -NonInteractive -Command`; same DSH background/description surface as bash (incl. runner-aware description and denial attribution) |
| `terminal_open`, `terminal_send`, `terminal_read`, `terminal_signal`, `terminal_close`, `terminal_list` | `tools/terminal` | real unix PTY (pipe fallback), ANSI strip, Ctrl-C; DSH `sessionId`/`text`/`submit`/`offset`/`count`/`type`/`name` aliases, run_in_background sends, SIG* signals |
| `job_start`, `job_output`, `job_kill`, `job_list`, `job_wait` | `tools/jobs` | background processes; DSH `wait`/`timeout_ms`/`reason` args, kinds/labels/statuses, completion notices; sandbox-denial attribution line on failed job views |

With an opt-in OS sandbox runner attached (`sandbox/osrunner`), the bash/pwsh
description states the OS-sandbox boundary instead of "not an OS sandbox". A
result whose merged output matches one of the runner's denial signatures gains a
structured attribution block (bash/pwsh foreground, promoted and background
paths; `job_output`/`job_wait`/`job_kill`; terminal send/read; worktree git
errors):

```text
[sandbox: denied create to /home/me/.some-tool/creds.json (matched "read-only file system"; mode workspace-write, enforcement full)]
workspace roots: /repo
additional writable roots: (none; only the workspace roots and platform temporary areas are writable)
1. Prefer redirecting the tool's own state into the workspace (...)
2. If the tool only writes to a fixed absolute path, request a writable root for exactly that directory (...)
3. Do not repeat the same failing command unchanged; a denial is deterministic under the same policy.
```

Attribution no longer requires a non-zero exit code (CLIs often wrap the
filesystem error and exit 0); results that match nothing, killed jobs and
`ctx`-cancelled runs are byte-for-byte unchanged. See
[SANDBOX-OS.md](SANDBOX-OS.md) §5.

### Sandbox observability and runtime grants (opt-in)

| Tool | Package | Notes |
| --- | --- | --- |
| `sandbox_status` | `tools/sandboxstatus` | read-only JSON view of the effective policy: mode, workspace roots, additional writable roots, temp toggles, protected paths, network, approved grants and remaining grant budget |
| `request_sandbox_access` | `tools/sandboxgrant` | asks for a **session-scoped, path-scoped** writable root (absolute directory + a reason of 8–500 chars) when a tool must keep credentials or cache at a fixed path; it never approves itself — `rig.Gate` decides, and the default gate denies it. Validation is fail-closed (no `..`, no symlink escape, no system directories, no `$HOME` itself, no ancestor of the workspace root, no overlap with protected paths) and the grant budget defaults to 5 |

Both are constructed only when `tools/full.AllOptions.SandboxGrants` (or
`kit.Config.SandboxGrants`) is enabled, so the default tool set is unchanged.
See [SANDBOX-OS.md](SANDBOX-OS.md) §6.

## Web / MCP / LSP

| Tool | Package | Notes |
| --- | --- | --- |
| `web_fetch` | `tools/web` | HTTP(S), redirect/size caps, HTML→text, URL policy |
| `web_search` | `tools/web` | pluggable `Searcher`; nil backend reports a config error |
| `mcp_list_resources`, `mcp_read_resource`, `mcp_list_resource_templates`, `mcp_list_prompts`, `mcp_get_prompt` | `tools/mcp` | **server-bound** (MCP client) |
| `lsp` (legacy action router) | `tools/lsp` | **server-bound** (LSP client) |
| `lsp_hover`, `lsp_definition`, `lsp_references`, `lsp_implementation`, `lsp_diagnostics`, `lsp_symbols` | `tools/lsp` | constructed with an LSP client; position args are 1-based, UTF-16 validated |
| `mcp__<server>__<tool>` | `tools/mcp` | **dynamic**, generated from each server's `tools/list` |

## Orchestration

| Tool | Package | Notes |
| --- | --- | --- |
| `subagent`, `subagent_fork` | `tools/team` (DSH surface) | async managed child: returns target id immediately and delivers completion notices via `rig.NoticeSource`; `subagent_fork` inherits the parent session history (`RunnerOptions.ParentSessionID`). The legacy synchronous constructors remain in `tools/subagent`, but `full.All` wires the async `tools/team` versions |
| `structured_output` | `agent` (child-context tool) | injected into structured children; arguments validated against `SubagentSpec.OutputSchema` |
| `workflow` | `tools/workflow` | Go-native declarative orchestration (sequential/parallel, `{{prev}}`/`{{steps.<name>}}`); no JavaScript engine |
| `ralph` | `tools/ralph` | iterate a child agent until `DONE`/round limit |
| `team_spawn`, `team_list`, `team_send`, `team_wait`, `team_last`, `team_close`, `team_interrupt` | `tools/team` | managed subagents |
| `spawn_teammate`, `list_agents`, `wait_agent` | `tools/team` | DSH names added; send_message is non-blocking, wait_agent waits for the next team change |
| `get_goal`, `create_goal`, `update_goal` | `tools/hosttools` | DSH revision-CAS goal tools (legacy `goal` retained) |
| `spawn_team`, `list_teams`, `team_status`, `close_team`, `wait_team`, `send_message`, `interrupt_agent`, `list_subagent_models` | `tools/team` (`legacy.go`) | legacy tool names mapped onto the manager |
| `team_task_create`, `team_task_list`, `team_task_get`, `team_task_update` | `tools/team` (`tasks.go`) | shared task board over `rig.TeamTaskManager` (in-memory default) |
| `code_review`, `auto_review` | `tools/review` | `rig.Reviewer` (SubagentReviewer adapter) with verdict/findings/blocking |

## Session / diagnostics

| Tool | Package | Notes |
| --- | --- | --- |
| `session_search`, `session_trace`, `session_event_read`, `session_event_search`, `session_event_trace` | `tools/session` | over `rig.SessionLog`; in-memory log included |
| `inspect` | `tools/inspect` | sessions/events/workspace/stats |

## Host interaction (interfaces)

| Tool | Package | Interface |
| --- | --- | --- |
| `ask_user` | `tools/hosttools` | `rig.Asker` (`Interactive=false` → "no interactive UI") |
| `get_goal`, `create_goal`, `update_goal` | `tools/hosttools` | DSH three-tool goal surface with revision CAS via optional `hosttools.GoalRevisionStore`; `goal` remains as a legacy extension |
| `present` | `tools/hosttools` | DSH-compatible: declares existing files as deliverables (`rig.FilePresenter`) |
| `plan_review`, `exit_plan_mode` | `tools/hosttools` | `rig.PlanController` |
| `goal` | `tools/hosttools` | `rig.GoalStore` (+ optional `rig.GoalUpdater` for complete/blocked/pause/resume/edit) |
| `run_code` (opt-in) | `tools/ptc` via `kit.Config.PTC` | model-authored Python 3/Node.js program in a system runtime; inner calls go through the Gate; when `kit.Config.SandboxRunner` is set the interpreter is wrapped by the OS runner just like `bash` (same policy; the program file is placed **inside the workspace root** so a confined child can read it, and denials get the same structured attribution) |

These tools need the host to supply the corresponding implementation: without
one they report a readable "host did not configure" tool error (memory, scheduler
and team-task board have in-memory fallbacks; host-interface, session,
runner, reviewer and searcher tools do not). Use `hosttools.NewMemoryHost()`,
`session.NewMemory()`, a `rig.SubagentRunner`, a `rig.Reviewer` and a
`web.Searcher` when a standalone agent should work out of the box. A product can
swap in its UI/store implementations without changing tool names or schemas.

## Memory / scheduling / worktrees / misc

**Memory is a progressive wiki, partitioned per workspace.** `index` returns only
`name — summary (age)` lines (never page bodies), `read` fetches a single page (8 KiB cap),
and `search` returns metadata only. Storage lives in the *user* state directory
(`$XDG_STATE_HOME/rig/memory/<workspaceID>/`, `%LOCALAPPDATA%\rig` on Windows), not in the
repository: memories may contain things that should not be committed, and a `.rig/`
directory would show up in `git status`. `kit.Config.Memory` accepts any
`memory.Wiki`; `nil` auto-creates the per-workspace file wiki when `Root` is set.

| Tool | Package | Notes |
| --- | --- | --- |
| `memory` | `tools/memory` | progressive wiki memory: `index` (metadata only) / `read` (one page, 8 KiB cap) / `write` / `append` (dated "## Log") / `search` (metadata only) / `delete`, partitioned per workspace |
| `schedule_create`, `schedule_list`, `schedule_update`, `schedule_delete` | `tools/schedule` | CRUD over `rig.Scheduler`; cron evaluation is the host's job |
| `enter_worktree`, `exit_worktree`, `list_worktrees` | `tools/worktree` | git worktree, sandbox-checked, porcelain parsing |
| `working_directory` | `tools/misc` | root-checked cwd |
| `todo_write`, `todo` | `tools/misc` | shared store; `todo` is the legacy name |

## Building a registry

```go
policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	sandbox.WithCommandAllowlist("ls", "git", "go"))
host := hosttools.NewMemoryHost()
devices := full.All(full.AllOptions{
	Policy: policy,
	Root:   root,
	Host: hosttools.Options{ // or drive a real UI
		Asker: host, Presenter: host, FilePresenter: host, Plan: host, Goal: host,
		Interactive: true,
	},
	Runner: agentRunner, // agent.NewRunner(...)
	Models: []string{"gpt-4o-mini"},
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
