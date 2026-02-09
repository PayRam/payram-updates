# Payram Internal Code Helper - Makefile
.DEFAULT_GOAL := help

# Use bash for all shell commands (required for [[ ]] syntax)
SHELL := /bin/bash

# Variables
BINARY_NAME := payram-updater
BUILD_DIR := bin
MAIN_PATH := ./cmd/payram-updater
GO := go
GOFLAGS := -v
COVERAGE_FILE := coverage.out
COVERAGE_HTML := coverage.html

# Colors for help text
CYAN := \033[36m
RESET := \033[0m

##@ Setup

.PHONY: setup
setup: ## Install dependencies and tools
	@echo "Installing dependencies..."
	@$(GO) mod download
	@$(GO) mod tidy
	@echo "Verifying dependencies..."
	@$(GO) mod verify
	@echo "Installing development tools..."
	@which golangci-lint > /dev/null || (echo "Installing golangci-lint..." && go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)
	@echo "Setup complete!"

##@ Development

.PHONY: fmt
fmt: ## Format all Go source files
	@echo "Formatting Go files..."
	@$(GO) fmt ./...
	@echo "Formatting complete!"

.PHONY: fmt-check
fmt-check: ## Check if all Go files are formatted
	@echo "Checking Go formatting..."
	@UNFORMATTED=$$(find . -name '*.go' -not -path './data*' -not -path './vendor/*' -exec gofmt -l {} +); \
	if [ -n "$$UNFORMATTED" ]; then \
		echo "The following files are not formatted:"; \
		echo "$$UNFORMATTED"; \
		exit 1; \
	fi
	@echo "All files are properly formatted!"

.PHONY: vet
vet: ## Run go vet on all packages
	@echo "Running go vet..."
	@$(GO) vet ./cmd/... ./internal/...
	@echo "Vet complete!"

##@ Testing

.PHONY: test
test: ## Run all tests
	@echo "Running tests..."
	@$(GO) test $(GOFLAGS) ./cmd/... ./internal/...
	@echo "Tests complete!"

.PHONY: cover
cover: ## Run tests with coverage and generate HTML report
	@echo "Running tests with coverage..."
	@$(GO) test -coverprofile=$(COVERAGE_FILE) ./cmd/... ./internal/...
	@$(GO) tool cover -html=$(COVERAGE_FILE) -o $(COVERAGE_HTML)
	@echo "Coverage report generated: $(COVERAGE_HTML)"
	@$(GO) tool cover -func=$(COVERAGE_FILE) | grep total

##@ Build

.PHONY: build
build: ## Build the payram-updater binary
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	@$(GO) build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN_PATH)
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)"

.PHONY: build-release
build-release: ## Build release binaries for all platforms
	@echo "Building release binaries..."
	@mkdir -p $(BUILD_DIR)/release
	@echo "Building for Linux AMD64..."
	@GOOS=linux GOARCH=amd64 $(GO) build -o $(BUILD_DIR)/release/$(BINARY_NAME)-linux-amd64 $(MAIN_PATH)
	@echo "Building for Linux ARM64..."
	@GOOS=linux GOARCH=arm64 $(GO) build -o $(BUILD_DIR)/release/$(BINARY_NAME)-linux-arm64 $(MAIN_PATH)
	@echo "Release binaries built in $(BUILD_DIR)/release/"
	@ls -lh $(BUILD_DIR)/release/

##@ Run

.PHONY: run
run: build ## Build and run the service
	@echo "Running $(BINARY_NAME)..."
	@$(BUILD_DIR)/$(BINARY_NAME)

##@ Quality Checks

.PHONY: precommit-checks
precommit-checks: ## Run all pre-commit checks (fmt-check, vet, test, build)
	@set -euo pipefail; \
	$(MAKE) fmt-check; \
	$(MAKE) vet; \
	$(MAKE) test; \
	$(MAKE) build

.PHONY: precommit
precommit: precommit-checks ## Alias for precommit-checks

.PHONY: commit
commit: precommit-checks ## Run checks and create a conventional commit with prompts
	@set -euo pipefail; \
	TYPE=$${TYPE-}; SCOPE=$${SCOPE-}; MSG=$${MSG-}; BODY=$${BODY-}; BR=$${BR-}; BREAKING=$${BREAKING-}; FOOTER=$${FOOTER-}; ADD=$${ADD-}; \
	if ! git rev-parse --git-dir >/dev/null 2>&1; then echo "Not a git repo"; exit 1; fi; \
	TYPES="feat fix chore docs refactor perf test build ci revert"; \
	if [[ -z "$$ADD" ]]; then read -p "Stage all changes (git add -A)? [Y/n]: " ADD || true; fi; \
	if [[ -z "$$ADD" || "$$ADD" =~ ^[Yy] ]]; then git add -A; fi; \
	if git diff --cached --quiet; then echo "No staged changes; aborting commit"; exit 1; fi; \
	if [ -z "$$TYPE" ]; then \
		echo "Select commit type:"; i=1; for t in $$TYPES; do echo "  $$i) $$t"; i=$$((i+1)); done; \
		read -p "Choose number: " N || true; TYPE=$$(echo $$TYPES | awk -v n=$$N '{split($$0,a," "); print a[n]}'); \
	fi; \
	if [ -z "$$TYPE" ]; then echo "Commit type required"; exit 1; fi; \
	if [ -z "$$SCOPE" ]; then read -p "Optional scope (e.g., core/tools): " SCOPE || true; fi; \
	while [ -z "$$MSG" ]; do read -p "Short description (<=72 chars): " MSG || true; done; \
	read -p "Body (optional): " BODY || true; \
	read -p "Breaking change? [y/N]: " BR || true; \
	if [[ $${BR:-N} =~ ^(y|Y)$$ ]]; then read -p "Describe breaking change: " BREAKING || true; else BREAKING=""; fi; \
	read -p "Footer (e.g., Closes #123) (optional): " FOOTER || true; \
	HEADER="$$TYPE"; [ -n "$$SCOPE" ] && HEADER="$$HEADER($$SCOPE)"; [ -n "$$BREAKING" ] && HEADER="$$HEADER!"; HEADER="$$HEADER: $$MSG"; \
	MSGFILE=$$(mktemp); echo "$$HEADER" > $$MSGFILE; \
	[ -n "$$BODY" ] && { echo; echo "$$BODY"; } >> $$MSGFILE; \
	[ -n "$$BREAKING" ] && { echo; echo "BREAKING CHANGE: $$BREAKING"; } >> $$MSGFILE; \
	[ -n "$$FOOTER" ] && { echo; echo "$$FOOTER"; } >> $$MSGFILE; \
	if git commit -F $$MSGFILE; then echo "Commit created"; else echo "Commit failed"; fi; \
	rm -f $$MSGFILE

##@ Cleanup

.PHONY: clean
clean: ## Remove build artifacts and coverage files
	@echo "Cleaning build artifacts..."
	@rm -rf $(BUILD_DIR)
	@rm -f $(COVERAGE_FILE) $(COVERAGE_HTML)
	@rm -f $(BINARY_NAME)
	@echo "Clean complete!"

##@ Help

.PHONY: help
help: ## Display this help message
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make $(CYAN)<target>$(RESET)\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  $(CYAN)%-20s$(RESET) %s\n", $$1, $$2 } /^##@/ { printf "\n%s\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
	@echo ""
