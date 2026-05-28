# WPMgr Development Plan

## Phase 0 — Setup
- [x] Create PLAN.md
- [x] Create DECISIONS.md
- [x] Create .claude/agents/ subagents
- [ ] User approval to proceed

## Phase 1 — Subagents (covered in 0.3)
- [x] tech-stack-researcher
- [x] backend-architect
- [x] frontend-architect
- [x] wp-agent-engineer
- [x] devops-engineer
- [x] security-reviewer
- [x] docs-writer

## Phase 2 — Repo Bootstrap
- [x] Root files (LICENSE, README, .gitignore, etc.)
- [x] Turborepo + pnpm workspace
- [x] Go workspace
- [x] apps/ scaffolds
- [x] packages/ scaffolds
- [x] infra/ scaffolds
- [ ] User approval to proceed

## Phase 3 — Tech Stack ADRs
- [x] ADR-001 ORM/query layer → **sqlc** (+ pgx/v5)
- [x] ADR-002 Migration tool → **Atlas CE** (Apache-2.0 only; fallback goose)
- [x] ADR-003 Job queue → **River** (Postgres-native)
- [x] ADR-004 OpenAPI codegen (Go) → **ogen** (isolate from Gin)
- [x] ADR-005 Validation (Go) → **go-playground/validator**
- [x] ADR-006 Logging (Go) → **log/slog**
- [x] ADR-007 Config (Go) → **koanf**
- [x] ADR-008 WebSocket (Go) → **coder/websocket**
- [x] ADR-009 HTTP client (Go) → **net/http + SSRF transport**
- [x] ADR-010 S3 client (Go) → **aws-sdk-go-v2** (⚠ MinIO server EOL — see risk register)
- [x] ADR-011 OTel stack → **OTel SDK + otelgin → Collector → Tempo + Prometheus + Grafana**
- [x] ADR-012 Frontend router → **TanStack Router**
- [x] ADR-013 Frontend data fetching → **TanStack Query v5**
- [x] ADR-014 Component lib → **shadcn/ui + Radix + TanStack Table**
- [x] ADR-015 Forms → **react-hook-form**
- [x] ADR-016 Validation (TS) → **Zod 4**
- [x] ADR-017 State (TS) → **Zustand**
- [x] ADR-018 Charts → **Tremor** (on Recharts)
- [x] ADR-019 i18n → **Lingui v5**
- [x] ADR-020 E2E → **Playwright**
- [x] ADR-021 PHP testing → **PHPUnit** (+ Brain Monkey, Polyfills)
- [x] ADR-022 PHP static analysis → **PHPStan** (+ WP stubs)
- [x] Risk register item 1 resolved → **SeaweedFS** for self-host object store
- [ ] User approval to proceed

## Phase 4 — V0 Skeleton
- [x] Backend skeleton (Gin, pgx, Atlas, sqlc, ogen, tenant+site CRUD, RLS, /healthz+/readyz)
- [x] Agent skeleton (PHP plugin, Ed25519 verify, AES-GCM keystore, jti anti-replay, wpmgr/v1)
- [x] Frontend skeleton (TanStack Router/Query, shadcn/Tailwind v4, @wpmgr/api, login+sites)
- [x] Infra skeleton (distroless Dockerfiles, compose w/ SeaweedFS, observability profile, CI)
- [x] Docs skeleton (README, architecture, install, agent, contributing, security, api)
- [x] Security review → PASS (no high/critical); 2 items carried to M1 (below)
- [x] Full-stack `docker compose up` E2E verification (healthz/readyz 200, CRUD, cross-tenant denied)
- [x] User approval to proceed

### Security carry-forward into Phase 5/M1 (from Phase 4 review)
- [ ] Enforce NOSUPERUSER/NOBYPASSRLS app DB role at startup (hard-fail) + split migration DSN from app DSN (currently only a startup WARNING — `db.WarnIfRLSBypassRole`)
- [ ] Replace unauthenticated `X-Tenant-ID` header stub in `middleware.Tenant()` with session-derived tenant (must land with auth)
- [ ] Apply ADR-009 SSRF-hardened transport to webhooks / agent calls / backup URLs
- [ ] Reject default `WPMGR_SESSION_SECRET` and enforce ≥32 bytes at startup
- [ ] Add security-headers middleware + CORS allowlist once SPA origin is finalized

## Phase 5 — V0 Feature Build
- [x] M1 — Auth + tenant + RBAC ✅ (E2E verified; security review PASS after fixes)
  - [x] ADR-023 OIDC client · ADR-024 sessions · ADR-025 password hash · ADR-026 Dex
  - [x] Non-superuser app DB role + migration/app DSN split + startup hard-fail
  - [x] Email+password (argon2id) + OIDC (go-oidc) login; SCS Redis sessions
  - [x] Replace X-Tenant-ID stub → session-derived tenant + membership
  - [x] RBAC roles owner/admin/operator/viewer + permission matrix middleware
  - [x] Tenant-scoped API keys (hashed, shown once)
  - [x] Append-only hash-chained audit log (+ /audit/verify)
  - [x] Dex in docker-compose for self-host OIDC
  - [x] RLS isolation tests prove cross-tenant denial; frontend real login
  - [x] Reject default WPMGR_SESSION_SECRET; nginx security headers
  - [x] Security review fixes: tenant read scoping (HIGH), invite role-ceiling, OIDC email_verified
  - Known follow-ups: dummy-hash login timing; API-key expiry; tenant-create→creator-membership; SeaweedFS healthcheck flakiness (S3 unused until M4)
