#!/usr/bin/env bash
# coverage-floor.sh — enforce the coverage gates from a `go test -coverprofile`.
#
# Usage: scripts/coverage-floor.sh /tmp/cover.out
#
# Two gates:
#   - total statement coverage must be at least 80%; and
#   - every package that has tests must average at least 70% function coverage.
#
# The per-package floor closes the gap of a total-only number: a large, heavily
# tested package cannot mask an under-tested one. Function-average is used
# because it is what `go tool cover -func` reports per package and it is stable
# across small statement-count changes.
set -euo pipefail

profile="${1:-}"
if [[ -z "$profile" ]]; then
	echo "usage: $0 <coverprofile>" >&2
	exit 2
fi
if [[ ! -s "$profile" ]]; then
	echo "coverage: profile $profile is missing or empty" >&2
	exit 1
fi

total="$(go tool cover -func="$profile" | tail -n 1 | awk '{print $3}' | tr -d '%')"
if [[ -z "$total" ]]; then
	echo "coverage: cannot read the total from $profile" >&2
	exit 1
fi
echo "total statement coverage: ${total}%"
awk -v total="$total" 'BEGIN {
  if (total + 0 < 80) {
    printf "::error::total coverage %s%% is below the 80%% threshold\n", total
    exit 1
  }
}'

# Only packages that actually have tests count toward the per-package floor:
# examples/ and tools/catalog are exercised by the build/catalog gates instead.
tested_packages="$(mktemp)"
trap 'rm -f "$tested_packages"' EXIT
go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... >"$tested_packages"

go tool cover -func="$profile" | awk -v tested_list="$tested_packages" '
  BEGIN {
    while ((getline line < tested_list) > 0) {
      tested[line] = 1
    }
  }
  /^github\.com\/zeokeel\/zeokeel-rig\// {
    file = $1
    pct = $3
    sub(/%/, "", pct)
    parts = split(file, segments, "/")
    pkg = segments[1]
    for (i = 2; i < parts; i++) {
      pkg = pkg "/" segments[i]
    }
    if (!(pkg in tested)) {
      next
    }
    functions[pkg]++
    sum[pkg] += pct
  }
  END {
    status = 0
    for (pkg in functions) {
      average = sum[pkg] / functions[pkg]
      if (average + 0 < 70) {
        printf "::error::package %s coverage %.1f%% is below the 70%% per-package floor\n", pkg, average
        status = 1
      }
    }
    if (status == 0) {
      print "coverage: every tested package is at or above the 70% per-package floor"
    }
    exit status
  }'
