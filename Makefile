# Toolchain-light by design: everything below is go, gofmt, or a pinned tool
# installed into ./bin. No global installs required.

GO          ?= go
MODULE      := github.com/brnom/ledger
BIN         := $(CURDIR)/bin
STATICCHECK := $(BIN)/staticcheck
PKGS        ?= ./...
FUZZTIME    ?= 30s
FUZZMINTIME ?= 5s
CHECKS      ?= 10000

# Fuzz targets are discovered rather than listed, so a new FuzzXxx is picked up
# by `make fuzz` without editing this file. Each entry is "package:target",
# because `go test -fuzz` refuses to run against more than one package at a
# time and the targets do not all live in the same one.
FUZZ_TARGETS = $(shell grep -rE '^func Fuzz[A-Za-z0-9_]+' --include='*_test.go' . | sed -E 's,^\./([^:]+)/[^/:]+_test\.go:func (Fuzz[A-Za-z0-9_]+).*,./\1:\2,' | sort -u)

# The property tests are discovered the same way and scoped to the packages
# that have them. -rapid.checks is registered only by those test binaries, and
# `go test` stops reading package arguments at the first flag it does not know
# itself -- so the packages have to come before it, and handing the flag to a
# binary that never registered it is an error.
PROP_PKGS = $(shell grep -rlE '^func TestProperty' --include='*_test.go' . | xargs -n1 dirname | sort -u)

.PHONY: help
help: ## List available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Compile every package
	$(GO) build $(PKGS)

# The PostgreSQL tests skip themselves unless this points at a database, so
# `make test` works with or without one running.
DSN ?= postgres://ledger:ledger@localhost:55432/ledger?sslmode=disable

.PHONY: test
test: ## Run unit tests (PostgreSQL tests skip without a database)
	$(GO) test $(PKGS)

.PHONY: test-integration
test-integration: pg ## Run every test, including the PostgreSQL ones
	LEDGER_TEST_POSTGRES_DSN="$(DSN)" $(GO) test -race $(PKGS)

.PHONY: pg
pg: ## Start PostgreSQL and wait for it
	docker compose up -d --wait postgres

.PHONY: pg-down
pg-down: ## Stop PostgreSQL and delete its data
	docker compose down -v

.PHONY: race
race: ## Run unit tests under the race detector
	$(GO) test -race $(PKGS)

.PHONY: prop
prop: ## Run the property tests hard
	$(GO) test $(PROP_PKGS) -run TestProperty -rapid.checks=$(CHECKS)

.PHONY: cover
cover: ## Run tests with coverage and print the total
	$(GO) test -race -covermode=atomic -coverprofile=coverage.out -coverpkg=./... $(PKGS)
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: fuzz
# -fuzzminimizetime is capped well under -fuzztime on purpose. Minimizing a
# newly interesting input is allowed to run long after the fuzzing deadline
# passes, and the coordinator gives up waiting on it: "context deadline
# exceeded", reported as a test failure with nothing actually wrong.
fuzz: ## Run every fuzz target for FUZZTIME (default 30s each)
	@for entry in $(FUZZ_TARGETS); do \
		pkg=$${entry%%:*}; target=$${entry##*:}; \
		echo "==> $$pkg $$target"; \
		$(GO) test $$pkg -run '^$$' -fuzz "^$$target$$" \
			-fuzztime=$(FUZZTIME) -fuzzminimizetime=$(FUZZMINTIME) || exit 1; \
	done

.PHONY: arch
arch: ## Fail if the dependency graph stops pointing inward
	@bad=$$($(GO) list -deps ./domain | grep '^$(MODULE)/' | grep -v '^$(MODULE)/domain$$'); \
	if [ -n "$$bad" ]; then echo "domain must depend on nothing else in the module:"; echo "$$bad"; exit 1; fi
	@bad=$$($(GO) list -deps ./app | grep '^$(MODULE)/' | grep -vE '^$(MODULE)/(app|domain)$$'); \
	if [ -n "$$bad" ]; then echo "app may depend only on domain:"; echo "$$bad"; exit 1; fi
	@echo "dependencies point inward"

.PHONY: fmt
fmt: ## Rewrite sources with gofmt
	gofmt -s -w .

.PHONY: fmtcheck
fmtcheck: ## Fail if any source needs gofmt
	@out=$$(gofmt -s -l .); \
	if [ -n "$$out" ]; then echo "not gofmt'd:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Run go vet
	$(GO) vet $(PKGS)

$(STATICCHECK):
	GOBIN=$(BIN) $(GO) install honnef.co/go/tools/cmd/staticcheck@latest

.PHONY: lint
lint: $(STATICCHECK) ## Run staticcheck
	$(STATICCHECK) $(PKGS)

.PHONY: check
check: fmtcheck vet lint arch race ## Everything CI runs

.PHONY: tidy
tidy: ## Tidy and verify module deps
	$(GO) mod tidy
	$(GO) mod verify

.PHONY: run
run: ## Start the ledger server in memory
	$(GO) run ./cmd/ledgerd

.PHONY: run-pg
run-pg: pg ## Start the ledger server against PostgreSQL
	$(GO) run ./cmd/ledgerd -dsn "$(DSN)"

.PHONY: clean
clean: ## Remove build and coverage artifacts
	rm -rf $(BIN) coverage.out
