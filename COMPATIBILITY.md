# Compatibility

ZeoKeel Rig has **no module dependencies** — only the Go standard library. There is no
dependency matrix to track; the only constraint is the Go language version.

| Rig | Go (min) | Dependencies |
| --- | --- | --- |
| v0.3.x | 1.27 | none |
| v0.2.x | 1.27 | none |
| v0.1.x | 1.27 | zeokeel-kernel (removed in v0.2.0) |

Policy:

- `contracts/v1` is additive within v1; a breaking change gets `contracts/v2`.
- Consumer products should pin an exact Rig version and upgrade deliberately; `v0.x` may break.
- Products that want compile-time module assembly use a kernel (e.g.
  [zeokeel-kernel](https://github.com/zeokeel/zeokeel-kernel)) themselves; Rig does not
  depend on or recommend a specific runtime.
