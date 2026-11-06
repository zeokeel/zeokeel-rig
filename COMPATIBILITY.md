# Compatibility

ZeoKeel Rig has **no module dependencies** — only the Go standard library. There is no
dependency matrix to track; the only constraint is the Go language version.

| Rig | Go (min) | Dependencies |
| --- | --- | --- |
| v0.8.x | 1.27 | none |
| v0.7.x | 1.27 | none |
| v0.6.x | 1.27 | none |
| v0.5.x | 1.27 | none |
| v0.4.x | 1.27 | none |
| v0.3.x | 1.27 | none |
| v0.2.x | 1.27 | none |
| v0.1.x | 1.27 | zeokeel-kernel (removed in v0.2.0) |

Policy:

- The module root (`package rig`) is the stable contract surface: additive within a
  module major version. After v1.0.0 a breaking change gets a new major version at the
  import path (`github.com/zeokeel/zeokeel-rig/v2`), never a versioned subdirectory.
- Consumer products should pin an exact Rig version and upgrade deliberately; `v0.x` may break.
- Products that want compile-time module assembly use a kernel (e.g.
  [zeokeel-kernel](https://github.com/zeokeel/zeokeel-kernel)) themselves; Rig does not
  depend on or recommend a specific runtime.
