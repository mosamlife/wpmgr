# ADR-037 — WPMgr site-management roadmap

**Status:** proposed
**Date:** 2026-05-29
**Sources:** ADR-036 (backup-restore gap, separate doc) + 8 parallel research reports
- D1 storage adapters · D2 backup engine · D3 restore engine · D4 UI/UX (backup)
- E1 wire protocol · E2 site health · E3 security suite · E4 ops + extensibility (site management)

This ADR is **deferred until restore is stable**. See _restore-must-work-first_ at the bottom.

---

## TL;DR

Two feature tracks, one merged plan.

- **Backup track** (ADR-036): the backup-restore gap (URL rewriter, storage destinations, mu-plugin trap, audit receipt, etc.).
- **Site-management track** (this doc): the broader site-management gap (diagnostics, error monitoring, activity log, security suite, multi-account, dynamic sync, form testing, whitelabel, staging environments, visual regression testing).
- Where WPMgr already leads across the board (Ed25519 + canonical message + JTI replay + age E2E encryption + 60s autologin + hash-chained audit log + Schema::ensureCurrent + chunk dedup) — no work needed.
- The 12 sprints below are prioritized so the most operator-visible value lands first.

---

## Where WPMgr already leads — site-management (no work needed)

| Dimension | WPMgr | Leading site-management plugins |
|---|---|---|
| Inbound auth | Ed25519 JWT + `aud`/`cmd` claims + jti replay table | RSA/Ed25519 + bundled public key + 5-min replay floor |
| Outbound auth | Ed25519 detached signature over canonical message | SHA1 querystring signing |
| Tenant binding | `aud == site_id` required | None — same key globally |
| Anti-replay | Per-jti row + per-request memo | Single moving floor (300s window) |
| Backup encryption (E2E) | age × ChaCha20-Poly1305 per chunk | None — plain over TLS |
| Schema migration | Local `Schema::ensureCurrent` on `plugins_loaded` | CP-driven (weaker) |
| Cron safety auto-deactivate | 1800s window | None |
| Autologin recovery | Ed25519 JWT, ≤60s, single-use, role-bounded | 24h weak random secret in HTML form |
| Audit log integrity | Hash-chained + `Verify()` recompute | Plain table, no integrity |
| Idle posture | No DB-event listeners, no error handler installed | ~150 event listeners always-on |

## Where WPMgr already leads — backup (no work needed)

| Capability | WPMgr | Leading backup plugins (free tier) |
|---|---|---|
| Encryption at rest | age × ChaCha20-Poly1305 per chunk (V1 flip) | None |
| Content dedup | blake3-addressed chunks + refcount | None (Pro: mtime-skip only) |
| 5 GB memory bound | ~30 MiB resident | ~512 MiB |
| Retention | days + monthly archive | count only (cap 7) |
| Progress | SSE + PhaseStepper + ETA + byte counters | 3s setTimeout, no backoff, free-form text |
| Restore confirm | typed-host destructive modal | Basic JS confirm dialog |
| Restore selection | mode + components + paths + tables | full only in free |
| Schedule | per-site, days-based retention, monthly archive | one global schedule |
| Per-chunk integrity | blake3 + AEAD | filesize-only |
| Restore safety (self-DoS, disk bomb) | Shipped (Track 1, v0.9.5) | In-place delete-extract |
| Per-component archive split | Shipped (Track 5, v0.9.6) | Pro-only |
| SQL inspection card in restore UI | Shipped (Track 4, v0.9.5) | Not shown to operator |

---

## Critical gaps WPMgr must close

### From E1 wire protocol
1. **130+ inbound actions vs WPMgr's 8.** Leading site-management plugins expose reusable primitives (fs/db/manage/info wings) so the CP can do almost anything; we have purpose-built commands.
2. **No streaming chunked protocol.** A streaming design with CRC32/MD5 chunk prefixes + multipart push directly into open CP sockets would outperform our current `wp_remote_request` PUT-only approach.
3. **No multi-account / multi-tenant agent.** Leading plugins allow N CPs per site. Strict 1:1 today.
4. **Sparse site fingerprint.** Our metadata = 8 fields. Leading plugins collect ~50.

