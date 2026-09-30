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

## lint: vet the tree, the end-to-end tests included, and check formatting
.PHONY: lint
lint:
	$(GO) vet ./...
	$(GO) vet -tags e2e ./e2e/
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

## e2e-up: create the sind cluster the end-to-end tests run against
.PHONY: e2e-up
e2e-up:
	sind create cluster --config e2e/testdata/sind-cluster.yaml

## e2e-hosts: print the /etc/hosts lines that resolve the cluster's nodes here
.PHONY: e2e-hosts
e2e-hosts:
	@sind --realm e2e get dns | awk 'NR > 1 { print $$2, $$1 }'

## e2e: run the end-to-end tests against that cluster
.PHONY: e2e
e2e: build
	CLUSTERCTL_E2E_BINARY="$(CURDIR)/$(BIN)" $(GO) test -tags e2e -count=1 -timeout 15m ./e2e/

## e2e-down: delete the cluster
.PHONY: e2e-down
e2e-down:
	sind --realm e2e delete cluster alpha

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
