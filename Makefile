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

# Release build settings. VERSION is stamped into the binary (payram-updater version);
# the release workflow passes the tag, local builds describe the checkout.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
RELEASE_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
RELEASE_LDFLAGS := -s -w -X main.version=$(VERSION)
# sha256sum on Linux runners, shasum on macOS; both print "<sum>  <file>".
SHA256 := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo "shasum -a 256")

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
fmt: ## Format all Go source files (skips gitignored paths)
	@echo "Formatting Go files..."
	@git ls-files --cached --others --exclude-standard '*.go' | xargs -r gofmt -w
	@echo "Formatting complete!"

.PHONY: fmt-check
fmt-check: ## Check if all Go files are formatted (skips gitignored paths)
	@echo "Checking Go formatting..."
	@UNFORMATTED=$$(git ls-files --cached --others --exclude-standard '*.go' | xargs -r gofmt -l); \
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
build-release: ## Build static release binaries for all platforms, plus SHA256SUMS (VERSION=<tag> to override)
	@echo "Building release binaries ($(VERSION))..."
	@rm -rf $(BUILD_DIR)/release
	@mkdir -p $(BUILD_DIR)/release
	@set -euo pipefail; \
	for target in $(RELEASE_TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; \
		out=$(BUILD_DIR)/release/$(BINARY_NAME)-$$os-$$arch; \
		echo "Building for $$os/$$arch..."; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o $$out $(MAIN_PATH); \
	done
	@echo "Writing checksums (per-binary .sha256 for older installers, plus SHA256SUMS)..."
	@set -euo pipefail; cd $(BUILD_DIR)/release; \
	for target in $(RELEASE_TARGETS); do \
		f=$(BINARY_NAME)-$${target%/*}-$${target#*/}; \
		$(SHA256) "$$f" > "$$f.sha256"; \
		cat "$$f.sha256" >> SHA256SUMS; \
	done
	@echo "Release binaries built in $(BUILD_DIR)/release/"
	@ls -lh $(BUILD_DIR)/release/

.PHONY: check-release
check-release: ## Verify release binaries: all present, checksums match, Linux ones static, version stamped
	@set -euo pipefail; cd $(BUILD_DIR)/release; \
	for target in $(RELEASE_TARGETS); do \
		f=$(BINARY_NAME)-$${target%/*}-$${target#*/}; \
		for need in "$$f" "$$f.sha256"; do \
			if [ ! -s "$$need" ]; then echo "ERROR: missing $$need"; exit 1; fi; \
		done; \
	done; \
	$(SHA256) -c SHA256SUMS; \
	for f in $(BINARY_NAME)-linux-*; do \
		case $$f in *.sha256) continue ;; esac; \
		if ! file "$$f" | grep -q 'statically linked'; then \
			echo "ERROR: $$f is not statically linked (it would need the server's glibc):"; file "$$f"; exit 1; \
		fi; \
	done; \
	native=$(BINARY_NAME)-$$($(GO) env GOOS)-$$($(GO) env GOARCH); \
	got=$$(./$$native version); \
	if [ "$$got" != "$(VERSION)" ]; then \
		echo "ERROR: $$native version printed '$$got', expected '$(VERSION)'"; exit 1; \
	fi; \
	echo "Release binaries OK ($(VERSION))"

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
