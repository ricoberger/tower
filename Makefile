VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build
build:
	go build -ldflags "$(LDFLAGS)" -o ./bin/tower .

.PHONY: test
test:
	go test -race ./...

.PHONY: lint
lint:
	go vet ./...
	golangci-lint run ./...