### From E2 site health (USER'S HEADLINE ASK)
- **14 inventory categories** uncollected today: Identity / PHP / MySQL / Filesystem / HTTP probes / Cron / Themes / Plugins / Users / Security constants / HTTPS / Mail / Performance hints / Hosting fingerprint
- **No error monitoring.** Leading site-management plugins store PHP errors with md5 dedup, 10k cap, CP pull-and-purge. We have nothing.
- **15 leapfrog opportunities** (not shipped by leading backup or site-management plugins):
  1. PHP-EOL countdown
  2. WP-Cron arrears histogram
  3. Per-route fatal counts
  4. "Plugin hiding updates" detection
  5. Hosting platform auto-detect (Kinsta/WPE/Pressable/Atomic/Flywheel/GridPane)
  6. Opcache health (hit-rate, restarts, wasted memory)
  7. Object-cache hit-rate sparkline
  8. Page-cache verifier (hit `home_url`, check `x-cache` / `cf-cache-status`)
  9. REST authentication map (which namespaces have `__return_true` overrides)
  10. Site-as-of fingerprint hash (sha256 of sorted plugins+versions+theme+core+php)
  11. Auto-update conformance check (`has_filter('auto_update_plugin')` callbacks)
  12. **Plugin licensing health** — ACF Pro, Gravity, WooSubscriptions, etc. — "killer feature for agencies, nobody ships it"
  13. Backup-readiness preflight (disk + max_allowed_packet + mysqldump reachable + uploads size)
  14. PHP error budget (rate-limit capture once exceeded)
  15. WP-CLI presence + version

### From E3 security suite
- WAF, brute-force login protection, TOTP 2FA, WP-side activity log, IP store, login whitelabel — zero today
- Smart pattern: wire all new security events into the existing `Dispatcher` (already does email + webhook for uptime) instead of building parallel notification systems

### From E4 ops + extensibility
- **Multisite likely-broken bug** — our `Settings` uses plain `get_option`/`update_option` with no `get_site_option` fallback. Leading site-management plugins implement a two-tier resolution.
- **Connection-key pairing** (paste a v2 blob into the CP, works behind firewalls that block CP→site WebHooks). We're push-only.
- **Form testing harness** (6 plugins: CF7, WPForms, Ninja, Gravity, Forminator, Formidable) — bypasses CAPTCHA + aborts email. CP can probe deliverability without spamming customers.
- **Dynamic sync** (~150 hooks → `bv_dynamic_sync` table). Foundation for incremental backups + WooCommerce real-time.
- **White-label** (per-slug plugin name/author/hide-from-list + login screen rebrand). Required for agency resale.
- **3-day "active plugin" decay** — security modules silently disable when site goes dark.

### From D1-D4 (backup track, in ADR-036)
- **URL rewriter with serialization safety** — P0 SHIPPED today (this conversation)
- **Storage destinations (Local + S3-compat)** — P1 SHIPPED today
- Pre-flight checks (max_allowed_packet, memory_limit, ZipArchive, DB ping) — pending
- Mu-plugin shutdown trap — pending
- Restore audit receipt — pending
- Backup-time component selection — pending
- 12h/fortnightly cadences + time-of-day + jitter — pending
- Env fingerprint in manifest — pending

---

## The 12 sprints (prioritized)

### Sprint 1 — Quick wins (3-5 days, P1)
- Multisite Settings two-tier resolution fix (`get_site_option → get_option` fallback)
- Connection-key pairing UX (15-min QR-mintable, not 24h `mt_rand` in HTML)
- Backup-readiness preflight (disk + max_allowed_packet + ZipArchive + DB ping + uploads size)
- Sparse metadata expansion (+ `plugin_uri`, `update_uri`, `author_uri`, `network` flag, host_flags, disk sizes)
- Manifest env fingerprint (php / mysql / wp / url / plugin slugs / theme slugs / table names)

### Sprint 2 — Diagnostics + Health tab (1.5-2 weeks) · **the user's headline ask**
- `class-diagnostics-command.php` (on-demand + daily 14-category collector)
- `class-error-monitor.php` + `wpmgr_php_errors` table (mu-plugin loader catches fatals during plugin load, md5 dedup, 10k cap)
- `/agent/v1/diagnostics` + `/agent/v1/errors` CP endpoints
- New `/sites/:id/health` route — 9 cards (PHP / MySQL / Filesystem / HTTP / Cron / Themes / Plugins / Users / Security) + freshness badges + "Re-run check" buttons
- New `/sites/:id/errors` route — fingerprint-grouped table + silence + download bundle
- **Plugin licensing health** (per-plugin option-key allowlist on CP) — the agency-killer feature
- **Hosting platform auto-detect** card (Kinsta/WPE/Pressable badge on Site overview)

### Sprint 3 — WP activity log + alerts wiring (1.5 weeks)
- `wpmgr_activity_log` table (implement ~30 hook handlers — MIT-clean, no third-party code)
- Hash-chained (steal from `audit.go::Verify`) for tamper-evidence — we leapfrog both products here
- `/agent/v1/activity` endpoint + CP storage + UI search/filter
- Wire security events into existing `AlertConfig` (add `notify_security bool`)

