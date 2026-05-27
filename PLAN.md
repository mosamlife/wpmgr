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
- [ ] Backend skeleton
- [ ] Agent skeleton
- [ ] Frontend skeleton
- [ ] Infra skeleton
- [ ] Docs skeleton
- [ ] Security review
- [ ] User approval to proceed

## Phase 5 — V0 Feature Build
- [ ] M1 — Auth + tenant + RBAC
- [ ] M2 — Site registry + agent enrollment
- [ ] M3 — Bulk updates with rollback
- [ ] M4 — Incremental backups + restore
- [ ] M5 — Uptime monitoring
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
