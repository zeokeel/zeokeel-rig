# Release procedure

This is the checklist the release manager follows to cut a release. The
version-number policy is in [docs/VERSIONING.md](VERSIONING.md); the release
automation is [`.github/workflows/release.yml`](../.github/workflows/release.yml).

Two facts drive the procedure:

1. **The tag is the release.** Pushing an annotated `vX.Y.Z` tag is what makes
   the version resolvable by `go get`; the tag is immutable once published.
2. **CI does not run on tag pushes.** `ci.yml` runs on branch pushes and pull
   requests, so the tag build re-runs the gates itself inside the release
   workflow before publishing anything.

## What the release workflow does

Triggered by a push of any tag matching `v*`:

1. `actions/checkout` with `fetch-depth: 0` (tags + full history) and
   `actions/setup-go` using `go-version-file: go.mod`.
2. Runs `bash scripts/sync-version.sh --require-tag`: fails if no tag points at
   `HEAD`, or if `const Version` in `version.go` differs from the tag.
3. Runs the gates: gofmt, `go build ./...`, `go vet ./...`,
   `go test ./... -count=1`, `go test ./... -race`, the tool-catalog coverage
   test and the modern-surface audit.
4. Extracts the release notes from the `## vX.Y.Z` section of `CHANGELOG.md`.
   The section runs until the next `## ` heading; a missing or empty section
   **fails the release** (no release with empty notes).
5. Creates the GitHub Release with `gh release create "$GITHUB_REF_NAME"
   --verify-tag --title "$GITHUB_REF_NAME" --notes-file ...`. The workflow needs
   `permissions: contents: write` and uses the automatic `GITHUB_TOKEN`; no
   personal access token is required.

Tags that do not follow `vX.Y.Z` (for example `release-0.9.0`) do not trigger
the workflow, and a lightweight tag produces a warning from `sync-version.sh`
(annotated tags are required by policy).

## Preconditions

- All work for the release is merged to `main` and `main` is green in CI.
- The working tree is clean (`git status --short` is empty) and you are on the
  latest `main` (`git fetch origin && git log --oneline -5`).
- The release scope is frozen; no new features land between the release commit
  and the tag.
- You have the maintainer rights to push tags and edit releases.

## Checklist

### 1. Prepare the release commit

1. Decide the version number per [docs/VERSIONING.md](VERSIONING.md): patch for
   fixes-only, minor for the pre-1.0 feature line, `v1.0.0` only when the
   project deliberately makes the stability commitment.
2. Update `CHANGELOG.md`:
   - replace the top `## Unreleased` section with
     `## vX.Y.Z — YYYY-MM-DD` (em dash); if a second stale `Unreleased` heading
     exists, fold it into the release section — there must be exactly one
     section per version;
   - make sure every breaking change is marked `BREAKING` with a migration note.
3. Update `version.go`: `const Version = "X.Y.Z"` (no leading `v`).
4. Update version references in documentation:
   - `README.md` install line (`go get ...@vX.Y.Z`) and the "current release" /
     roadmap entry;
   - `COMPATIBILITY.md` release-line row;
   - `docs/COVERAGE.md` / `docs/TOOLS.md` counts if the tool surface changed;
   - `docs/ARCHITECTURE.md` if behavior changed.
5. Commit the release preparation:
   ```sh
   git add CHANGELOG.md version.go README.md COMPATIBILITY.md docs
   git commit -m "chore(release): prepare vX.Y.Z"
   ```
6. Push the branch / merge the release PR to `main` and wait for CI to pass.

### 2. Verify the release candidate locally

From an up-to-date `main` with a clean tree:

```sh
make ci                            # fmt, build, vet, deps, test, race, gate,
                                   # quickstart drift, examples, cross-compile, version
make fuzz FUZZTIME=5s              # fuzz smoke (if the tree has fuzz targets)
make bench                         # every benchmark once
go build ./... && go vet ./...     # redundant with make ci, explicit in release log
```

Notes:

- `make version` prints `SKIP` on this untagged commit — that is expected.
- Run `bash scripts/sync-version.sh vX.Y.Z` to prove the explicit tag will match
  `version.go` before you create it.
- The release workflow re-runs the compile/test gates on the tag; only the
  cross-compile, fuzz and bench steps are local-only, so do not skip them.

### 3. Create and push the annotated tag

The tagged commit must be the release commit on `main` that CI passed:

```sh
git switch main
git pull --ff-only origin main
git tag -a vX.Y.Z -m "ZeoKeel Rig vX.Y.Z"
git tag -n9 vX.Y.Z                # inspect the annotation
git push origin vX.Y.Z
```

