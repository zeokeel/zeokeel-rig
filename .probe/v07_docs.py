import io

# README
p = 'README.md'
s = io.open(p, encoding='utf-8').read()
s = s.replace('go get github.com/zeokeel/zeokeel-rig@v0.6.0', 'go get github.com/zeokeel/zeokeel-rig@v0.7.0')
s = s.replace('''  memory/          memory tool + in-memory store
  schedule/        schedule_create/list/update/delete + in-memory scheduler''', '''  memory/          memory tool + in-memory store
  skill/           skill tool + filesystem SkillStore (SKILL.md directories)
  schedule/        schedule_create/list/update/delete + in-memory scheduler''')
s = s.replace('''  ralph/           iterated subagent loop until DONE''', '''  ralph/           iterated subagent loop until DONE
  review/          code_review / auto_review + SubagentReviewer''')
s = s.replace('''- **v0.7**: MCP SSE subscriptions/OAuth, workflow DSL (conditions/loops),
  Windows ConPTY, cross-process subagents, compaction/SQLite history,
  `tools/full` convenience presets.''', '''- **v0.7.0** (this tag): modern-surface gap close — `skill`, team task board
  (`team_task_*`), full goal lifecycle (`goal action=update`), code review
  (`code_review`/`auto_review` + `SubagentReviewer`), and a modern-surface audit
  (`tools/audit`, `catalog.Modern`) alongside the legacy audit.
- **v0.8**: MCP SSE subscriptions/OAuth, workflow DSL (conditions/loops),
  Windows ConPTY, cross-process subagents, compaction/SQLite history.''')
s = s.replace('''| `tools/full` + `tools/catalog` | `full.All()` returns every static tool; `catalog.Check/AuditLegacy` is the release coverage gate (`go test ./tools/full`). |''', '''| `tools/full` + `tools/catalog` | `full.All()` returns every static tool; `catalog.Check/AuditLegacy/AuditModern` is the release coverage gate (`go test ./tools/full`). |
| `tools/skill` / `tools/review` | `skill` (list/search/load skill docs) and `code_review` / `auto_review` over `v1.Reviewer` (SubagentReviewer adapter). |
| `tools/team` (tasks) | `team_task_create/list/get/update` shared task board over `v1.TeamTaskManager`. |''')
io.open(p, 'w', encoding='utf-8').write(s)

# TOOLS.md
p = 'docs/TOOLS.md'
s = io.open(p, encoding='utf-8').read()
s = s.replace('''| Static tools built by `tools/full.All()` | **66** |
| Legacy tool names tracked (`catalog.Legacy`) | **61** |
| Server-bound tools (need an MCP/LSP client) | 5 |''', '''| Static tools built by `tools/full.All()` | **73** |
| Legacy tool names tracked (`catalog.Legacy`) | **61** |
| Modern tool names tracked (`catalog.Modern`) | **61** |
| Documented gaps (`catalog.DocumentedGaps`) | 1 (`subagent_fork`) |
| Server-bound tools (need an MCP/LSP client) | 5 |''')
s = s.replace('''| `read_image` | `tools/image` | magic-byte MIME, image in `ToolResult.Parts` |''', '''| `read_image` | `tools/image` | magic-byte MIME, image in `ToolResult.Parts` |
| `skill` | `tools/skill` | list/search/load skill documents (`SKILL.md` dirs or injected `v1.SkillStore`) |''')
s = s.replace('''| `spawn_team`, `list_teams`, `team_status`, `close_team`, `wait_team`, `send_message`, `interrupt_agent`, `list_subagent_models` | `tools/team` (`legacy.go`) | legacy tool names mapped onto the manager |''', '''| `spawn_team`, `list_teams`, `team_status`, `close_team`, `wait_team`, `send_message`, `interrupt_agent`, `list_subagent_models` | `tools/team` (`legacy.go`) | legacy tool names mapped onto the manager |
| `team_task_create`, `team_task_list`, `team_task_get`, `team_task_update` | `tools/team` (`tasks.go`) | shared task board over `v1.TeamTaskManager` (in-memory default) |
| `code_review`, `auto_review` | `tools/review` | `v1.Reviewer` (SubagentReviewer adapter) with verdict/findings/blocking |''')
s = s.replace('''| `goal` | `tools/hosttools` | `v1.GoalStore` |''', '''| `goal` | `tools/hosttools` | `v1.GoalStore` (+ optional `v1.GoalUpdater` for complete/blocked/pause/resume/edit) |''')
io.open(p, 'w', encoding='utf-8').write(s)

# COVERAGE.md
p = 'docs/COVERAGE.md'
s = io.open(p, encoding='utf-8').read()
s = s.replace('''# catalog complete: 66 static tools, 61 legacy names, 5 server-bound, dynamic [mcp__<server>__<tool>]''', '''# catalog complete: 73 static tools, 61 legacy names, 61 modern names, 1 documented gaps, 5 server-bound, dynamic [mcp__<server>__<tool>]''')
s = s.replace('''- **Dynamic** — `mcp__<server>__<tool>` names are generated from each server's
  own tool list at runtime.''', '''- **Modern** — 61 tool names a current-generation coding agent is expected to
  expose (DSH-style surface). `catalog.AuditModern` runs in the same test.
- **DocumentedGaps** — modern capabilities deliberately not exposed as a tool
  yet, with the reason in this file. Currently `subagent_fork`: Rig's Runner
  starts a fresh child history instead of forking the parent conversation; use
  `subagent` with an explicit prompt.
- **Dynamic** — `mcp__<server>__<tool>` names are generated from each server's
  own tool list at runtime.''')
io.open(p, 'w', encoding='utf-8').write(s)

# CHANGELOG
p = 'CHANGELOG.md'
s = io.open(p, encoding='utf-8').read()
head = '# Changelog\n\n'
entry = '''## v0.7.0 — 2026-11-06

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

'''
s = s.replace(head, head + entry, 1)
io.open(p, 'w', encoding='utf-8').write(s)

# COMPATIBILITY
p = 'COMPATIBILITY.md'
s = io.open(p, encoding='utf-8').read()
s = s.replace('| v0.6.x | 1.27 | none |', '| v0.7.x | 1.27 | none |\n| v0.6.x | 1.27 | none |')
io.open(p, 'w', encoding='utf-8').write(s)
print('docs updated')
