# OpenGate development Makefile.
# Run `make help` (or just `make`) to list available targets.
#
# Conventions:
# - Every .PHONY target has `## Description` after the colon-separator;
#   the help target parses these to render the menu.
# - Tool binaries live under ./bin/ (project-local, not $GOPATH/bin).
# - All recipes use bash (not sh) for predictable shell behavior.

SHELL := /usr/bin/env bash

PROJECT_BIN          := ./bin
GOLANGCI_LINT_VERSION := v2.12.2
GOLANGCI_LINT         := $(PROJECT_BIN)/golangci-lint
OPENGATE_BIN          := $(PROJECT_BIN)/opengate

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help message
	@awk 'BEGIN {FS = ":.*?## "; printf "\nOpenGate Makefile targets:\n\n"} \
		/^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2} \
		END {printf "\n"}' $(MAKEFILE_LIST)

# ---------------------------------------------------------------------------
# Tool provisioning
# ---------------------------------------------------------------------------

.PHONY: tools
tools: $(GOLANGCI_LINT) ## Install or refresh all development tools
	@echo "==> Verifying lefthook is registered as a go tool..."
	@go tool lefthook version >/dev/null 2>&1 || { \
		echo "lefthook not registered. Run: go get -tool github.com/evilmartians/lefthook@latest"; \
		exit 1; \
	}
	@echo "==> All tools ready."

# golangci-lint is installed via the official install script per upstream
# recommendation; do not switch to `go install` or the tool directive.
$(GOLANGCI_LINT):
	@echo "==> Installing golangci-lint $(GOLANGCI_LINT_VERSION) to $(PROJECT_BIN)..."
	@mkdir -p $(PROJECT_BIN)
	@curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
		| sh -s -- -b $(PROJECT_BIN) $(GOLANGCI_LINT_VERSION)
	@$(GOLANGCI_LINT) --version

# ---------------------------------------------------------------------------
# Formatting and static analysis
# ---------------------------------------------------------------------------

.PHONY: fmt
fmt: ## Format all Go files in place (writes changes)
	@gofmt -w .

.PHONY: fmt-check
fmt-check: ## Verify all Go files are formatted; fail if any are not
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "Unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet over the entire module
	@go vet ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint over the entire module
	@$(GOLANGCI_LINT) run ./...

# ---------------------------------------------------------------------------
# Code generation
# ---------------------------------------------------------------------------

# sqlc is pinned as a go `tool` directive in go.mod (same convention as
# lefthook), so `go tool sqlc` runs the pinned version with no global install.
# The generated code is COMMITTED, so this target only needs re-running when a
# query in internal/adapters/outbound/postgres/queries or a migration a query
# depends on changes. CI builds the committed output and does not invoke sqlc.
.PHONY: generate
generate: ## Regenerate sqlc typed queries (commit the result)
	@echo "==> Running sqlc generate..."
	@go tool sqlc generate
	@echo "==> Done. Review and commit changes under internal/adapters/outbound/postgres/db/."

# Drift check: regenerate and fail if the working tree changed, proving the
# committed generated code matches the queries + schema. Kept OUT of `make ci`
# on purpose — `make ci` must not depend on sqlc being runnable, so a missing or
# broken sqlc binary can never break the main pipeline. Run it manually or as an
# optional, non-blocking CI job.
.PHONY: generate-check
generate-check: generate ## Fail if `sqlc generate` would change committed code
	@if ! git diff --quiet -- internal/adapters/outbound/postgres/db; then \
		echo "Generated code is stale. Run 'make generate' and commit the result:"; \
		git --no-pager diff --stat -- internal/adapters/outbound/postgres/db; \
		exit 1; \
	fi
	@echo "==> Generated code is up to date."

# ---------------------------------------------------------------------------
# Build and test
# ---------------------------------------------------------------------------

.PHONY: test
test: ## Run all tests
	@go test -coverprofile=coverage.out -covermode=atomic ./...

.PHONY: build
build: ## Build the opengate binary into ./bin/opengate
	@mkdir -p $(PROJECT_BIN)
	@go build -o $(OPENGATE_BIN) ./cmd/opengate
	@echo "==> Built $(OPENGATE_BIN)"

# ---------------------------------------------------------------------------
# Git hooks
# ---------------------------------------------------------------------------

.PHONY: hooks-install
hooks-install: ## Install lefthook git hooks into .git/hooks/
	@go tool lefthook install
	@echo "==> Hooks installed. Bypass with 'git commit --no-verify'."

# ---------------------------------------------------------------------------
# Composite
# ---------------------------------------------------------------------------

.PHONY: ci
ci: tools fmt-check vet lint test build ## Run the full CI pipeline locally

.PHONY: clean
clean: ## Remove build artifacts and tool binaries
	@rm -rf $(PROJECT_BIN) coverage.out coverage.html
	@echo "==> Cleaned."
