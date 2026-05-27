.DEFAULT_GOAL := help
SHELL := /bin/bash

# node@22 is keg-only on this host; ensure it is on PATH for pnpm
export PATH := /opt/homebrew/opt/node@22/bin:$(PATH)

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: bootstrap
bootstrap: ## First-time dev setup
	./scripts/bootstrap.sh

COMPOSE := docker compose -f infra/docker-compose.yml
COMPOSE_DEV := $(COMPOSE) -f infra/docker-compose.dev.yml

.PHONY: dev
dev: ## Run full stack for local development
	$(COMPOSE_DEV) up

.PHONY: up
up: ## Run the production-style stack (built images)
	$(COMPOSE) up -d

.PHONY: down
down: ## Stop the local stack
	$(COMPOSE_DEV) down

.PHONY: observability
observability: ## Run the stack with the observability profile (otel-lgtm)
	$(COMPOSE) --profile observability up -d

.PHONY: docker-build
docker-build: ## Build the api + web container images
	docker build -f infra/Dockerfile.api -t wpmgr-api:dev .
	docker build -f infra/Dockerfile.web -t wpmgr-web:dev .

.PHONY: build
build: build-api build-web ## Build everything

.PHONY: build-api
build-api: ## Build the Go API binary
	cd apps/api && go build -o bin/wpmgr ./cmd/wpmgr

.PHONY: build-web
build-web: ## Build the web SPA
	pnpm --filter @wpmgr/web build

.PHONY: test
test: test-api test-web ## Run all tests

.PHONY: test-api
test-api: ## Run Go tests
	cd apps/api && go test ./...

.PHONY: test-web
test-web: ## Run frontend tests
	pnpm run test

.PHONY: lint
lint: ## Lint everything
	cd apps/api && go vet ./...
	pnpm run lint

.PHONY: agent-zip
agent-zip: ## Package the WordPress agent plugin as a zip
	cd apps/agent && zip -r ../../release/wpmgr-agent.zip . -x 'vendor/*' 'tests/*' '*.dist'

.PHONY: gen
gen: ## Regenerate OpenAPI clients (Go + TS)
	./scripts/gen-openapi.sh
