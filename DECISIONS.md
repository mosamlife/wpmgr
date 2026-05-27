# WPMgr Architecture Decision Records

This log captures every material technical decision for WPMgr. Each entry follows the
template below. ADRs are assigned monotonic numbers and never reused; superseded ADRs
stay in place with a `Superseded` status and a pointer to the replacement.

## Template

```markdown
## ADR-NNN: <Title>
- **Status:** Proposed | Accepted | Superseded
- **Date:** YYYY-MM-DD
- **Context:** why this decision is needed
- **Options considered:** table with scores
- **Decision:** chosen option + reasoning
- **Consequences:** tradeoffs accepted
```

---

## Locked stack summary (Phase 3 outcome)

| Area | Choice | ADR |
|------|--------|-----|
| ORM / query layer | sqlc (+ pgx/v5) | ADR-001 |
| Migrations | Atlas Community Edition (Apache-2.0 only) | ADR-002 |
| Job queue | River (Postgres-native) | ADR-003 |
| OpenAPI codegen (Go) | ogen | ADR-004 |
| Validation (Go) | go-playground/validator v10 | ADR-005 |
| Logging (Go) | log/slog | ADR-006 |
| Config (Go) | koanf | ADR-007 |
| WebSocket (Go) | coder/websocket | ADR-008 |
| HTTP client (Go) | net/http + SSRF-hardened transport | ADR-009 |
| S3 client (Go) | aws-sdk-go-v2 | ADR-010 |
| Observability | OTel SDK + otelgin → Collector → Tempo + Prometheus + Grafana | ADR-011 |
| Frontend router | TanStack Router | ADR-012 |
| Data fetching | TanStack Query v5 | ADR-013 |
| Component lib | shadcn/ui + Radix + TanStack Table | ADR-014 |
| Forms | react-hook-form | ADR-015 |
| Validation (TS) | Zod 4 | ADR-016 |
| Client state | Zustand | ADR-017 |
| Charts | Tremor (on Recharts) | ADR-018 |
| i18n | Lingui v5 | ADR-019 |
| E2E | Playwright | ADR-020 |
| PHP testing | PHPUnit (+ Brain Monkey, PHPUnit Polyfills) | ADR-021 |
| PHP static analysis | PHPStan (+ WordPress stubs) | ADR-022 |

## Phase 3 risk register

Cross-cutting findings surfaced during research that need a decision or follow-up
before/within Phase 4:

