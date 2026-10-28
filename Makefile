# ============================================================================
# hyper-go — Makefile
# Standard operating procedure: build, run, test, clean
# ============================================================================
#
# Source organization:
#   core/      — entry, runtime, WA I/O, storage, concurrency, core features
#   plugins/   — feature plugins (AI, downloaders, stickers, media, …)
#   owner/     — owner-only commands
#   security/  — anti-abuse, audit, hardening
#   util/      — OS helpers, generic utilities
#
# Build strategy: FLATTEN
#   Go doesn't allow one package to span multiple directories, so `build`
#   copies all .go files from subfolders into a temp .build/ directory,
#   runs `go build` there, then moves the binary back to root.
#
# Cross-compiling:
#   make build GOOS=linux GOARCH=arm64
#   Set CC=<toolchain> when a C compiler is required for the target.

BINARY     := bot
GO         := go
GOFLAGS    := -ldflags="-s -w"
LDFLAGS    := -s -w
# CGO is required: the sticker encoder (github.com/chai2010/webp) bundles and
# compiles libwebp from source, so a C compiler must be present on the build
# machine. Set CC=<toolchain> when cross-compiling.
BUILD_ENV  := CGO_ENABLED=1
GOTOOLCHAIN?= auto

# Target platform — defaults to the host platform.
# Override for cross builds, e.g.: make build GOOS=linux GOARCH=arm64
GOOS       ?= $(shell $(GO) env GOOS)
GOARCH     ?= $(shell $(GO) env GOARCH)

# C compiler used when CGO is enabled (e.g. an aarch64 cross toolchain).
# The sticker encoder bundles libwebp and compiles it from source, so a C
# compiler is required on the build machine.
CC         ?=

# Optional build tags (rarely needed).
BUILD_TAGS ?=

# Source directories (in upload order — order doesn't matter for build)
SRC_DIRS   := core plugins owner security util

# Temp build directory (gitignored)
BUILD_DIR  := .build

