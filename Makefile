# pve-real-memory — build entirely inside local docker; no Go toolchain on
# the host. The release artifact is a static linux/amd64 binary.

BINARY  := pve-real-memory
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

IMAGE ?= golang:1.26
# go 1.26 module needs a matching toolchain image; pin if you bump go.mod.
DOCKER ?= docker
SRC := $(shell pwd)

# Cached modules/build cache so repeated runs stay fast.
GOFLAGS_ENV := -e GOFLAGS=-trimpath -e CGO_ENABLED=0
RUN := $(DOCKER) run --rm $(GOFLAGS_ENV) \
	-v $(SRC):/src -w /src \
	-v prm-gomod:/go/pkg/mod -v prm-gocache:/root/.cache/go-build \
	$(IMAGE)

.PHONY: all build build-local test vet fmt tidy cover shell clean image

all: build

## image: pull the builder image
image:
	$(DOCKER) pull $(IMAGE)

## build: static linux/amd64 release binary into ./bin
build:
	@mkdir -p bin
	$(RUN) env GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' \
		-o /src/bin/$(BINARY) ./cmd/$(BINARY)
	@file bin/$(BINARY)

## build-local: native binary (for running inside an arm64 test VM)
build-local:
	@mkdir -p bin
	$(RUN) go build -trimpath -o /src/bin/$(BINARY)-local ./cmd/$(BINARY)

test:
	$(RUN) go test ./...

vet:
	$(RUN) go vet ./...

fmt:
	$(RUN) gofmt -l -w /src

tidy:
	$(RUN) go mod tidy

cover:
	$(RUN) go test -coverprofile=/src/coverage.out ./...
	$(RUN) go tool cover -func=/src/coverage.out | tail -1

## shell: debug shell inside the builder image
shell:
	$(DOCKER) run --rm -it -v $(SRC):/src -w /src \
		-v prm-gomod:/go/pkg/mod -v prm-gocache:/root/.cache/go-build \
		$(IMAGE) bash

clean:
	rm -rf bin/ coverage.out
