# Configuration conventions

ZeoKeel Rig is a **library**: it reads no configuration files, keeps no global
settings, and does not look at flags or a config directory. The host application
loads configuration from its own sources (environment, flags, YAML, database) and
passes plain Go values into constructors. This document describes the three
configuration styles the SDK uses, the defaults, and the precedence rules.

The recommended starting point is the one-import facade:

```go
a, err := kit.New(kit.Config{Model: "gpt-4o-mini"})
```

`kit.New` wires a provider, the full tool set, a safe approval gate, in-memory
history and the agent loop, so a host that only needs "a complete agent" does not
have to assemble the pieces. The manual packages remain available for hosts that
need a custom composition.

## The three styles

| Style | Shape | Used for | Examples |
| --- | --- | --- | --- |
| **Config struct + constructor** | `New(Config) (*T, error)` | An object whose options are resolved once at construction; a clearly named struct is easier to document and pass around than a long option list | `openai.New(openai.Config{...})`, `full.All(full.AllOptions{...})`, `kit.New(kit.Config{...})`, `builtin.NewPwshWithConfig(builtin.PwshConfig{...})`, `inspect.New(inspect.Options{...})`, `report.New(report.Options{...})` |
| **Functional options** | `New(opts ...Option)` with `WithX(...)` | Immutable policy/manager objects with documented defaults and a growing option set; options can be added without breaking callers | `sandbox.New(sandbox.WithRoots(...), ...)`, `web.NewHTTPFetcher(web.WithPolicy(...), ...)`, `terminal.NewManager(terminal.WithPolicy(...), ...)`, `jobs.NewManager(jobs.WithPolicy(...), ...)`, `lsp`/`mcp` client builders |
| **Static spec + per-call request** | `New(Spec)` then `Run(ctx, Request)` | Long-lived runtime objects where per-turn behavior differs from static configuration | `agent.New(rig.AgentSpec{...})` + `agent.Run(ctx, rig.RunRequest{...})`, `agent.NewRunner(agent.RunnerOptions{...})`, `rig.SubagentSpec`, `kit.Agent.Run` / `kit.Agent.RunText` |

Rules of thumb:

- If the value is **immutable after construction** and the set of knobs is
  stable, use a config struct.
- If the value is **immutable but the knobs will grow** (policies, managers),
  use functional options so adding an option is additive.
- If the knob is **per operation** (model override, iteration cap, approval
  gate), put it on the request, not on the object.

## Conventions shared by all styles

1. **Constructors validate and return errors.** `openai.New` rejects an invalid
   base URL; `agent.New` rejects a nil provider or empty model (wrapping
   `rig.ErrProviderRequired` / `rig.ErrModelRequired`) and duplicate tool names;
   `kit.New` returns an error instead of panicking.
2. **Options never fail; they fall back to the documented default.** Passing a
   zero/negative duration or size to a `WithX` option keeps the default (see the
   tables below); this keeps options composable.
3. **Zero values are meaningful and documented.** `openai.Config{}` is usable,
   `full.AllOptions{}` is usable, `sandbox.New()` deliberately denies
   everything. Security-relevant zero values fail closed.
4. **Configuration is immutable after construction.** A `*sandbox.Policy`, a
   `*openai.Provider`, a `*terminal.Manager` and a `*jobs.Manager` are safe for
   concurrent use; do not mutate a `Policy` after handing it to tools.