# ----------------------------------------------------------------------------
# Internal: flatten source into $(BUILD_DIR)
# ----------------------------------------------------------------------------
define flatten_source
	@echo ">> Flattening source files into $(BUILD_DIR)/..."
	@rm -rf $(BUILD_DIR)
	@mkdir -p $(BUILD_DIR)
	@cp go.mod go.sum $(BUILD_DIR)/
	@for dir in $(SRC_DIRS); do \
	        if [ -d $$dir ]; then \
	                cp $$dir/*.go $(BUILD_DIR)/ 2>/dev/null || true; \
	        fi; \
	done
	@if [ -f main.go ]; then cp main.go $(BUILD_DIR)/; fi
endef

# ----------------------------------------------------------------------------
# Build
# ----------------------------------------------------------------------------
.PHONY: build build-debug build-local tidy

build:
	$(flatten_source)
	@echo ">> Building $(BINARY) for $(GOOS)/$(GOARCH)..."
	@cd $(BUILD_DIR) && \
	        GOTOOLCHAIN=$(GOTOOLCHAIN) \
	        CC=$(CC) \
	        $(BUILD_ENV) GOOS=$(GOOS) GOARCH=$(GOARCH) \
	        $(GO) build $(GOFLAGS) -o ../$(BINARY) .
	@chmod +x $(BINARY)
	@rm -rf $(BUILD_DIR)
	@ls -lh $(BINARY)
	@file $(BINARY) 2>/dev/null || true

build-debug:
	$(flatten_source)
	@echo ">> Building $(BINARY) with debug symbols for $(GOOS)/$(GOARCH)..."
	@cd $(BUILD_DIR) && \
	        GOTOOLCHAIN=$(GOTOOLCHAIN) \
	        CC=$(CC) \
	        $(BUILD_ENV) GOOS=$(GOOS) GOARCH=$(GOARCH) \
	        $(GO) build -o ../$(BINARY) .
	@chmod +x $(BINARY)
	@rm -rf $(BUILD_DIR)

# Build for the local architecture — useful for testing
build-local:
	$(flatten_source)
	@echo ">> Building $(BINARY) for LOCAL architecture..."
	@cd $(BUILD_DIR) && GOTOOLCHAIN=$(GOTOOLCHAIN) $(BUILD_ENV) $(GO) build $(GOFLAGS) -o ../$(BINARY) .
	@chmod +x $(BINARY)
	@rm -rf $(BUILD_DIR)
	@ls -lh $(BINARY)

tidy:
	@GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) mod tidy

# ----------------------------------------------------------------------------
# Run / Dev
# ----------------------------------------------------------------------------
.PHONY: run run-web

run: build
	@./$(BINARY)

run-web: build
	@WEB_PORT=3000 ./$(BINARY)

# ----------------------------------------------------------------------------
# Test
# ----------------------------------------------------------------------------
.PHONY: test test-race vet fmt

test:
	$(flatten_source)
	@cd $(BUILD_DIR) && GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) test -v . 2>&1
	@rm -rf $(BUILD_DIR)

test-race:
	$(flatten_source)
	@cd $(BUILD_DIR) && GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) test -race . 2>&1
	@rm -rf $(BUILD_DIR)

# vet & fmt operate on the flattened tree (the project is ONE package main
# spread over several directories, so vetting a single directory cannot
# resolve the cross-directory symbols).
vet:
	$(flatten_source)
	@cd $(BUILD_DIR) && GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) vet . 2>&1
	@rm -rf $(BUILD_DIR)

fmt:
	@for dir in $(SRC_DIRS); do \
	        [ -d $$dir ] && $(GO) fmt ./$$dir/ || true; \
	done

# ----------------------------------------------------------------------------
# Backup
# ----------------------------------------------------------------------------
.PHONY: backup-archive

backup-archive:
	@ts=$$(date -u +%Y-%m-%dT%H%M%SZ); \
	archive="archive-$$ts.tar.gz"; \
	tar -czf "$$archive" \
	  --exclude='.git' \
	  --exclude='$(BINARY)' \
	  --exclude='$(BUILD_DIR)' \
	  --exclude='data' \
	  --exclude='*.tar.gz' \
	  --exclude='__pycache__' \
	  . ; \
	ls -lh "$$archive"; \
	echo ">> To restore: tar -xzf $$archive"

# ----------------------------------------------------------------------------
# Cleanup
# ----------------------------------------------------------------------------
.PHONY: clean clean-all

clean:
	@rm -f $(BINARY)
	@rm -rf $(BUILD_DIR)
	@echo ">> Removed $(BINARY) and $(BUILD_DIR)/"

clean-all: clean
	@rm -rf debug/ *.tar.gz archive-*.tar.gz
	@echo ">> Removed all build artifacts and archives"

# ----------------------------------------------------------------------------
# Docs / Help
# ----------------------------------------------------------------------------
.PHONY: help tree

help:
	@echo "hyper-go — Makefile targets"
	@echo ""
	@echo "Build:"
	@echo "  make build           Flatten + build the binary for \$$(GOOS)/\$$(GOARCH)"
	@echo "  make build-debug     Flatten + build with debug symbols"
	@echo "  make build-local     Flatten + build for the local architecture"
	@echo "  make tidy            Run go mod tidy"
	@echo ""
	@echo "Run:"
	@echo "  make run             Build + run"
	@echo "  make run-web         Build + run with WEB_PORT=3000"
	@echo ""
	@echo "Test:"
	@echo "  make test            Flatten + run all tests"
	@echo "  make test-race       Flatten + run tests with the race detector"
	@echo "  make vet             go vet on each subfolder"
	@echo "  make fmt             go fmt on each subfolder"
	@echo ""
	@echo "Backup:"
	@echo "  make backup-archive  Create a timestamped tar.gz of the source"
	@echo ""
	@echo "Cleanup:"
	@echo "  make clean           Remove the binary and .build/"
	@echo "  make clean-all       Remove all artifacts"
	@echo ""
	@echo "Cross-compiling:"
	@echo "  make build GOOS=linux GOARCH=arm64 CC=<toolchain>"

tree:
	@find . -type f -not -path './.git/*' -not -name '$(BINARY)' -not -path './$(BUILD_DIR)/*' | sort
