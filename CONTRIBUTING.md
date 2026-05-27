# Contributing to WPMgr

Thanks for helping build WPMgr. This file covers dev setup and the PR checklist.

## Prerequisites

Pinned in `.tool-versions` (asdf-compatible):

- Go 1.26+
- Node 22+ and pnpm 11+
- PHP 8.5+ and Composer (for the agent)
- Docker + Docker Compose (for the full stack)

## Setup

```bash
make bootstrap     # installs JS deps, agent composer deps, sets up env
make dev           # bring up the full stack
```

## Project layout

Modular monolith. Backend domains live under `apps/api/internal/<domain>/`
with `handler/`, `service/`, `repo/`, `model/` subpackages. Frontend features
under `apps/web/src/features/<domain>/` mirror the backend domains.

## Commits

Conventional Commits (`feat:`, `fix:`, `chore:`, `docs:`, `refactor:`, `test:`).
One logical change per commit.

## PR checklist

- [ ] `make lint` passes
- [ ] `make test` passes
- [ ] `make build` passes
- [ ] OpenAPI (`packages/openapi/openapi.yaml`) updated if endpoints changed
- [ ] Docs updated if behavior changed
- [ ] Security-sensitive changes (auth, crypto, agent protocol, RBAC, tenant
      isolation) noted in the PR description for security review

## Architecture decisions

Material technical choices are recorded in [DECISIONS.md](./DECISIONS.md) as
ADRs. Propose new dependencies via an ADR before adding them.
