import io

# TOOLS.md
p = 'docs/TOOLS.md'
s = io.open(p, encoding='utf-8').read()
s = s.replace('''| Static tools built by `tools/full.All()` | **73** |
| Legacy tool names tracked (`catalog.Legacy`) | **61** |
| Modern tool names tracked (`catalog.Modern`) | **61** |
| Documented gaps (`catalog.DocumentedGaps`) | 1 (`subagent_fork`) |
| Server-bound tools (need an MCP/LSP client) | 5 |''', '''| Static tools built by `tools/full.All()` | **74** |
| Legacy tool names tracked (`catalog.Legacy`) | **61** |
| Modern tool names tracked (`catalog.Modern`) | **62** |
| Documented gaps (`catalog.DocumentedGaps`) | 0 (structured_output is a subagent capability) |
| Server-bound tools (need an MCP/LSP client) | 6 |''')
s = s.replace('''| `subagent` | `tools/subagent` | one in-process child agent (`agent.NewRunner`) |''', '''| `subagent` | `tools/subagent` | one in-process child agent (`agent.NewRunner`); `output_schema` for structured output |
| `subagent_fork` | `tools/subagent` | same delegation with `Fork=true`: child inherits the parent session history (`RunnerOptions.ParentSessionID`) |
| `structured_output` | `agent` (child-context tool) | injected into structured children; arguments validated against `SubagentSpec.OutputSchema` |''')
s = s.replace('''| `mcp_list_resources`, `mcp_read_resource`, `mcp_list_prompts`, `mcp_get_prompt` | `tools/mcp` | **server-bound** (MCP client) |''', '''| `mcp_list_resources`, `mcp_read_resource`, `mcp_list_resource_templates`, `mcp_list_prompts`, `mcp_get_prompt` | `tools/mcp` | **server-bound** (MCP client) |''')
io.open(p, 'w', encoding='utf-8').write(s)

# COVERAGE.md
p = 'docs/COVERAGE.md'
s = io.open(p, encoding='utf-8').read()
s = s.replace('''# catalog complete: 73 static tools, 61 legacy names, 61 modern names, 1 documented gaps, 5 server-bound, dynamic [mcp__<server>__<tool>]''', '''# catalog complete: 74 static tools, 61 legacy names, 62 modern names, 0 documented gaps, 6 server-bound, dynamic [mcp__<server>__<tool>]''')
s = s.replace('''- **Static** — 66 tools a headless SDK build can construct with no external
  process: filesystem/editor/image, shell/terminal/jobs, web, session/inspect/
  report, host-interface tools with in-memory defaults, memory/schedule/
  worktree, orchestration and misc.''', '''- **Static** — 74 tools a headless SDK build can construct with no external
  process: filesystem/editor/image, shell/terminal/jobs, web, session/inspect/
  report, host-interface tools with in-memory defaults, memory/schedule/
  worktree/skill, orchestration (including subagent_fork) and misc.''')
s = s.replace('''- **DocumentedGaps** — modern capabilities deliberately not exposed as a tool
  yet, with the reason in this file. Currently `subagent_fork`: Rig's Runner
  starts a fresh child history instead of forking the parent conversation; use
  `subagent` with an explicit prompt.''', '''- **DocumentedGaps** — catalog-level gaps: currently none. The subagent-context
  tool `structured_output` is delivered as a capability (`SubagentSpec.OutputSchema`)
  rather than a standalone static name. The modern-surface audit keeps its own
  6 host/kernel-bound gaps, documented in `docs/AUDIT-REPORT.md`.''')
io.open(p, 'w', encoding='utf-8').write(s)

# README
p = 'README.md'
s = io.open(p, encoding='utf-8').read()
s = s.replace('go get github.com/zeokeel/zeokeel-rig@v0.7.0', 'go get github.com/zeokeel/zeokeel-rig@v0.8.0')
s = s.replace('''- **v0.8**: MCP SSE subscriptions/OAuth, workflow DSL (conditions/loops),
  Windows ConPTY, cross-process subagents, compaction/SQLite history.''', '''- **v0.8.0** (this tag): audit gaps closed — MCP resource templates
  (`mcp_list_resource_templates`), subagent fork (`subagent_fork` +
  `RunnerOptions.ParentSessionID`), structured child output
  (`SubagentSpec.OutputSchema` + `structured_output` tool), 74 static tools,
  modern-surface audit missing=0 (6 remaining gaps are host/kernel-bound).
- **v0.9**: MCP SSE subscriptions/OAuth, workflow DSL (conditions/loops),
  Windows ConPTY, PTY resize, cross-process subagents, compaction/SQLite history.''')
s = s.replace('''| `tools/skill` / `tools/review` | `skill` (list/search/load skill docs) and `code_review` / `auto_review` over `v1.Reviewer` (SubagentReviewer adapter). |''', '''| `tools/skill` / `tools/review` | `skill` (list/search/load skill docs) and `code_review` / `auto_review` over `v1.Reviewer` (SubagentReviewer adapter). |
| `agent.NewRunner` fork/structured | `subagent_fork` tool + `RunnerOptions.ParentSessionID`, and `SubagentSpec.OutputSchema` with the child-context `structured_output` tool. |
| `tools/mcp` resource templates | `mcp_list_resource_templates` (resources/templates/list, server-bound). |''')
io.open(p, 'w', encoding='utf-8').write(s)

# CHANGELOG
p = 'CHANGELOG.md'
s = io.open(p, encoding='utf-8').read()
head = '# Changelog\n\n'
entry = '''## v0.8.0 — 2026-11-06

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

'''
s = s.replace(head, head + entry, 1)
io.open(p, 'w', encoding='utf-8').write(s)

# COMPATIBILITY
p = 'COMPATIBILITY.md'
s = io.open(p, encoding='utf-8').read()
s = s.replace('| v0.7.x | 1.27 | none |', '| v0.8.x | 1.27 | none |\n| v0.7.x | 1.27 | none |')
io.open(p, 'w', encoding='utf-8').write(s)
print('docs updated')
