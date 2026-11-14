#!/usr/bin/env bash
# sync-version.sh — fail when `const Version` in version.go disagrees with the
# release tag this commit is expected to carry.
#
# Why: the git tag is what `go get` resolves, while `const Version` is what
# consumers report from `rig.Version` in diagnostics and support requests. A
# release where the two disagree ships metadata that lies, so the release
# workflow, `make version` and `make ci` all run this check.
#
# Usage:
#   scripts/sync-version.sh                 # infer the tag from the checkout/CI
#   scripts/sync-version.sh v0.9.0          # compare against an explicit tag
#   scripts/sync-version.sh --require-tag   # release build: a tag is required
#
# Tag discovery order:
#   1. the explicit vX.Y.Z argument;
#   2. GITHUB_REF / GITHUB_REF_NAME when GitHub Actions runs for a tag;
#   3. `git describe --tags --exact-match HEAD` in a local checkout.
#
# Exit status: 0 on a match (or on a development checkout that is not the
# release tag for version.go, unless --require-tag is passed); 1 on a mismatch
# in an explicit/CI check, or on an invalid version file.
#
# Release-prep commits are supported: after tagging vX.Y.Z, main's version.go is
# bumped for the next release while HEAD still carries the previous exact tag.
# A mismatch discovered only through `git describe --exact-match` (a local
# development checkout) is therefore reported as SKIP; the release workflow
# always has the tag in GITHUB_REF and enforces the match strictly.
#
# Tags are immutable: never move a published tag to fix a release — publish a
# new patch version instead. See docs/VERSIONING.md and docs/RELEASE.md.
set -euo pipefail

cd "$(dirname "$0")/.."

usage() {
	cat <<'EOF'
usage: scripts/sync-version.sh [--require-tag] [vX.Y.Z]

Verify that `const Version` in version.go matches the release tag.

  (no args)     infer the tag from GITHUB_REF or `git describe --tags --exact-match HEAD`
  vX.Y.Z        compare against this explicit tag
  --require-tag fail when no release tag points at HEAD (use in release CI)
EOF
}

require_tag=0
expected_tag=""

for arg in "$@"; do
	case "$arg" in
		--require-tag) require_tag=1 ;;
		--help | -h) usage; exit 0 ;;
		v[0-9]*) expected_tag="$arg" ;;
		*)
			echo "sync-version: unknown argument: $arg" >&2
			usage >&2
			exit 2
			;;
	esac
done

fail() {
	echo "sync-version: $*" >&2
	exit 1
}

# 1. Read `const Version = "X.Y.Z"` from the module root. Keep that declaration
#    on one line: it is the machine-readable version source for this script.
version_file="version.go"
[ -f "$version_file" ] || fail "$version_file not found (run this from the module root)"
version="$(
	sed -nE 's/^const Version[[:space:]]*=[[:space:]]*"([^"]+)".*$/\1/p' "$version_file" | head -n 1
)"
[ -n "$version" ] || fail "cannot find 'const Version = \"...\"' in $version_file"
if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]]; then
	fail "invalid version \"$version\" in $version_file (want semver X.Y.Z)"
fi

# 2. Resolve the release tag this checkout is supposed to be.
tag="$expected_tag"
tag_source="explicit"
if [ -z "$tag" ]; then
	tag_source="ci"
	if [ "${GITHUB_REF_TYPE:-}" = "tag" ] && [ -n "${GITHUB_REF_NAME:-}" ]; then
		tag="$GITHUB_REF_NAME"
	elif [[ "${GITHUB_REF:-}" = refs/tags/* ]]; then
		tag="${GITHUB_REF#refs/tags/}"
	else
		tag_source=""
	fi
fi
if [ -z "$tag" ] && git rev-parse --git-dir >/dev/null 2>&1; then
	if exact="$(git describe --tags --exact-match HEAD 2>/dev/null)"; then
		tag="$exact"
		tag_source="git"
	fi
fi

if [ -z "$tag" ]; then
	if [ "$require_tag" -eq 1 ]; then
		fail "no release tag points at HEAD; create an annotated tag 'v$version' before releasing (see docs/RELEASE.md)"
	fi
	echo "sync-version: SKIP: HEAD is not an exact tag checkout (development build); version.go says $version"
	exit 0
fi

# 3. Compare the tag with the constant. The repository tag format is vX.Y.Z;
#    version.go carries the bare X.Y.Z.
case "$tag" in
	v[0-9]*) ;;
	*) fail "release tag must start with 'v' (got: $tag)" ;;
esac
tag_version="${tag#v}"
if [ "$tag_version" != "$version" ]; then
	# A local checkout can legitimately carry the previous release tag while
	# version.go is already bumped for the next release (release prep). Only an
	# explicit argument, a CI tag context or --require-tag makes this fatal.
	if [ "$tag_source" = "git" ] && [ "$require_tag" -eq 0 ]; then
		echo "sync-version: SKIP: HEAD carries the previous release tag $tag but $version_file already says $version (release prep); the release workflow enforces the match on the new tag"
		exit 0
	fi
	fail "version mismatch: tag $tag says $tag_version but $version_file has $version. Tag the commit with the matching version, or update version.go before tagging; do not move a published tag."
fi

# 4. Release tags must be annotated (docs/RELEASE.md). Warn here instead of
#    failing so a maintainer's local check and signed tags both keep working.
if [ "${GITHUB_REF_TYPE:-}" = "tag" ] && git rev-parse --git-dir >/dev/null 2>&1; then
	tag_type="$(git cat-file -t "refs/tags/$tag" 2>/dev/null || true)"
	if [ "$tag_type" = "commit" ]; then
		echo "sync-version: WARNING: $tag is a lightweight tag; release tags must be annotated (git tag -a)" >&2
	fi
fi

echo "sync-version: OK: version.go $version matches release tag $tag"
