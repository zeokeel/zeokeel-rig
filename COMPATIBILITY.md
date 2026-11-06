# Compatibility

ZeoKeel Rig pins its only dependency, the ZeoKeel kernel, in `go.mod`. This table
records the tested ranges; a `v0.x` SDK minor may require a newer kernel.

| Rig | Kernel | Go (min) | Notes |
| --- | --- | --- | --- |
| v0.1.x | v0.1.1 | 1.27 | first public SDK tag: agent loop, OpenAI-compatible provider, built-in tools, assembly/host. |

Policy:

- Kernel upgrades are a normal dependency bump of this repo; the SDK absorbs kernel
  changes in `internal/kernelx` only.
- `contracts/v1` is additive within v1. A breaking change gets `contracts/v2`.
- Consumer products should pin an exact SDK version (`v0.1.0`) and upgrade deliberately.
