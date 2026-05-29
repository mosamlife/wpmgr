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

.PHONY: agent-vendor
agent-vendor: ## Build a clean prod-only vendor/ for the agent (no-dev, stripped)
	# ADR-033 / M5.6: agent has ZERO production composer deps (we dropped phpbu
	# and ifsnop/mysqldump-php after the mysqli rewrite). composer install is
	# still needed to generate vendor/autoload.php's classmap for includes/.
	# Composer 2 in a container (no host PHP requirement). --no-dev drops dev
	# tooling; --ignore-platform-reqs skips ext-* runtime checks (the build
	# container doesn't ship ext-mysqli/zip/zlib; those checks happen on the
	# actual WP host at runtime, where the extensions are present).
	cd apps/agent && rm -rf vendor composer.lock
	docker run --rm -v "$(PWD)/apps/agent:/app" -w /app composer:2 install --no-dev --optimize-autoloader --classmap-authoritative --ignore-platform-reqs
	# Strip non-runtime files from the runtime vendors. Be conservative: only
	# drop directories named exactly tests/Tests/doc/docs/examples/.git, plus
	# CHANGELOG/UPGRADING/README .md files. Never touch *.php.
	cd apps/agent/vendor && find . -type d \( -name tests -o -name Tests -o -name doc -o -name docs -o -name examples -o -name .git -o -name .github \) -prune -exec rm -rf {} +
	cd apps/agent/vendor && find . -type f \( -name 'CHANGELOG*.md' -o -name 'UPGRADING*.md' -o -name 'README*.md' -o -name 'CONTRIBUTING*.md' -o -name '.gitignore' -o -name 'phpunit.xml*' -o -name 'phpstan.neon*' -o -name '.editorconfig' \) -delete
	@echo "agent vendor size: $$(du -sh apps/agent/vendor | cut -f1)"

.PHONY: agent-zip
agent-zip: agent-vendor ## Package the WordPress agent plugin as a zip (with ifsnop vendor/)
	mkdir -p release
	# Rebuild (not update) the zip — without this, `zip -r` appends to the
	# existing archive, leaving stale entries from prior plugin versions (e.g.
	# old phpbu/ vendor tree, deleted files). Removing the target file forces
	# a clean rebuild every run.
	rm -f release/wpmgr-agent.zip
	# Sweep dev-only files (tests, caches, macOS resource forks, nested
	# archives someone may have unzipped here for debugging) before packaging.
	cd apps/agent && rm -f Archive.zip .DS_Store .phpunit.result.cache && find . -name ".DS_Store" -delete
	cd apps/agent && zip -r ../../release/wpmgr-agent.zip . \
		-x 'tests/*' '*.dist' '.phpunit.cache/*' '.phpunit.result.cache' \
		   'composer.lock' '.DS_Store' '*/.DS_Store' '*.zip'
	@echo "agent zip: $$(du -sh release/wpmgr-agent.zip | cut -f1)"

.PHONY: gen
gen: ## Regenerate OpenAPI clients (Go + TS)
	./scripts/gen-openapi.sh
