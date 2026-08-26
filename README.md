# WPMgr

**Open-source, self-hostable WordPress fleet management.**

WPMgr lets you enroll, monitor, update, back up, and secure a fleet of WordPress sites from one dashboard, all running on infrastructure you control. The control plane is a Go binary with a React dashboard; a lightweight PHP plugin on each managed site handles the work. Everything between the agent and the control plane is Ed25519-signed.

[![Latest release](https://img.shields.io/github/v/release/mosamlife/wpmgr?label=release&style=flat)](https://github.com/mosamlife/wpmgr/releases)
[![License](https://img.shields.io/badge/license-AGPL--3.0-blue?style=flat)](./LICENSE)
[![CI](https://img.shields.io/github/actions/workflow/status/mosamlife/wpmgr/ci.yml?branch=main&label=CI&style=flat)](https://github.com/mosamlife/wpmgr/actions/workflows/ci.yml)
[![WordPress.org](https://img.shields.io/wordpress/plugin/v/fleet-agent-site-manager?label=wordpress.org&style=flat)](https://wordpress.org/plugins/fleet-agent-site-manager/)

[![WPMgr: run your whole WordPress fleet from one dashboard you own](docs/images/landing.png)](https://wpmgr.app)

<p align="center">
  <a href="https://wpmgr.app">Website</a> ·
  <a href="https://manage.wpmgr.app">Live dashboard</a> ·
  <a href="https://wpmgr.app/docs/">API reference</a>
</p>

<!-- wpmgr-install-pins:start (the version below is a required pin; scripts/check-version-surfaces.sh keeps it current) -->
**v0.61.146**: open-source and production-usable for self-hosters. The agent plugin is reviewed and listed in the [WordPress.org plugin directory](https://wordpress.org/plugins/fleet-agent-site-manager/).
<!-- wpmgr-install-pins:end -->

---

## Quick start

Get the whole stack running on your own server with one command, no repo clone needed:

```bash
curl -fsSL https://raw.githubusercontent.com/mosamlife/wpmgr/main/scripts/quickstart-selfhost.sh | bash
```

The script downloads every file the stack needs, generates all secrets, and prints the exact command to bring WPMgr up. You need a 64-bit Linux host with Docker 24+ (2 GB RAM is enough to start). See [system requirements](./docs/requirements.md) for sizing by fleet size, or the [full quickstart](#quickstart-self-host) below for options, prebuilt images, and the build-from-source path.

---

## Features

### Fleet connection

- **Live enrollment**: Add a site by URL; paste a one-time code into the agent plugin; the dashboard flips from "Awaiting" to "Connected" automatically, no refresh needed.
- **Real-time connection state machine**: Six precise states (`pending_enrollment` → `connected` → `degraded` → `disconnected` → `revoked` → `archived`) replace a vague up/down flag. Every transition is written to auditable, hash-chained history.
- **60 s heartbeat with auto-recovery**: A background sweeper degrades → disconnects silent sites within minutes. A returning agent auto-recovers without operator action.
- **Fleet SSE stream**: One shared Server-Sent Events stream keeps the entire sites list live (status dots, last-seen counters) without polling. Cursor-based replay catches events missed while offline; all performance and scan surfaces refresh automatically on reconnect.
- **One-click autologin to wp-admin**: Single-use, short-lived, audited EdDSA tokens; no shared passwords. Deep-links straight to Plugins or Themes, or log in as a specific user. Bypasses common two-factor plugins.
- **Signed dashboard-to-agent revoke**: Revoke from the dashboard; the agent verifies a signed token on its next heartbeat and self-destructs. A man-in-the-middle on the heartbeat response cannot forge the teardown.
- **Signed last-will on deactivate/uninstall**: Agent disconnects itself on deactivation (3 s best-effort); the timeout sweeper is the safety net if it never arrives.
- **Re-enrollment under a stable identity**: Re-connecting a site keeps the same `site_id`, preserving all backup history, scan runs, and lifecycle generations.
- **Archive / restore soft-delete**: Retire sites from the active view without losing their history; restore them later.
- **Per-site sharing**: Share exactly one site with a collaborator, enforced by both Gin middleware and Postgres RESTRICTIVE RLS, without exposing the rest of the fleet.
- **Agent self-update channel**: Self-hosted control planes push signed agent releases to enrolled sites. The [WordPress.org build](https://wordpress.org/plugins/fleet-agent-site-manager/) ships without the self-updater and updates from the site's own Plugins screen instead.

### Backups & restore

- **Pure-PHP streaming DB dump**: Server-side cursor (`MYSQLI_USE_RESULT`), `REPEATABLE READ` snapshot, ~1 MiB batched INSERTs. No mysqldump binary, no shell access, so it works on locked-down managed hosts. Memory use is independent of database size.
- **Pure-PHP streaming file archiver**: ZipArchive streaming (never loads file bodies into memory), rotated at 200 MiB / 55 k entries per part. Splits wp-content into per-component sequences (plugins, themes, uploads, other, WP core) for targeted restore.
- **Incremental archive-delta backups**: Each increment diffs the live file tree against the parent snapshot's `files.list` by size + mtime and packs only changed or new files into standard part archives, with deletions recorded as tombstone manifest sidecars. The database is dumped in full every run.
- **Selective-component backups + exclusion patterns**: Per-site choice of which components to archive (plugins / themes / uploads / wp-content / database / WP core) plus exclude-path, exclude-extension, and max-file-size filters pushed to the agent on every run. Backup content settings are decoupled from the schedule so manual and scheduled runs share one definition.
- **Content-addressed chunking with dedup**: Each artifact is chunked at ~4 MiB, BLAKE3-hashed, and deduplicated across snapshots. Only changed chunks re-upload on the next full backup.
- **Client-side age encryption**: Unconditional X25519 + ChaCha20-Poly1305 per-chunk encryption to the site's public recipient, auto-provisioned by the agent on connect; the control plane stores only ciphertext and never holds a decryption key.
- **Three backup destinations**: Control-plane-managed bucket, customer-owned S3-compatible bucket (agent never holds the credentials), or a local folder on the WordPress host. See [docs/features/backups.md](./docs/features/backups.md#backup-destinations).
- **SQL inspection at backup time**: A streaming constant-memory scanner produces `sql-inspection.json` (charset, table prefix, per-table row/byte estimates, WordPress detection, `siteurl`/`home`/`db_version`) stored with every snapshot.
- **Environment fingerprint**: `environment.json` captures PHP/MySQL/WordPress versions, plugin/theme slugs, table list, and size at snapshot time.
- **Resumable watchdog-driven state machine**: Phases are checkpointed to a task row; a watchdog re-enters stalled backups up to 6 times. Long backups survive PHP time limits and FPM worker recycling without redoing finished work.
- **Scheduled backups**: Hourly, every-N-hours, daily, weekly, or monthly cadences, run in the site's own WordPress timezone, jittered per site so the fleet doesn't fire simultaneously.
- **Snapshot lock**: Lock a snapshot to exempt it from retention GC regardless of age or keep-last rules; unlock explicitly when no longer needed.
- **Mark-and-sweep retention GC**: Tenant-global reachability walk deletes a chunk only when it is unreachable from every retained snapshot across all sites and predates a fail-closed grace floor. Refcount is observability-only and never consulted for delete decisions.
- **Backup-completion email**: Per-site notification config (always / on-failure / never + recipient list) sends a branded email on backup completion or failure for both manual and scheduled runs.
- **Live SSE progress**: Phase-by-phase progress including chunk and byte counters streams to the dashboard in real time.
- **Point-in-time chain restore with version picker**: Restoring a chain member overlays each generation's parts in order (newest-wins extraction) then applies tombstone deletes, so any snapshot in the chain restores correctly. The restore dialog renders a version picker across all completed chain generations.
- **Component-scoped restore**: Restore the whole site, just the database, just files, or fine-grained components (plugins, themes, uploads, wp-content). Compose with per-path or per-table lists.
- **Atomic file restore**: Extract to a hidden staging directory, move live tree aside to a `.wpmgr-old-files-<id>/` rollback dir, rename staging into place. A crash never leaves a half-merged site.
- **Online DB restore**: Import into `tmp<id>_`-prefixed tables while WordPress stays live; swap each table atomically via `DROP + RENAME` at the end. A failed import leaves the live database untouched.
- **Resumable restore**: Same watchdog pattern as backup: persisted phase state, chunk-level download resume, mid-table URL-rewrite resume.
- **Two-leg disk-free precheck**: Estimates required disk before touching anything; refuses with a GB-denominated message if there is not enough space.
- **Self-preservation guards**: Never clobbers the running agent plugin, keystore, `wp-config.php`, `.htaccess`, or cache drop-ins; copies them forward from the live tree into staging before the swap.
- **Path-traversal & integrity hardening**: Every downloaded chunk is BLAKE3-verified; every zip entry is traversal-checked with a canonical-path containment check against the staging root.
- **Maintenance-mode windowing**: Drops WordPress's `.maintenance` file around the destructive swap; removes it and flushes object cache, OPcache, and rewrite rules on completion.

### Updates

- **Per-site available-updates inventory**: WordPress core, plugin, and theme update lists with current → new versions, active-first sorted.
- **On-demand inventory refresh**: Force re-poll WP update transients via a signed CP→agent command; returns 409 with a clear message when a site is unreachable.
- **Bulk fleet-wide update runs**: Target an explicit set of site IDs or a tag; expands to per-(site, item) tasks.
- **Dry-run preview**: The bulk wizard defaults to `dry_run=true` so the first submit is a safe preview with exact version deltas.
- **Pre-update snapshot + automatic health-check rollback**: The agent snapshots the component before updating; the CP health-probes the site after; a bad update is reverted automatically.
- **WP-CLI-first with PHP upgrader fallback**: Works with or without WP-CLI; preserves active-plugin state on the PHP fallback path.
- **Live SSE progress per run**: Task status transitions stream in real time; a snapshot-on-connect plus a 2 s poll safety net prevent stale "Queued" states.
- **Post-update inventory auto-refresh**: Debounced per-site (30 s window) after every terminal task so the available-updates list self-heals.
- **Argument-injection hardening**: Slugs and versions are validated on both the CP (`validateItems`) and agent (`sanitizeSlug`, `isValidVersion`) against a safe charset; shell metacharacters, `..` traversal, and flag separators are rejected.
- **Per-tenant concurrency isolation**: Sharded River queues with a per-tenant running-task limit; one large fleet update cannot starve other tenants.
- **Update run history**: Auditable trail of who updated what, when, outcome per site/item, with from/to version and timings.

### Monitoring & health

- **Sites grid view with screenshots**: A list/grid toggle on the Sites dashboard. Each grid card is led by a real screenshot captured server-side (headless Chromium in the media-encoder, SSRF-guarded) and refreshed on connect, weekly, and on demand. Cards show capability status (page cache, object cache, HTTPS, backups, multisite), version/host/client/tag metadata, uptime, latency, SSL expiry, and backup health. The Status and Tags filters compose with search and persist in the URL. See [docs/features/sites.md](./docs/features/sites.md).
- **Active uptime monitoring**: SSRF-hardened probes classify up/down with a full timing breakdown: DNS, connect, TLS handshake, TTFB, total. Also detects a WordPress fatal-error or database-connection page served with HTTP 200 and reclassifies it as down. See [docs/features/monitoring.md](./docs/features/monitoring.md).
- **Uptime % + latency charts**: Windowed reports over 7 d / 30 d / 90 d; downsampled server-side. Tenant-wide status summary across the whole fleet.
- **TLS certificate tracking**: Expiry, issuer, and subject captured on every HTTPS probe; flags "renew soon" at < 14 days.
- **Pluggable time-series store**: Postgres (default, one row per probe) or ClickHouse (MergeTree, 90-day TTL) selectable at boot.
- **Downtime/recovery alerts**: Transition-only (opens incident on N consecutive failures, closes on recovery). One downtime alert and one recovery alert per outage, never a flood.
- **Alert channels**: Email (SMTP) and HMAC-SHA256-signed webhook. Both uptime and high-severity security events share one channel config.
- **Full WordPress Site Health collection**: Ships the complete `WP_Debug_Data::debug_data()` dump (all native sections plus third-party plugin contributions from Yoast, WooCommerce, ACF, etc.) centrally.
- **14-category extended diagnostics with fault isolation**: Identity, PHP, MySQL, filesystem, HTTP loopback, cron, themes, plugins, users, security constants, HTTPS, mail, performance, hosting, each in an isolated wrapper so one failing probe doesn't blank the screen.
- **Leapfrog diagnostic signals**: Max-overdue WP-Cron age, OPcache hit-rate/memory, `site_as_of_hash` fingerprint (changes when any managed component moves), agent-side managed-host detection (Pressable, GridPane, WP Engine, Atomic/WPCOM, Kinsta, Flywheel, RunCloud, Cloudways), and control-plane offline egress-IP inference of the underlying cloud/IaaS provider (DigitalOcean, Hetzner, AWS, Google Cloud, Azure, Vultr, OVH) via embedded DB-IP ASN lookup (no API key; no IP leaves the operator). IP data by DB-IP.com.
- **JIT directory sizes**: Fresh or cached (< 6 h), computed via `du` or PHP fallback, annotated with method + computed-at timestamp. Never blank like the native Site Health screen.
- **Privacy redaction**: Agent-side recursive walker redacts `admin_email`, `*_password`, `*_secret`, `*_token`, API keys, and auth salts before the diagnostics blob leaves the site.
- **On-demand diagnostics refresh**: One click re-collects all categories and lands the data before the response returns.
- **PHP error monitoring**: Error + shutdown handlers (including a must-use plugin that arms before other plugins boot, catching bootstrap-time fatals). Deduped by `md5(code:file:line:message)` with occurrence counters and gzip-compressed backtraces. Near-immediate ship on fatal.
- **Per-site error config**: Level bitmask + fingerprint silence list pushed to the agent. Silencing a fingerprint deletes its existing rows immediately. Fatals always captured regardless of mask.
- **Hash-chained activity log**: ~30 WordPress events (posts, comments, users, auth + failed logins, plugins, themes, core updates, terms, security-relevant options, WooCommerce order/product events) written as a SHA-256 chain. Control plane re-verifies byte-for-byte on ingest and on demand.
- **Filtered, paginated activity feed**: Newest-first cursor pagination with filters by event type, object type, actor, severity, and time range. Per-row `chain_valid` flag; a `/verify` endpoint reports the first broken link if any row was altered.

### Security

- **Dashboard two-factor authentication**: Protect your account with a TOTP authenticator app and/or a WebAuthn passkey or security key. A guided setup wizard walks through QR scan (or manual key entry), live-code confirmation, and saving 10 one-time recovery codes. A second-step challenge appears at login with an optional "remember this device for 30 days" (revocable per device). The Settings → Security screen manages authenticator apps, passkeys, recovery codes, and trusted devices. TOTP secret is age-encrypted at rest; recovery codes are argon2id-hashed and single-use; cloned-authenticator detection (WebAuthn sign-count); brute-force lockout; re-auth required to disable; all events in the audit log. Every session-issuing path (password login, SSO, email verify) gates through one function, so no parallel path can bypass it. Self-hosted operators must set `WPMGR_AUTH_WEBAUTHN_RPID` and `WPMGR_AUTH_WEBAUTHN_RP_ORIGINS` for passkeys to work on their own domain; the authenticator-app factor works on any origin. See [docs/features/2fa.md](./docs/features/2fa.md).
- **Core file-integrity scan**: Resumable, cursor-paginated MD5 walk diffed against the official WordPress.org Checksums API for the site's exact version + locale.
- **Three finding types**: `core_modified` (high), `core_missing` (medium), `core_unknown_injected` (high). Only flags within core paths; operator-mutated files (`wp-content/`, `wp-config.php`, cache drop-ins, etc.) are allow-listed to minimize false positives.
- **Checksum caching**: 30-day positive TTL (releases are immutable), 6-hour negative TTL, transparent `en_US` locale fallback. Repeated fleet scans never hammer the public wp.org API.
- **Finding triage**: Mark findings ignored (audited). Fetch raw file contents in-dashboard (server-gated: only stored findings, access audited) without SSH.
- **Brute-force login protection**: Three escalating tiers per sliding window: captcha gate → per-IP temporary block → global site-wide block. Known-good bypass (recent success from same IP) avoids locking out legitimate admins.
- **Three protection modes**: `disabled` (inert by default), `audit` (records/logs, never blocks), `protect` (enforces 403). Malformed config push falls back to safe defaults.
- **Early-boot WAF IP firewall**: Must-use plugin (loads before any other plugin) checks deny CIDRs at the very start of WordPress boot; allow CIDRs and private/loopback IPs always win first. DB error fails open.
- **Operator-controlled CIDR allow/deny**: IPv4 + IPv6, validated on the CP (`net.ParseCIDR`), binary `inet_pton` bitmask comparison on the agent. Spoof-resistant configurable real-IP header.
- **Lockout safety rail**: Enabling protect mode with an empty allow-list auto-adds the operator's own IP as a `/32` or `/128`.
- **Manual IP unblock**: Deletes the IP's failure rows, resetting its sliding-window counter while preserving success rows.
- **Login-page whitelabeling**: Per-site logo URL, logo link, and message. URLs scheme-validated (http/https only); message run through a narrow `wp_kses` allowlist (no `script`/`style`/`iframe`/`on*`). Safe to inject CP-controlled content into `wp-login.php`.
- **Hashed, role-scoped API keys**: `wpmgr_<prefix>_<secret>` format; only sha256 hash + prefix stored; secret shown once at creation; constant-time compare (`crypto/subtle`); per-key role flows through the full RBAC matrix.
- **Per-site access enforcement**: Every scan, login-protection, login-brand, and unblock route requires `RequireSiteAccess`. Routes resolved by global ID (get-run, ignore-finding, fetch-file) re-resolve the object's real site and call `CanAccessSite` before reading or mutating.
- **Site-user two-factor, password policy, hide-login, and hardening toggles, plus a vulnerability scanner**: per-site controls covering the WordPress site's own users (not the dashboard operator). See [docs/features/security.md](./docs/features/security.md).

### Performance

- **Zero-DB page cache fast path**: An `advanced-cache.php` drop-in serves anonymous pages as pre-gzipped HTML straight from disk (`wp-content/cache/wpmgr`) before WordPress, plugins, or the theme load. A cache HIT makes zero DB or plugin calls and streams the `.html.gz` with a 304 Not-Modified short-circuit. Every cached response carries an `x-wpmgr-cache` header.
- **Variant-aware cache key**: Pages cache separately per device (mobile UA), per logged-in role, per operator-included cookie, and per cache-varying query string (marketing params stripped). The drop-in's key algorithm is byte-identical to the PHP writer.
- **Safe cacheability gates**: Never caches non-200, admin/login/AJAX/feed/sitemap paths, password-protected posts, or any request carrying a bypass cookie (WooCommerce/EDD cart + session, logged-in, comment-author). WooCommerce cart/checkout/account pages are excluded outright.
- **WooCommerce cart-session shell caching**: Catalog pages (shop, category, home, blog) can be cached for shoppers who already have items in their cart; cart totals update live via WooCommerce's own cart-fragments mechanism. WPMgr auto-probes theme support across real storefront renders before surfacing the toggle, requiring three independent page confirmations before reporting support.
- **Automatic server fast-path install**: Enabling the cache writes the `WP_CACHE` define, installs the drop-in, and adds an Apache `.htaccess` block that serves `.gz` without PHP. nginx gets a copy-paste `try_files` snippet; built-in handling for OpenLiteSpeed and WP Engine Atomic.
- **Purge: all, per-URL, and auto on content change**: Manual purge of the whole cache or a single URL's variants, plus automatic invalidation on post/comment/product/template change that purges and re-warms the exact affected URL set (permalink, home, archives, every assigned + ancestor term).
- **Host / edge-cache purge integrations**: Every purge fires `wpmgr_purge_*:before/:after` actions so managed-host and CDN edge caches (Varnish, Kinsta, WP Engine, etc.) clear in lockstep.
- **Full-site preload warmer**: Warms every cacheable URL (home, all public post types including WooCommerce products, every non-empty term archive, author archives) across desktop and mobile UAs, via a custom self-dispatching MySQL queue with atomic claim/lock, retry/backoff, and same-host SSRF filtering.
- **Operator-tunable preload throttle**: Concurrency (1-4 workers), inter-request delay (0-10000 ms), batch size (1-500), and a per-core load-average ceiling above which a pass defers, clamped on both control plane and agent.
- **Remove Unused CSS: own engine, no headless browser**: The agent POSTs page HTML + CSS to the control plane, which computes used-CSS with its own engine (no third-party service), caches it in object storage, and dedups across pages of the same structure hash. The agent inlines the used-CSS and defers the originals. Any failure or cache-miss serves full CSS unchanged, so rendering is never blocked.
- **RUCSS safelist**: Built-in safelist of slider/lightbox/runtime-state classes plus operator-defined selectors are force-kept so JS-driven widgets survive the purge.
- **Post-compute cache reheat**: When the async RUCSS worker finishes, the control plane purges the URL and re-fetches it so the next render is a HIT with optimized HTML already cached.
- **CSS/JS minification**: Local same-site stylesheets and scripts are minified, content-addressed into a local asset cache, and tag URLs rewritten. Already-minified and external files are skipped.
- **JavaScript delay (defer / interaction / idle)**: Defers first-party JS via the `defer` attribute, or rewrites scripts to `data-src` and runs them only on first interaction or `requestIdleCallback`. Structural scripts (ld+json, speculation rules) are never delayed.
- **Font optimization**: `font-display:swap` injected into `@font-face` + Google Fonts links, optional self-hosting of Google Fonts stylesheets + woff2 files, and heuristic `rel=preload` for the first critical fonts.
- **WOFF2 font transcoding**: Self-hosted TTF, OTF, and WOFF fonts are transcoded to WOFF2 by the media-encoder service (50 to 65% smaller for TTF/OTF, 20 to 30% for WOFF). The original serves until transcoding completes; any failure falls back to the original.
- **Glyph subsetting**: When subsetting is enabled alongside WOFF2 transcoding, the encoder produces a latin-ext subset (U+0000 to 024F, U+1E00 to 1EFF) alongside the full WOFF2. Typical additional savings are 60 to 90% for body-text Latin fonts. Variable and icon fonts are detected and skipped. Per-font processing states (pending / converting / ready / subsetted / skipped / failed) are shown in a live table on the Optimize tab.
- **Third-party asset self-hosting**: Downloads cross-origin CSS/JS to the local asset cache and rewrites tags to same-origin. Best-effort: failure leaves the external URL in place.
- **Image markup optimization (CLS + lazy-load)**: Fills missing `width`/`height` from `getimagesize()` or the filename WxH suffix to prevent layout shift. Adds `loading=lazy` + `decoding=async` to below-the-fold images while keeping the first two eager; existing srcset preserved.
- **YouTube facade + Gravatar self-host**: Replaces YouTube iframes with a click-to-load thumbnail facade (no YouTube JS until clicked) and downloads/self-hosts Gravatars to the local asset cache.
- **Speculation Rules prefetch**: Injects a `<script type=speculationrules>` block that prefetches same-origin links (eagerness moderate) while excluding wp-admin/login, cart/checkout/logout, query-string, and nofollow URLs for near-instant navigations.
- **CDN URL rewrite with encrypted credentials**: Rewrites same-site static-asset URLs to a configured CDN host (all / css-js-font / image groups), runs last so it catches minified + self-hosted URLs, and purges the CDN edge (Cloudflare/Bunny/KeyCDN) on purge using age-encrypted credentials the control plane decrypts only in-process.
- **WordPress bloat removal**: Per-toggle de-bloat (block-library CSS, dashicons, emojis, jquery-migrate, XML-RPC, RSS feeds, oEmbeds, Heartbeat throttling, post-revision cap).
- **Optimize-on-MISS, cache the optimized bytes**: The full optimization pipeline runs inside the cache-writer's MISS path before gzip + write. Every transform is config-gated, wrapped so a failure degrades to a no-op, and skipped for logged-in/personalized responses.
- **Cache hit-ratio history**: The drop-in tallies HIT/MISS to per-hour counter files with zero DB work on a cache hit; the heartbeat drains completed buckets to the control plane, which appends to a Postgres time-series (120-day GC).
- **Hit-ratio trend chart (7/30/90-day)**: Dashboard renders a cache hit-percentage trend with a 7/30/90-day window toggle and an average-ratio badge.
- **Live cache telemetry + verify card**: Agent reports cached-page count, cache size, last purge, and live preload progress over SSE, plus the real install state (web server, drop-in present, `WP_CACHE` define, `.htaccess` managed) so the verify card reflects on-host truth.
- **Page-cache conflict + multi-currency/i18n detection**: Detects other active cache/optimization plugins (reported to the dashboard, never deactivated) and auto-derives the cache-varying cookies/queries that multi-language and multi-currency plugins need so language/currency variants cache separately.
- **Portfolio bulk actions + presets**: Purge the cache across many sites at once, or apply a safe / balanced / aggressive optimization preset to a whole group in one run (each preset spreads a small toggle set without clobbering per-site lists).
- **Page-source footprint marker**: Every cached response carries an HTML-comment footprint (with timestamp, and an `(optimized)` suffix when transformed) so operators can confirm via view-source that WPMgr wrote and optimized the page.

### Redis object cache

- **Per-site persistent object cache**: Full WordPress cache API surface (`add`/`get`/`set`/`replace`/`delete`, multi-key variants, `flush_group`, `flush_runtime`, `wp_cache_supports`, `switch_to_blog` for multisite). Accelerates logged-in users, admin screens, WooCommerce checkout, and every database round-trip the page cache cannot serve. See [docs/features/object-cache.md](./docs/features/object-cache.md).
- **Connect test before enable**: The agent dials the candidate config without persisting it, probes phpredis capabilities (igbinary serializer, lzf/lz4/zstd compression, TLS), reads the eviction policy, and returns a structured result. Enable is blocked until a test passes. Codec mismatch is caught up front and rejected with a clear message.
- **Encrypted credentials**: The control plane age-encrypts (X25519) the connection credential and delivers it via the signed command channel to a 0600 file on the site. Plaintext never appears in GET responses, logs, SSE payloads, test results, heartbeats, or backups.
- **Two-level graceful degradation**: A boot failure swaps in a pure in-memory array cache so the site never goes down. Mid-request Redis errors become cache misses with one reconnect attempt, then degrade for the rest of that request. A persisted reconnect cool-down (15 s, doubling to 5 min) stops a down Redis from being re-dialed on every request.
- **Fully self-contained drop-in**: One generated file contains the complete engine, connection layer, and config loader, with no runtime dependence on the plugin folder name or location. The agent invalidates the drop-in's opcache on every version change; an outdated stub self-heals on the next heartbeat.
- **Configuration drift detection**: The agent reports the fingerprint of the configuration file it is actually reading; the dashboard flags divergence from saved settings.
- **Live status + analytics**: Dashboard shows connected / degraded / down with a 10 s SSE-debounced update. Charts track hit ratio, used memory, average command latency, and ops/sec over the last 7 days with a 90-day daily downsample. A per-site-prefixed flush never touches another site's keys on a shared Redis.
- **`x-wpmgr-object-cache` debug header**: When the "Debug response header" setting is on, front-end responses carry `x-wpmgr-object-cache: state=connected hits=42 misses=3 reads=45 writes=12 ms=1.4`. Administrators always receive it while logged in; visitor-side is opt-in. Pages served by the page cache do not carry the header because WordPress does not run on those responses.

### Real user monitoring

- **Web-vitals collection**: A tiny first-party collector script, enqueued on every front-end request via WordPress's own script-loading API (independent of whether page caching is on, off, or third-party), beacons LCP, INP, CLS, FCP, and TTFB directly to the control plane. Versioned collector URL forces edge-cache revalidation on every agent update.
- **CrUX-style p75 dashboard**: p75 per metric with a PageSpeed Insights-style good/needs-improvement/poor distribution bar, a 28-day trend chart with threshold lines drawn on, and per-URL and per-device breakdowns. Live updates stream over SSE.
- **Postgres histogram rollups**: Hourly and daily, with ClickHouse available as an opt-in scale backend via the same boot-selection pattern as the uptime store.
- **Privacy-first**: Off by default. No cookies, no cross-site identifiers. Page paths stored with query string stripped. Visitor IP used only transiently for coarse country lookup, then discarded. On a self-hosted control plane all RUM data stays on the operator's own infrastructure. See [docs/features/monitoring.md](./docs/features/monitoring.md).

### Per-site email

- **Multi-provider sending**: Amazon SES, SendGrid, Mailgun, Postmark, or any generic SMTP server, configured per-site or inherited from an org-wide default. Org default propagates automatically to every inheriting connected site in parallel on save.
- **Named connections with failover**: Define multiple named connections per site. Map FROM addresses to a specific connection; a fallback connection retries automatically on primary failure. The email log records which connection handled each send.
- **Central cross-site email log**: Every outgoing email logged with full detail: to, from, subject, headers, status, provider response, retry count, and attachment names/sizes. Paginated with free-text and column-scoped search, status and date filters, per-row detail with previous/next navigation, single and bulk resend, and CSV/JSON export. Email bodies not stored by default; opt-in per tenant. Log entries prune after 14 days.
- **HTML preview sandbox**: A logged email's body renders inside a locked-down sandboxed iframe (no scripts, no same-origin) with a strict CSP. Remote images and tracking pixels blocked by default with a per-message opt-in.
- **Bounce and complaint suppression**: Connect a provider webhook (SES SNS, SendGrid, Mailgun, Postmark) and WPMgr automatically suppresses hard-bounced and complained addresses fleet-wide. Manual add/remove supported. Scoped per-site or org-wide.
- **Fleet deliverability dashboard**: Cross-site sent/failed/bounced/complained counts in one view. Per-site deliverability charts on each site's Email tab. Live SSE updates.
- **Failure alerts + digest**: Opt-in alerts sent to operator-chosen recipients when sends start failing (throttled; configurable 15 min to 24 h). A separate weekly or monthly deliverability digest summarises sent, failed, and bounced counts per site with a top-failures list.
- **Credentials encrypted at rest**: age (X25519); a `secret_set` flag is returned in place of the plaintext. A test-send button confirms delivery before saving.

### Media

- **Dedicated cloud encoder**: Separate `media-encoder` container decodes from magic bytes (never trusted MIME) and re-encodes to AVIF (q50/speed8), WebP (q80), or re-optimized original. Animated GIFs → animated WebP.
- **Runs by default, disable if unneeded**: Part of the base self-host stack (`docker compose up -d` starts it, no profile needed); disable on constrained hosts with `--scale media-encoder=0`. Zero weight or native-library CVE surface on the core API either way, since encoding never runs there.
- **Per-image optimize and full reversible restore**: Originals archived on-site (`.wpmgr-original.*` rename); restore reverts every variant. A separate admin-gated delete-originals reclaims disk.
- **No image bytes on the control plane**: Source and optimized bytes move agent-to-storage and encoder-to-storage via presigned URLs only. CP stores metadata rows only.
- **`.htaccess` Accept-header fallback**: Serves the modern format only when the browser's `Accept` header advertises support; legacy twin is served otherwise. `Vary: Accept` for CDN correctness. nginx map equivalent emitted for non-Apache hosts.
- **Auto-optimize on upload**: Per-site opt-in hooks `wp_generate_attachment_metadata`, debounces (~25 s) batch pushes to the CP, and runs the existing optimize pipeline. Four stacked guards prevent self-optimization loops.
- **Library sync + savings metrics**: Total assets, optimized/pending/failed/unsupported counts, total bytes saved across all variants including thumbnails.
- **Serialization-safe DB URL rewrite**: Rewrites old-to-new media URLs across `post_content` and postmeta in a way safe for PHP-serialized data; recorded for reversible restore.
- **Resilient per-variant encoding**: Per-variant failures never fail siblings; 50 MB / 100 megapixel source limits; 60 s per-encode timeout; size + magic-byte verification of downloaded outputs before write.
- **Unused image library scan**: Walks the entire attachment library and classifies every image as in-use or orphaned by building an exhaustive cross-content reference index. No image is flagged unused unless it appears nowhere.
- **"Where is this image used" attribution**: For every referenced image, surfaces each in-use location: post/page content, excerpts, revisions, featured images, gallery/block IDs, page-builder data (Elementor, Beaver, Bricks, Divi, WPBakery, Oxygen, Breakdance, ACF), SEO/OpenGraph meta (Yoast, Rank Math, SEOPress), theme mods, widgets, nav menus, term meta, and user/avatar meta, with a deep-link to edit the referencing object.
- **Conservative "ambiguous = in use" design**: Casts the widest net (revisions, builder JSON, serialized arrays, sub-size derivation, protocol-agnostic URL matching) and aborts the whole scan if the uploads directory is unresolvable rather than risk a false orphan.
- **Reversible server-backed quarantine**: Instead of deleting, isolate moves an image's original plus every generated sub-size (and optimizer-produced variants) into a web-blocked `wp-content/wpmgr-quarantine/` store with a JSON manifest. The attachment post stays in the library so the action is fully reversible.
- **One-click restore from quarantine**: Any isolated set can be moved back to its exact original upload paths from the manifest, with path-containment checks ensuring files only ever land back inside the uploads directory.
- **Confirmation-gated permanent delete**: Permanent removal of quarantined files plus their attachment posts requires an explicit `DELETE` confirmation token, enforced independently at both the control plane and the agent (constant-time compare).
- **Role-gated, audited cleaner endpoints**: Scan and quarantine-list are viewer-level reads; isolate/restore/delete require operator-level write permission. Every action is recorded to the audit log and emitted over the live site-event bus.

### Site tools

- **File Manager**: Browse, edit, upload, download, and manage files on any managed site from the dashboard, no SFTP required. Read access and write access are separate per-site toggles, both off by default, restricted to owner and admin. Write operations block PHP and executable content and refuse protected roots (wp-admin, wp-includes, core). Zip and download any path; extract a zip with zip-slip and zip-bomb protection. Search by name or content. Every edit auto-saves an encrypted prior version with one-click restore from a per-file history panel. Every read, write, delete, upload, extraction, and denial is written to the operator audit log, filterable by the File manager action group. See [docs/features/file-manager.md](./docs/features/file-manager.md).
- **Database Cleaner (scan / preview / clean)**: Read-only scan estimates then bounded, batched cleanup across 14 categories: post revisions, auto-drafts, trashed posts, spam/trashed comments, expired transients, orphaned post/comment meta, orphaned term relationships, oEmbed cache, duplicate postmeta, Action Scheduler completed/failed, and OPTIMIZE TABLE on non-InnoDB tables. Per-category `{rows_deleted, bytes_freed, state}` results stream back over signed progress pushes. Cleanup results are stored server-side and restored on page load and stream reconnect so a running cleanup survives a refresh.
- **Database Cleaner safety**: Each task deletes only the rows it targets; SELECT→DELETE batching in 2000-row chunks with a hard iteration cap, OPTIMIZE TABLE on a 12 h cooldown, and operator-level permission gate. WP core options/cron/tables and installed-plugin-attributable items are skipped.
- **Per-table inventory + maintenance actions**: Every table listed with row count, size, storage engine, overhead, and a "Belongs to" label (WordPress core / active plugin or theme / orphan). Per-table actions: optimize, repair, analyze, convert to InnoDB, empty, delete. Each is gated by a typed confirmation. Orphaned options and cron events classified by corpus confidence level (exact / prefix / heuristic / unknown) against a seed of ~120 high-orphan-risk plugins.
- **Database Snapshots**: One-click local DB snapshot before a risky change and one-click revert. Dumps to gzipped SQL under `wp-content/wpmgr-snapshots/db/` (web-hardened, never uploaded), with create / list / revert / delete actions and a configurable retention cap (default 5, hard max 20, oldest pruned first).
- **Snapshot revert safety**: Revert is a destructive whole-DB overwrite gated behind an exact `REVERT` confirmation token (constant-time compare). Before importing, auto-captures a pre-revert safety snapshot, then replays the dump into `tmp`-prefixed tables and atomically swaps each over the live table so WordPress stays readable throughout. A failed import leaves the live DB untouched.
- **Search & Replace**: Operator-driven literal find-and-replace across the whole database (or a chosen table allowlist) for URL/domain migrations and string fixes. Mandatory dry-run preview returns tables-scanned / rows-matched counts before any write, then reports rows-changed on apply.
- **Search & Replace safety**: Serialization-safe walker (PHP-serialized blobs are unserialized, rewritten, and re-serialized so `s:NN:` length prefixes are recomputed, never a naive `str_replace`). Minimum 3-char search guard; community migration table denylist; binary/blob columns and `posts.guid` always skipped; all values mysqli-escaped; an advisory `X-Backup-Warning` header fires on a live run with no recent backup.

### Clients for agencies

- **Client grouping**: Create, edit, and delete clients (name, company, contact email, phone, notes, brand color, logo URL). Assign one or many sites via bulk "Set client" on the fleet view; filter by client; jump to a per-client detail page. Clients are tenant-isolated with RLS; a database-level composite constraint makes cross-tenant assignment impossible. See [docs/features/clients.md](./docs/features/clients.md).
- **White-label reports**: Monthly or weekly schedule per client, with a "Generate now" button for any custom period up to 92 days. Aggregates uptime, backups, updates, Core Web Vitals p75, and email deliverability. Per-section on/off toggles; custom intro and closing text. Delivered as branded HTML email, print-optimized page, and server-side vector-chart PDF. The client's brand color and logo appear throughout; the "powered by" footer is removable on any plan.
- **Read-only client portal**: Invite client users by email from the Portal access tab. Existing users added immediately; new addresses receive a tokenized invite (7-day expiry) with a copyable fallback link. Clients land at `/portal` with their logo and brand color applied, a softened-wording sites overview ("Monitoring active", "Needs attention"), per-site uptime and incident history, backup inventory, applied updates, Core Web Vitals field data, and completed report downloads. The `client` role ranks below viewer with zero permissions; access is revoked instantly on removal, client archive, or client delete.
- **Live portal summary dashboard**: The portal landing shows a status banner, five animated headline counters (sites monitored, average uptime, backups, updates applied, speed rating), a month-at-a-glance section with fleet uptime trend and Core Web Vitals distribution, site cards with 30-day sparklines, and a day-grouped "Recent work" timeline. All data is strictly scoped to the client's own sites.

### Team & access

- **Multi-tenant organizations**: Each resource scoped to an org. Isolation enforced by Postgres Row-Level Security (`app.tenant_id` GUC) with the app role `NOSUPERUSER NOBYPASSRLS`.
- **Four-role RBAC**: `owner > admin > operator > viewer` with a discrete permission matrix. Privilege ceiling prevents granting a role higher than your own.
- **Per-site collaborator sharing**: Outside users (no org membership) scoped to one site, enforced by both Gin middleware and RESTRICTIVE RLS. Blocked from all org-level actions.
- **Tokenized invitations**: Single-use, 7-day-expiry SHA-256-hashed tokens bound to the invited email. Existing accounts must re-authenticate; a leaked link alone cannot log anyone in.
- **Email/password auth with a provisioning claim on first run**: argon2id hashing; claiming the first, owner account requires the self-host provisioning secret (`WPMGR_BOOTSTRAP_CLAIM_SECRET`), presented as a request header — the dashboard Sign Up form alone cannot create it. Once the install has an owner, anyone can self-register, but new accounts are created pending and gain access only after clicking the emailed verification link. Login responses never reveal whether an email exists. See [Quickstart](#quickstart-self-host) for claiming a fresh install.
- **Self-serve password reset**: Public forgot-password + reset endpoints issue a 30-minute single-use SHA-256-hashed token. Both endpoints are enumeration-safe (always-200, timing-equalized).
- **Change-password with session invalidation**: Verifies the current password then stamps `password_changed_at`, which logs out all other active sessions on their next request while keeping the acting session alive.
- **UI-configured per-instance SMTP**: Owners configure the instance SMTP relay (host/port/TLS/credentials/From) in Settings. The password is age-encrypted at rest, masked on read. A test-send button confirms delivery before saving.
- **OIDC / SSO**: OpenID Connect relying party with PKCE, state, and nonce. Account linking only when the IdP asserts the email is verified. Ships with a Dex container so SSO works locally.
- **Superadmin console**: Instance-level cross-tenant admin area (`/api/v1/admin`) gated by `is_superadmin`, granted only via the `WPMGR_SUPERADMIN_EMAILS` env allowlist and boot seeder, never API-settable. Lists/searches users cross-tenant, disables/deletes users with orphaned-org cleanup, resends verification, and shows instance stats. Intended for operators running multi-tenant self-hosted instances.
- **Tamper-evident audit log**: SHA-256 hash-chained, append-only (DB role denied `UPDATE`/`DELETE`). Covers logins, member/role changes, API-key changes, site lifecycle, sharing, autologin, media consent, updates. `/audit/verify` reports the first broken link. See [docs/features/audit-log.md](./docs/features/audit-log.md).
- **API keys**: Role-scoped, revocable, audited. Carries the same RBAC permission matrix as a user session.

---

## Architecture

```text
apps/api    Go 1.26 + Gin control plane (modular monolith)
apps/web    React 19 + TypeScript + Vite + TanStack dashboard
apps/agent  PHP 8.1+ WordPress agent plugin (MIT; GPLv2+ on the wp.org build)
```

**Data:** Postgres (primary + RLS) · Redis (sessions, cache, dedup) · S3-compatible object storage (backups, media) · on-disk page cache (`wp-content/cache/wpmgr`, pre-gzipped `.html.gz`) · ClickHouse (optional, uptime + RUM time-series)

**Agent ↔ CP auth:** Ed25519 signed requests (canonical `METHOD\nPATH\nTIMESTAMP\nNONCE\nsha256(body)`) with per-request nonce + timestamp for anti-replay. CP→agent commands are short-lived Ed25519-signed JWTs scoped to one site and one operation.

See [docs/architecture.md](./docs/architecture.md) for a full system diagram.

---

## Quickstart (self-host)

### One-click install (recommended)

No repo clone needed. This single command downloads every file the stack needs, generates all secrets (session, agent signing, age), and prints the exact command to start WPMgr:

```bash
curl -fsSL https://raw.githubusercontent.com/mosamlife/wpmgr/main/scripts/quickstart-selfhost.sh | bash
```

Options: `--hostname=https://wpmgr.example.com` (sets `WPMGR_PUBLIC_BASE_URL` non-interactively), `--version=vX.Y.Z` (pin a release tag, default `:latest`), `--dir=PATH` (default `./wpmgr`), `--force` (re-download files).

Host requirements: Docker 24+ with the Compose plugin, `curl` or `wget`, and `openssl`.

The script prints the exact command to run next, using the pull-only overlay (prebuilt images, no local build):

```bash
cd wpmgr
docker compose -f infra/docker-compose.yml -f infra/docker-compose.prod.yml up -d
curl localhost:8081/healthz   # {"status":"ok"}
```

The script also generates a provisioning secret (`WPMGR_BOOTSTRAP_CLAIM_SECRET`) and prints the exact command to claim ownership of the install — the dashboard Sign Up form cannot create the first account by itself, since the claim only travels as a request header. That command reads the secret out of `.env` itself, so you never look it up or paste it; run the printed command once, then open `http://localhost:8088` in your browser and log in. Full detail, including the upgrade case: [docs/install.md](./docs/install.md#first-run-notes).

`WPMGR_S3_ENDPOINT` in the generated `.env` must be reachable by your WordPress hosts, not just the control plane. The default (`http://seaweedfs:8333`) only resolves inside Docker; for remote WordPress sites, point it at a tunnel or public URL.

See [docs/requirements.md](./docs/requirements.md) for CPU, RAM, and disk sizing by fleet size before you provision the host.

### Build from source (clone path)

If you have cloned the repo, the bundled compose brings up the full stack (control plane, dashboard, Postgres, Redis, object storage, and the media encoder for screenshots and the Media Optimizer), building all three images from source:

```bash
./scripts/init-env.sh   # or: make quickstart — writes .env and generates every required secret
docker compose -f infra/docker-compose.yml up -d
curl localhost:8081/healthz   # {"status":"ok"}   (default WPMGR_API_PORT=8081)
```

The script prints the exact command to claim ownership of the install, reading the provisioning secret (`WPMGR_BOOTSTRAP_CLAIM_SECRET`) it generated straight out of `.env` — you never look it up or paste it. Run that command once; the dashboard Sign Up form cannot create the first account by itself. Then open `http://localhost:8088` in your browser (the default `WPMGR_WEB_PORT`) and log in. Once the install has an owner, anyone can self-register and gains access only after verifying their email. Full detail, including the upgrade case: [docs/install.md](./docs/install.md#first-run-notes).

The media encoder runs headless Chromium for screenshots, so it adds some RAM/CPU over the api/web images. On a constrained host you can skip it without editing the compose file:

```bash
docker compose -f infra/docker-compose.yml up -d --scale media-encoder=0
```

Site screenshot cards then fall back to favicon/monogram, the Media Optimizer tab is unavailable, and WOFF2 font transcoding stops: the screenshot, image-encode, and font-transcode workers all run only in that image. Everything else, including backups, restores, updates, uptime, security, and the DB cleaner, is unaffected, because the control plane never encodes anything itself.

### Prebuilt container images

The control plane, dashboard, and media encoder are published on GitHub Container Registry, for wiring into your own compose, Kubernetes, or Swarm setup. The api and web images are multi-arch (`linux/amd64` + `linux/arm64`); the media encoder is `linux/amd64` only, because its image codec library ships prebuilt static libraries for that architecture alone:

<!-- wpmgr-install-pins:start (required pins; scripts/check-version-surfaces.sh keeps them current) -->

```bash
docker pull ghcr.io/mosamlife/wpmgr-api:v0.61.146
docker pull ghcr.io/mosamlife/wpmgr-web:v0.61.146
docker pull ghcr.io/mosamlife/wpmgr-media-encoder:v0.61.146
```

Or bring up the whole stack from the published images (no local build) with the pull-only Compose overlay:

```bash
export WPMGR_VERSION=v0.61.146   # omit to track :latest
docker compose -f infra/docker-compose.yml -f infra/docker-compose.prod.yml up -d
```

<!-- wpmgr-install-pins:end -->

Migrations apply automatically on boot. The `validate-env` command (`make validate-env`) checks your configuration and prints every problem at once before the stack starts.

Full install guide, env reference, and production hardening: [docs/install.md](./docs/install.md).

---

## Install the Agent

**From the WordPress admin (recommended).** **Plugins → Add New** → search for **Fleet Agent Site Manager** → **Install Now → Activate**. That is the plugin's listing name in the [WordPress.org plugin directory](https://wordpress.org/plugins/fleet-agent-site-manager/).

**From GitHub.** Download `wpmgr-agent.zip` from the [Releases page](https://github.com/mosamlife/wpmgr/releases), then **Plugins → Add New → Upload Plugin** → choose the zip → **Install Now → Activate**.

**With Composer.** The directory listing is mirrored by both Composer repositories that serve WordPress.org, so pick whichever your project already declares:

| Your project declares | Require this |
| --- | --- |
| `https://wpackagist.org` | `wpackagist-plugin/fleet-agent-site-manager` |
| `https://repo.wp-packages.org` (current Bedrock) | `wp-plugin/fleet-agent-site-manager` |

Bedrock already configures its repository and its installer paths, so add only the `require` entry. For a plain Composer-managed WordPress:

```json
{
  "repositories": [
    {
      "type": "composer",
      "url": "https://wpackagist.org",
      "only": ["wpackagist-plugin/*", "wpackagist-theme/*"]
    }
  ],
  "require": {
    "composer/installers": "^2.0",
    "wpackagist-plugin/fleet-agent-site-manager": "*"
  },
  "extra": {
    "installer-paths": {
      "wp-content/plugins/{$name}/": ["type:wordpress-plugin"]
    }
  }
}
```

Do not paste that `extra` block into a Bedrock project: `installer-paths` is a single key, so it replaces rather than merges, and Bedrock installs to `web/app/plugins/` instead. Use `*` rather than `^0.61`, because a caret on a 0.x version pins the minor and would leave you on 0.61.x forever once 0.62 is tagged.

Then, whichever path you used: open the plugin's top-level admin menu (**WPMgr Agent** on the GitHub build, **Fleet Agent Site Manager** on the directory build), set the **Control-plane URL** field and click **Save URL**, then paste the dashboard's **Pairing code** into the **Enroll** form and click **Enroll**.

The two builds are the same code; the difference that matters is how they update. The GitHub build self-updates through your control plane's signed update channel. The directory build ships without the self-updater and updates from the site's own Plugins screen, or through `composer update` on a Composer-managed site.

The agent requires PHP 8.1+ and WordPress 6.2+. See [docs/agent.md](./docs/agent.md) for WP-CLI install and configuration options.

---

## Roadmap

The following are accepted architectural decisions with no implementation yet:

- **Fleet-wide backup browser**: Browse and filter snapshots across all sites from one view (route placeholder present, not built).
- **Backup download**: No presigned-download endpoint or web UI; restore is the current recovery path.
- **Scheduled update runs**: `scheduled_at` is stored and accepted; no deferred dispatcher yet (tasks enqueue immediately on create).
- **CAPTCHA challenge on login block**: Login protection currently serves a static 403; no challenge/solve flow built.
- **Plugin/theme content malware scanning**: The scan engine already verifies plugin and theme files against wordpress.org checksums (flagging modified and unknown files alongside core); what is missing is signature or heuristic detection of malicious content itself.
- **Automatic restore rollback UI**: The `.wpmgr-old-files-<id>/` rollback directory is preserved; no operator-initiated rollback endpoint exposed.
- **Redis Sentinel / Cluster**: Object cache v1 supports single instance or unix socket with TLS; the config schema reserves fields for both topologies.
- **Helm chart / Terraform provider**: Stubs exist under `infra/`; not implemented.
- **AI features**: `apps/api/internal/ai/` contains only a `.gitkeep` placeholder.

---

## Repository layout

```text
apps/
  api/      Go control plane
  web/      React dashboard
  agent/    WordPress agent plugin
packages/
  openapi-client/   generated TypeScript API client
infra/
  docker-compose.yml
  Dockerfile.api · Dockerfile.web · Dockerfile.media-encoder
  postgres/ · seaweedfs/ · nginx/ · dex/ · grafana/
docs/
  install.md · requirements.md · agent.md · architecture.md · contributing.md · security.md · api.md · adr/
```

---

## Development

```bash
cp .env.example .env
docker compose -f infra/docker-compose.yml up -d   # data plane

# API
cd apps/api && go run ./cmd/wpmgr

# Web
cd apps/web && pnpm dev

# Regenerate OpenAPI client after schema changes
go generate ./internal/api/gen/...
pnpm -C packages/openapi-client generate
```

See [docs/contributing.md](./docs/contributing.md) for the full dev setup, PR checklist, and ADR process.

---

## License

| Component | License |
|---|---|
| Control plane + dashboard (`apps/api`, `apps/web`) | [AGPL-3.0-only](./LICENSE) |
| WordPress agent plugin (`apps/agent`) | [MIT](./LICENSE-AGENT) (the WordPress.org build declares GPLv2 or later) |

---

## Links

- [Install (self-host)](./docs/install.md)
- [System requirements](./docs/requirements.md)
- [WordPress agent](./docs/agent.md)
- [Architecture](./docs/architecture.md)
- [API reference](./docs/api.md)
- [Contributing](./docs/contributing.md)
- [Security policy](./docs/security.md)
- [Architecture decisions](./docs/adr/)
- [GitHub Releases](https://github.com/mosamlife/wpmgr/releases)
