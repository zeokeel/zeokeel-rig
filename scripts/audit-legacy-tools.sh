#!/usr/bin/env bash
# Audit the legacy ZeoKeel agent's tool surface against Rig's catalog.
#
# Usage: scripts/audit-legacy-tools.sh [path-to-old-repo]
#        (default: /data/mygo/old)
#
# Scans non-test Go files for `Name() string { return "..." }` implementations,
# adds the runtime-level settings_get/settings_set tools, and diffs the result
# with the string list in tools/catalog/catalog.go (var Legacy). Test-fixture
# tools (big, binder, block, clock, echo, side_effect, ...) are filtered out.
set -euo pipefail
repo="${1:-/data/mygo/old}"
cd "$(dirname "$0")/.."

if [ ! -d "$repo/internal" ]; then
	echo "audit: $repo/internal not found" >&2
	exit 2
fi

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

# Legacy tool names: Name() returns in non-test files + runtime settings tools.
{
	# one-line implementations: func (t *x) Name() string { return "name" }
	grep -rhE '^[[:space:]]*func \([^)]*\) Name\(\) string.*return "[a-z_0-9]+"' "$repo/internal" \
		--include='*.go' --exclude='*_test.go' \
		| grep -oE 'return "[a-z_0-9]+"' | grep -oE '"[a-z_0-9]+"' | tr -d '"'
	# multi-line implementations: return on one of the next lines
	grep -rhA3 -E '^[[:space:]]*func \([^)]*\) Name\(\) string' "$repo/internal" \
		--include='*.go' --exclude='*_test.go' \
		| grep -E '^[[:space:]]*return "[a-z_0-9]+"' \
		| grep -oE '"[a-z_0-9]+"' | tr -d '"' || true
} | sort -u > "$tmp"
printf 'settings_get\nsettings_set\n' >> "$tmp"
sort -u -o "$tmp" "$tmp"

# Catalog legacy names: quoted strings inside `var Legacy = []string{...}`.
catalog=$(mktemp)
trap 'rm -f "$tmp" "$catalog"' EXIT
awk '/^var Legacy = \[\]string\{/{flag=1;next} flag&&/^\}/{flag=0} flag' \
	tools/catalog/catalog.go | grep -oE '"[a-z_0-9]+"' | tr -d '"' | sort -u > "$catalog"

# Test-fixture helper tools that only exist in _test files are never scanned,
# but allow a small exclusion list for names that also appear in test helpers.
exclude="big binder block clock echo side_effect"
for name in $exclude; do sed -i "/^${name}$/d" "$tmp"; done

missing=$(comm -23 "$tmp" "$catalog" || true)
extra=$(comm -13 "$tmp" "$catalog" || true)

echo "legacy tools scanned: $(wc -l < "$tmp" | tr -d ' ')"
echo "catalog legacy names: $(wc -l < "$catalog" | tr -d ' ')"
if [ -n "$missing" ]; then
	echo "MISSING from catalog (add to tools/catalog + full.All):" >&2
	echo "$missing" >&2
	exit 1
fi
if [ -n "$extra" ]; then
	echo "catalog additions (Rig-only tools, expected):" 
	echo "$extra"
fi
echo "audit: legacy tool surface complete"
