# pve-real-memory — static linux/amd64 binary for Proxmox VE hosts.

BINARY    := pve-real-memory
MODULE    := github.com/Wu-Wu/pve-real-memory
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)
GO        ?= go

.PHONY: all build test vet clean

all: build

## build: static linux/amd64 binary into bin/
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/$(BINARY)

## build-local: native binary for development on the build host
build-local:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY)-local ./cmd/$(BINARY)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

clean:
	rm -rf bin/
