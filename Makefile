.DEFAULT_GOAL := help
.PHONY: help run test lint fmt build db-up db-down spec check

help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-10s\033[0m %s\n", $$1, $$2}'

run: ## Start the API server
	go run ./cmd/api

build: ## Compile all binaries into bin/
	go build -o bin/ ./cmd/...

test: ## Run tests with the race detector
	go test ./... -race

fmt: ## Format all Go source
	gofmt -w .

lint: ## Vet and check formatting
	go vet ./...
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

check: lint test ## Everything CI runs

spec: ## Write the OpenAPI spec to bin/openapi.json — the contract for lms-web
	@mkdir -p bin
	go run ./cmd/api -dump-spec > bin/openapi.json
	@echo "wrote bin/openapi.json"

db-up: ## Start Postgres
	docker compose up -d postgres

db-down: ## Stop Postgres
	docker compose down