### Sprint 4 — Login protection + 2FA (2 weeks)
- `class-login-protect.php` (tiered thresholds 3 captcha / 10 IP-block / 100 site-wide, 30-min cooldowns)
- `class-2fa.php` (RFC-6238 TOTP, AES-256-GCM secrets keyed off keystore, **WITH backup codes** — many plugins omit these)
- Per-role enforcement option (`require_2fa_for_admins`)
- Lockout events flow into the existing alert dispatcher

### Sprint 5 — Storage adapter polish (1 week, sits on Phase 1 foundation shipped in P1)
- Per-destination retention policies
- Multi-destination fan-out (sequential V1; parallel V2)
- Storage health checks (HeadBucket roundtrip in heartbeat for misconfig detection)
- File-download from snapshot UI

### Sprint 6 — Staging environments (not sized — see note)
- Clone-to-target: run the existing restore pipeline (URL rewriter + storage-destination foundation, both P0/P1 shipped) against a new host or subpath instead of swapping into the live site
- Reuses the shipped `URL_REWRITE` phase to rewrite the cloned site's URLs to the staging host in the same pass restores already use
- Deny-by-default posture on the staging host by default (search-engine exclusion + HTTP auth gate), same posture already shipped for `LocalDestination`
- `wpmgr_staging_sites` table (parent site, staging host, created-at, last-synced-at, expiry)
- `/agent/v1/staging` CP endpoints (create / refresh / promote / destroy) + new `/sites/:id/staging` dashboard route
- **Not sized.** Whether staging is one-way (clone, inspect, discard) or round-trip (push staging edits back to the live site) is a product decision this document doesn't make, and the two scopes differ by weeks, not days. Size it once that's decided.

### Sprint 7 — Visual regression testing (~1 week)
- Same shape as the form-testing harness below: a bounded, on-demand check the CP triggers and reports on, not an always-on watcher
- Headless render + screenshot capture of a fixed URL set (home, a sample post, a sample page) before/after a restore, an update, or a staging promotion
- Pixel-diff against the stored baseline; percentage-changed + diff image returned to the CP
- `class-visual-regression.php` + `/agent/v1/visual-diff` endpoint, mirroring the form-testing harness's request/response shape
- Baseline images stored through the existing storage-destination adapters (Local + S3-compat) rather than a new blob path
- Usable standalone against a single site, and as the before/after check on a staging promotion (Sprint 6)
- Sized the same as the form-testing harness (below): single bounded capture-and-compare tool, no new protocol or storage layer

### Sprint 8 — Streaming chunked protocol (1-1.5 weeks)
- Streaming design with our JWT signing
- CRC32/MD5 chunk prefixes + multipart boundary
- `X-WPMgr-Stream-Hash` trailer for integrity over streamed payload
- Enables better backup pipeline + future file-scan / DB-introspection wings

### Sprint 9 — Form testing + login whitelabel + recovery polish (1 week)
- Form-testing harness for 6 form plugins (CF7, WPForms, Ninja, Gravity, Forminator, Formidable) — bypass CAPTCHA + abort email
- Login screen whitelabel (data-URI-only logo + banner via `login_message` filter — no remote URLs)
- Tightened recovery channel (15-min QR vs 24h weak random in HTML)

### Sprint 10 — Dynamic sync + incremental backup (4-6 weeks, the BIG bet)
- ~150 WP+WC hook listeners → `wpmgr_dynamic_sync` table
- Incremental backup pipeline (delta semantics on top of existing chunk dedup)
- WooCommerce CDC (HPOS `wc_orders*` + items + payment tokens + shipping zones)
- The biggest engineering bet — only justified if you commit to "incremental backup" as a product story
- Unlocks the WooCommerce real-time backup story

### Sprint 11 — Multi-account refactor (4-8 weeks, ONLY if MSP demand validated)
- Architectural lift; redesign Keystore to be pubkey-keyed
- Per-account `WPMgr\Agent\Account` value object
- Rewrite `Router::authorize` to look up CP public key by token `iss`/key-id
- Bet-the-product — **strongly recommend validating with a real customer before starting**

### Sprint 12 — WAF / Firewall (defer to last) (6-9 weeks)
- Strategy 1 (hardcoded SQLi/XSS/path-traversal rules, no rule engine) — 4-6 weeks
- Strategy 2 (data-driven JSON-AST rule engine) — 6-9 weeks
- Defer until everything else lands; this is the heaviest piece

---

## Explicit SKIP list (deliberately do NOT adopt)

