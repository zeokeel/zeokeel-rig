# Memory and spilled results

Two optional capabilities let an agent keep state that outlives one context window.
Both are **contracts in public packages** plus reference implementations, so a host
can plug its own storage in.

| Capability | Contract | Reference implementations |
| --- | --- | --- |
| Long-term notes | `memory.Wiki` (`github.com/zeokeel/zeokeel-rig/memory`) | `internal/memwiki` (files on disk, in-process) and `session/sqlite` (one table, `rig_memory_pages`, in the session database) |
| Oversized tool results | `rig.SpillStore` (`SpillPolicy.Store`) | a directory of files (default) and `session/sqlite` (`Spill()`); `spill_read` reads them back |

## Long-term notes (`memory`)

A progressive wiki: `Index` returns one metadata line per page (`name — title — summary`),
`Read` fetches one page's body, and `Search` matches in title, summary or body while
still returning metadata only. The point is that a model can survey dozens of pages
for a couple of kilobytes and only pay for the bodies it actually reads.

```go
wiki := store.Memory()                     // session/sqlite
// or memwiki.NewFileWiki(...) / memwiki.NewMemoryWiki() for the built-in ones

if err := wiki.Write(ctx, memory.Page{
    Name:    "build-flaky-tests",
    Title:   "Flaky build tests",
    Summary: "Which tests are flaky and how we retried them",
    Content: "# Flaky build tests\n…",
}); err != nil { /* ErrInvalidName / ErrSummaryRequired / ErrSummaryTooLong */ }

metas, _ := wiki.Index(ctx)        // metadata only; no bodies
page, err := wiki.Read(ctx, "build-flaky-tests")
```

Contract rules worth knowing before writing a backend:

- names are slugs `[a-z0-9-]{1,64}`, never silently rewritten;
- a summary is required (the index only shows summaries) and capped at `MaxSummaryBytes`;
- `Write` on an existing page appends `- YYYY-MM-DD rewritten (was N bytes)` to the
  `## Log` section; `Append` only appends (it is not a rewrite and records nothing);
- `Append` to a page that does not exist is `ErrNotFound` — it never creates a page;
- `Index` returns **every** page: truncation (and the "… and N more" line) belongs to
  the tool layer;
- `Search` ranks title hits over summary hits over body-only hits, then by recency.
  Body filtering reads every candidate row (a full-table scan); at personal-client
  scale that is fine, and a full-text index would be a separate contract change.

## Spilled tool results (`rig.SpillStore`)

When a tool result exceeds `SpillPolicy.ThresholdBytes`, the agent replaces it with a
short summary; the model sees a `ref:` handle instead of the payload.

```go
a, _ := kit.New(kit.Config{
    Spill: &rig.SpillPolicy{Store: store.Spill(), MaxFiles: 200},
})
```

- `Save` stores the payload and returns a handle; `Load` reads it back; `GC(keep)`
  keeps the newest rows.
- **Reading back**: `tools/full.All` registers the `spill_read` tool whenever
  `AllOptions.SpillStore` is set (kit passes `SpillPolicy.Store` through), so the
  `ref:` the model sees always has a way back. A host that assembles its own tool
  list must provide an equivalent entry point.
- **Collection**: the file backend prunes to `MaxFiles` after every write; the store
  backend gets one `GC(MaxFiles)` call after every `Save` (see `agent/spill.go`).
  A host using a `SpillStore` directly, without the agent layer, must call `GC` itself.
