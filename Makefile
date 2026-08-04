# HarborMaster development tasks.
#
# HarborMaster is read-only: nothing here starts, changes, or removes a
# container.

SHELL := /bin/sh

BINARY      := harbormaster
CMD_PKG     := ./cmd/harbormaster
BIN_DIR     := bin
WEB_DIR     := web
DIST_DIR    := $(WEB_DIR)/dist
VERSION_PKG := github.com/Aznyi/HarborMaster/internal/version

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(VERSION_PKG).version=$(VERSION) \
	-X $(VERSION_PKG).commit=$(COMMIT) \
	-X $(VERSION_PKG).buildDate=$(BUILD_DATE)

# CGO is off deliberately: the SQLite driver is pure Go, so the binary is
# static and the container can run as an unprivileged user on a minimal base.
export CGO_ENABLED := 0

.DEFAULT_GOAL := help
IMAGE       ?= harbormaster
IMAGE_TAG   ?= dev
COMPOSE     := docker compose -f deployments/compose.yaml
COMPOSE_DEV := $(COMPOSE) -f deployments/compose.build.yaml

.PHONY: help tidy fmt fmt-check vet vet-integration test test-race test-integration cover \
        web-install web-test web-build web-dev \
        build run clean \
        docker-build docker-smoke docker-inspect \
        compose-config compose-up compose-up-build compose-down ci

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

## ---------------------------------------------------------------- backend --

tidy: ## Sync go.mod and go.sum
	go mod tidy

fmt: ## Format Go sources
	go fmt ./...

fmt-check: ## Fail if any Go source is unformatted (used by CI)
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "These files need gofmt:"; echo "$$unformatted"; exit 1; \
	fi

vet: ## Run go vet
	go vet ./...

test: ## Run Go tests
	go test ./...

test-race: ## Run Go tests with the race detector (needs cgo and a C toolchain)
	CGO_ENABLED=1 go test -race ./...

test-integration: ## Run the live-Docker integration suite (needs a reachable daemon)
	go test -tags integration -v -timeout 15m ./internal/integration/...

vet-integration: ## Vet the build-tagged integration suite, which the default vet skips
	go vet -tags integration ./...

cover: ## Run Go tests and report coverage
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1

## --------------------------------------------------------------- frontend --

web-install: ## Install frontend dependencies from the lockfile
	cd $(WEB_DIR) && npm ci

web-test: ## Run frontend tests
	cd $(WEB_DIR) && npm test

web-build: ## Type-check and build the frontend into web/dist
	cd $(WEB_DIR) && npm run build

web-dev: ## Run the Vite dev server (proxies /api to the backend)
	cd $(WEB_DIR) && npm run dev

## ------------------------------------------------------------------ build --

build: web-build ## Build the binary with the frontend embedded
	mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(CMD_PKG)

run: ## Run the server from source (binds to loopback by default)
	go run $(CMD_PKG)

clean: ## Remove build output
	rm -rf $(BIN_DIR) $(DIST_DIR)/* coverage.out
	@touch $(DIST_DIR)/.gitkeep

## ------------------------------------------------------------- deployment --

docker-build: ## Build the container image
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMAGE):$(IMAGE_TAG) .

docker-smoke: ## Build the image and run the container smoke tests against it
	BUILD=1 IMAGE=$(IMAGE):$(IMAGE_TAG) bash deployments/smoke-test.sh

docker-inspect: ## Show the image's user, entrypoint, health check, labels, and size
	@docker image inspect $(IMAGE):$(IMAGE_TAG) \
		--format 'user:       {{.Config.User}}{{"\n"}}entrypoint: {{json .Config.Entrypoint}}{{"\n"}}cmd:        {{json .Config.Cmd}}{{"\n"}}health:     {{json .Config.Healthcheck.Test}}{{"\n"}}size:       {{.Size}} bytes'
	@echo "labels:"
	@docker image inspect $(IMAGE):$(IMAGE_TAG) \
		--format '{{range $$k, $$v := .Config.Labels}}  {{$$k}}={{$$v}}{{"\n"}}{{end}}'

compose-config: ## Validate both Compose files
	$(COMPOSE) config >/dev/null && echo "compose.yaml OK"
	$(COMPOSE_DEV) config >/dev/null && echo "compose.yaml + compose.build.yaml OK"

compose-up: ## Start the stack from the published image
	$(COMPOSE) up -d

compose-up-build: ## Start the stack, building from this checkout
	$(COMPOSE_DEV) up --build -d

compose-down: ## Stop the stack
	$(COMPOSE) down

## ---------------------------------------------------------------------- CI --

ci: fmt-check vet vet-integration test web-install web-test web-build ## Everything CI runs
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(CMD_PKG)
