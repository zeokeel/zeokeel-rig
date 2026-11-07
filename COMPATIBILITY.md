# Compatibility

From v0.10.0 Rig has **one dependency**: [`mvdan.cc/sh/v3`](https://pkg.go.dev/mvdan.cc/sh/v3),
the POSIX shell parser used by the `sandbox` policy layer. Everything else is the
Go standard library, the module root stays standard-library-only, and all builds
are cgo-free (`CGO_ENABLED=0`).

| Rig | Go (min) | Dependencies |
| --- | --- | --- |
| v0.11.x | 1.27 | mvdan.cc/sh/v3 (POSIX shell parser); no cgo |
| v0.10.x | 1.27 | mvdan.cc/sh/v3 (POSIX shell parser); no cgo. `v0.10.0` also shipped the opt-in OS-sandbox experiment removed in v0.11.0 |
| v0.9.x | 1.27 | none |
| v0.8.x | 1.27.3 (patch-pinned in that tag) | none |
| v0.7.x | 1.27.3 (patch-pinned in that tag) | none |
| v0.6.x | 1.27.3 (patch-pinned in that tag) | none |
| v0.5.x | 1.27.3 (patch-pinned in that tag) | none |
| v0.4.x | 1.27.3 (patch-pinned in that tag) | none |
| v0.3.x | 1.27.3 (patch-pinned in that tag) | none |
| v0.2.x | 1.27.3 (patch-pinned in that tag) | none |
| v0.1.x | 1.27.3 (patch-pinned) | zeokeel-kernel (removed in v0.2.0) |

Policy:

- The module root (`package rig`) is the stable contract surface: additive within a
  module major version. After v1.0.0 a breaking change gets a new major version at the
  import path (`github.com/zeokeel/zeokeel-rig/v2`), never a versioned subdirectory.
- Consumer products should pin an exact Rig version and upgrade deliberately; `v0.x` may break.
- Release tags are annotated and SSH-signed from v0.9.1 onward; `.github/allowed_signers`
  lists the accepted signing key and the release workflow verifies it.
- Builds and tests must not require cgo: `CGO_ENABLED=0`. The race detector is a
  Go-tooling exception (`go test -race` needs cgo) and is not part of the library
  build or the published module.
- Products that want compile-time module assembly use a kernel (e.g.
  [zeokeel-kernel](https://github.com/zeokeel/zeokeel-kernel)) themselves; Rig does not
  depend on or recommend a specific runtime.
