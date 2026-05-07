.PHONY: help dev build test test-go test-integration test-frontend lint lint-go lint-frontend typecheck e2e chaos bench bench-regress up down clean frontend-dev fmt

GO_PKGS := ./internal/... ./pkg/...
BIN_DIR := bin

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-20s %s\n", $$1, $$2}'

dev: ## Run the server with race detector
	go run -race ./cmd/server

frontend-dev: ## Run the Vite dev server
	cd frontend && pnpm dev

build: ## Build server + reaper + bench binaries
	mkdir -p $(BIN_DIR)
	go build -trimpath -o $(BIN_DIR)/server ./cmd/server
	go build -trimpath -o $(BIN_DIR)/reaper ./cmd/reaper
	go build -trimpath -o $(BIN_DIR)/bench ./bench/
	go build -trimpath -o $(BIN_DIR)/bench-regress ./bench/regress

test: test-go test-frontend ## Run all tests

test-go: ## Run Go unit tests
	go test -race -count=1 -coverprofile=coverage.out $(GO_PKGS)

test-integration: ## Run Go integration tests (requires Docker)
	RUN_INTEGRATION=1 go test -race -count=1 -tags=integration -timeout=120s ./internal/...

test-frontend: ## Run frontend unit tests
	cd frontend && pnpm test

lint: lint-go lint-frontend ## Run all linters

lint-go: ## Run golangci-lint
	golangci-lint run ./...

lint-frontend: ## Run eslint + prettier check
	cd frontend && pnpm lint && pnpm format:check

typecheck: ## Type-check frontend
	cd frontend && pnpm typecheck

e2e: ## Run Playwright e2e tests
	cd frontend && pnpm e2e

chaos: ## Run the chaos test (requires local Docker)
	bash bench/chaos.sh

bench: ## Run the 100-session concurrent bench (requires local Docker)
	bash bench/run-bench.sh

bench-regress: ## Run the bench at smoke size and gate against the baseline
	BENCH_N=$${BENCH_N:-10} BENCH_GATE=1 bash bench/run-bench.sh

up: ## Start the local docker-compose stack
	docker compose -f infra/docker-compose.yml up -d

down: ## Stop the local docker-compose stack
	docker compose -f infra/docker-compose.yml down

fmt: ## Format Go and frontend
	gofmt -w .
	cd frontend && pnpm format

clean:
	rm -rf $(BIN_DIR) coverage.out frontend/dist frontend/coverage