1. **MinIO server is no longer maintained (HIGH).** The MinIO server community edition
   went maintenance-only in late 2025 and the repo was marked no-longer-maintained /
   archived ~2026-02-12 ([minio/minio#21714](https://github.com/minio/minio/issues/21714)).
   The *client* SDK `minio-go` is unaffected, and our client choice (aws-sdk-go-v2,
   ADR-010) is vendor-neutral. But the **locked stack names "MinIO for self-host"** —
   the self-host object-storage *server* should be re-evaluated: SeaweedFS (Apache-2.0,
   Go), Garage (AGPLv3 — license-aligned), or RustFS. All speak the S3 API, so the
   `blobstore` interface keeps us decoupled. **Needs user decision.**
2. **Atlas is open-core (MEDIUM).** We restrict ourselves to the Apache-2.0 Community
   Edition and must avoid Pro/EULA-gated features to stay fully OSS. Fallback: goose
   (plain portable SQL migrations). See ADR-002.
3. **ogen is not Gin-native (MEDIUM).** ogen generates its own router; Gin (locked)
   stays the outer HTTP layer (middleware/auth/static) while ogen owns the typed API
   route group. If integration friction is high, oapi-codegen (native Gin generator)
   is the documented fallback — same `openapi.yaml`. See ADR-004.
4. **openapi-react-query → maintenance mode (LOW).** Prefer Hey API's
   `@hey-api/openapi-ts` TanStack Query plugin for client generation; isolate behind one
   adapter. See ADR-013.
5. **WordPress PHPStan stubs bus-factor (LOW).** `php-stubs/wordpress-stubs` +
   `szepeviktor/phpstan-wordpress` depend on a single maintainer who signaled possible
   discontinuation without funding; mitigate via sponsorship or be ready to vendor/fork.
   See ADR-022.
6. **shadcn/ui WCAG 2.2 AA gaps (LOW).** ~34/48 components pass out of the box; budget a
   remediation audit (the Data Table example needs a `<caption>`, etc.). See ADR-014.

---

## ADR-001: Go ORM / Query Layer — sqlc

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr is a Go 1.26 + Gin modular monolith on Postgres, AGPLv3, intended to be self-hostable and to age well across many years and contributors. We need a data-access layer that is type-safe, predictable in production (no query surprises), license-clean for redistribution, and that composes cleanly with our migration tool and OpenAPI codegen. We prioritize boring/proven, thin and swappable, and good DX. Candidates: [sqlc](https://github.com/sqlc-dev/sqlc) (compile SQL → type-safe Go), [Bun](https://github.com/uptrace/bun) (SQL-first query builder/ORM), [Ent](https://github.com/ent/ent) (graph/schema-as-code ORM with codegen), [GORM](https://github.com/go-gorm/gorm) (full reflective ORM).
- **Options considered:**

| Tool | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|------|:-:|:-:|:-:|:-:|:-:|-------|
| **sqlc** | 5 | 5 | 4 | 5 | 5 | v1.31.1 Apr 2026, 17.8k★, MIT. Generates `database/sql`/pgx code from raw SQL — zero runtime reflection, native pgx perf. |
| Bun | 4 | 4 | 4 | 4 | 5 | Latest Feb 2026, ~4.8k★, BSD-2-Clause. SQL-first query builder, lighter than a full ORM but uses runtime reflection. |
| Ent | 4 | 4 | 3 | 4 | 5 | v0.14.x, 17.1k★, Apache-2.0, maintained by the Atlas team. Powerful but opinionated schema-as-code; heavier mental model + generated graph API. |
| GORM | 5 | 2 | 3 | 5 | 5 | v1.x (Nov 2025), ~39k★, MIT. Most popular but reflection-heavy, runtime query surprises, weaker type safety. |

  Sources: [sqlc releases](https://github.com/sqlc-dev/sqlc/releases), [sqlc repo/license](https://github.com/sqlc-dev/sqlc), [Bun repo/license](https://github.com/uptrace/bun), [Bun changelog](https://github.com/uptrace/bun/blob/master/CHANGELOG.md), [Ent repo/license](https://github.com/ent/ent), [Ent on entgo.io](https://entgo.io/), [GORM releases](https://github.com/go-gorm/gorm/releases), [GORM stars/ossinsight](https://ossinsight.io/collections/golang-orm), [Go ORM comparison 2026 (Encore)](https://encore.cloud/resources/go-orms).

- **Decision:** **sqlc**, because it gives compile-time type safety over hand-written, reviewable SQL with no runtime reflection or query-builder magic — the "boring, predictable" property we want for a long-lived self-hosted product on Postgres. It pairs with `pgx` for top-tier performance, and crucially it composes with our chosen migration tool: sqlc reads the same SQL schema files Atlas manages, so schema is a single source of truth (see ADR-002). Tie-break vs. Bun went to sqlc on ecosystem fit (raw SQL is maximally swappable; the generated layer is a thin interface you can replace) and DX for a SQL-centric team. We avoid GORM (reflection/perf/predictability) and Ent (heavier graph abstraction than a Gin monolith needs).
- **Consequences:** All queries live in `.sql` files reviewed like code; developers must know SQL (acceptable, arguably a feature). No dynamic query building from sqlc itself — for the rare dynamic-filter endpoint we add a thin hand-written `pgx` query or a small builder (e.g. `squirrel`) behind the same repository interface, keeping the layer swappable. We standardize on the `pgx/v5` driver. Schema `.sql` files are shared with Atlas, enforcing one source of truth.

## ADR-002: Migration Tool — Atlas (Community Edition)

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** We need schema migrations for Postgres that (a) keep a single source-of-truth schema that sqlc can also consume, (b) support versioned migrations for safe self-hosted upgrades, (c) are license-clean for AGPLv3 redistribution, and (d) have a CLI usable in CI and in the self-host upgrade path. Candidates: [goose](https://github.com/pressly/goose), [golang-migrate](https://github.com/golang-migrate/migrate), [Atlas](https://github.com/ariga/atlas).
- **Options considered:**

| Tool | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|------|:-:|:-:|:-:|:-:|:-:|-------|
| **Atlas (CE)** | 5 | 5 | 5 | 5 | 4 | Releases through 2026, very active, broad DB support. Declarative + versioned, diffs schema automatically, **first-class sqlc integration**. Open-core: CE binary is Apache-2.0; some advanced features are EULA/Pro-only. |
| goose | 5 | 5 | 4 | 4 | 5 | v3, latest Apr 2026, ~10.4k★. Simple SQL/Go migrations, library + CLI, embeddable. No declarative diffing; you write migrations by hand. |
| golang-migrate | 3 | 5 | 3 | 4 | 5 | v4.19.1 Nov 2025. Stable and widely used but lower release cadence; up/down SQL only, no schema diffing; recent Docker-image CVE backlog. |

  Sources: [Atlas repo](https://github.com/ariga/atlas), [Atlas Community Edition (Apache-2.0)](https://atlasgo.io/community-edition), [Atlas + sqlc versioned guide](https://atlasgo.io/guides/frameworks/sqlc-versioned), [Atlas + sqlc declarative guide](https://atlasgo.io/guides/frameworks/sqlc-declarative), [goose repo](https://github.com/pressly/goose), [goose releases](https://github.com/pressly/goose/releases), [golang-migrate releases](https://github.com/golang-migrate/migrate/releases), [golang-migrate CVE issue #1357](https://github.com/golang-migrate/migrate/issues/1357).

- **Decision:** **Atlas Community Edition**, because it is the only candidate with first-class sqlc integration: the same `schema.sql` is the desired state for both tools, and `atlas migrate diff` auto-generates versioned migrations from schema changes, eliminating the hand-written-migration drift that plagues goose/golang-migrate. This directly realizes the "single source of truth" decided in ADR-001 and wins the tie on DX + ecosystem fit. The CE binary we ship/use is Apache-2.0 — fully compatible with AGPLv3 redistribution.
- **Consequences:** We use **only the Apache-2.0 Community Edition** and the open Apache-2.0 versioned-migration workflow; we must avoid depending on Atlas Pro/EULA-gated features so the self-hosted product stays fully OSS (this is the reason for the 4/5 license score — flagged in the risk register). Migrations are committed as SQL under `migrations/` and applied via `atlas migrate apply` in CI and on self-host upgrades; a `dev-url` Postgres (Docker) is required at authoring time for diffing. Fallback: if Atlas's open features ever regress behind the paywall, goose is the drop-in plan B — migrations are plain SQL and portable, keeping this layer swappable.

## ADR-003: Job Queue — River

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr runs backups, updates, and scans as background jobs. These are durable, retryable, sometimes long-running tasks. Self-hosters want minimal infra; the stack already ships Postgres and Redis. Candidates: River (Postgres-native), Asynq (Redis-native), Temporal (durable workflow engine), and raw Postgres LISTEN/NOTIFY. Raw LISTEN/NOTIFY is not durable on its own — notifications live in an in-memory queue and are lost if a listener is disconnected; there is no replay, no dead-letter, no consumer groups, and an ~8KB payload cap ([postgresql.org NOTIFY docs](https://www.postgresql.org/docs/current/sql-notify.html), [thinhdanggroup.github.io](https://thinhdanggroup.github.io/postgres-as-a-message-bus/)). It can be a wake-up "doorbell" over a durable table but is not a queue by itself. Temporal is a different class: self-hosting requires Postgres/MySQL plus Cassandra or Elasticsearch and multiple services, with high operational cost — overkill for cron-like task fan-out ([docs.temporal.io self-hosted guide](https://docs.temporal.io/self-hosted-guide)). That leaves River vs Asynq as realistic fits.
- **Options considered:**

| Library | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **River** (Postgres) | 5 — v0.31.0 Feb 2026, ~5.2k★ ([releases](https://github.com/riverqueue/river/releases)) | 4 — Postgres-backed; ample for backups/scans | 5 — transactional enqueue, typed jobs, web UI ([riverqueue.com](https://riverqueue.com/)) | 5 — uses existing Postgres + pgx; zero new infra | 5 — MPL-2.0, AGPLv3-compatible | Postgres-only |
| Asynq (Redis) | 3 — v0.26.0 Feb 2026, 13.3k★, pre-1.0 ([repo](https://github.com/hibiken/asynq)) | 5 — Redis-backed, very fast | 4 — clean API, asynqmon UI | 4 — Redis present, but no transactional enqueue with Postgres data | 5 — MIT | Redis required |
| Temporal | 5 ([sdk-go](https://github.com/temporalio/sdk-go)) | 4 | 2 — workflow paradigm, steep | 1 — needs Cassandra/ES + multi-service cluster | 4 | Heavy infra |
| Postgres LISTEN/NOTIFY (raw) | 5 — core Postgres | 3 | 2 — hand-build retries/DLQ/visibility | 3 — no new infra but not a real queue | 5 | Not durable alone |

- **Decision:** **River**, because it gives a durable, retryable, production queue with a web UI while adding zero infrastructure beyond the Postgres + pgx layer already locked in. Transactional enqueue means a "schedule backup" job is guaranteed consistent with the row that triggered it — a real correctness win for a management SaaS. Its Postgres-only model is exactly what minimal-footprint self-hosters want. Asynq is faster and more starred but is pre-1.0, requires Redis as a durable store, and can't transactionally couple jobs to Postgres state. Temporal and raw LISTEN/NOTIFY are eliminated on infra weight and lack of durability respectively.
- **Consequences:** Queue load lands on Postgres — size connection pool and monitor table bloat/autovacuum on the jobs table; River's retention settings mitigate this. Throughput ceiling is Postgres, acceptable for backup/update/scan cadence. Redis stays available for caching/rate-limiting, not the queue. MPL-2.0 is file-level copyleft and compatible with the AGPLv3 project. Keep the enqueue/worker surface behind a thin internal interface so Asynq remains a swap-in if Redis-scale throughput is ever needed.

## ADR-004: OpenAPI Codegen (Go) — ogen

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr's frontend is React 19 + Vite and the PHP 8 agent also talks to the API, so a single OpenAPI 3 contract is the integration backbone. We want spec-first codegen producing type-safe Go server stubs (and ideally a typed client), strong validation, good performance, and a permissive license. We must reconcile this with Gin as the locked HTTP layer. Candidates: [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen) (spec → Go, Gin/echo/chi/stdlib stubs), [ogen](https://github.com/ogen-go/ogen) (spec → full typed server+client+validation, own router), [huma](https://github.com/danielgtaylor/huma) (code-first framework that emits OpenAPI 3.1).
- **Options considered:**

| Tool | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **ogen** | 5 | 5 | 4 | 4 | 5 | v1.20.x, latest Apr 2026, ~2.1k★, Apache-2.0. Spec-first; generated JSON (jx) + validation + zero-alloc radix router. Routing benchmarks far ahead of chi/echo. |
| oapi-codegen | 4 | 4 | 5 | 5 | 5 | v2.6.0 May 2026, MIT. **Native Gin server generator**, mature. Maintainers note reduced bandwidth; relies on a runtime validator. |
| huma | 4 | 4 | 4 | 4 | 5 | v2.38.0 May 2026, ~4.1k★, MIT. Code-first (reflection) emitting OpenAPI 3.1 — inverts our spec-first requirement and brings its own framework. |

  Sources: [ogen repo/license](https://github.com/ogen-go/ogen), [ogen intro + routing benchmarks](https://ogen.dev/blog/ogen-intro/), [ogen releases](https://github.com/ogen-go/ogen/releases), [oapi-codegen repo](https://github.com/oapi-codegen/oapi-codegen), [oapi-codegen v2.6.0 release](https://github.com/oapi-codegen/oapi-codegen/releases/tag/v2.6.0), [oapi-codegen maintainer note (jvt.me)](https://www.jvt.me/posts/2026/02/17/oapi-codegen-github-secure/), [huma repo/license](https://github.com/danielgtaylor/huma), [huma releases](https://github.com/danielgtaylor/huma/releases).

- **Decision:** **ogen**, because it is spec-first (matching our contract-first need to serve React + the PHP agent from one source), generates the full stack — typed models, request/response validation, and a typed client — with code-generated (not reflective) marshaling and validation for the best performance and predictability, and ships under clean Apache-2.0. huma is eliminated for being code-first/reflection-based. The real tie-break was ogen vs. oapi-codegen: oapi-codegen scores higher on DX + ecosystem fit because of its native Gin generator, but its maintainers have publicly flagged reduced bandwidth, and its runtime-validator model is less robust than ogen's generated validation. We choose ogen for its long-term performance/correctness profile and active maintenance.
- **Consequences:** ogen brings its own generated HTTP server/router rather than emitting Gin handlers. We isolate the ogen-generated API surface in its own package and mount it on a route group; Gin remains the app's outer HTTP layer (middleware, auth, static serving) while ogen owns the typed API endpoints. If that integration friction proves costly, oapi-codegen with its Gin server generator is the documented fallback — both consume the same `openapi.yaml`, so the spec stays the swappable interface. The OpenAPI spec is the single contract checked into the repo and used to regenerate Go stubs and the TypeScript client.

## ADR-005: Validation (Go) — go-playground/validator

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr needs request/DTO validation for the Gin API and domain-object validation in the service layer. Candidates: go-playground/validator (struct-tag, integrates with Gin's binding), ozzo-validation (programmatic rules), and Cue (external schema language).
- **Options considered:**

| Library | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **go-playground/validator** | 5 — v10.30.1, Mar 2026, ~19.8k★ ([releases](https://github.com/go-playground/validator/releases)) | 4 — reflection/tag-based | 4 — declarative tags; cryptic for complex rules | 5 — **Gin's default binding validator** | 5 — MIT | |
| ozzo-validation | 2 — original unmaintained 2+ yrs; fork at invopop/validation ([issue #181](https://github.com/go-ozzo/ozzo-validation/issues/181)) | 4 — programmatic | 5 — code-defined rules, great for conditional logic | 3 — not wired into Gin binding | 5 — MIT | Prefer invopop fork |
| Cue | 5 — actively maintained ([repo](https://github.com/cue-lang/cue)) | 3 — external evaluation, heavier | 2 — separate language; overkill for HTTP DTOs | 2 — best for config/policy, not per-request structs | 4 — Apache-2.0 | Wrong tool here |

- **Decision:** **go-playground/validator (v10)**, because it is the validator Gin uses for request binding out of the box, so it adds nothing to the dependency surface and keeps request validation idiomatic and declarative. It is well-maintained and the de-facto standard. ozzo's programmatic style is nicer for complex conditional rules, but the canonical repo is effectively unmaintained and not Gin-integrated. Cue is a schema/config language — the right tool for validating config/policy, not HTTP DTOs.
- **Consequences:** Tag-based rules become awkward for cross-field conditional logic; for those few cases, write plain Go validation methods in the service layer (or selectively adopt invopop/validation) rather than overloading struct tags. Register custom validators (WordPress site URL, version constraints) via the custom-func API. Keep a small `Validate(any) error` wrapper so the service layer doesn't depend directly on the tag engine.

## ADR-006: Logging (Go) — log/slog

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr needs structured, leveled logging across the Gin API and background workers, ideally JSON in production. Candidates: stdlib log/slog, zerolog, zap. The Go ecosystem has largely aligned behind slog as the standard logging interface ([dash0 2026](https://www.dash0.com/guides/golang-logging-libraries), [betterstack](https://betterstack.com/community/guides/logging/best-golang-logging-libraries/)).
- **Options considered:**

| Library | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **log/slog** (stdlib) | 5 — Go standard library | 4 — ~40 B/op; efficient allocs | 5 — no dep, standard API, swappable Handler | 5 — the interface everything now targets | 5 — BSD (Go) | Default for new projects |
| zerolog | 5 — Apr 2026, ~12.3k★, MIT | 5 — fastest, ~25 ns, ~0 alloc | 4 — chainable, JSON-first | 4 — own API, not slog-native | 5 — MIT | Fastest |
| zap | 5 — active, zapslog handler | 4 — ~51 ns, 168 B/op | 3 — sugared/typed split, more ceremony | 4 — widely used | 4 — MIT/BSD | Most knobs |

  Sources: [betterstack benchmarks](https://betterstack-community.github.io/go-logging-benchmarks/), [zerolog releases](https://github.com/rs/zerolog/releases), [zapslog](https://pkg.go.dev/go.uber.org/zap/exp/zapslog).

- **Decision:** **log/slog**, because the ecosystem has standardized on its `Handler` interface and it ships in the standard library — no dependency, no version risk, portable code. Performance (~40 B/op) is more than adequate for an API + worker SaaS where logging is not the hot path. slog's pluggable backend means we can drop in a zerolog or zap handler later if a bottleneck appears, without touching call sites. zerolog wins raw benchmarks but locks call sites into a non-standard API; zap is the heaviest on allocations and most verbose.
- **Consequences:** Standardize on slog's `Logger`/`Handler` everywhere; `slog.JSONHandler` in production, text handler in dev. If profiling later shows logging cost matters, swap in a zerolog-backed `slog.Handler` (e.g. samber/slog-zerolog) with no call-site changes. Establish structured key conventions (request_id, tenant_id, site_id, job_id) early so logs are queryable.

## ADR-007: Config (Go) — koanf

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** Self-hosted WPMgr must read config from env vars (12-factor / container deploys) and likely a config file (YAML/TOML) for self-hosters. Candidates: koanf, Viper, envconfig.
- **Options considered:**

| Library | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **koanf** | 5 — v2.3.4 Mar 2026, ~4k★, MIT | 5 — lightweight, modular providers | 4 — explicit providers/parsers | 5 — env + file + flags, clean merge | 5 — MIT | Lean Viper alternative |
| Viper | 4 — heavy; ~313% bigger binary, lowercases keys | 3 — large dep tree | 4 — batteries-included magic | 4 — broad formats, limited multi-file merge | 5 — MIT | Heavyweight |
| envconfig | 3 — stable/simple, env-only | 5 — trivial | 4 — struct tags, defaults | 2 — **no file support** | 5 — MIT | Env-only |

  Sources: [koanf repo](https://github.com/knadh/koanf), [koanf vs viper wiki](https://github.com/knadh/koanf/wiki/Comparison-with-spf13-viper), [envconfig repo](https://github.com/kelseyhightower/envconfig).

- **Decision:** **koanf**, because self-hosters need both env vars and a config file with predictable merge semantics, and koanf does this with a small dependency footprint and modular providers. It avoids Viper's known issues (large binary, key lowercasing that breaks YAML/TOML spec) while covering the same env+file+flag sources — exactly the layered "file then env override" pattern a self-hostable product needs. envconfig is env-only; Viper is heavier with spec-correctness footguns.
- **Consequences:** Slightly more explicit setup than Viper's autoload. Define a typed `Config` struct and `Unmarshal` once at startup with clear precedence (defaults < file < env). Install only the `env`, `file`, and a YAML/TOML parser provider. Keep loading behind a single `LoadConfig()` so the source library is swappable.

## ADR-008: WebSocket Library (Go) — coder/websocket

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr needs WebSocket transport for terminal/log streaming and a bidirectional agent command hub. Requirements: idiomatic `context.Context` cancellation/timeouts, safe concurrent writes (multiplexing log + control frames), `net.Conn` wrapping for piping PTY/log byte streams, and a healthy maintenance pulse. Candidates: [coder/websocket](https://github.com/coder/websocket) (maintained successor to nhooyr.io/websocket), [gorilla/websocket](https://github.com/gorilla/websocket), [gobwas/ws](https://github.com/gobwas/ws).
- **Options considered:**

| Library | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **coder/websocket** | 5 — v1.8.x (Sep 2025), ~5.2k★, ISC | 4 — zero-alloc r/w, 1.75x faster masking than gorilla | 5 — context-native, safe concurrent writes, net.Conn wrapper | 5 | 5 — ISC | Modern + maintained |
| gorilla/websocket | 2 — stuck at v1.5.0, seeking maintainers | 4 — comparable throughput | 3 — no context; manual deadlines + writer mutex | 4 | 5 — BSD-2 | Maintenance limbo |
| gobwas/ws | 3 — MIT, low-level epoll API | 5 — fastest point-to-point | 2 — verbose, assemble framing yourself | 3 | 5 — MIT | Overkill for our scale |

  Sources: [coder/websocket repo](https://github.com/coder/websocket), [pkg.go.dev](https://pkg.go.dev/github.com/coder/websocket), [gorilla maintainer issue](https://github.com/argoproj/argo-workflows/issues/7403).

- **Decision:** **coder/websocket**, because its `context.Context`-native API maps directly onto our per-session lifecycle (cancel a terminal/log stream when the JWT expires or the tab closes), concurrent-write safety lets us multiplex log + control frames without hand-rolling a writer mutex, and the `net.Conn` wrapper makes PTY/log byte-piping trivial. It is the only candidate both modern and actively maintained; performance is within noise of gorilla at our scale. gorilla is in maintenance-seeking limbo; gobwas trades DX for throughput we don't need.
- **Consequences:** Adopt ISC-licensed `github.com/coder/websocket` (AGPLv3-compatible). Standardize on context-scoped read/write helpers and a single read-loop-per-conn pattern. If we ever need raw epoll fan-out (>100k idle agent connections), revisit gobwas behind our thin transport interface — keep the dependency isolated so a swap stays local.

## ADR-009: HTTP Client (Go) — net/http + SSRF-hardened transport

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr makes outbound HTTP calls to (a) self-hosted agents, (b) third-party threat-intel APIs (Wordfence, Patchstack), and (c) user-configured webhooks. Webhook and agent URLs are partly user-controlled, so **SSRF defense is the dominant requirement**: resolve-then-pin destination IPs and block private/link-local/loopback ranges at dial time to defeat DNS-rebinding (TOCTTOU) ([agwa](https://www.agwa.name/blog/post/preventing_server_side_request_forgery_in_golang)). Other needs: retries/backoff for flaky agents, timeouts, tracing. Candidates: `net/http` (stdlib), [go-resty/resty](https://github.com/go-resty/resty), [imroc/req](https://github.com/imroc/req).
- **Options considered:**

| Library | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **net/http** | 5 — stdlib, BSD | 5 | 3 — no built-in retries | 5 — full control of `Transport.DialContext` (only place SSRF pinning is correct) | 5 | Pairs with MIT [`code.dny.dev/ssrf`](https://github.com/daenney/ssrf) |
| resty | 4 — ~11.6k★, MIT, v2.17.x | 4 | 5 — chainable, retries, SSE | 4 — wraps http.Client; convenience tempts SSRF bypass | 5 — MIT | v3 in beta |
| imroc/req | 4 — ~4.7k★, MIT, very active | 4 | 4 — "black magic" auto-detection | 3 — auto behaviors unwanted on SSRF paths | 5 — MIT | Smaller footprint |

- **Decision:** **net/http with a custom SSRF-hardened `http.Transport`**, because the security requirement is non-negotiable and only owning the `DialContext`/`net.Dialer.Control` path lets us pin the resolved IP and reject private ranges atomically before connect. We wrap stdlib in a thin internal `httpclient` package exposing one safe client (webhooks/agents) and one plain client (fixed-host vendor APIs), layering `otelhttp` for tracing and `cenkalti/backoff` for retries. This centralizes the SSRF guarantee rather than relying on developers remembering to route a wrapper library through a safe transport.
- **Consequences:** Slightly more boilerplate than resty (retry/SSE helpers written once). Use `code.dny.dev/ssrf` (MIT) for IANA-synced deny ranges via `net.Dialer.Control`, plus an allow-list for known vendor hosts. All outbound calls go through the internal package — enforce with a lint/import rule. If DX friction grows, resty can be adopted *inside* the wrapper later (it accepts a custom transport) without changing call sites.

## ADR-010: S3 Client (Go) — aws-sdk-go-v2

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr stores backups/artifacts in S3-compatible storage that must work against both **AWS S3** (managed tier) and a **self-hosted S3-compatible endpoint** (custom endpoint URL + path-style + V4 signing). Candidates: [aws/aws-sdk-go-v2](https://github.com/aws/aws-sdk-go-v2) and [minio/minio-go v7](https://github.com/minio/minio-go). **Critical context:** the MinIO *server* community edition went maintenance-only in late 2025 and was marked no-longer-maintained ~2026-02-12 ([minio/minio#21714](https://github.com/minio/minio/issues/21714)). The *client* SDK `minio-go` remains actively maintained (v7.2.0, May 2026).
- **Options considered:**

| Library | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **aws-sdk-go-v2** | 5 — Apache-2.0, continuous releases | 4 | 3 — verbose config, modular | 5 — vendor-neutral; `BaseEndpoint` + `UsePathStyle` targets any S3-compatible server | 5 — Apache-2.0 | First-class presign/multipart/OTel |
| minio-go v7 | 4 — Apache-2.0, active (v7.2.0 May 2026) | 5 | 5 — leaner, ergonomic | 4 — works against AWS + any endpoint, but governed by the vendor whose server is now unmaintained | 5 — Apache-2.0 | Governance risk |

- **Decision:** **aws-sdk-go-v2**, because it is the most strategically durable choice: vendor-neutral, Apache-2.0, AWS-funded continuous maintenance, and fully capable of targeting a self-hosted endpoint via `BaseEndpoint` + path-style addressing — so the same code path serves AWS S3 and the self-host backend. Given MinIO's server abandonment, tying our client to a MinIO-governed SDK adds avoidable correlated risk, even though `minio-go`'s DX is nicer. We isolate all S3 calls behind a thin internal `blobstore` interface (Put/Get/Delete/Presign/List).
- **Consequences:** Accept heavier configuration ergonomics; encapsulate endpoint/region/path-style/credentials setup once in `blobstore`. **The locked stack names "MinIO for self-host," but MinIO server is no longer maintained — see risk register item 1**; candidates: SeaweedFS (Apache-2.0, Go), Garage (AGPLv3), RustFS. All speak the S3 API, so aws-sdk-go-v2 + the `blobstore` interface keeps us decoupled from that server decision.

## ADR-011: Observability Stack — OTel SDK + otelgin → Collector → Tempo + Prometheus + Grafana

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr (Go 1.26 + Gin) needs traces and metrics that are self-hostable and AWS-friendly. We want one instrumentation API, an out-of-process collection layer so the app stays vendor-agnostic, and OSS backends. We must pick a concrete SDK, Gin middleware, collector, and trace+metric backends. (ClickHouse for product metrics is a separate analytics path, not the ops-observability backend chosen here.)
- **Options considered:**

| Component | Choice | Maint | Perf | DX | Fit | License | Notes |
|---|---|---|---|---|---|---|---|
| SDK | [go.opentelemetry.io/otel](https://opentelemetry.io/docs/languages/go/getting-started/) | 5 | 5 | 4 | 5 | 5 | Official; traces+metrics stable, Apache-2.0 |
| Gin middleware | [otelgin](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin) | 5 | 5 | 5 | 5 | 5 | Official contrib; auto spans + `http.server.*` metrics |
| Collector | [OTel Collector](https://grafana.com/docs/opentelemetry/collector/opentelemetry-collector/) | 5 | 5 | 4 | 5 | 5 | OTLP in, fan-out to any exporter, Apache-2.0 |
| Traces | [Grafana Tempo](https://grafana.com/blog/an-opentelemetry-backend-in-a-docker-image-introducing-grafana-otel-lgtm/) | 5 | 5 | 4 | 5 | 5 | Object-storage-backed (reuses S3), no ES/Cassandra |
| Metrics | Prometheus (+ Grafana viz) | 5 | 5 | 4 | 5 | 5 | De-facto standard |

- **Decision:** **OTel Go SDK + otelgin → OTLP → OTel Collector → Tempo (traces) + Prometheus (metrics), visualized in Grafana** — the Grafana LGTM-family stack minus Loki/Mimir for v1. A single vendor-neutral instrumentation API in the app; an out-of-process Collector so we can re-target backends without code changes; Tempo because it stores traces in our existing S3-compatible storage (no extra indexing DB) and auto-derives RED metrics; Prometheus + Grafana as the boring proven metrics path. The `grafana/otel-lgtm` image makes the self-host story a single container for evaluators. We prefer Tempo over Jaeger to avoid a separate trace-index datastore.
- **Consequences:** Instrument Gin with `otelgin`, wrap the SSRF `http.Client` (ADR-009) with `otelhttp`, export OTLP/gRPC to the Collector. Self-host docs ship a compose profile with `otel-lgtm`; managed tier points the same Collector at hosted backends with zero app changes. Add Loki/Mimir later if needed — the Collector makes that additive. Keep exporter config in env/Collector, never in app code.

## ADR-012: Frontend Router — TanStack Router

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr is a React 19 + TypeScript (strict) Vite SPA — explicitly not Next.js, no SSR. We want file-based routing, end-to-end type-safe params/loaders, and tight integration with the data layer (ADR-013). React Router 7 only delivers file-based routing in its "framework mode" (a Vite-plugin full-stack framework), and its `ssr:false`/SPA path still has known rough edges ([discussion 12360](https://github.com/remix-run/react-router/discussions/12360)). TanStack Router is client-first and supports file-based routing in a plain Vite SPA via `@tanstack/router-plugin/vite` with no SSR dependency ([docs](https://tanstack.com/router/latest/docs/installation/with-vite)).
- **Options considered:**

| Option | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **TanStack Router** | 5 — MIT, ~14.5k★, released 2026-05-26 | 5 — client-first, autoCodeSplitting | 5 — fully type-safe routes/params/loaders, file-based via plugin, no SSR | 5 — same vendor as TanStack Query (ADR-013) | 5 — MIT | Built for this SPA shape |
| React Router 7 | 5 — MIT, ~56.4k★, v7.15.1 | 4 — framework mode adds layers | 3 — file-based only in framework mode; SPA `ssr:false` has known bugs | 4 — huge ecosystem, but loaders compete with ADR-013 | 5 — MIT | Better for SSR, which we don't want |

- **Decision:** **TanStack Router**, because it delivers file-based, fully type-safe routing in a pure Vite SPA without a full-stack framework or SSR caveats, and composes natively with TanStack Query (ADR-013) from the same maintainers.
- **Consequences:** Smaller community than React Router (fewer SO answers); team learns TanStack's typed-route idioms and treats `routeTree.gen.ts` as generated (lint/format-ignored). Route loaders integrate cleanly with the Query cache. We deliberately forgo React Router's SSR option, consistent with the locked SPA constraint.

## ADR-013: Data Fetching — TanStack Query v5

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr needs server-state caching for a dashboard backed by an OpenAPI-generated client, with mutations, background refetch, pagination, and SSE-driven cache invalidation. Maintenance signal: `openapi-fetch`/`openapi-react-query` are moving to maintenance-mode, while Hey API's `@hey-api/openapi-ts` (used by Vercel/PayPal) is the actively-developed codegen and can emit TanStack Query hooks ([discussion 2559](https://github.com/openapi-ts/openapi-typescript/discussions/2559)).
- **Options considered:**

| Option | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **TanStack Query v5** | 5 — MIT, ~49.5k★, released 2026-05-23 | 4 — ~13KB; tracked queries re-render only on accessed fields | 5 — DevTools, mutations, optimistic updates | 5 — pairs with OpenAPI codegen; same vendor as Router | 5 — MIT | Default for REST + heavy cache/mutation |
| SWR | 4 — MIT, by Vercel | 5 — ~4KB | 3 — minimal mutation/invalidation primitives | 3 — fewer first-class OpenAPI+hooks paths | 5 — MIT | Under-powered for our dashboard |

- **Decision:** **TanStack Query v5**, because the dashboard's mutation management, background refetch, pagination, and explicit SSE-triggered cache invalidation map directly onto Query's lifecycle and `queryClient.invalidateQueries`, which SWR only partially covers. It also pairs with the same vendor's Router (ADR-012).
- **Consequences:** ~13KB vs SWR's ~4KB — acceptable for a dashboard. Keep a thin swappable seam: generate types/hooks and wrap them behind our own `api/` module. Given `openapi-react-query` is entering maintenance mode, prefer Hey API's `@hey-api/openapi-ts` TanStack Query plugin for generation (fallback: `openapi-typescript` + `openapi-react-query`), isolating whichever we pick behind one adapter.

## ADR-014: Component Library — shadcn/ui + Radix + TanStack Table

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** We need Tailwind-compatible components with built-in dark mode, WCAG 2.2 AA, and strong support for heavy data tables in a Vite SPA. Candidates: shadcn/ui (copy-in components over Radix + Tailwind), Park UI (Ark UI + Panda CSS), Mantine (own CSS engine), Radix-only (unstyled primitives). Tailwind is a hard requirement: Park UI uses Panda CSS and Mantine uses CSS-modules theming — both poor fits. shadcn/ui is Tailwind-native with built-in dark mode; heavy tables are met by pairing it with headless TanStack Table.
- **Options considered:**

| Option | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **shadcn/ui (+ Radix + TanStack Table)** | 5 — MIT; Radix MIT (WorkOS) | 5 — copy-in, no runtime lock-in | 5 — Tailwind-native, code-owned, dark mode | 5 — composes with Router/Query/Table | 5 — MIT, code in our repo | 34/48 components pass WCAG 2.2 AA OOTB |
| Mantine | 5 — MIT | 4 — ~130–160KB | 4 — 100+ components, batteries-included table | 2 — own CSS engine, not Tailwind | 5 — MIT | Violates Tailwind requirement |
| Park UI (Ark UI + Panda) | 4 — Ark UI active | 4 — headless | 3 — requires Panda CSS | 2 — conflicts with Tailwind | 5 — MIT | Wrong styling engine |
| Radix-only | 4 — MIT, velocity slowed | 5 — unstyled | 2 — build/style everything | 4 — Tailwind-compatible | 5 — MIT | Too much hand-rolling |

  Sources: [shadcn radix changelog](https://ui.shadcn.com/docs/changelog/2026-02-radix-ui), [shadcn data-table](https://ui.shadcn.com/docs/components/base/data-table), [TanStack Table](https://tanstack.com/table/latest), [shadcn a11y audit](https://thefrontkit.com/blogs/shadcn-ui-accessibility-audit-2026).

- **Decision:** **shadcn/ui (Radix primitives + Tailwind) with headless TanStack Table**, because it is the only candidate Tailwind-native with built-in dark mode while inheriting Radix's WAI-ARIA accessibility, and TanStack Table gives full control over heavy sortable/filterable/paginated tables that compose with our Query layer. Mantine and Park UI lose on the Tailwind requirement; Radix-only is too much hand-rolling.
- **Consequences:** Run a WCAG 2.2 AA audit and remediate components with known gaps (34/48 pass OOTB; the Data Table example needs a `<caption>`). Data tables require wiring TanStack Table ourselves — a one-time cost we accept for Tailwind ownership. Radix velocity has slowed for complex combobox/multi-select; budget extra time there. Components live in-repo (MIT) — full ownership, no runtime lock.

## ADR-015: Forms — react-hook-form

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr forms are mostly standard config/CRUD inputs in a Tailwind SPA, validated against schemas derived from OpenAPI types, integrated with TanStack Query mutations and shadcn/ui inputs. Both candidates are stable, MIT, React 19-ready. react-hook-form is ~12KB, uncontrolled/ref-based; TanStack Form v1 is ~20KB, signal-based, excels at deeply nested dynamic forms.
- **Options considered:**

| Option | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **react-hook-form** | 5 — MIT, ~45k★, v7.76.x | 5 — ~12KB, uncontrolled, scales to hundreds of inputs | 5 — battle-tested, shadcn `<Form>` built on it, Zod resolver | 5 — shadcn wraps RHF natively | 5 — MIT | Default for standard forms |
| TanStack Form v1 | 5 — MIT, v1 stable | 4 — ~20KB, signal-based | 4 — strong types, standard-schema | 3 — newer; shadcn integration not first-party | 5 — MIT | Wins for deeply nested dynamic forms |

- **Decision:** **react-hook-form**, because our forms are predominantly standard CRUD/config, RHF is smaller and battle-tested, and shadcn/ui's `<Form>` (ADR-014) is built directly on RHF — zero-friction integration with our UI library and Zod schema validation.
- **Consequences:** If we later hit genuinely complex nested/dynamic forms, TanStack Form v1 is the documented escape hatch; keeping form logic behind small wrapper components and a shared resolver makes that swap localized. Validation schemas derive from the OpenAPI-generated types (ADR-013/016) so form and API contracts stay in sync. RHF pairs with `@hookform/resolvers` + Zod.

## ADR-016: Validation (TypeScript) — Zod 4

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr needs runtime validation for forms (react-hook-form) and for parsing API responses against OpenAPI-derived types. Key requirement: clean resolver integration with react-hook-form and the ability to derive/align with OpenAPI types, while keeping the SPA bundle lean. All three candidates co-authored the [Standard Schema](https://standardschema.dev/schema) spec, and react-hook-form ships a `standardSchemaResolver` — so the form-library integration is a non-differentiator and lock-in risk is low.
- **Options considered:**

| Library | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **Zod 4** | 5 | 3 | 5 | 5 | 5 | Largest bundle (~17.7KB; Zod Mini ~6.88KB), ~180ms/100k; deepest OpenAPI tooling (openapi-zod-client, zod-openapi, Hey API, Orval). MIT |
| Valibot | 5 | 3 | 4 | 4 | 5 | Smallest (~1.37KB login schema), modular; thinner OpenAPI codegen story. MIT |
| ArkType | 5 | 5 | 3 | 3 | 5 | Fastest (~12ms/100k, JIT); higher learning curve; weakest OpenAPI ecosystem. MIT |

  Sources: [PkgPulse valibot-vs-zod](https://www.pkgpulse.com/guides/valibot-vs-zod-v4-typescript-validator-2026), [type-system teardown](https://dev.to/gabrielanhaia/zod-4-vs-valibot-vs-arktype-a-type-system-teardown-4lha), [Hey API zod plugin](https://heyapi.dev/openapi-ts/plugins/zod).

- **Decision:** **Zod 4**, because the load-bearing requirement is OpenAPI-type derivation and DX, not raw validation throughput. Zod has by far the deepest OpenAPI codegen ecosystem, the most familiar API, and the richest tooling. Validation cost in a management dashboard (hundreds of validations, not millions) is negligible, so ArkType's speed edge doesn't pay rent. Because all three are Standard Schema-compliant, we keep a thin swappable seam.
- **Consequences:** Accept the largest validation bundle; mitigate with Zod Mini's standalone-function imports for tree-shaking where it matters. Import schemas through the Standard Schema resolver (not the Zod-specific one) so a future swap stays cheap. Verify `zod-openapi` v4 feature support at adoption time and pin the codegen tool accordingly.

## ADR-017: Client/UI State (TypeScript) — Zustand

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** Server state lives in TanStack Query; this ADR covers only client/UI state (modals, sidebar, selected-site context, multi-step wizard, theme, optimistic toggles). The store must stay small, be easy for an OSS contributor base to reason about, and not duplicate server-cache responsibility. Redux Toolkit's headline RTK Query overlaps with TanStack Query and would be dead weight.
- **Options considered:**

| Library | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **Zustand** | 5 | 5 | 5 | 5 | 5 | ~1.16KB, no Provider, minimal boilerplate; fastest single update. MIT |
| Jotai | 5 | 4 | 4 | 4 | 5 | Atomic model; more concepts than coarse UI state needs. MIT |
| Redux Toolkit | 5 | 3 | 3 | 5 | 5 | ~13.8KB, heavy boilerplate; RTK Query duplicates TanStack Query. MIT |

  Sources: [Better Stack state mgmt](https://betterstack.com/community/guides/scaling-nodejs/zustand-vs-redux-toolkit-vs-jotai/), [DEV 2026 state mgmt](https://dev.to/jsgurujobs/state-management-in-2026-zustand-vs-jotai-vs-redux-toolkit-vs-signals-2gge).

- **Decision:** **Zustand**, because for client/UI-only state it is the consensus best balance of simplicity and power: smallest, fastest on the measured update path, no Provider, lowest cognitive cost for an OSS contributor pool. Redux Toolkit's strengths are unnecessary at this scale or redundant with TanStack Query; Jotai's atomic granularity is power we don't yet need for coarse UI state.
- **Consequences:** Keep stores small and slice-by-feature; explicitly forbid putting server/cache data in Zustand (that boundary stays with TanStack Query). We forgo built-in time-travel debugging (Zustand has a Redux DevTools middleware if needed). If a future feature demands heavy derived/computed graphs, Jotai can be introduced locally.

## ADR-018: Charts — Tremor (on Recharts)

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** Metrics to render: uptime %, latency time-series, Core Web Vitals, backup sizes — standard dashboard line/area/bar/gauge charts over modest datasets (hundreds to a few thousand points). Priorities: React-first declarative API, polished SaaS look, MIT license, low integration cost.
- **Options considered:**

| Library | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **Tremor** | 5 | 3 | 5 | 5 | 5 | Pre-styled dashboard charts on Recharts, shadcn aesthetic; MIT, backed by Vercel. Inherits SVG perf ceiling |
| Recharts v3 | 5 | 3 | 5 | 5 | 5 | 2.4M weekly dl, composable SVG (~150KB). MIT |
| visx | 4 | 4 | 3 | 4 | 5 | Low-level D3+React primitives; build charts yourself. MIT |
| ECharts | 5 | 5 | 3 | 4 | 5 | Canvas, 100k–millions of points, imperative-options API. Apache-2.0 |

  Sources: [PkgPulse recharts-vs-tremor](https://www.pkgpulse.com/guides/recharts-v3-vs-tremor-vs-nivo-react-charting-2026), [Vercel acquires Tremor](https://vercel.com/blog/vercel-acquires-tremor).

- **Decision:** **Tremor** (built on Recharts), because the use case is exactly its sweet spot — a SaaS metrics dashboard with conventional charts and a polished look matching shadcn/ui. It minimizes chart-building effort (vs visx) and avoids ECharts' imperative-options friction, while staying MIT under Vercel's stewardship. ECharts' million-point capability is irrelevant for these datasets. Tremor clearly wins on DX over plain Recharts for this app.
- **Consequences:** We sit on the SVG performance ceiling (Recharts under the hood) — fine for stated datasets; if we later add high-frequency real-time streams with tens of thousands of points, budget a targeted swap to ECharts/Canvas for that one view. Because Tremor *is* Recharts, dropping to raw Recharts for a custom chart is friction-free. Tailwind (already in the stack) is assumed for Tremor styling.

## ADR-019: Internationalization — Lingui v5

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr is a self-hostable OSS dashboard that will likely need multiple UI locales contributed by the community. We want type-safe messages, small runtime/bundle impact, a healthy translation-tooling ecosystem (so non-dev contributors can translate), and a permissive license.
- **Options considered:**

| Library | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **Lingui** | 5 — MIT, v5.5.x active | 5 — compile-time extraction, ~2–3KB runtime, ICU | 5 — mature CLI/extractor, type-safe | 4 — broad TMS support | 5 — MIT | Best balance |
| react-i18next | 5 — MIT, largest ecosystem | 3 — ~25KB runtime | 4 — plugin-rich | 5 — every TMS integrates it | 5 — MIT | Heavier runtime |
| Paraglide | 5 — MIT | 5 — tree-shakable per-message fns, constant bundle | 4 — newer | 3 — younger ecosystem, tied to inlang SDK | 5 — MIT | Best raw bundle, higher risk |

  Sources: [Lingui releases](https://github.com/lingui/js-lingui/releases), [Lingui vs i18next](https://lingui.dev/misc/i18next), [Paraglide benchmark](https://inlang.com/m/gerre34r/library-inlang-paraglideJs/benchmark).

- **Decision:** **Lingui (v5)**, because it is the strongest balance of the project's competing needs: near-best-in-class bundle/runtime efficiency (compile-time extraction, ~2–3KB runtime, ICU) *and* a mature, contributor-friendly tooling ecosystem (CLI extractor, catalogs, broad TMS support). react-i18next is the boring pick with the biggest ecosystem but ships a heavier runtime and lacks Lingui's compile-time type safety. Paraglide has the best raw bundle story but a younger, in-flux ecosystem — higher-risk for an OSS project depending on community translators.
- **Consequences:** Adopt Lingui's build-step (macro/extractor) into the Vite pipeline; contributors run `lingui extract`. ICU MessageFormat covers pluralization/gender. Migration risk if a wanted TMS only targets i18next — mitigated by keeping message access behind a thin `t()` wrapper. Catalogs are compiled, so translation changes require a rebuild.

## ADR-020: End-to-End Testing — Playwright

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr needs E2E coverage of critical flows (login, connect WordPress site, run backup, view metrics) for an OSS project where CI cost and parallelization matter and contributors run tests locally across browsers. Cross-browser (incl. WebKit/Safari) coverage is desirable.
- **Options considered:**

| Framework | Maintenance | Performance | DX | Ecosystem fit | License fit | Notes |
|---|---|---|---|---|---|---|
| **Playwright** | 5 — 75k★+, 33M weekly npm | 5 — ~290ms/action, native Chromium/Firefox/WebKit, free `--shard` | 5 — trace viewer, UI mode | 5 — 5x adoption momentum | 5 — Apache-2.0 | |
| Cypress | 5 | 3 — ~420ms/action, no WebKit, parallelization needs paid Cloud | 5 — best-in-class time-travel debugger | 4 | 5 — MIT | |

  Sources: [tech-insider cypress-vs-playwright](https://tech-insider.org/cypress-vs-playwright-2026/), [Autonoma comparison](https://getautonoma.com/blog/playwright-vs-cypress).

- **Decision:** **Playwright**, because for a new 2026 project it wins on nearly every axis here: native cross-browser coverage including WebKit/Safari, lower per-action latency and RAM, and — critically for an OSS budget — free built-in parallel sharding, whereas Cypress gates real parallelization behind paid Cypress Cloud. It also has 5x the adoption momentum. Cypress's main edge (time-travel debugger) no longer outweighs these; Playwright's trace viewer narrows that gap.
- **Consequences:** Contributors install browser binaries via `npx playwright install`; CI uses `--shard` for free fan-out. We give up Cypress's interactive-runner ergonomics, mitigated by Playwright's trace viewer and UI mode. Apache-2.0 is AGPLv3-compatible. Standardize on Playwright's test runner so fixtures/sharding come for free.

## ADR-021: PHP Testing Framework — PHPUnit

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr's agent plugin is MIT PHP 8.0–8.5 code under `WPMgr\Agent\`, using libsodium, AES-256-GCM, and the WP REST API. It must run on real WordPress hosts, so testing needs (a) pure-PHP unit tests of crypto/serialization without a WP bootstrap, (b) WP-aware unit tests that mock the hook/filter API (Brain Monkey / WP_Mock), and (c) optional integration tests against the WordPress core PHPUnit test suite. The WP ecosystem (core, Yoast wp-test-utils, Brain Monkey, WP_Mock) is built on PHPUnit; Pest runs on top of PHPUnit (Pest v4 on PHPUnit 12).
- **Options considered:**

| Criterion | PHPUnit 13.x | Pest 4.x |
|---|---|---|
| Maintenance | 5 — v13.1.x (May 2026), de-facto standard | 5 — v4.7.x (May 2026), very active |
| Performance | 4 — baseline | 4 — wraps PHPUnit; equivalent + parallel |
| DX | 3 — verbose class-based xUnit | 5 — concise functional DSL, type-coverage |
| Ecosystem fit (WP) | 5 — WP core suite, Yoast Polyfills, Brain Monkey, WP_Mock all target PHPUnit | 3 — runs on PHPUnit but WP tooling/docs assume PHPUnit; Pest strengths are Laravel-centric |
| License fit | 5 — BSD-3-Clause | 5 — MIT |

  Sources: [phpunit.de versions](https://phpunit.de/supported-versions.html), [Pest v4](https://pestphp.com/docs/pest-v4-is-here-now-with-browser-testing), [Yoast wp-test-utils](https://github.com/Yoast/wp-test-utils), [Brain Monkey](https://github.com/Brain-WP/BrainMonkey).

- **Decision:** **PHPUnit**, because the entire WordPress testing ecosystem WPMgr depends on targets PHPUnit directly, and matching the version constraints those tools document (via PHPUnit Polyfills) is critical when the plugin must run on PHP 8.0–8.5 across many hosts. Pest's DX edge is real but mostly realized in Laravel; in a WP plugin it adds a DSL layer and PHPUnit version-coupling without removing WP-specific friction. Use PHPUnit + Brain Monkey for fast unit tests, Yoast's Polyfills for cross-version compatibility, and the WP core suite for integration.
- **Consequences:** Tests are class-based and more verbose; pin PHPUnit through `phpunit/phpunit-polyfills` so the suite runs across the PHP 8.0–8.5 / WP version matrix in CI. Brain Monkey covers hook/filter mocking without a WP install; reserve a WP-core-suite job for true integration. Pest can be layered on later without abandoning this foundation.

## ADR-022: PHP Static Analysis — PHPStan

- **Status:** Proposed
- **Date:** 2026-05-27
- **Context:** WPMgr needs strict static analysis over security-sensitive code (Ed25519 signing, AES-256-GCM, REST handling) on PHP 8.0–8.5. The key dependency is high-quality, current WordPress stubs plus a WP extension that understands hooks/filters and core return types. Two WP stub ecosystems exist: `php-stubs/wordpress-stubs` + `szepeviktor/phpstan-wordpress` (PHPStan) and `psalm/plugin-wordpress` (Psalm). Maintenance health of analyzers and stubs is decisive.
- **Options considered:**

| Criterion | PHPStan 2.x | Psalm 6.x |
|---|---|---|
| Maintenance | 5 — v2.1.x (May 2026), near-weekly, large team | 3 — v6.16.x (Mar 2026), effectively single-maintainer |
| Performance | 5 — 25–40% faster since 2.1.34 | 4 — fast, multi-threaded |
| DX | 4 — levels 0–10, huge extension catalog | 4 — strong taint analysis for security code |
| Ecosystem fit (WP) | 5 — `php-stubs/wordpress-stubs` (12M+ installs, WP 6.9.1) + `szepeviktor/phpstan-wordpress` | 3 — `psalm/plugin-wordpress` slower cadence, smaller WP base |
| License fit | 5 — MIT | 5 — MIT |

  Sources: [PHPStan repo](https://github.com/phpstan/phpstan), [php-stubs/wordpress-stubs](https://github.com/php-stubs/wordpress-stubs), [szepeviktor/phpstan-wordpress](https://github.com/szepeviktor/phpstan-wordpress), [Psalm repo](https://github.com/vimeo/psalm).

- **Decision:** **PHPStan**, because its WordPress integration is the ecosystem standard and is materially better resourced: `php-stubs/wordpress-stubs` plus `szepeviktor/phpstan-wordpress` give hook/filter docblock validation and dynamic return types that Psalm's slower-cadence plugin doesn't match. PHPStan ships near-weekly with a large team and full PHP 8.5 support, vs Psalm's single-maintainer model. Both MIT and within the freshness rule, but PHPStan wins decisively on WP ecosystem fit and maintenance depth. Adopt at level 8+, raising toward max on crypto/REST modules.
- **Consequences:** Add `phpstan/phpstan`, `php-stubs/wordpress-stubs`, `szepeviktor/phpstan-wordpress` as dev deps; configure stubs in `phpstan.neon`. **Bus-factor risk** (risk register item 5): the WP stubs/extension maintainer signaled possible discontinuation without funding — budget sponsorship or be ready to vendor/fork. PHPStan lacks Psalm's built-in taint analysis, so security review of crypto/REST paths shouldn't rely on static analysis alone.
