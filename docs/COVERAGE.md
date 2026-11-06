# Coverage gate — "is any tool missing?"

The SDK keeps a machine-readable record of the complete agent tool surface and
fails CI when something goes missing. This is the process every release runs.

## Pieces

| Piece | Location | Purpose |
| --- | --- | --- |
| Ledger | `tools/catalog/catalog.go` | `Static` (must be constructible), `Legacy` (exact old agent surface), `ServerBound` (needs an MCP/LSP client), `Dynamic` (runtime name patterns) |
| Aggregator | `tools/full/full.go` | `full.All(options)` builds every static tool |
| Gate | `tools/full/catalog_test.go` | `TestToolCatalogCoverage` asserts: every `Static` name exists, every legacy name is either built or explicitly server-bound, no duplicate names |
| CI | `.github/workflows/ci.yml` | runs the gate explicitly after the normal test/race steps |

Run it locally:

```bash
go test ./tools/full -run TestToolCatalogCoverage -count=1 -v
# catalog complete: 66 static tools, 61 legacy names, 5 server-bound, dynamic [mcp__<server>__<tool>]
```

## The three lists

- **Static** — 66 tools a headless SDK build can construct with no external
  process: filesystem/editor/image, shell/terminal/jobs, web, session/inspect/
  report, host-interface tools with in-memory defaults, memory/schedule/
  worktree, orchestration and misc.
- **Legacy** — the 61 tool names extracted from the previous ZeoKeel agent
  (`old/internal/tools` plus the runtime `settings_get`/`settings_set`). The
  audit fails if any name disappears and is not listed in `ServerBound`.
- **ServerBound** — 5 tools that only exist while an MCP server or language
  server is configured (`mcp_list_resources`, `mcp_read_resource`,
  `mcp_list_prompts`, `mcp_get_prompt`, legacy `lsp`). Their constructors and
  tests live in `tools/mcp` / `tools/lsp`.
- **Dynamic** — `mcp__<server>__<tool>` names are generated from each server's
  own tool list at runtime.

## Adding a tool (checklist)

1. Implement it in a `tools/<name>` package (stdlib only; `contracts/v1`
   interfaces for host capabilities).
2. Wire it into `tools/full/full.go` (`All`) — otherwise the gate fails.
3. Add its name to `catalog.Static`; if it comes from the old agent add it to
   `catalog.Legacy` too (or explain why it is server-bound).
4. Add tests in the tool package; run `gofmt`, `go vet ./...`,
   `go test ./... -count=1 -race`, then the coverage gate.
5. Update `docs/TOOLS.md` and `CHANGELOG.md`; tag the release.

## Reproducible legacy audit

The packaged script diffs the old agent against the ledger (test-fixture tools
are excluded because they only live in `_test.go` files):

```bash
./scripts/audit-legacy-tools.sh /data/mygo/old
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
