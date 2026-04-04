# ============================================================================
# log_analyser — Makefile
# ============================================================================

# Build metadata
APP       := loganalyser
MODULE    := log_analyser
BIN_DIR   := bin
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -ldflags "-X main.version=$(VERSION)"

# Tools
GO        := go
GOTEST    := $(GO) test
GOBUILD   := $(GO) build
GOLINT    := golangci-lint

# Platforms for cross-compilation
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

# ============================================================================
# Default target
# ============================================================================

.PHONY: all
all: build

# ============================================================================
# Build
# ============================================================================

.PHONY: build
build: ## Build the binary for the current platform
	$(GOBUILD) $(LDFLAGS) -o $(BIN_DIR)/$(APP) ./cmd/loganalyser

.PHONY: build-all
build-all: ## Cross-compile for all supported platforms
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; \
		arch=$${platform#*/}; \
		ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		echo "Building $$os/$$arch..."; \
		GOOS=$$os GOARCH=$$arch $(GOBUILD) $(LDFLAGS) \
			-o $(BIN_DIR)/$(APP)-$$os-$$arch$$ext ./cmd/loganalyser; \
	done

# ============================================================================
# Test
# ============================================================================

.PHONY: test
test: ## Run all tests
	$(GOTEST) ./... -timeout 120s

.PHONY: test-verbose
test-verbose: ## Run all tests with verbose output
	$(GOTEST) ./... -v -timeout 120s

.PHONY: test-coverage
test-coverage: ## Run tests with coverage and generate HTML report
	$(GOTEST) ./... -coverprofile=coverage.out -timeout 120s
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

.PHONY: test-coverage-func
test-coverage-func: ## Show per-function coverage summary
	$(GOTEST) ./... -coverprofile=coverage.out -timeout 120s
	$(GO) tool cover -func=coverage.out

# ============================================================================
# Lint & format
# ============================================================================

.PHONY: lint
lint: ## Run golangci-lint
	$(GOLINT) run ./...

.PHONY: fmt
fmt: ## Format all Go source files
	$(GO) fmt ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

# ============================================================================
# Dependencies
# ============================================================================

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy

.PHONY: deps
deps: ## Download all dependencies
	$(GO) mod download

# ============================================================================
# Docker
# ============================================================================

.PHONY: docker
docker: ## Build Docker image
	docker build -t $(APP):$(VERSION) -t $(APP):latest .

# ============================================================================
# Clean
# ============================================================================

.PHONY: clean
clean: ## Remove build artifacts and coverage files
	rm -rf $(BIN_DIR) coverage.out coverage.html

# ============================================================================
# Help
# ============================================================================

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
