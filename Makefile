# AI Development Platform — build tooling.
# The CLI is a single static binary (CLI spec §1.5).

BINARY      := ai
PKG         := github.com/jt-helsinki/ideal-robot
VERSION_PKG := $(PKG)/internal/version
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
LDFLAGS     := -X $(VERSION_PKG).Version=$(VERSION)

.PHONY: all build release fmt fmt-check vet lint test test-acceptance test-integration tidy clean models-refresh

# Release targets. Asset names are ai-<os>-<arch> — the exact names
# installers/install.sh downloads. macOS is Apple Silicon only (arch §6.2).
DIST_PLATFORMS := darwin/arm64 linux/amd64 linux/arm64

all: build

build: ## Build the ai binary into ./bin
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/ai

release: ## Cross-compile static release binaries into ./dist (ai-<os>-<arch>)
	@mkdir -p dist
	@for platform in $(DIST_PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "building dist/$(BINARY)-$$os-$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-$$os-$$arch ./cmd/ai || exit 1; \
	done

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

tidy: ## Sync go.mod/go.sum
	go mod tidy

models-refresh: ## Re-scrape ollama.com and rewrite the bundled popular-models snapshot (internal/ollama/models.yaml). Needs internet; maintainer-only.
	go run ./internal/ollama/internal/gen

clean:
	rm -rf bin dist
