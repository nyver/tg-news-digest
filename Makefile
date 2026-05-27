# TG News Digest Bot — Makefile
# ==============================

APP_NAME     := tg-news-digest
BUILD_DIR    := bin
MAIN_PKG     := ./cmd/bot
MODULE       := github.com/nyver/tg-news-digest

# Go toolchain
GO           := go
GOFMT        := gofmt
GOFLAGS      := -mod=mod

# Versions
VERSION      := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS      := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)
BUILDFLAGS   := $(GOFLAGS) -ldflags "$(LDFLAGS)"

.PHONY: build run test coverage fmt clean help

# Build the binary
build:
	@echo "→ Building $(APP_NAME) $(VERSION)..."
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(BUILDFLAGS) -o $(BUILD_DIR)/$(APP_NAME) $(MAIN_PKG)
	@echo "✓ Built $(BUILD_DIR)/$(APP_NAME)"

# Run in development mode
run:
	@$(GO) run $(GOFLAGS) $(MAIN_PKG) --config configs/config.yaml

# Test
test:
	@echo "→ Running tests..."
	$(GO) test ./... -count=1 -race -timeout 120s

# Test with coverage
coverage:
	@echo "→ Running tests with coverage..."
	$(GO) test ./... -count=1 -race -coverprofile=coverage.out -timeout 120s
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "✓ Coverage report: coverage.html"

# Format
fmt:
	@echo "→ Formatting code..."
	$(GOFMT) -s -w .

# Clean build artifacts
clean:
	@echo "→ Cleaning..."
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html
	@echo "✓ Cleaned"

# Show help
help:
	@echo "TG News Digest Bot — Makefile targets:"
	@echo "  make build          Build the binary"
	@echo "  make run            Run in development mode"
	@echo "  make test           Run tests with race detector"
	@echo "  make coverage       Run tests with coverage report"
	@echo "  make fmt            Format code"
	@echo "  make clean          Remove build artifacts"
	@echo "  make help           Show this help"
