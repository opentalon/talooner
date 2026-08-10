.PHONY: build test lint vet tidy

# VERSION is stamped into both binaries. CI and the release workflow pass a tag;
# a local build reports the git describe, or "dev" outside a checkout.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/opentalon/talooner/internal/version.Version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/talooner-action ./cmd/talooner-action
	go build -ldflags "$(LDFLAGS)" -o bin/talooner ./cmd/talooner

test:
	go test -race ./...

lint:
	golangci-lint run

vet:
	go vet ./...

tidy:
	go mod tidy
