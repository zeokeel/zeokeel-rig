# AI-readable SDK catalog

- `tools.json` — generated from the real `tools/full.All()` constructors:
  **84 entries = 83 static tools + the opt-in `run_code`**; includes name, category,
  availability, reference-surface mapping, description, required args and full JSON Schema
  properties.
- `tools.md` — the same 84-entry catalog as a Markdown table for quick reading.
- `../AI-SDK.md` — Chinese guide: assembly recipes, permissions, capability
  categories, host interfaces and AI working rules.

Regenerate after changing tools or schemas:

```sh
go run ./cmd/rigdoc
go run ./cmd/rigdoc -check   # fails when the checked-in files are stale
```

Conditional tools are not in the static JSON: dynamic
`mcp__<server>__<tool>`, server-bound MCP/LSP tools (only with a configured
client), and the child-only `structured_output`.
