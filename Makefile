# AI Development Platform — build tooling.
#
# The CLI links the Microsandbox Go SDK, a cgo binding that dlopens an embedded FFI
# library at runtime (docs/MSB-SDK-MIGRATION.md). The build therefore requires CGO —
# it is NO LONGER a pure CGO_ENABLED=0 static binary. We export CGO_ENABLED=1 for
# every compiling target so the build is deterministic regardless of the runner's
# default. The library bytes are embedded (go:embed) and extracted at first run, so
# the result is still a single self-contained binary per platform.
export CGO_ENABLED := 1

BINARY      := ai
PKG         := github.com/jt-helsinki/stack-genie
VERSION_PKG := $(PKG)/internal/version
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
LDFLAGS     := -X $(VERSION_PKG).Version=$(VERSION)

.PHONY: all build release fmt fmt-check vet lint test test-acceptance test-integration tidy clean check

# Release assets are named ai-<os>-<arch> — the exact names installers/install.sh
# downloads. macOS is Apple Silicon only (arch §6.2). Because cgo cross-compilation
# needs a C toolchain per target, multi-arch assets are produced by a CI matrix that
# runs `make release` on each platform (darwin/arm64, linux/amd64, linux/arm64) — every
# runner builds its OWN native arch; `make release` here builds the host arch only.

all: build

check: fmt-check vet lint test build ## Pre-commit gate: everything that must pass before committing

build: ## Build the ai binary into ./bin (cgo)
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/ai

release: ## Build the release binary for the HOST platform into ./dist (ai-<os>-<arch>)
	@mkdir -p dist
	@os=$$(go env GOOS); arch=$$(go env GOARCH); \
		echo "building dist/$(BINARY)-$$os-$$arch (cgo, native)"; \
		go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-$$os-$$arch ./cmd/ai

fmt: ## Format all Go source
	gofmt -w .

fmt-check: ## Fail if any file is not gofmt-clean
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet: ## go vet
	go vet ./...

lint: ## golangci-lint (skipped with a warning if not installed)
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "warning: golangci-lint not installed; skipping"; \
	fi

test: ## Run unit tests
	go test ./...

test-acceptance: ## Run the acceptance suite (set AIP_HARDWARE_TESTS=1 to include the full-stack [S1] tests on a provisioned host)
	go test -count=1 ./test/acceptance/

test-integration: ## Run the LIVE integration suite against a running Docker + Microsandbox stack (self-skips if the stack is absent). See test/integration/README.md.
	go test -tags integration ./test/integration/... -v -timeout 30m

.PHONY: test-all

test-all: ## Run all test suites
	$(MAKE) test
	$(MAKE) test-acceptance
	$(MAKE) test-integration

tidy: ## Sync go.mod/go.sum
	go mod tidy

diagram:
	mmdc -i docs/architecture.mmd -o docs/architecture.png -s 3 -w 2400 -H 1600 -b white

install:
	make build && ./installers/install-local.sh

clean:
	rm -rf bin dist
