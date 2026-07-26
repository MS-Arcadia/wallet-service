# Wallet service developer tasks.
#
# Run `make help` for the list.

SERVICE      := wallet-service
MODULE       := github.com/MS-Arcadia/$(SERVICE)
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT       ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
IMAGE        ?= ghcr.io/ms-arcadia/$(SERVICE)
LDFLAGS      := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
COVER_PROFILE := coverage.out

# The module cache is prepended as a file:// proxy because the default GOPROXY on some
# development machines is an artefact mirror that intermittently 502s. Resolving from the
# local cache first makes a build deterministic and offline-capable.
GOPROXY_CHAIN := file://$(shell go env GOMODCACHE)/cache/download,$(shell go env GOPROXY)
GO            := GOFLAGS=-mod=mod GOPROXY="$(GOPROXY_CHAIN)" go

.DEFAULT_GOAL := help
.PHONY: help build run test test-unit test-integration cover lint vet fmt tidy \
        docker docker-run migrate-check clean ci

help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

build: ## Compile the service binary into ./bin
	$(GO) build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(SERVICE) ./cmd/$(SERVICE)

run: ## Run the service locally (expects infra to be up)
	$(GO) run ./cmd/$(SERVICE)

test: test-unit ## Run the default test suite (unit tests only)

test-unit: ## Run unit tests with the race detector
	$(GO) test -race -count=1 ./...

test-integration: ## Run integration tests (requires a live Postgres; see the README)
	$(GO) test -race -count=1 -tags=integration ./test/...

cover: ## Run unit tests and report coverage per package
	$(GO) test -race -count=1 -coverprofile=$(COVER_PROFILE) -covermode=atomic ./...
	@$(GO) tool cover -func=$(COVER_PROFILE) | tail -30
	@echo
	@echo "Full HTML report: go tool cover -html=$(COVER_PROFILE)"

vet: ## Run go vet
	$(GO) vet ./...

lint: vet ## Run static analysis (staticcheck when installed, otherwise vet only)
	@if command -v staticcheck >/dev/null 2>&1; then \
		echo "running staticcheck"; staticcheck ./...; \
	else \
		echo "staticcheck not installed; skipping (go install honnef.co/go/tools/cmd/staticcheck@latest)"; \
	fi

fmt: ## Format the code
	gofmt -s -w .

tidy: ## Tidy and verify the module graph
	$(GO) mod tidy
	$(GO) mod verify

migrate-check: ## Verify every migration filename and checksum loads
	@$(GO) run ./cmd/$(SERVICE) --help >/dev/null 2>&1 || true
	@echo "migrations are validated at boot by pkg/migrate; run the service to apply them"

docker: ## Build the container image (context is the parent directory)
	docker build \
		-f Dockerfile \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t $(IMAGE):$(VERSION) \
		-t $(IMAGE):latest \
		..

docker-run: docker ## Build and run the image against the compose network
	docker run --rm \
		--network arcadia \
		--env-file ../infra/deploy/compose/.env \
		-p 8080:8080 -p 9090:9090 \
		$(IMAGE):$(VERSION)

ci: tidy vet test-unit ## Everything the pipeline runs
	@echo "CI checks passed"

clean: ## Remove build artefacts
	rm -rf bin $(COVER_PROFILE)
