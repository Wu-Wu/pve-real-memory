# pve-real-memory — build entirely inside local docker; no Go toolchain on
# the host. Release artifacts are static binaries for linux/amd64 AND
# linux/arm64 (Proxmox 9.2+ officially supports both; pick by `uname -m`:
# x86_64 -> amd64, aarch64 -> arm64).

BINARY  := pve-real-memory
HELPER  := proxmox-rmem-helper
BUILDS  := $(BINARY) $(HELPER)
ARCHS   := amd64 arm64
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
	@for b in $(BUILDS); do \
		for a in $(ARCHS); do \
			$(RUN) env GOOS=linux GOARCH=$$a go build -trimpath -ldflags '$(LDFLAGS)' \
				-o /src/bin/$$b-linux-$$a ./cmd/$$b || exit 1; \
		done; \
	done
	@file bin/*-linux-*

## build-local: native binary (for running inside an arm64 test VM)
build-local:
	@mkdir -p bin
	@for b in $(BUILDS); do \
		$(RUN) go build -trimpath -o /src/bin/$$b-local ./cmd/$$b || exit 1; \
	done

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
