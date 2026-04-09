# budgetclaw Makefile
#
# Convention: `make` with no args shows help. All targets are thin
# wrappers around the standard Go toolchain so contributors can read
# the commands and understand what's happening.

BINARY      := budgetclaw
PKG         := github.com/RoninForge/budgetclaw
CMD         := ./cmd/budgetclaw
BIN_DIR     := bin
COVER_FILE  := coverage.txt

# Version metadata injected via -ldflags. VERSION falls back to the
# current git tag, then `dev` for untagged working trees.
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_DATE  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/internal/version.version=$(VERSION) \
	-X $(PKG)/internal/version.commit=$(COMMIT) \
	-X $(PKG)/internal/version.buildDate=$(BUILD_DATE)

GO_BUILD_FLAGS := -trimpath -ldflags "$(LDFLAGS)"

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: build
build: ## Compile the binary into ./bin/
	@mkdir -p $(BIN_DIR)
	go build $(GO_BUILD_FLAGS) -o $(BIN_DIR)/$(BINARY) $(CMD)

.PHONY: install
install: ## Install the binary to $GOBIN / $GOPATH/bin
	go install $(GO_BUILD_FLAGS) $(CMD)

.PHONY: run
run: ## Compile and run with any ARGS (e.g. make run ARGS=version)
	go run $(GO_BUILD_FLAGS) $(CMD) $(ARGS)

.PHONY: test
test: ## Run tests with race detector and coverage
	go test -race -coverprofile=$(COVER_FILE) -covermode=atomic ./...

.PHONY: cover
cover: test ## Show coverage by function
	go tool cover -func=$(COVER_FILE)

.PHONY: cover-html
cover-html: test ## Open HTML coverage report
	go tool cover -html=$(COVER_FILE)

.PHONY: lint
lint: ## Run golangci-lint
	@command -v golangci-lint >/dev/null 2>&1 || { echo >&2 "golangci-lint not installed. See https://golangci-lint.run/welcome/install/"; exit 1; }
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Format code with gofmt
	gofmt -s -w .

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: tidy
tidy: ## Tidy and verify go.mod
	go mod tidy
	go mod verify

.PHONY: snapshot
snapshot: ## Build a local goreleaser snapshot (no publish)
	@command -v goreleaser >/dev/null 2>&1 || { echo >&2 "goreleaser not installed. See https://goreleaser.com/install/"; exit 1; }
	goreleaser release --snapshot --clean

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) dist $(COVER_FILE) coverage.html

.PHONY: check
check: fmt vet lint test ## Run fmt, vet, lint, and tests
