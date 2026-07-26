# wallet-service tasks. Run `make help` for the list.

SERVICE := wallet-service
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo local)
IMAGE   ?= arcadia/$(SERVICE)

.DEFAULT_GOAL := help
.PHONY: help build run test cover vet lint fmt tidy proto docker clean ci

help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Compile the binary into ./bin
	go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/$(SERVICE) ./cmd/$(SERVICE)

run: ## Run locally (expects the data layer to be up: cd ../infra && make up)
	go run ./cmd/$(SERVICE)

test: ## Unit tests with the race detector. Needs no infrastructure.
	go test -race -count=1 ./...

cover: ## Unit tests with a coverage report
	go test -race -count=1 -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -20

vet: ## go vet
	go vet ./...

lint: vet ## Vet plus staticcheck when it is installed
	@command -v staticcheck >/dev/null 2>&1 \
		&& staticcheck $$(go list ./... | grep -v '/internal/pb/') \
		|| echo "staticcheck not installed (go install honnef.co/go/tools/cmd/staticcheck@latest)"

fmt: ## Format everything except the generated code
	gofmt -s -w $$(find . -name '*.go' -not -path './internal/pb/*')

tidy: ## Tidy and verify the module graph
	go mod tidy && go mod verify

proto: ## Regenerate internal/pb from api/proto (needs buf)
	cd api/proto && buf lint && buf generate

docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):local -t $(IMAGE):$(VERSION) .

ci: tidy vet test ## Everything the pipeline runs

clean: ## Remove build artefacts
	rm -rf bin coverage.out
