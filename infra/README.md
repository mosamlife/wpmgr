# infra — deployment & local stack

Built by the devops-engineer in Phase 4:

- `docker-compose.yml` — full self-host (Postgres, Redis, SeaweedFS, ClickHouse, API, web)
- `docker-compose.dev.yml` — dev overrides
- `Dockerfile.api`, `Dockerfile.web` — multi-stage builds
- `nginx/`, `grafana/`, `prometheus/` — supporting config
- `helm/` (V1), `terraform-provider/` (V2)
