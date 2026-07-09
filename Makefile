.DEFAULT_GOAL := help
.PHONY: help run worker test test-db lint fmt build spec check migrate migrate-down migrate-status seed db-up db-down db-create

DB_URL      ?= postgres://lms:lms@localhost:5432/lms?sslmode=disable
TEST_DB_URL ?= postgres://lms:lms@localhost:5432/lms_test?sslmode=disable

help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-14s\033[0m %s\n", $$1, $$2}'

run: ## Start the API server
	LMS_DATABASE_URL="$(DB_URL)" go run ./cmd/api

worker: ## Start the background job worker
	LMS_DATABASE_URL="$(DB_URL)" go run ./cmd/worker

build: ## Compile all binaries into bin/
	go build -o bin/ ./cmd/...

test: ## Run tests (database tests skip without a test database)
	go test ./... -race

test-db: ## Run every test, including the ones that need Postgres
	LMS_TEST_DATABASE_URL="$(TEST_DB_URL)" go test ./... -race

fmt: ## Format all Go source
	gofmt -w .

lint: ## Vet and check formatting
	go vet ./...
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

check: lint test-db ## Everything CI runs

spec: ## Write the OpenAPI spec to bin/openapi.json — the contract for lms-web
	@mkdir -p bin
	go run ./cmd/api -dump-spec > bin/openapi.json
	@echo "wrote bin/openapi.json"

migrate: ## Apply migrations to the development database
	LMS_DATABASE_URL="$(DB_URL)" go run ./cmd/migrate up
	LMS_DATABASE_URL="$(TEST_DB_URL)" go run ./cmd/migrate up

migrate-down: ## Roll back the last migration
	LMS_DATABASE_URL="$(DB_URL)" go run ./cmd/migrate down

migrate-status: ## Show migration status
	LMS_DATABASE_URL="$(DB_URL)" go run ./cmd/migrate status

seed: ## Create the development workspace that resolves for host "localhost"
	@psql -q "$(DB_URL)" -c "INSERT INTO tenants (subdomain, name) VALUES ('localhost', 'Acme Academy') \
		ON CONFLICT (lower(subdomain)) DO NOTHING"
	@echo "workspace 'localhost' is ready; lms-web on :5173 resolves to it"

db-create: ## Create the lms role and both databases on a local Postgres
	@psql -q postgres -tAc "SELECT 1 FROM pg_roles WHERE rolname='lms'" | grep -q 1 || \
		psql -q postgres -c "CREATE ROLE lms LOGIN PASSWORD 'lms'"
	@psql -q postgres -tAc "SELECT 1 FROM pg_database WHERE datname='lms'" | grep -q 1 || \
		psql -q postgres -c "CREATE DATABASE lms OWNER lms"
	@psql -q postgres -tAc "SELECT 1 FROM pg_database WHERE datname='lms_test'" | grep -q 1 || \
		psql -q postgres -c "CREATE DATABASE lms_test OWNER lms"
	@echo "role lms and databases lms, lms_test are ready"

db-up: ## Start Postgres in Docker
	docker compose up -d postgres

db-down: ## Stop Postgres
	docker compose down
