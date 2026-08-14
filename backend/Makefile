.PHONY: build build-dev build-release generate test test-unit test-integration test-e2e

VERSION ?= $(shell ./scripts/resolve-version.sh)
COMMIT ?= $(shell git rev-parse --short=8 HEAD 2>/dev/null || printf unknown)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BUILD_TYPE ?= source
UPDATE_REPO ?= zcj-ui/sub2api-plus
DEV_VERSION ?= $(VERSION)-dev.$(COMMIT)
GO_BUILD_FLAGS ?=
LDFLAGS ?= -s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.Date=$(DATE) -X main.BuildType=$(BUILD_TYPE) -X main.UpdateRepo=$(UPDATE_REPO)

build:
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags="$(LDFLAGS)" -trimpath -o bin/server ./cmd/server

build-dev:
	@$(MAKE) build VERSION=$(DEV_VERSION) BUILD_TYPE=dev GO_BUILD_FLAGS="-tags=embed"

build-release:
	@$(MAKE) build BUILD_TYPE=release GO_BUILD_FLAGS="-tags=embed"

generate:
	go generate ./ent
	go generate ./cmd/server

test:
	go test ./...
	golangci-lint run ./...

test-unit:
	go test -tags=unit ./...

test-integration:
	go test -tags=integration ./...

test-e2e:
	./scripts/e2e-test.sh

test-e2e-local:
	go test -tags=e2e -v -timeout=300s ./internal/integration/...
