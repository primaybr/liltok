BINARY_NAME=liltok
BUILD_DIR=bin
CMD_DIR=./cmd/liltok

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.2.4-beta")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || echo "unknown")

LDFLAGS=-ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE) -s -w"

GOLANGCI_LINT_VERSION ?= v2.14.0
# Floor for total statement coverage; raise it as coverage improves (target 85%, see docs/ROADMAP.md M10).
COVERAGE_MIN ?= 85

.PHONY: all build test clean run fmt fmt-check vet lint cover check

all: build

build:
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) $(CMD_DIR)

test:
	go test -v -race -coverprofile=coverage.out ./...

fmt:
	gofmt -w $$(git ls-files '*.go')

fmt-check:
	@out=$$(gofmt -l $$(git ls-files '*.go')); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	golangci-lint run ./...

# Tests open many temporary databases; skip seeding the 20 MB starter pack into each one
# (the seeding test re-enables it for itself).
cover:
	LILTOK_SKIP_STARTER_SEED=1 go test -race -coverprofile=coverage.out ./...
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/ {sub("%","",$$3); print $$3}'); \
	echo "total coverage: $$total% (minimum $(COVERAGE_MIN)%)"; \
	awk -v t="$$total" -v m="$(COVERAGE_MIN)" 'BEGIN { exit (t+0 < m+0) }' || { echo "coverage below minimum"; exit 1; }

check: fmt-check vet lint cover

bench:
	go test -bench=. -benchmem ./...

run: build
	./$(BUILD_DIR)/$(BINARY_NAME) start

clean:
	rm -rf $(BUILD_DIR) coverage.out
