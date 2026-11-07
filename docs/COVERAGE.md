# Coverage gate — "is any tool missing?"

> 中文文档以 [README.md](../README.md) 与 [docs/zh/](zh/README.md) 为主；本文件保留英文原文作为参考，对应中文版见 [docs/zh/COVERAGE.md](zh/COVERAGE.md)。

The SDK keeps a machine-readable record of the complete agent tool surface and
fails CI when something goes missing. This is the process every release runs.

## Pieces

| Piece | Location | Purpose |
| --- | --- | --- |
| Ledger | `tools/catalog/catalog.go` | `Static` (must be constructible), `Legacy` (exact old agent surface), `ServerBound` (needs an MCP/LSP client), `Dynamic` (runtime name patterns) |
| Aggregator | `tools/full/full.go` | `full.All(options)` builds every static tool |
| Gate | `tools/full/catalog_test.go` | `TestToolCatalogCoverage` asserts: every `Static` name exists, every legacy name is either built or explicitly server-bound, no duplicate names |
| Modern-surface audit | `tools/audit/` | `Reference` (DSH built-ins), `Mappings`, `DocumentedGaps` (host/kernel-bound), `Modern`, `ServerBound`, `Dynamic`; gate: `tools/audit/modern_test.go` |
| CI | `.github/workflows/ci.yml` | runs the gate explicitly after the normal test/race steps |

Run it locally:

```bash
go test ./tools/full -run TestToolCatalogCoverage -count=1 -v
# catalog complete: 81 static tools, 61 legacy names, 65 modern names, 0 documented gaps, 6 server-bound, dynamic [mcp__<server>__<tool>]
```

## The coverage lists

- **Static** — 81 tools a headless SDK build can construct with no external
  process: filesystem/editor/image, shell/terminal/jobs, web, session/inspect/
  report, host-interface tools with in-memory defaults, memory/schedule/
  worktree/skill, orchestration (including subagent_fork) and misc.
- **Legacy** — the 61 tool names extracted from the previous ZeoKeel agent
  (`old/internal/tools` plus the runtime `settings_get`/`settings_set`). They are
  kept for compatibility; removing one is a breaking change that must be
  announced in `CHANGELOG.md` (none are scheduled for removal in the current
  line). The audit fails if any name disappears and is not listed in
  `ServerBound`.
- **ServerBound** — 6 tools that only exist while an MCP server or language
  server is configured (`mcp_list_resources`, `mcp_read_resource`,
  `mcp_list_resource_templates`, `mcp_list_prompts`, `mcp_get_prompt`, legacy
  `lsp`). Their constructors and tests live in `tools/mcp` / `tools/lsp`.
- **Modern** — 65 tool names a current-generation coding agent is expected to
  expose (DSH-style surface). `catalog.AuditModern` runs in the same test.
- **DocumentedGaps** — catalog-level gaps: currently none. The subagent-context
  tool `structured_output` is delivered as a capability (`SubagentSpec.OutputSchema`)
  rather than a standalone static name. The modern-surface audit keeps its own
  6 host/kernel-bound gaps, documented in `docs/AUDIT-REPORT.md`.
- **Dynamic** — `mcp__<server>__<tool>` names are generated from each server's
  own tool list at runtime.

## Modern-surface audit (`tools/audit`)

The catalog gate answers "did any legacy/static tool disappear?". The
modern-surface audit answers the complementary question: "does today's Rig tool
surface still cover the reference modern agent (DeepSeek Harness)?". It was
introduced in v0.7 and re-run every release:

- `Reference` — 63 model-facing built-ins extracted from the installed
  `@deepseek-ai/dsh 0.2.1-rc.2` (tool packages + `toolName` config overrides +
  constant-registered tools; extraction script in `docs/AUDIT-REPORT.md`).
- `Mappings` — 57 of the 63 reference tools mapped to the Rig tool/capability
  that covers them (a Rig tool may cover several reference tools, e.g. `goal`
  covers `create_goal`/`get_goal`/`update_goal`).
- `Modern` — 55 static Rig names the audit requires; `Missing()` must be empty.
- `ServerBound` — 10 Rig MCP/LSP names that exist only with an external client
  (including the v0.8 `mcp_list_resource_templates`), so they are not asserted
  against the static build.
- `DocumentedGaps` — 6 remaining capabilities, each with a machine-readable
  `Category`: `kernel-bound` (2: `cordis_inspect_list`, `cordis_inspect_query`;
  needs a product that owns a module runtime) or `host-bound` (4:
  `load_workspace_dependencies`, `plugin_manager`, `plugin_market`, `run_code`;
  process/product concerns). `run_code` is available as an **opt-in kit
  extension** (`kit.Config.PTC`; system-installed Python/Node runtime, inner
  calls checked by the Gate) but is deliberately not a static `full.All` tool,
  so the audit gap remains host-bound. Full reasons and alternatives are in
  `docs/AUDIT-REPORT.md`; the gap set and category split (2/4) are locked by
  tests so it cannot drift silently.
- `Dynamic` — `mcp__<server>__<tool>`, identical to the reference naming.

```bash
go test ./tools/audit/... -count=1 -v
# modern tool surface covered: reference=63 mapped=57 gaps=6 modern=55 missing=0 (static tools=81)
# audit tables: reference=63 mapped=57 gaps=6 modern=55 server-bound=10
```

## Adding a tool (checklist)

1. Implement it in a `tools/<name>` package (stdlib only; the root `rig`
   package interfaces for host capabilities).
2. Wire it into `tools/full/full.go` (`All`) — otherwise the gate fails.
3. Add its name to `catalog.Static`; if it comes from the old agent add it to
   `catalog.Legacy` too (or explain why it is server-bound).
4. Add tests in the tool package; run `gofmt`, `go vet ./...`,
   `go test ./... -count=1 -race`, then the coverage gate.
5. If the tool closes a modern-surface capability, update `tools/audit`
   (`Reference`/`Mappings`/`Modern`/`ServerBound`/`DocumentedGaps`), its count
   gates, and `docs/AUDIT-REPORT.md`.
6. Update `docs/TOOLS.md` and `CHANGELOG.md`; tag the release.

## Reproducible legacy audit

The packaged script diffs the old agent against the ledger (test-fixture tools
are excluded because they only live in `_test.go` files):

```bash
./scripts/audit-legacy-tools.sh /path/to/old-agent-repo
# legacy tools scanned: 58
# catalog legacy names: 61
# catalog additions (Rig-only tools, expected): exit_plan_mode, plan_review, web_search
# audit: legacy tool surface complete
```

Known test-fixture tool names (`big`, `binder`, `block`, `clock`, `echo`,
`side_effect`) are never real tools and are filtered by the scan; if the old
repo grows a new real tool, this script fails with the name to add.

## Auditing a newer legacy surface

If another tool appears in an older product line, extract its names and diff:

```bash
# old repo
grep -rhA1 'Name() string' old/internal/tools/*.go | grep -oE '"[a-z_0-9]+"' | tr -d '"' | sort -u
```

Compare with `catalog.Legacy` (the `TestToolCatalogCoverage` test prints any
name that is missing and not server-bound).
