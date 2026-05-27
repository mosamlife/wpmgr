# Contributing (developer guide)

This is the hands-on dev guide. For the canonical PR rules and conventions, see
[CONTRIBUTING.md](../CONTRIBUTING.md) at the repo root — this page complements it
with the concrete toolchain and workflow.

## Toolchain

Versions are pinned in [`.tool-versions`](../.tool-versions) (asdf-compatible):

| Tool | Version |
|------|---------|
| Go | 1.26.3 |
| Node | 22.22.3 |
| pnpm | 11.1.1 |
| PHP | 8.5.6 |

> **macOS / Homebrew:** `node@22` is keg-only. Put it on `PATH` so `pnpm`
> resolves the right Node:
>
> ```bash
> export PATH="/opt/homebrew/opt/node@22/bin:$PATH"
> ```
>
> The `Makefile` already prepends this for `make` targets.

Also needed: Composer (agent PHP deps) and Docker + Compose (full stack).

## Setup

```bash
make bootstrap     # installs JS deps, agent composer deps, sets up env
make dev           # bring up the full stack (docker compose + dev overrides)
```

Component dev ports: API `:8080`, web SPA `:5173` (Vite), SeaweedFS S3 `:8333`.

## Everyday commands

```bash
make dev           # full stack for local development
make test          # all tests (Go + frontend)
make build         # build api + web
make lint          # go vet + pnpm lint
make gen           # regenerate OpenAPI clients (Go + TS) from packages/openapi/openapi.yaml
make agent-zip     # package the WordPress agent plugin
```

Per-stack:

```bash
cd apps/api && go test ./...      # Go tests
pnpm run test                     # frontend tests
cd apps/agent && composer test    # PHP tests (PHPUnit)
```

## Monorepo layout

Turborepo + pnpm workspace (JS) and a Go workspace (`go.work`).

```
apps/      api (Go + Gin) · web (React + Vite) · agent (PHP plugin)
           tracker (JS) · cli (Go, Roadmap)
packages/  openapi · openapi-client · tsconfig · eslint-config · ui
infra/     docker-compose · Dockerfiles · nginx · grafana · prometheus
docs/      this directory
```

Backend domains: `apps/api/internal/<domain>/` with `handler/`, `service/`,
`repo/`, `model/`. Frontend features `apps/web/src/features/<domain>/` mirror
the backend domains. The Go binary entrypoint is `apps/api/cmd/wpmgr/main.go`.

## Contract-first API

The OpenAPI spec at `packages/openapi/openapi.yaml` is the single source of
truth. Change the spec, then run `make gen` to regenerate the Go server types
(ogen, ADR-004) and the TS client. Don't hand-edit generated code. See
[api.md](./api.md).

## Commits & PRs

- **Conventional Commits:** `feat:`, `fix:`, `chore:`, `docs:`, `refactor:`,
  `test:`. One logical change per commit.
- **PR checklist** (full list in [CONTRIBUTING.md](../CONTRIBUTING.md)):
  `make lint`, `make test`, `make build` pass; OpenAPI updated if endpoints
  changed; docs updated if behavior changed; security-sensitive changes (auth,
  crypto, agent protocol, RBAC, tenant isolation) flagged for review.

## Architecture decisions

Propose new dependencies via an ADR in [DECISIONS.md](../DECISIONS.md) before
adding them. Background on the stack is in [architecture.md](./architecture.md).