- [x] M2 — Site registry + agent enrollment ✅ (E2E verified; security review PASS)
  - [x] Pairing-code enrollment (one-time, hashed, TTL) + public /enroll; per-site Ed25519 (agent gen, CP stores pubkey)
  - [x] Agent-auth (signed METHOD\nPATH\nTS\nNONCE\nhash; skew + single-use nonce; identity-from-key)
  - [x] Site metadata sync (WP/PHP/server/themes/plugins/active/multisite) + tags + tag filter
  - [x] River (ADR-003) wired; 5-min health sweep (heartbeat freshness) + nonce pruning
  - [x] WordPress agent: enroll/sign/metadata/heartbeat/wp-cron + 30-min auto-deactivate
  - [x] Frontend: Add-site pairing dialog, health/enrollment badges, site metadata + components
  - [x] Security: hardened nonce pruning (DoS), prod guard for dev CP signing key
  - Deferred to M3: enroll/agent RLS policy tenant-predicate (defense-in-depth; correct today via Go filters); agent-key revocation/disabled-site rejection; /enroll edge rate-limiting; force https for agent CP URL outside localhost
- [x] M3 — Bulk updates with rollback ✅ (E2E smoke + security review PASS after fixes)
  - [x] Update orchestrator (River, per-tenant parallelism) + update_runs/update_tasks (RLS)
  - [x] SSRF-hardened HTTP client (ADR-009) for all CP→agent/site calls
  - [x] CP→agent signed command channel (EdDSA JWT bound to aud=site + cmd); update/rollback
  - [x] Agent: WP-CLI + PHP-fallback update/rollback, pre-update snapshots (path-traversal safe)
  - [x] Post-update health probe + auto-rollback on 5xx/fatal
  - [x] Bulk UI: multi-select/tag, dry-run default, schedule; live SSE progress + polling fallback
  - [x] Update history (from→to version diffs); audit events
  - [x] Security fixes: JWT site+command binding (HIGH cross-tenant replay), version/slug validation (MED)
  - Deferred to later: per-run SSE subscriber cap (LOW); full backup-primitive snapshot integration after M4
- [x] M4 — Incremental backups + restore ✅ (E2E enroll-through-nginx verified; security review pending)
  - [x] blobstore (aws-sdk-go-v2) over SeaweedFS/S3; presigned PUT/GET
  - [x] blake3 content-addressed ~4MB chunks + per-tenant dedup/refcount; manifests in Postgres
  - [x] client-side age encryption (CP stores only ciphertext + public recipient; cannot decrypt) — ADR-027
  - [x] backup (files|db|full) + restore (full|paths|db_tables partial) + per-site schedule
  - [x] River scheduled backups + retention GC (30d rolling + monthly archive); orphan-chunk deletion
  - [x] agent: pure-PHP age+blake3 (real-age interop), presign upload, manifest, blake3-verify restore
  - [x] frontend: backups section, snapshot detail, restore dialog, schedule editor
  - [x] RLS on all backup tables; audit events
  - [ ] M4 security review
  - Also fixed (prod): single-origin API routing (enrollment 405 + dashboard 404); agent keystore portable master key + graceful activation
- [x] M5 — Uptime monitoring ✅ (live: real site probed up + TLS expiry; security review pending)
  - [x] ADR-028 clickhouse-go v2 · ADR-029 wneessen/go-mail (SMTP)
  - [x] ClickHouse metrics store (auto schema, MergeTree+TTL 90d, native batch insert)
  - [x] HTTPS probe (httptrace timings: DNS/connect/TLS/TTFB; TLS expiry from peer cert) via SSRF-hardened client
  - [x] River periodic probe every ~60s with concurrency cap; site health_status updated from results
  - [x] Uptime API per-site (7d/30d/90d windows: uptime%, avg latency, series) + dashboard summary
  - [x] Downtime alerts: email (go-mail SMTP) + signed webhook on transition >threshold consecutive downs (dedupe + recovery)
  - [x] Alert config (email recipients + webhook URL), RLS-scoped; webhook secret write-only
  - [x] Frontend: uptime section with window toggle + chart + TLS expiry warn; sites list status; alerts settings
  - [x] M5 security review → PASS (no high/critical)
  - [x] Hardening applied: loud-log SSRF/TLS test escape hatches; enforce http(s) scheme on site URL + webhook URL
  - Deferred to later: encrypt alert_configs.webhook_secret at rest (CP-side AES-GCM with a master key); per-tenant probe fairness in the sweep (interleave or per-tenant cap)
- [ ] M6 — Vuln scan (Wordfence Intelligence)
- [ ] M7 — Reports
- [ ] M8 — Polish & launch (audit log, V0 release)
- [ ] User approval to proceed

## Phase 6 — V1
- [ ] M9 — Visual regression
- [ ] M10 — AI update advisor
- [ ] M11 — Real-time backup (DynSync)
- [ ] M12 — Multi-region probes
- [ ] M13 — White-label + client portal
- [ ] M14 — Webhooks + CLI
- [ ] M15 — Patchstack + WPScan integration
- [ ] M16 — Hosted SaaS launch
- [ ] User approval to proceed

## Phase 7 — V2
- [ ] M17 — Terraform provider
- [ ] M18 — GitOps update flow
- [ ] M19 — Vulnerability auto-patching
- [ ] M20 — Headless WordPress & multi-CMS
- [ ] M21 — Temporal-based workflows
- [ ] M22 — Mobile dashboard
- [ ] M23 — SOC2 prep
- [ ] M24 — Plugin marketplace
- [ ] V2 release
