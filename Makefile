# SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
# SPDX-License-Identifier: LGPL-3.0-or-later

GO      ?= go
BIN     ?= bin/clusterctl
PKG     := github.com/GSI-HPC/clusterctl
COVER   ?= coverage.out

.PHONY: all
all: build

## build: compile the binary into bin/
.PHONY: build
build:
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN) ./cmd/clusterctl

## test: run the unit and command tests
.PHONY: test
test:
	$(GO) test ./...

## race: run the tests under the race detector
.PHONY: race
race:
	$(GO) test -race ./...

## cover: run the tests and report total statement coverage
.PHONY: cover
cover:
	$(GO) test -coverprofile=$(COVER) -covermode=atomic ./...
	$(GO) tool cover -func=$(COVER) | tail -n 1

## lint: vet the tree and check formatting
.PHONY: lint
lint:
	$(GO) vet ./...
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

## tidy: prune and verify the module requirements
.PHONY: tidy
tidy:
	$(GO) mod tidy
	$(GO) mod verify

## docs: regenerate the command reference under the documentation site
.PHONY: docs
docs:
	$(GO) run ./internal/tools/gendocs -out site/content/reference

## clean: remove build and test output
.PHONY: clean
clean:
	rm -rf bin dist $(COVER)

## help: list the available targets
.PHONY: help
help:
	@sed -n 's/^## //p' $(MAKEFILE_LIST)