- Use an **annotated** tag (`-a`); lightweight tags are not release tags.
- Signing is recommended when a GPG/SSH signing key is configured:
  `git tag -s vX.Y.Z -m "..."` and verify with `git tag -v vX.Y.Z`.
- **Never** move, delete or force-push a published tag (see
  [docs/VERSIONING.md](VERSIONING.md#tag-immutability-and-retractions)). If the
  wrong commit was tagged, the fix is a new patch version.
- Push only the one tag (`git push origin vX.Y.Z`), not `--tags`, so unrelated
  local tags cannot slip out.

### 4. Watch the release workflow

```sh
gh run list --workflow=release.yml --limit 5
gh run watch <run-id>             # or watch in the Actions tab
```

If a gate fails, the release is not created. Fix the cause forward: merge the
fix and cut a **new** tag only if the current tag must be abandoned; otherwise
re-run the failed job (the workflow is safe to re-run as long as the release was
not partially created). Never force-push the tag to "fix" the workflow input.

### 5. Verify the published release

```sh
gh release view vX.Y.Z            # title, tag, notes
gh release view vX.Y.Z --json notesUrl,isDraft,isPrerelease,tagName
```

Check:

- the release exists, is not a draft, and its notes are exactly the
  `## vX.Y.Z` section of `CHANGELOG.md`;
- the release tag points at the expected commit
  (`git rev-parse vX.Y.Z^{commit}` equals the release commit);
- `https://pkg.go.dev/github.com/zeokeel/zeokeel-rig@vX.Y.Z` renders (the proxy
  may need a few minutes; a plain `go get` also warms it).

Optionally smoke-test a fresh consumer with the released version after the proxy
has updated:

```sh
tmp=$(mktemp -d)
cd "$tmp"
go mod init example.com/smoke
GOPROXY=https://proxy.golang.org go get github.com/zeokeel/zeokeel-rig@vX.Y.Z
```

For pre-1.0 releases, note in the release body or announcement that the `v0.x`
line may still contain breaking changes (docs/VERSIONING.md).

### 6. Post-release

- Leave `version.go` at the released version: it always equals the latest
  published tag. Do not pre-bump it on `main`.
- Announce the release where the project announces releases; the GitHub Release
  is the canonical record.
- Open a fresh `## Unreleased` section in `CHANGELOG.md` with the standard
  category headings for the next line.
- Check Dependabot's weekly GitHub Actions PRs stay green.

## Hotfix releases

1. Branch from the release tag: `git switch -c hotfix/X.Y.Z+1 vX.Y.Z`.
2. Apply the minimal fix plus a regression test; update `version.go`,
   `CHANGELOG.md` (`## vX.Y.Z+1 — YYYY-MM-DD`) and any affected docs.
3. Run `make ci` and the targeted tests; open a PR to `main`, merge, then tag the
   merge commit as in step 3. The same workflow publishes the release.
4. Do not rewrite the previous release's notes; the advisory/section belongs to
   the new version.

## Emergency rollback

There is no rollback that rewrites a tag. If a published release must not be
used:

1. Publish a fixed patch release as soon as possible.
2. Add a `retract` directive to `go.mod` for the broken version in the fixed
   release (see the Go documentation link in `docs/VERSIONING.md`).
3. If the issue is a security vulnerability, publish a GitHub Security Advisory
   and follow [SECURITY.md](../SECURITY.md).

## Release checklist (copy/paste)

```text
[ ] Scope frozen; main green; tree clean
[ ] CHANGELOG Unreleased -> ## vX.Y.Z — YYYY-MM-DD (exactly one section)
[ ] version.go const Version = "X.Y.Z"
[ ] README install tag / roadmap, COMPATIBILITY.md, COVERAGE/TOOLS counts, ARCHITECTURE
[ ] Release-prep commit merged to main; CI green
[ ] make ci (all gates) on the release commit
[ ] make fuzz FUZZTIME=5s and make bench (local-only release gates)
[ ] bash scripts/sync-version.sh vX.Y.Z passes
[ ] git tag -a vX.Y.Z -m "ZeoKeel Rig vX.Y.Z" on the release commit
[ ] git push origin vX.Y.Z (nothing else; never move a published tag)
[ ] release.yml workflow green; GitHub Release created with CHANGELOG notes
[ ] gh release view vX.Y.Z verified; pkg.go.dev renders the tag
[ ] fresh Unreleased section in CHANGELOG for the next line
```
