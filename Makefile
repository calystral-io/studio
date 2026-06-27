# Calystral Studio BFF - developer Makefile.
# ASCII only. All targets assume Go on PATH and, for proto, the protoc plugins
# from `make proto-tools` on PATH ($(go env GOPATH)/bin).

SHELL := /bin/bash
GOBIN := $(shell go env GOPATH)/bin
MODULE := github.com/calystral-io/studio

# Build identity injected via -ldflags (safe zero-values in dev).
VERSION ?= 0.1.0
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X $(MODULE)/internal/version.Version=$(VERSION) \
           -X $(MODULE)/internal/version.Commit=$(COMMIT) \
           -X $(MODULE)/internal/version.BuildTime=$(BUILD_TIME)

# Proto sources and generated output.
PROTO_DIR := api/proto
PROTO_FILES := $(wildcard $(PROTO_DIR)/*.proto)
# Canonical upstream location for the vendored protos.
CORE_PROTO_SRC := ../core/core-api/proto

.PHONY: all build run test lint vet fmt fmt-check proto proto-tools proto-sync tidy clean

all: build

## build: compile the studio binary into ./bin/studio
build:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/studio ./cmd/studio

## run: run the BFF server (fixture source, mock auth by default)
run:
	go run -ldflags "$(LDFLAGS)" ./cmd/studio serve

## test: full unit + integration suite with the race detector
test:
	go test ./... -race -count=1

## vet: go vet across the module
vet:
	go vet ./...

## fmt: format all Go sources
fmt:
	gofmt -w .

## fmt-check: fail if any file is not gofmt-clean
fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

## lint: gofmt-check + vet (the CI gate; golangci-lint is optional locally)
lint: fmt-check vet

## tidy: prune and verify module requirements
tidy:
	go mod tidy

## proto-tools: install the protoc Go plugins into $(GOBIN)
proto-tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

## proto: regenerate Go stubs from the vendored protos into internal/corepb
proto:
	PATH="$(GOBIN):$$PATH" protoc \
		--proto_path=$(PROTO_DIR) \
		--go_out=. --go_opt=module=$(MODULE) \
		--go-grpc_out=. --go-grpc_opt=module=$(MODULE) \
		$(PROTO_FILES)

## proto-sync: re-copy the canonical protos from core, re-stamping the vendor
## header and re-injecting the go_package option (upstream omits it), then
## regenerate. Run when core's proto contract changes.
proto-sync:
	@for f in query schema mutate proc; do \
		src="$(CORE_PROTO_SRC)/$$f.proto"; dst="$(PROTO_DIR)/$$f.proto"; \
		if [ ! -f "$$src" ]; then echo "missing canonical source $$src"; exit 1; fi; \
		gp="$(MODULE)/internal/corepb/$${f}pb;$${f}pb"; \
		printf '// Vendored from canonical source calystral-io/core/core-api/proto (do not edit by hand; sync via `make proto-sync`).\n' > "$$dst"; \
		awk -v gp="$$gp" '/^package /{print; print ""; print "option go_package = \"" gp "\";"; next} {print}' "$$src" >> "$$dst"; \
		echo "synced $$dst"; \
	done
	$(MAKE) proto
	@echo "proto-sync complete; review the diff and run 'make test'."

## clean: remove build artifacts
clean:
	rm -rf bin .gotmp
