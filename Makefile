.DEFAULT_GOAL := help
.PHONY: help run worker test test-db lint fmt build spec check migrate migrate-down migrate-status seed db-up db-down db-create

DB_URL      ?= postgres://muallim:muallim@localhost:5432/muallim?sslmode=disable
TEST_DB_URL ?= postgres://muallim:muallim@localhost:5432/muallim_test?sslmode=disable
TEST_S3_URL ?= http://localhost:9002

help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-14s\033[0m %s\n", $$1, $$2}'

run: ## Start the API server
	MUALLIM_DATABASE_URL="$(DB_URL)" go run ./cmd/api

worker: ## Start the background job worker
	MUALLIM_DATABASE_URL="$(DB_URL)" go run ./cmd/worker

build: ## Compile all binaries into bin/
	go build -o bin/ ./cmd/...

test: ## Run tests (database tests skip without a test database)
	go test ./... -race

test-db: ## Run every test, including the ones that need Postgres and MinIO
	MUALLIM_TEST_DATABASE_URL="$(TEST_DB_URL)" \
	MUALLIM_TEST_S3_ENDPOINT="$(TEST_S3_URL)" \
	go test ./... -race

fmt: ## Format all Go source
	gofmt -w .

lint: ## Vet and check formatting
	go vet ./...
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

check: lint test-db ## Everything CI runs

spec: ## Write the OpenAPI spec to bin/openapi.json — the contract for muallim-web
	@mkdir -p bin
	go run ./cmd/api -dump-spec > bin/openapi.json
	@echo "wrote bin/openapi.json"

migrate: ## Apply migrations to the development database
	MUALLIM_DATABASE_URL="$(DB_URL)" go run ./cmd/migrate up
	MUALLIM_DATABASE_URL="$(TEST_DB_URL)" go run ./cmd/migrate up

migrate-down: ## Roll back the last migration
	MUALLIM_DATABASE_URL="$(DB_URL)" go run ./cmd/migrate down

migrate-status: ## Show migration status
	MUALLIM_DATABASE_URL="$(DB_URL)" go run ./cmd/migrate status

seed: ## Fill the dev database with a workspace, a demo account, and enough data to click around
	MUALLIM_DATABASE_URL="$(DB_URL)" go run ./cmd/seed -reset

seed-huge: ## The same, at the size the pages will really be: ~300k rows
	MUALLIM_DATABASE_URL="$(DB_URL)" go run ./cmd/seed -reset \
		-workspaces 3 -courses 60 -students 1200 -topics 5 -lessons 6

seed-test: ## Only the bare workspace muallim-web's end-to-end tests need
	@psql -q "$(TEST_DB_URL)" -c "INSERT INTO tenants (subdomain, name) VALUES ('localhost', 'Acme Academy') \
		ON CONFLICT (lower(subdomain)) DO NOTHING"
	@echo "workspace 'localhost' is ready in muallim_test"

db-create: ## Create the muallim role and both databases on a local Postgres
	@psql -q postgres -tAc "SELECT 1 FROM pg_roles WHERE rolname='muallim'" | grep -q 1 || \
		psql -q postgres -c "CREATE ROLE muallim LOGIN PASSWORD 'muallim'"
	@psql -q postgres -tAc "SELECT 1 FROM pg_database WHERE datname='muallim'" | grep -q 1 || \
		psql -q postgres -c "CREATE DATABASE muallim OWNER muallim"
	@psql -q postgres -tAc "SELECT 1 FROM pg_database WHERE datname='muallim_test'" | grep -q 1 || \
		psql -q postgres -c "CREATE DATABASE muallim_test OWNER muallim"
	@echo "role muallim and databases muallim, muallim_test are ready"

db-up: ## Start Postgres in Docker
	docker compose up -d postgres

storage-up: ## Start MinIO, and create the bucket
	docker compose up -d minio minio-bucket
	@echo "MinIO on :9002, console on :9003 — muallim / muallim-secret-key"

db-down: ## Stop Postgres
	docker compose down