5. **Additive evolution.** New fields are appended and their zero value
   preserves the previous behavior; options are added, never repurposed; fields
   and options are removed only through the
   [deprecation policy](VERSIONING.md#deprecation-policy).
6. **Same name, same knob.** A setting that appears in several places (`Model`,
   `MaxIterations`, `Gate`) means the same thing everywhere; the more specific
   layer wins (see [precedence](#precedence)).
7. **No hidden global state.** Constructors do not read files or package-level
   mutable variables. The only environment variables the SDK itself reads are
   listed [below](#environment-variables).

## Precedence

Settings are resolved from most specific to least specific:

| Setting | Precedence (highest first) |
| --- | --- |
| Model | `rig.RunRequest.Model` → `rig.AgentSpec.Model` (agent always sends a model) → `openai.Config.DefaultModel` (only for direct `Provider.Stream` calls with an empty `Request.Model`) |
| Max iterations | `rig.RunRequest.MaxIterations` (> 0) → `rig.AgentSpec.MaxIterations` (> 0) → `agent.DefaultMaxIterations` = 20 |
| Approval gate | `rig.RunRequest.Gate` (non-nil) → `rig.AgentSpec.Gate` → nil, meaning *allow all* |
| History / session | `rig.RunRequest.SessionID` (empty = fresh history, no persistence) → `rig.AgentSpec.History` (nil = no persistence) |
| Temperature / TopP / MaxTokens | `rig.AgentSpec` only (there are no per-run sampling fields) |
| Sandbox policy | the `*sandbox.Policy` passed to the tool constructors; there is no policy global |
| Provider | `kit.Config.Provider` override → `openai.New(openai.Config{...})` built from the kit fields |

`kit.Agent` (the facade) delegates to a normal `agent.Agent` built from
`kit.Config`, so the same precedence applies; fields unset in `kit.Config` fall
back to the facade defaults below.

## Defaults reference

### `openai.Config` (provider)

`openai.New(openai.Config{})` is valid. The zero value means:

| Field | Zero value behavior | Default |
| --- | --- | --- |
| `BaseURL` | use the SDK default endpoint | `https://api.openai.com/v1` (`openai.DefaultBaseURL`); trailing slashes trimmed; scheme must be `http`/`https` |
| `APIKey` | read `APIKeyEnv` (or `OPENAI_API_KEY`) from the environment | none — an empty resolved key sends no `Authorization` header (local servers) |
| `APIKeyEnv` | read `OPENAI_API_KEY` | `openai.DefaultAPIKeyEnv` = `OPENAI_API_KEY` |
| `DefaultModel` | `Stream` requires `Request.Model` | empty; `Stream` returns an error wrapping `rig.ErrModelRequired` if neither is set |
| `Headers` | no extra headers | nil |
| `HTTPClient` | SDK client with a 60s response-header timeout; the request context bounds the streamed body | nil |
| `MaxRetries` | use the SDK default | `openai.DefaultMaxRetries` = 2; a negative value disables retries |
| `RetryBackoff` | use the SDK default | `openai.DefaultRetryBackoff` = 500ms, doubled per attempt; negative = retry immediately |
| `IncludeUsage` | always on (compatibility field) | Rig always sends `stream_options.include_usage` |

Retryable failures are HTTP 408/425/429/5xx and transport errors, honoring
`Retry-After` (delays capped at 30s). Failures after the stream is established
are not retried.

### `sandbox.New()` (policy)

`sandbox.New()` with no options **allows nothing** — no file access, no shell,
no network. Options (all optional, applied in order):

| Option | Zero/default behavior | Notes |
| --- | --- | --- |
| `WithRoots(roots...)` | no roots: every path is denied | first root is the primary root for relative paths; no arguments removes access; roots that cannot be opened are dropped (fail closed) |
| `WithReadOnly(bool)` | `false` (writes allowed if root + tool allow it) | `true` also disables shell commands, because a shell can write |
| `WithCommandAllowlist(commands...)` | no allowlist: **shell execution disabled** | matched against `argv[0]` basename; `/usr/bin/git` matches `"git"` |
| `WithCommandDenylist(commands...)` | empty | deny wins over allow |
| `WithEnvAllowlist(names...)` | `PATH`, `HOME`, `LANG` | calling it with **no names** runs commands with an empty environment |
| `WithNetwork(bool)` | `false` | `true` still only permits public HTTP(S); loopback/private/link-local/metadata are refused |
| `WithMaxOutputBytes(int)` | `sandbox.DefaultMaxOutputBytes` = 32 KiB | non-positive keeps the default |
| `WithTimeout(duration)` | `sandbox.DefaultTimeout` = 30s | non-positive keeps the default; per-call timeouts are capped by it |
| `WithDeniedGlobs(patterns...)` | none | glob patterns always denied, e.g. `**/.env`, `**/*.pem` |
| `WithAllowShellOperators()` (v0.9) | operators rejected when an allowlist is set | opt-in, weak boundary; needed only for pipelines/redirection in allowlisted commands; combine with an approval gate |

The policy also enforces `..`/NUL rejection, symlink-escape checks and `os.Root`
I/O confinement. It is an **in-process policy, not an OS sandbox**; see
[SECURITY.md](../SECURITY.md).

### `agent.New` / `rig.AgentSpec`

| Field | Zero value behavior | Default / note |
| --- | --- | --- |
| `Provider` | **required** | `agent.New` returns an error wrapping `rig.ErrProviderRequired` |
| `Model` | **required** | error wrapping `rig.ErrModelRequired` when empty |
| `SystemPrompt` | no system message | trimmed; prepended to each request, never persisted |
| `MaxIterations` | use the default | `agent.DefaultMaxIterations` = 20 |
| `Tools` | no tools | nil/empty means the model cannot call anything |
| `Gate` | allow all | dangerous for untrusted models; see `gate` below |
| `History` | no persistence | nil; `RunRequest.SessionID` is then storage-irrelevant |
| `Temperature`, `TopP` | unset | nil pointers; the provider/endpoint default applies |
| `MaxTokens` | unset | 0 |
| `Logger` | discard | nil; `Printf`-style diagnostics only |

`agent.New` is safe for concurrent `Run` calls when the provider, tools and
history are; the loop keeps no mutable state.

### `rig.RunRequest` (per turn)

| Field | Zero value behavior |
| --- | --- |
| `SessionID` | `""` = fresh history; nothing is loaded or persisted for the run |
| `Input` | nil = continuation without adding a user message |
| `Model` | `""` = the agent's model from `AgentSpec` |
| `MaxIterations` | 0 = the agent's default (which is 20 unless the spec sets it) |
| `Gate` | nil = the agent's gate (if that is also nil: allow all) |
| `Emit` | nil = drop events; events are emitted synchronously on the run goroutine |

### `full.AllOptions` (complete tool set)

`full.All(full.AllOptions{})` builds every static tool. Each dependency is
optional and degrades to a documented fallback rather than a nil panic:

| Option | Zero value behavior |
| --- | --- |
| `Policy` | `sandbox.New(sandbox.WithRoots(opts.Root))` — a policy with the workspace root *(see note)* and no shell/network |
| `Root` | `""`; workspace root reported by `working_directory`/inspect/report and used by the default policy; note that an empty root normalizes to *no roots* (file access denied) — pass an absolute path |
| `Settings` | nil: `settings_get`/`settings_set` return a readable "host did not configure a store" tool error |
| `Host` (`hosttools.Options`) | nil fields: the corresponding tool (`ask_user`, `present`, `plan_review`, `exit_plan_mode`, `goal`) returns a readable "host did not configure" tool error; use `hosttools.NewMemoryHost()` for deterministic in-memory behavior |
| `Memory` | nil: fresh in-memory store |
| `Scheduler` | nil: fresh in-memory scheduler |
| `SessionLog` | nil: `session_*`/inspect/report tools degrade to readable errors |
| `Searcher` | nil: `web_search` reports a configuration error when called |
| `LSP`, `LSPPathToURI` | nil: LSP tools not built |
| `MCPClients` | nil: MCP tools not built (`mcp__<server>__<tool>` is dynamic) |
| `MCPPrefix` | `mcp__` |
| `Runner` | nil: orchestration tools (`subagent`, `workflow`, `ralph`, team tools) exist but report a configuration error when called |
| `Models` | nil/empty: `list_subagent_models` explains that subagents inherit the parent model |
| `Skill` | nil: filesystem store rooted at `$ZEOKEEL_SKILLS`, `<root>/.zeokeel/skills`, `~/.zeokeel/skills` |
| `TeamTasks` | nil: fresh in-memory task board |
| `Reviewer` | nil: `code_review`/`auto_review` report a configuration error |

Use `tools.NewRegistry(full.All(opts)...)` to get the model-facing registry;
`NewRegistry` panics on nil/duplicate names (a compile-time programming error),
while `Registry.Register` returns errors for runtime composition.

### `kit.Config` (recommended facade)

`kit.New(kit.Config{...})` returns a `*kit.Agent` wiring provider, tools, gate,
history and the loop. All fields are optional; common defaults:

| Field | Zero value behavior | Default |
| --- | --- | --- |
| `Model` | **required** | `kit.New` returns an error wrapping `rig.ErrModelRequired` when empty (also when a custom `Provider` is supplied — the agent still needs a default model) |
| `SystemPrompt` | no system message | `""` |
| `BaseURL` | provider default | `https://api.openai.com/v1` (ignored when `Provider` is set) |
| `APIKey` / `APIKeyEnv` | provider behavior | `APIKeyEnv` = `OPENAI_API_KEY` (ignored when `Provider` is set) |
| `Provider` | build an OpenAI-compatible provider | nil = built-in `openai` provider |
| `Tools` | build the full tool set | nil = `full.All(...)`; a non-nil empty slice means no tools |
| `Gate` | safe approval gate | `gate.AllowTools("read_file", "list_dir", "glob", "grep")` (no shell) |
| `History` | in-memory history | `session.NewMemory()` |
| `Root` | `kit.DefaultRoot` = `"."` | when `Policy` is nil, `kit.New` validates that the root exists and is a directory |
| `Policy` | build a policy from Root/AllowNetwork/CommandAllowlist | nil = built policy; when set, AllowNetwork and CommandAllowlist are ignored and the root is not validated by kit |
| `AllowNetwork` | network disabled | `false` (public HTTP(S) only when enabled) |
| `CommandAllowlist` | shell disabled | nil (empty) |
| `MaxIterations` | loop default | `kit.DefaultMaxIterations` = 20 |

The facade is additive (fields are appended) and wraps the same contract types
(`kit.RunRequest`, `kit.RunResult`, `kit.Event`, `kit.Version` are aliases of the
`rig` equivalents), so a host can start with `kit.New` and drop down to
`agent.New` later without rewriting its tool wiring. Exact validation messages
and fields are in the package docs
(`go doc github.com/zeokeel/zeokeel-rig/kit.Config`).

### `gate` package

A gate decides whether a tool call may run. nil (no gate configured) means
**allow all**, which is only appropriate for fully trusted tool sets.

| Constructor | Behavior |
| --- | --- |
| `gate.AllowAll()` | allow everything |
| `gate.DenyAll()` | deny everything |
| `gate.AllowTools(names...)` | allow only the named tools; anything else denied |
| `gate.Func(fn)` | host callback (for example a UI approval prompt); called on the run goroutine |
| `nil` | same as `AllowAll` |

A denied call is reported to the model as a tool error (`DecisionDeny`); the run
continues unless the host's gate returns an error. `DecisionAllowAlways` skips
the gate for identical (tool, arguments) calls for the rest of that run only.

### Session history

`session.NewMemory()` is the in-memory `rig.History`; nil means no persistence.
Implement `rig.History` (safe for concurrent use) for durable storage; the agent
loads/append per `SessionID`. `kit.New` uses `session.NewMemory()` by default.

### `web_fetch` / `web_search` (`tools/web`)

| Setting | Default |
| --- | --- |
| Timeout (whole call, all redirects) | `web.DefaultTimeout` = 30s |
| Response body cap | `web.DefaultMaxBytes` = 2 MiB |
| Output character cap | `web.DefaultMaxChars` = 20000 (per-call max `web.MaxCharsLimit` = 200000) |
| Redirect chain | `web.MaxRedirects` = 5 hops |
| User-Agent | `web.DefaultUserAgent` = `ZeoKeel-Rig/0.3` |
| Search result limit | `web.DefaultSearchLimit` = 5 (max 20) |
| URL policy | `nil` allows every URL — **always pass a `sandbox.Policy` for untrusted models** |
| Private addresses | a non-nil policy without `web.HostResolver` gets a fetcher-side guard that rejects loopback/private/link-local/CGNAT/metadata IPs; `web.WithAllowPrivateHosts()` is the explicit opt-out |

`web.NewFetcher(policy)` is the simple form; `web.NewHTTPFetcher(opts...)` accepts
`WithPolicy`, `WithAllowPrivateHosts`, `WithTimeout`, `WithMaxBytes`, `WithMaxChars`,
`WithUserAgent` and `WithTransport`. Zero/negative option values keep the defaults.
The `sandbox.Policy` path validates addresses in the policy (`web.HostResolver`);
the fetcher-side guard covers `web.PolicyFunc` and custom `CheckURL` policies.

## OS-level process sandbox (opt-in)

The in-process `sandbox.Policy` decides what tools may touch; the approval `Gate`
decides what the model may do. For untrusted models, attach an OS-level sandbox to
the spawned processes as a second layer:

```go
osExec, err := sandbox.NewOSExec(sandbox.OSExecOptions{
    Root:         root,        // must be the same workspace root as the policy
    AllowNetwork: false,       // default: deny network where the backend can
    WritableDirs: []string{cacheDir},
})
policy := sandbox.New(
    sandbox.WithRoots(root),
    sandbox.WithCommandAllowlist("git", "go", "ls", "cat"),
    sandbox.WithOSExec(osExec),
)
a, err := agent.New(rig.AgentSpec{ /* Tools built with that policy */ })
```

With `kit`, use `kit.Config{Model: ..., Root: root, OSExec: osExec}` instead (it
cannot be combined with `Config.Policy`; attach it with `sandbox.WithOSExec` when
supplying a custom policy). Backends: Linux `bwrap`, macOS `sandbox-exec`, Windows
restricted token + low integrity (`CGO_ENABLED=0`; no cgo). Windows does not
isolate the network — use a container/VM for untrusted models there. `NewOSExec`
fails closed with `sandbox.ErrOSExecUnavailable` on unsupported platforms or when
the helper binary is missing.

Environment variables

The SDK reads only these variables itself:

| Variable | Read by | Meaning |
| --- | --- | --- |
| `OPENAI_API_KEY` | `provider/openai` | API key when `Config.APIKey` is empty and `Config.APIKeyEnv` is unset |
| `ZEOKEEL_SKILLS` | `tools/skill` | colon-separated (`os.PathListSeparator`) extra skill directories |
| `HOME` | `tools/skill` | expands `~/.zeokeel/skills` |
| `PATH`, `HOME`, `LANG` | `sandbox` shell policy | the default environment allowlist a command inherits when shell execution is enabled |

The provider does **not** read an `OPENAI_BASE_URL` (or similar) variable: pass
`openai.Config.BaseURL` explicitly. There is no config file, no `RIG_*` variable
convention and no global flags; the host owns configuration loading.

## Recommended layering in a host

1. **Code defaults** — zero values / `kit.Config` defaults; fail closed in
   security-relevant places.
2. **Environment** — provider credentials through `APIKeyEnv`, skill paths
   through `ZEOKEEL_SKILLS`; never commit keys.
3. **Host config file/flags** — model name, base URL, roots, command allowlist,
   iteration caps, gate policy; the host loads these itself and fills the Config
   structs.
4. **Per request** — `rig.RunRequest` overrides (model, iteration cap, gate,
   `Emit`) and `kit.RunRequest`/`RunText` arguments.

## Configuration and versioning

Config structs and option sets are part of the public API. They follow the
additive rules in [docs/VERSIONING.md](VERSIONING.md): new fields are appended
with behavior-preserving zero values, options are only added, and removals are
deprecated first. Consumers should use **keyed struct literals**
(`openai.Config{BaseURL: ...}`) and never rely on positional literals, so new
optional fields never break their code.
