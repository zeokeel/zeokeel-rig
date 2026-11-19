# Release procedure

> 中文文档以 [README.md](../README.md) 与 [docs/zh/](zh/README.md) 为主；本文件保留英文原文作为参考，对应中文版见 [docs/zh/RELEASE.md](zh/RELEASE.md)。

This is the checklist the release manager follows to cut a release. The
version-number policy is in [docs/VERSIONING.md](VERSIONING.md); the release
automation is [`.github/workflows/release.yml`](../.github/workflows/release.yml)
plus the manual publish step in
[`.github/workflows/publish-release.yml`](../.github/workflows/publish-release.yml).

Three facts drive the procedure:

1. **The tag is the release.** Pushing an annotated, signed `vX.Y.Z` tag is what
   makes the version resolvable by `go get`; the tag and the module checksums it
   produces are immutable once published.
2. **子模块有自己的 tag。** `session/sqlite` 是**独立 module**，主模块的 `vX.Y.Z`
   tag **不覆盖**它：`go get github.com/zeokeel/zeokeel-rig/session/sqlite` 认的是
   `session/sqlite/vX.Y.Z` 这个 tag（Go 的多 module 仓库规则：子目录 module 的名字
   就是 `<仓库路径>/<子目录>`，tag 必须带目录前缀）。详见下面的
   [Submodule tags](#submodule-tags-sessionsqlitevxyz)。

   发布步骤（在一次主模块发布里一起做）：

   ```bash
   # 1. 先把子模块的 require 指到本次要发布的主模块版本
   cd session/sqlite && go mod edit -require=github.com/zeokeel/zeokeel-rig@vX.Y.Z
   go mod tidy && go build ./... && go vet ./... && go test ./... -count=1
   cd ../.. && git add session/sqlite/go.mod session/sqlite/go.sum && git commit -m "..."
   git push origin main   # 等 CI 绿（含 submodule release tags）

   # 2. 主模块先发（子模块要能 go get 到它）
   git tag -s vX.Y.Z -m "vX.Y.Z" && git push origin vX.Y.Z

   # 3. 再给子模块打 tag（同一个 commit 即可）
   git tag -s session/sqlite/vX.Y.Z -m "session/sqlite vX.Y.Z"
   git push origin session/sqlite/vX.Y.Z
   ```

   顺序不能反：子模块的 `require` 指向主模块版本，主模块没有 tag 时下游
   `go get session/sqlite` 会解析不到依赖。

   当前 `release.yml` **不会**自动产出 `session/sqlite/vX.Y.Z`，第 3 步是手工的；
   等这条流程稳定后可以交给 `scripts/` 里的脚本补齐。

3. **The tag runs the full merge gates.** `ci.yml` is a reusable workflow
   (`workflow_call`): the release workflow calls it, so a tag runs exactly the
   gates a merge runs — including coverage floors, fuzz/bench smoke, staticcheck,
   govulncheck, the dependency policy (exactly one vetted parser dependency), the
   cgo-free build/test gate, cross-compile, and the three submodule gates
   (`test (session/sqlite submodule)`, `sqlite-consumer smoke`,
   `submodule release tags`). The unit suite runs on Linux;
   windows/darwin/freebsd are
   cross-compiled and `GOOS`-vetted (hosted macOS/Windows runners are not
   available for this repository — disclosed in README.md). There is one gate
   definition, not two.
4. **Nothing is published automatically.** The tag workflow verifies the tag and
   creates a **draft** GitHub Release; a maintainer publishes the draft with
   `publish-release.yml` only after the post-release re-audit
   (docs/RELEASE.md §6) passes.

## Submodule tags (`session/sqlite/vX.Y.Z`)

子模块 tag 是这套流程里最容易漏的一步，所以它有三条硬约束（都有 CI 门禁）：

1. **顺序：先根 tag，后子模块 tag。** 子模块 `go.mod` 的 `require` 指向根 module 的
   版本；根 tag 不存在时，下游 `go get session/sqlite@vX.Y.Z` 解析不到依赖。
2. **`require` 必须等于"最近一个根 tag"。** 唯一的例外是待发版窗口：`require` 指向
   本次要发的版本（尚未打 tag），根 tag 打出后两者才相等。`submodule release tags`
   job（在 `push` main 与 `workflow_call` release 上跑，PR 上跳过）校验：
   - `require` == 最近根 tag（`git tag --merged HEAD --list 'v[0-9]*' --sort=-v:refname`）；
   - `session/sqlite/<最近根 tag>` 这个 tag 存在，且包含根 tag 的提交。

   任一条不满足**直接红**。这是"发了根 tag 却忘了发子模块 tag"（以及"`require` 指着
   一个从未发布的版本"）唯一能被发现的地方——它不依赖任何人记得检查。
3. **`replace github.com/zeokeel/zeokeel-rig => ../..` 仅供本仓开发**（本地与 CI 直接用
   工作树里的根 module，不必先发 tag）。**依赖方会忽略它**：Go 只应用主 module 的
   `replace`，子模块作为依赖被 `go get` 时这条声明是死的。所以"本地能编"从来不是
   "宿主能编"的证据，证据是 `sqlite-consumer smoke`（见下）。

发版前后各跑一次下游消费冒烟（它在**空的**临时 module 里 `go get` 子模块并编译：没有
`go.work`、没有 `replace`，`GOPROXY` 只有本仓两个 module 走 `file://` 代理，第三方
依赖回退真实代理）：

```bash
# 从 ci.yml 抽出 sqlite-consumer-smoke job 的脚本原样跑（GOMODCACHE 必须是空的，
# 热缓存会掩盖"拉不到依赖"，等于把这条门禁跑成假绿）。
python3 - <<'PY'
import yaml
doc = yaml.safe_load(open(".github/workflows/ci.yml"))
job = doc["jobs"]["sqlite-consumer-smoke"]
body = "\n".join(s["run"] for s in job["steps"] if "run" in s)
open("/tmp/consumer-smoke.sh", "w").write(body + "\n")
PY
GOMODCACHE="$(mktemp -d)" bash /tmp/consumer-smoke.sh   # 需要能访问 proxy.golang.org
```

它读的是 `session/sqlite/go.mod` 里的 `require` 版本（不写死），所以 `require` 一 bump，
冒烟就跟着对着新版本跑；版本拼错或依赖漏进 `go.sum` 都会当场红。

## What the release workflow does

Triggered by a push of any tag matching `v*`:

1. **CI job** (`uses: ./.github/workflows/ci.yml`): the complete merge gate set
   (gofmt on tracked files, build, vet, test, test -race, 80 % total / 70 %-per-
   package coverage floors, 5 s-per-target fuzz smoke, benchmark smoke, catalog
   and modern-surface gates, dependency-policy and root-package leaf checks,
   README drift, cross-compile + `GOOS` vet for windows/darwin/freebsd, examples,
   and staticcheck + govulncheck lint; the unit suite runs on Linux (hosted
   macOS/Windows runners are unavailable for this repository — disclosed in
   README.md), and the bash/jobs/terminal tools are Unix-only).
2. **verify job**: checkout with `fetch-depth: 0`; checks the tag is signed by a
   key in [`.github/allowed_signers`](../.github/allowed_signers) (`git tag -v`);
   checks `const Version` in `version.go` matches the tag
   (`scripts/sync-version.sh --require-tag`); checks the tagged commit is an
   ancestor of `origin/main`. Any failure stops the release before a Release
   object exists.
3. **smoke job**: clean-room `go get github.com/zeokeel/zeokeel-rig@<tag>` in a
   fresh module through the public proxy, with retries while the proxy indexes
   the new tag; `go mod verify` checks the module against the checksum database;
   a tiny program imports `rig`+`kit` and asserts both report the tag version.
4. **publish job**: `permissions: contents: write`, `environment: release`; it
   re-checks the tag itself (the tag must have been for the release workflow
   run), extracts the matching `## vX.Y.Z` section from `CHANGELOG.md` (missing
   or empty section fails the release), and creates a **draft** GitHub Release
   (`gh release create --draft --verify-tag`). It is idempotent: an existing
   Release is left untouched.

`main` and `v*` tags are protected on GitHub: required CI checks must pass, force
pushes and deletions are rejected, and a tag ruleset blocks moving/deleting
published tags. The re-run of a failed release job is safe while no Release
exists; after a Release exists, fix forward with a new patch version.

## Preconditions

- All work for the release is merged to `main` and `main` is green in CI.
- The working tree is clean (`git status --short` is empty) and you are on the
  latest `main` (`git fetch origin && git log --oneline -5`).
- The release scope is frozen; no new features land between the release commit
  and the tag.
- You have the maintainer rights to push tags, edit releases and run
  `publish-release`.
- Your signing key's public half is in `.github/allowed_signers` and configured
  locally (`git config gpg.format ssh`, `git config user.signingkey <key>`).

## Repository protection status (single-maintainer disclosure)

The repository is configured with the strongest protection that does not require
a second maintainer, and the remaining limits are stated here instead of implied:

- `main` requires the `test` and `lint` CI checks (strict), signed commits,
  linear history and conversation resolution; force-pushes and deletions are
  rejected. A `v*` tag ruleset blocks updating, deleting or force-moving a
  published tag (no bypass actors).
- `enforce_admins=false` is intentionally left set so the sole maintainer can
  recover from a broken gate; it also means required checks/signatures do not
  constrain that account. This is a safety valve, not enforced peer review.
- There is no required pull-request reviewer: with one maintainer a self-review
  requirement would be vacuous (GitHub does not allow approving your own PR).
- GitHub only shows commits as *Verified* when the SSH signing key is registered
  on the account (`admin:ssh_signing_key`). The release workflow verifies tag
  signatures independently against `.github/allowed_signers`, which is the
  project-level trust anchor; the account badge is a separate, optional step.
- The `release` environment exists but has no required-reviewer rule; publication
  is gated procedurally by the separate `publish-release` workflow dispatch
  (`confirm_reviewed=yes`), not by an environment reviewer.

If the project gains a second maintainer, the follow-up is: require PR review,
set `enforce_admins=true`, register signing keys on both accounts, and add a
required reviewer to the `release` environment.

## Checklist

### 1. Prepare the release commit

1. Decide the version number per [docs/VERSIONING.md](VERSIONING.md): patch for
   fixes-only, minor for the pre-1.0 feature line, `v1.0.0` only when the project
   deliberately makes the stability commitment. The current release line is
   `v0.19.x`; `v1.0.0` is not planned until the API-freeze work in
   docs/VERSIONING.md is complete.
2. Update `CHANGELOG.md`:
   - replace the top `## Unreleased` section with
     `## vX.Y.Z — YYYY-MM-DD` (em dash); there must be exactly one section per
     version;
   - make sure every breaking change is marked `BREAKING` with a migration note
     and every deprecation is under `### Deprecated`;
   - the release workflow copies this section verbatim into the GitHub Release.
3. Update `version.go`: `const Version = "X.Y.Z"` (no leading `v`).
4. Update version references in documentation:
   - `README.md` install line (`go get ...@vX.Y.Z`) and the "current release" /
     roadmap entry;
   - `COMPATIBILITY.md` release-line row;
   - `docs/COVERAGE.md` / `docs/TOOLS.md` counts if the tool surface changed;
   - `docs/ARCHITECTURE.md` / `SECURITY.md` if security-relevant behavior changed.
5. Commit the release preparation:
   ```sh
   git add CHANGELOG.md version.go README.md COMPATIBILITY.md docs
   git commit -m "chore(release): prepare vX.Y.Z"
   ```
6. Push the branch / merge the release PR to `main` and wait for the
   `test`, `lint` and `test-os` required checks to pass.

### 2. Verify the release candidate locally

From an up-to-date `main` with a clean tree:

```sh
make ci                            # fmt, build, vet, deps, test, race,
                                   # coverage floors, gate, quickstart drift,
                                   # examples, cross-compile, version
make fuzz FUZZTIME=5s              # fuzz smoke (local-only in the checklist)
make bench                         # every benchmark once (local-only)
go build ./... && go vet ./...     # redundant with make ci, explicit in release log
```

Notes:

- `make version` prints `SKIP` on this untagged commit, and also on a release-prep
  commit that still carries the previous release tag — that is expected. The
  release workflow enforces tag == version.go strictly.
- Run `bash scripts/sync-version.sh vX.Y.Z` to prove the explicit tag will match
  `version.go` before you create it.
- CI runs fuzz and benchmark smoke on the tag itself; the longer local runs stay
  a release-manager duty for parser/input-facing changes.

### 3. Create and push the signed annotated tag

The tagged commit must be the release commit on `main` that CI passed:

```sh
git switch main
git pull --ff-only origin main
git tag -s vX.Y.Z -m "ZeoKeel Rig vX.Y.Z"
git tag -v vX.Y.Z                 # must print a good signature for an allowed key
git push origin vX.Y.Z
```

- Release tags are **annotated and signed**; an unsigned tag fails the release
  workflow's `git tag -v` check against `.github/allowed_signers`.
- The tag must point at a commit on `main`: the workflow checks that it is an
  ancestor of `origin/main` and refuses to release otherwise.
- **Never** move, delete or force-push a published tag (see
  [docs/VERSIONING.md](VERSIONING.md#tag-immutability-and-retractions) and the
  repository's `v*` tag ruleset). If the wrong commit was tagged, the fix is a
  new patch version.
- Push only the one tag (`git push origin vX.Y.Z`), not `--tags`, so unrelated
  local tags cannot slip out.

### 4. Watch the release workflow

```sh
gh run list --workflow=release.yml --limit 5
gh run watch <run-id>             # or watch in the Actions tab
```

If a gate fails, the draft Release is not created. Fix forward on `main` and cut
a **new** patch version; never force-push the tag. Re-running the failed job is
safe while no Release exists.

### 5. Review the draft release and the published artifact

```sh
gh release view vX.Y.Z            # title, tag, notes, draft state
gh release view vX.Y.Z --json isDraft,isPrerelease,tagName,notesUrl
```

Check:

- the release exists, is a **draft**, and its notes are exactly the `## vX.Y.Z`
  section of `CHANGELOG.md`;
- the tag is signed and points at the expected commit
  (`git rev-parse vX.Y.Z^{commit}` equals the release commit);
- the smoke job's `go get @vX.Y.Z` + `go mod verify` succeeded;
- `https://pkg.go.dev/github.com/zeokeel/zeokeel-rig@vX.Y.Z` renders (the proxy
  may need a few minutes; the release workflow already retries the smoke).

### 6. Post-release re-audit, then publish the draft

The release is still a draft until this step, which is deliberate: publishing is
the irreversible part.

1. Run the post-release re-audit checklist (the release gate the release manager
   maintains for the current audit round; it is not part of the published SDK):
   - G1 version consistency: tag ↔ commit ↔ `version.go` ↔ CHANGELOG ↔ README ↔
     draft notes;
   - G2 consumability: clean-room `go get @vX.Y.Z`, `go mod verify`, sumdb
     record, pkg.go.dev page;
   - G3/G4 security and correctness fixes verified by their regression tests;
   - G5–G7 gate/approval/provenance: full CI reuse, signed tag, SHA-pinned
     actions, scoped token, clean-room smoke;
   - G8/G9/G10 trust channels, deprecation record and documentation truth;
   - G11–G13 CI quality, branch/tag protection and release-history disclosure.
2. Publish only when every gate passes:

   ```sh
   gh workflow run publish-release.yml -f tag=vX.Y.Z -f confirm_reviewed=yes
   gh run watch <run-id>
   ```

   The `publish-release` workflow re-verifies the tag signature, the
   `version.go` match and the draft state, then publishes the Release with
   `--latest`. Publishing without a passing re-audit is a release incident.

### 7. Post-release

- Leave `version.go` at the released version: it always equals the latest
  published tag. Do not pre-bump it on `main`.
- Open a fresh `## Unreleased` section in `CHANGELOG.md` with the standard
  category headings for the next line (this is part of the release commit's
  follow-up and is checked by the re-audit).
- Verify `sum.golang.org/lookup/github.com/zeokeel/zeokeel-rig@vX.Y.Z` returns
  the published checksums and that pkg.go.dev shows the new version.
- Announce the release where the project announces releases; the GitHub Release
  is the canonical record.
- Check Dependabot's weekly GitHub Actions PRs stay green (they update the
  SHA pins).

## Hotfix releases

1. Branch from the release tag: `git switch -c hotfix/X.Y.Z+1 vX.Y.Z`.
2. Apply the minimal fix plus a regression test; update `version.go`,
   `CHANGELOG.md` (`## vX.Y.Z+1 — YYYY-MM-DD`) and any affected docs.
3. Run `make ci` and the targeted tests; open a PR to `main`, merge, then tag the
   merge commit as in step 3 (signed annotated tag). The same workflow creates
   the draft release; the same re-audit and publish steps apply.
4. Do not rewrite the previous release's notes; the advisory/section belongs to
   the new version.

## Emergency rollback / retraction

There is no rollback that rewrites a tag. If a published release must not be
used:

1. Publish a fixed patch release as soon as possible.
2. Add a `retract` directive to `go.mod` for the broken version in the fixed
   release (see the Go documentation link in `docs/VERSIONING.md`).
3. If the issue is a security vulnerability, publish a GitHub Security Advisory
   and follow [SECURITY.md](../SECURITY.md).
4. Delete a draft Release (never a published one) only if the tag itself must be
   abandoned; the tag stays immutable and a new patch supersedes it.

## Release checklist (copy/paste)

```text
[ ] Scope frozen; main green; tree clean; signing key configured
[ ] CHANGELOG: ## Unreleased -> ## vX.Y.Z — YYYY-MM-DD (exactly one section,
    BREAKING markers and migration notes, Deprecated entries)
[ ] version.go const Version = "X.Y.Z"
[ ] README install tag / roadmap, COMPATIBILITY.md, COVERAGE/TOOLS counts, ARCHITECTURE/SECURITY if behavior changed
[ ] Release-prep commit merged to main; required checks green (test, lint, test-os)
[ ] make ci (all gates) on the release commit
[ ] make fuzz FUZZTIME=5s and make bench (local-only release gates)
[ ] bash scripts/sync-version.sh vX.Y.Z passes
[ ] git tag -s vX.Y.Z -m "ZeoKeel Rig vX.Y.Z" on the release commit; git tag -v passes
[ ] git push origin vX.Y.Z (nothing else; never move a published tag)
[ ] release.yml green: full CI + signed-tag verification + tag-on-main + clean-room go get/go mod verify
[ ] draft GitHub Release created with the CHANGELOG notes; gh release view verified
[ ] post-release re-audit passed (pkg.go.dev renders; sumdb record; G1–G13)
[ ] publish-release workflow run with confirm_reviewed=yes; release is no longer a draft
[ ] fresh Unreleased section in CHANGELOG for the next line
```
