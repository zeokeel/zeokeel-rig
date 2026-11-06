# ZeoKeel Rig developer tasks.
#
# Go 1.27+ is the only prerequisite: the module has zero dependencies. Every
# target mirrors a step of .github/workflows/ci.yml or the release checklist in
# docs/RELEASE.md, so `make ci` is the local gate a pull request must pass.
#
# Usage:
#   make                    # same as `make ci`
#   make help               # list targets
#   make fuzz FUZZTIME=10s  # longer fuzz smoke
#   make cover              # coverage profile in coverage.out (git-ignored)

GO        ?= go
GOFMT     ?= gofmt
FUZZTIME  ?= 3s
BENCHTIME ?= 1x
COVERAGE  ?= coverage.out

.DEFAULT_GOAL := ci

.PHONY: all ci help build vet deps fmt test race cover cover-floor fuzz bench gate quickstart examples cross version clean

all: ci

help: ## list available targets
	@awk 'BEGIN {FS = ":.*## "}; /^[a-zA-Z0-9_-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# --- formatting --------------------------------------------------------------

fmt: ## fail if gofmt would change any tracked file (fix with `gofmt -w <file>`)
	@set -e; command -v $(GOFMT) >/dev/null 2>&1 || { \
		echo "gofmt not found; install Go 1.27+ and add it to PATH (or pass GOFMT=/path/to/gofmt)"; \
		exit 1; \
	}; \
	unformatted="$$(git ls-files -z '*.go' | xargs -0 $(GOFMT) -l)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on these files:"; \
		echo "$$unformatted"; \
		echo "fix with: gofmt -w ."; \
		exit 1; \
	fi

# --- compile and static checks ----------------------------------------------

build: ## compile every package
	$(GO) build ./...

vet: ## run go vet
	$(GO) vet ./...

deps: ## enforce the zero-dependency and stdlib-only contract
	@deps="$$($(GO) list -m all | tail -n +2)"; \
	if [ -n "$$deps" ]; then \
		echo "unexpected module dependencies (the SDK must stay zero-dependency):"; \
		echo "$$deps"; \
		exit 1; \
	fi
	@self="github.com/zeokeel/zeokeel-rig"; \
	deps="$$($(GO) list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' . | grep -v '^$$' | grep -vxF "$$self" || true)"; \
	if [ -n "$$deps" ]; then \
		echo "root package imports non-stdlib packages:"; \
		echo "$$deps"; \
		exit 1; \
	fi

# --- tests ------------------------------------------------------------------

test: ## run the unit tests
	$(GO) test ./... -count=1

race: ## run the unit tests with the race detector
	$(GO) test ./... -count=1 -race

cover: ## run tests with a coverage profile and print the total
	$(GO) test ./... -count=1 -coverprofile=$(COVERAGE)
	@$(GO) tool cover -func=$(COVERAGE) | tail -n 1

cover-floor: ## fail when total coverage <80% or a package is below 70%
	$(GO) test ./... -count=1 -coverprofile=$(COVERAGE) >/dev/null
	bash scripts/coverage-floor.sh $(COVERAGE)

fuzz: ## short fuzz smoke for every Fuzz target in the module
	@set -e; fuzztime="$(FUZZTIME)"; found=0; \
	for pkg in $$($(GO) list ./...); do \
		targets=$$($(GO) test -list '^Fuzz' "$$pkg" 2>/dev/null | grep -E '^Fuzz' || true); \
		for target in $$targets; do \
			found=1; \
			echo "== $$target ($$pkg) =="; \
			$(GO) test -run='^$$' -fuzz="^$${target}$$" -fuzztime="$$fuzztime" "$$pkg"; \
		done; \
	done; \
	if [ "$$found" -eq 0 ]; then echo "fuzz: no Fuzz targets found; nothing to do"; fi

bench: ## run every benchmark once (smoke test for benchmark code)
	$(GO) test ./... -run='^$$' -bench=. -benchtime=$(BENCHTIME) -count=1

# --- repository gates -------------------------------------------------------

gate: ## tool-catalog and modern-surface coverage gates
	$(GO) test ./tools/full -run TestToolCatalogCoverage -count=1 -v
	$(GO) test ./tools/audit -count=1 -v

quickstart: ## verify the README quickstart still matches examples/quickstart/main.go
	@tmp="$$(mktemp)"; trap 'rm -f "$$tmp"' EXIT; \
	awk '/^```go$$/{f=1;next} f&&/^```$$/{exit} f' README.md > "$$tmp"; \
	diff -u examples/quickstart/main.go "$$tmp"

examples: ## compile the example programs
	$(GO) build ./examples/...

cross: ## cross-compile for the CI target matrix
	@for target in windows/amd64 darwin/arm64 freebsd/amd64; do \
		echo "== GOOS/GOARCH: $$target =="; \
		GOOS=$${target%/*} GOARCH=$${target#*/} $(GO) build ./...; \
	done

version: ## verify version.go matches the release tag (scripts/sync-version.sh)
	bash scripts/sync-version.sh

# --- aggregation ------------------------------------------------------------

ci: ## run the full local gate (mirrors CI and the release checklist)
	$(MAKE) fmt
	$(MAKE) build
	$(MAKE) vet
	$(MAKE) deps
	$(MAKE) test
	$(MAKE) race
	$(MAKE) cover-floor
	$(MAKE) gate
	$(MAKE) quickstart
	$(MAKE) examples
	$(MAKE) cross
	$(MAKE) version

clean: ## remove test artifacts
	rm -f $(COVERAGE)
