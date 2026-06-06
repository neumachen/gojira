# Makefile for gojira. Wraps the standard Go toolchain commands
# the project's CI workflow and scripts/aider-*.sh also use.

.PHONY: build test test-race lint fmt install docker-build docker-run help

help: ## Print this help.
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the gojira CLI binary.
	go build -trimpath -ldflags="-s -w" -o gojira ./cmd/gojira

test: ## Run the full test suite.
	go test ./...

test-race: ## Run the full test suite with the race detector.
	go test -race ./...

lint: ## Run gofmt -l, go vet, and the aider-lint-go.sh script.
	@gofmt -l .
	go vet ./...
	@./scripts/aider-lint-go.sh

fmt: ## Format every Go file in place.
	gofmt -w .

install: ## Install the gojira CLI to $$GOBIN (or $$GOPATH/bin).
	go install ./cmd/gojira

docker-build: ## Build the gojira container image.
	docker build -t gojira:dev .

docker-run: ## Run the gojira container with the local .env.
	docker compose run --rm gojira --help
