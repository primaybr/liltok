BINARY_NAME=liltok
BUILD_DIR=bin
CMD_DIR=./cmd/liltok

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.1.4-beta")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || echo "unknown")

LDFLAGS=-ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE) -s -w"

.PHONY: all build test clean run

all: build

build:
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) $(CMD_DIR)

test:
	go test -v -race -coverprofile=coverage.out ./...

bench:
	go test -bench=. -benchmem ./...

run: build
	./$(BUILD_DIR)/$(BINARY_NAME) start

clean:
	rm -rf $(BUILD_DIR) coverage.out