### Site-management patterns to skip
| Pattern | Why skip |
|---|---|
| Raw `$_SERVER` dump | Info leak; ship curated allowlist instead |
| `phpinfo()` direct dump wire | Security smell |
| 2FA secret writing over the wire | Secrets never transmitted; locally generated only |
| Plugin hide-from-menu | Antagonistic to operator transparency |
| SHA1 outbound signature | We use Ed25519 detached |
| Bundled CP public key in source tree | We fetch at enrollment |
| PHP-`serialize()` response framing | JSON only; serialize is an unmarshal attack vector |
| 24h weak-random recovery secret in HTML | Tighten to 15-min QR mintable on demand |
| Per-user opt-in 2FA without backup codes | Ship WITH backup codes from day 1 |
| Recursive upload retry (stack-grows) | Anti-pattern; bounded loop |
| Base64 credential "encryption" | Real age encryption (already in keystore) |
| Colocated WP-admin HTML per adapter | We have React |
| Cross-site migration via backup destinations | Out of scope for backup destinations |

### Backup patterns to skip
| Pattern | Why skip |
|---|---|
| PclZip fallback for hosts without ext-zip | Modern managed hosts ship ext-zip; maintenance burden not worth it |
| RSA + Rijndael custom crypto format | Superseded by age + ChaCha20-Poly1305 |
| Snapshot module (in-DB shadow tables as "git tags") | We have chunk-level dedup; in-DB shadow doubles MySQL storage |
| Mega-merge step (`backup_all.zip`) | Re-pack with no compression upside, doubles transient disk |
| Env info duplicated INSIDE every zip | We have a CP-side manifest; duplicating splits source-of-truth |
| Cache-toggle for file compression | Footgun; always stream |

---

## Restore-must-work-first

This roadmap is **DEFERRED until full backup restore reliably works end-to-end** on `curvabykerline.in` (or another QA site).

Currently known issues (2026-05-29, agent v0.9.6-component-split installed):
1. Full backup restore failed at SSE event level (operator-reported)
2. 500 error from `GET /api/v1/backups/{snapshotId}/sql-inspection` for snapshot `546e2dab-0a78-4d4b-8137-d3b3f62c4119`

Diagnostic + fix is the next priority. ADR-037 implementation starts only after these are closed.

---

## Done already in this conversation arc (recorded for completeness)

**Agent v0.9.5-restore-safety** (Track 1 + Track 4):
- Self-DoS prevention: `PRESERVE_FROM_LIVE` allowlist + `ENTRY_DENYLIST_PREFIXES`
- Disk-bomb fix: `OLDFILES_GC_AGE_SECONDS` 24h→1h + opt-in 24h
- Disk-free precheck (two-leg: artifact × 1.5 + wp-content × 1.0)
- SQL inspector at backup time (gzopen streaming, 613 LOC, manifest entry `kind=inspection`)

**Agent v0.9.6-component-split** (Track 5):
- Per-component archive split: `plugins.partNNN.zip` / `themes.partNNN.zip` / `uploads.partNNN.zip` / `wp-content.partNNN.zip` (catch-all others)
- Per-component restore routing (atomic-when-everything / per-component-when-subset swap)
- Backward-compat: legacy `'file'` kind routes through original atomic swap

**API + Web v0.9.5-restore-tracks → v0.9.6-component-split → v0.9.7-csp-version-fix → v0.9.8-tabs-restore**:
- Restore dialog 3-mode (Everything / Database only / Files only) + 4 file-component sub-checkboxes
- SQL inspection card (200 / 202 polling / 503 / 404 state coverage)
- ManifestInspectionFetcher adapter (V0 plaintext chunks; V1 SaaS encryption flip ready)
- River queue `sql_inspect_legacy` (max 1 worker, 5-min timeout)
- Site detail tabs (6 child routes, non-sticky header + tab bar)
- CSP fixed (Google Fonts + inline styles allowed)
- BUILD_VERSION baked from env at build time

**P0 URL rewriter** (just shipped, on disk, ready for v0.9.9 deploy):
- `class-url-rewriter.php` (~400 LOC) with 44-entry skip-tables denylist + serialization-safe walk
- 7 URL form variants per pair (raw, JSON-escaped, URL-encoded, scheme-relative, ×2 for http+https)
- New `URL_REWRITE` phase between `RESTORE_DB` and `SWAP_DB` with same-URL short-circuit
- Migration adds `source_*_url` columns to snapshot

**P1 storage adapter foundation** (just shipped, on disk, ready for v0.9.9 deploy):
- `BackupDestination` interface + `LocalDestination` (deny-by-default headers for Apache/IIS/nginx) + `CPDestination` wrapper + `DestinationResolver`
- New `sitedestination/` CP package: model + repo + service + handler (6 routes)
- `blobstore.Registry` per-destination Store cache
- age-encrypted secret storage
- Migration `m7_site_destinations.sql` with RLS + partial unique index for `is_default`
- New `/destinations` route (under Operations, next to Backups) + per-provider form + test-connection (HeadBucket + PutObject + DeleteObject); moved out of Settings in 0.51.1
- Sensitive-credential UX: `••••••••` placeholder + "Re-enter to save" amber warning
