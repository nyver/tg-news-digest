# TG News Digest Bot — Makefile
# ==============================

APP_NAME     := tg-news-digest
BUILD_DIR    := bin
MAIN_PKG     := ./cmd/bot
MODULE       := github.com/nyver/tg-news-digest

# Go toolchain
GO           := go
GOFMT        := gofmt
LINTER       := golangci-lint
GOFLAGS      := -mod=mod

# Versions
VERSION      := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS      := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)
BUILDFLAGS   := $(GOFLAGS) -ldflags "$(LDFLAGS)"

.PHONY: all build test lint clean docker docker-run help

# Default target
all: lint test build

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

# Lint
lint:
	@echo "→ Running linter..."
	$(LINTER) run ./... --config=.golangci.yml

# Lint fix
lint-fix:
	@echo "→ Running linter with auto-fix..."
	$(LINTER) run ./... --config=.golangci.yml --fix

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

# Docker
docker:
	@echo "→ Building Docker image..."
	docker build -t $(APP_NAME):$(VERSION) .

docker-run: docker
	@echo "→ Running in Docker..."
	docker run --rm \
		--name $(APP_NAME) \
		-p 9100:9100 \
		-v $$(pwd)/configs:/etc/$(APP_NAME):ro \
		-v $$(pwd)/data:/app/data \
		$(APP_NAME):$(VERSION)

docker-compose-up:
	docker-compose up -d

docker-compose-down:
	docker-compose down

# Install linter
install-linter:
	@echo "→ Installing golangci-lint..."
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $$(go env GOPATH)/bin v1.59.1

# Sysdep install (for systemd deployment)
install-sys: build
	@echo "→ Installing systemd service..."
	sudo cp $(BUILD_DIR)/$(APP_NAME) /usr/local/bin/
	sudo cp tg-news-digest.service /etc/systemd/system/
	sudo systemctl daemon-reload
	sudo systemctl enable $(APP_NAME)
	@echo "✓ Installed. Run: sudo systemctl start $(APP_NAME)"

# Show help
help:
	@echo "TG News Digest Bot — Makefile targets:"
	@echo "  make build          Build the binary"
	@echo "  make run            Run in development mode"
	@echo "  make test           Run tests with race detector"
	@echo "  make coverage       Run tests with coverage report"
	@echo "  make lint           Run linter"
	@echo "  make lint-fix       Run linter with auto-fix"
	@echo "  make fmt            Format code"
	@echo "  make clean          Remove build artifacts"
	@echo "  make docker         Build Docker image"
	@echo "  make docker-run     Run in Docker"
	@echo "  make install-sys    Install systemd service"
	@echo "  make help           Show this help"
