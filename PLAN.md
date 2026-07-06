# WPMgr Development Plan

Updated 2026-07-06 (v0.61.18). History of everything shipped lives in [CHANGELOG.md](./CHANGELOG.md); architecture decisions in [docs/adr/](./docs/adr/) and [DECISIONS.md](./DECISIONS.md).

## Shipped (V0 + most of V1)

The original Phase 0-6 plan is complete and in production:

- **Foundation** — multi-tenant CP (Gin + sqlc + River + RLS), Ed25519-signed agent protocol, React dashboard, self-host compose stack + quickstart script, GCP hosted deployment.
- **Auth & tenancy** — email/OIDC login, RBAC, organisations, per-site sharing (outside collaborators), operator 2FA (TOTP + passkeys), superadmin console, hash-chained audit log with integrity re-baseline.
- **Sites & lifecycle** — live enrollment, connection-state machine, heartbeats, one-click login (autologin), site screenshots, host-provider detection.
- **Backups** — pure-PHP engine, archive-delta incrementals, selective components + exclusions, encrypted off-site storage, restore with inspection preview, DB snapshots, snapshot locking, retention mark-and-sweep GC, chain-aware bulk delete, update-failure auto-restore.
- **Updates** — bulk plugin/theme/core updates with pre-update snapshots, health probe + auto-rollback, half-write recovery.
- **Monitoring** — uptime + TLS expiry probes, WP fatal (HTTP-200 WSOD) detection, downtime alerts (email + webhook), fleet dashboards (uptime, backup health, performance, email deliverability), real-user monitoring (Core Web Vitals).
- **Performance suite** — page cache, object cache, RUCSS, minify/delay/defer, font optimization + WOFF2 transcoding, image optimization (media-encoder), CDN rewrite, DB + media cleaners.
- **Security suite** — hardening + login bans, file integrity, site-user 2FA + password policy, vulnerability scanner (Wordfence Intelligence feed), File Manager (jailed, audited, off by default).
- **Agency** — white-label client reports, branded client portal, client invites.
- **Email** — instance SMTP, alert/digest cadences, fleet deliverability dashboard.
- **Distribution** — OSS releases (GHCR images + agent zip), marketing site (wpmgr.app), WordPress.org agent submission (in review).

## In flight

- [ ] **WordPress.org listing** — agent submitted as "Fleet Agent Site Manager"; passed the automated scans' real finding (2FA resume binding, fixed 0.61.9); awaiting human review. Crux item: autologin (MainWP/ManageWP precedent). Fallback if rejected: strip autologin + file-write from the wp.org build only.
- [ ] **Patchstack decision** — research complete (docs/security/ internal). Recommended: Phase 0 provider abstraction + Phase 1a BYO App-API-key co-existence; paid feed/vPatch only via a for-Hosts contract. Awaiting go/no-go.

## Next up (priority order)

1. [ ] **M16 — Hosted SaaS monetization** (the critical gap: pricing designed, tiers defined, `WPMGR_HOSTED` open-core gating exists, superadmin console growing — but **no billing**). Stripe subscriptions, plan limits enforcement (sites/storage per tier), upgrade/downgrade flows, dunning. Billable entity = org.
2. [ ] **Per-site outgoing email (SMTP/API providers) + cross-site email log** — plan locked, 6-phase CP-first build not started (docs/per-site-email-smtp-plan-2026-06-10.md).
3. [ ] **Font subsetting Phase 2** — per-font processing UI + WP Font Library discovery; media-encoder-first ordering (docs/adr/font-subsetting-phase2-plan.md).
4. [ ] **Security suite P5/P6** — IP reputation + geo controls (docs/security/security-suite-plan.md).
5. [ ] **Community feature requests** — #128 SFTP fallback for file operations, #113 bulk-update run basket.

## Later (V1 remainder + V2 candidates)

- [ ] Multi-region uptime probes
- [ ] Visual regression testing (pre/post-update screenshots diff)
- [ ] AI update advisor
- [ ] Real-time/continuous backup
- [ ] Standing webhooks + integrations surface; real CLI
- [ ] WPScan as an additional vuln source; Patchstack vPatch orchestration (Phase 2, on demand)
- [ ] Real CDN provider integration (beyond URL rewriting)
- [ ] Restic-style packing for the backup chunk store (deferred decision)
- [ ] Terraform provider, GitOps updates, mobile dashboard, SOC2 prep

## Standing engineering debt (tracked, low urgency)

- Perf-suite real-WP+Apache cache E2E harness; addendum punch-list (N1-N5, A7, A9)
- Auto-prune failed 0-byte backup runs (follow-up from #115)
- Audit re-baseline acknowledgment in the same tx as the baseline upsert
- TLS-enable redigo pool + re-enable Memorystore SERVER_AUTHENTICATION
- Swap blobstore GC + EnsureBucket to the native GCS Go SDK
- task-runner cleanup glob for per-component part filenames
