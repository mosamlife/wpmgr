# WPMgr

Open-source, self-hostable WordPress management for agencies — a modern alternative to WP Remote, ManageWP, and MainWP. Manage many WordPress sites from one dashboard: bulk updates, incremental backups, uptime monitoring, security scanning, visual regression, and AI-powered safe-update recommendations.

> **Status:** pre-alpha. Bootstrapping (Phase 2 of the development plan). Not yet usable. See [PLAN.md](./PLAN.md).

## Why WPMgr

| Capability | WPMgr | WP Remote | ManageWP | MainWP |
| --- | --- | --- | --- | --- |
| Self-hostable | ✅ | ❌ | ❌ | ✅ |
| Open source | ✅ AGPL-3.0 | ❌ | ❌ | partial |
| Bulk updates + rollback | ✅ | ✅ | ✅ | ✅ |
| Incremental encrypted backups | ✅ | ✅ | ✅ | ✅ |
| Uptime monitoring | ✅ | ✅ | ✅ | add-on |
| Vulnerability scanning | ✅ | ✅ | ✅ | add-on |
| Visual regression on update | ✅ (Roadmap V1) | ❌ | ❌ | ❌ |
| AI update advisor | ✅ (Roadmap V1) | ❌ | ❌ | ❌ |
| Terraform provider / GitOps | ✅ (Roadmap V2) | ❌ | ❌ | ❌ |

## Architecture

Modular monolith in a single Go binary (control plane), a React SPA dashboard, and a PHP agent plugin installed on each managed site.

- **Backend:** Go 1.26 + Gin — `apps/api`
- **Frontend:** React 19 + TypeScript + Vite — `apps/web`
- **Agent:** PHP 8.0+ WordPress plugin — `apps/agent`
- **Data:** Postgres + Redis + S3-compatible object storage + ClickHouse (metrics)

See [docs/architecture.md](./docs/architecture.md).

## Quickstart (self-host)

```bash
cp .env.example .env
docker compose -f infra/docker-compose.yml up -d
curl localhost:8080/healthz   # {"status":"ok"}
```

Full instructions: [docs/install.md](./docs/install.md).

## Repository layout

```
apps/      api (Go) · web (React) · agent (PHP) · tracker (JS) · cli (Go, V1)
packages/  openapi · openapi-client · tsconfig · eslint-config · ui
infra/     docker-compose · Dockerfiles · helm (V1) · terraform-provider (V2)
docs/      install · agent · architecture · contributing · security · adr
```

## Development

```bash
make bootstrap   # first-time setup
make dev         # run the full stack
make test        # run all tests
make build       # build api + web
```

## License

- Control plane + dashboard: **AGPL-3.0-only** ([LICENSE](./LICENSE))
- Agent plugin + JS tracker: **MIT** ([LICENSE-AGENT](./LICENSE-AGENT))

## Docs

- [Install (self-host)](./docs/install.md)
- [Architecture](./docs/architecture.md)
- [API](./docs/api.md)
- [WordPress agent](./docs/agent.md)
- [Contributing](./docs/contributing.md)
- [Security](./docs/security.md)

## Links

- [Development plan](./PLAN.md)
- [Architecture decisions](./DECISIONS.md)
- [Contributing](./CONTRIBUTING.md)
- [Security policy](./SECURITY.md)
