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
- [ ] Root files (LICENSE, README, .gitignore, etc.)
- [ ] Turborepo + pnpm workspace
- [ ] Go workspace
- [ ] apps/ scaffolds
- [ ] packages/ scaffolds
- [ ] infra/ scaffolds
- [ ] User approval to proceed

## Phase 3 — Tech Stack ADRs
- [ ] ADR-001 ORM/query layer — sqlc vs Bun vs Ent vs GORM
- [ ] ADR-002 Migration tool — goose vs golang-migrate vs Atlas
- [ ] ADR-003 Job queue — River vs Asynq vs Temporal vs Postgres LISTEN/NOTIFY
- [ ] ADR-004 OpenAPI codegen (Go) — oapi-codegen vs ogen vs huma
- [ ] ADR-005 Validation (Go) — go-playground/validator vs ozzo-validation vs Cue
- [ ] ADR-006 Logging (Go) — log/slog vs zerolog vs zap
- [ ] ADR-007 Config (Go) — koanf vs Viper vs envconfig
- [ ] ADR-008 WebSocket (Go) — coder/websocket vs gorilla/websocket vs gobwas/ws
- [ ] ADR-009 HTTP client (Go) — net/http vs resty vs req
- [ ] ADR-010 S3 client (Go) — aws-sdk-go-v2 vs minio-go
- [ ] ADR-011 OTel stack — Gin OTel middleware + collector + Tempo/Jaeger
- [ ] ADR-012 Frontend router — TanStack Router vs React Router 7
- [ ] ADR-013 Frontend data fetching — TanStack Query vs SWR
- [ ] ADR-014 Component lib — shadcn/ui vs Park UI vs Mantine vs Radix-only
- [ ] ADR-015 Forms — react-hook-form vs TanStack Form
- [ ] ADR-016 Validation (TS) — Zod vs Valibot vs ArkType
- [ ] ADR-017 State (TS) — Zustand vs Jotai vs Redux Toolkit
- [ ] ADR-018 Charts — Recharts vs Tremor vs visx vs ECharts
- [ ] ADR-019 i18n — Lingui vs react-i18next vs Paraglide
- [ ] ADR-020 E2E — Playwright vs Cypress
- [ ] ADR-021 PHP testing — PHPUnit vs Pest
- [ ] ADR-022 PHP static analysis — PHPStan vs Psalm
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
