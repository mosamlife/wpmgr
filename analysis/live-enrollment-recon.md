# Live Enrollment + Connection-State Recon (read-only)

Scope: map what exists today for (a) live enrollment over SSE and (b) a connection-state lifecycle. All citations are `file:line` against the repo at audit time. "GAP:" marks something that does not exist.

---

## 1. SSE bus

**Architecture: in-process, per-resource hubs. There is NO shared/global SSE bus, NO Redis pubsub, NO River-backed fan-out. Two independent copies of the same pattern exist (backup, update).**

- **Files**
  - Backup hub: `apps/api/internal/backup/hub.go` (whole file). Handler: `apps/api/internal/backup/handler.go:240-361` (`events`, `writeBackupEvent`).
  - Update hub: `apps/api/internal/update/hub.go` (whole file). Handler: `apps/api/internal/update/handler.go:124-210` (`events`, `writeEvent`).
  - React clients: `apps/web/src/features/backups/use-backup-stream.ts` (whole file), `apps/web/src/features/updates/use-updates.ts:170-260` (`useRunEventStream`).

- **Pub/sub abstraction** — a plain Go struct `Hub` with `sync.Mutex` + `map[uuid.UUID]map[*subscription]struct{}`, each subscription a buffered channel `chan Event` of size 64 (`backup/hub.go:30-48`, `update/hub.go:32-49`). `Publish` is non-blocking: a full subscriber buffer **drops** the event (`backup/hub.go:74-90`, `update/hub.go:76-92`). Comment explicitly states the dropped-event reconciliation strategy is "re-read authoritative state from the DB on connect" (`backup/hub.go:26-29`).

- **Topic scoping** — keyed strictly by **resource id**, not tenant/user/site: backup hub keys on `snapshot_id` (`backup/hub.go:47`), update hub keys on `run_id` (`update/hub.go:49`). Authorization happens in the HTTP handler BEFORE subscribe, not in the bus: backup `events` verifies the snapshot exists in-tenant + `canReadSite` (`backup/handler.go:259-283`); update `events` requires org scope + tenant match (`update/handler.go:42-46, 128-146`). The bus itself has no tenant awareness.

- **Wire envelope** — named SSE events, JSON `data:` line, double-newline framed.
  - Backup: `event: progress\ndata: {json}\n\n` (`backup/handler.go:353-361`). Payload `BackupEvent{snapshot_id, phase, phase_detail, status, ts}` (`backup/hub.go:15-21`). Zod mirror at `use-backup-stream.ts:30-65`.
  - Update: `event: task\ndata: {json}\n\n` (`update/handler.go:202-210`). Payload `Event{run_id, task_id, site_id, target_type, target_slug, status, from_version, to_version, detail, run_status}` (`update/hub.go:9-24`).
  - Keep-alive: a bare comment line `:\n\n` every **15 s** (`sseHeartbeat` const, `backup/handler.go:20-21,336-339`; `update/handler.go:19-20,184-187`). Headers set on connect: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`, `X-Accel-Buffering: no` (`backup/handler.go:295-298`).

- **Initial state on connect** — both handlers emit a current-state snapshot immediately after subscribe so a late subscriber is complete: backup emits one synthetic frame from persisted `progress` JSONB (`backup/handler.go:301-319`, `snapshotToEvent`/`initialFrameToSend` `:363-447`), with a "suppress stale terminal echo > 60 s old" rule. Update emits one frame per task (`update/handler.go:164-170`).

- **Server lifetime** — backup stream stays open until client disconnect OR a **30-min** `sseMaxLifetime` safety timer (`backup/handler.go:24-31, 327-335`); it deliberately does NOT close on terminal status (restore events overlay a completed snapshot, `:240-258`). Update stream closes when `run.Status == RunCompleted` (`update/handler.go:172-174, 194-196`).

- **React client subscribe** — `new EventSource(url, { withCredentials: true })` against the same-origin Vite proxy so the session cookie flows (`use-backup-stream.ts:133-136`). Listens via `addEventListener("progress", …)` (named, not `onmessage`) (`:210`). Validates each frame with Zod, then `queryClient.setQueryData` patches the TanStack cache (`:166-199`).

- **Reconnection / backoff** — relies on the browser's **native EventSource auto-reconnect**; there is NO custom backoff. The app layers a failure counter: on `onerror` it increments `failures`, drops the live flag, and after `MAX_FAILURES = 6` calls `teardown()` and falls back to short-polling (`use-backup-stream.ts:98-99, 212-230`). Update client gives up after the FIRST hard error and switches to a 2 s poll (`use-updates.ts:84-85, 239-260`, `RunStreamState`). The poll fallback is a TanStack `refetchInterval` on the detail query (`use-backups.ts`, `use-updates.ts:83-86`).

- **Event IDs / replay** — **GAP: none.** No `id:` line is written, no `Last-Event-ID` is read, no `?since=` cursor exists. Replay-after-reconnect is impossible; correctness depends on the on-connect DB re-read + the polling fallback. (Browser native reconnect will re-establish but the server replays only the single current-state frame, not missed deltas.)

- **Where SSE is already used** — exactly two resources: backup snapshot progress (incl. restore overlaid on the same channel) and bulk update runs. The security scan panel references `sse` (`apps/web/src/features/security/scan-panel.tsx`) but scan progress is delivered via `refetchInterval` polling (`use-scan.ts`), not a dedicated SSE endpoint. **GAP: no SSE for sites/enrollment, diagnostics, uptime, or activity.**

---

## 2. Enrollment / pairing

**A complete pairing-code + public `/enroll` flow exists. It is one-shot, single-use, hashed-at-rest, TTL-bounded. There is no live signal on enroll completion.**

- **Owning package/handler** — `apps/api/internal/site` (service `service.go`, repo `repo.go`, handler `handler.go`, code logic `pairingcode.go`). SQL at `apps/api/db/query/pairing_codes.sql` + the enroll-specific site queries in `apps/api/db/query/sites.sql:61-87`.

- **Pairing code minting** — operator-initiated `POST /api/v1/sites/pairing-codes` (`site/handler.go:224-268`, `createPairingCode`). Code is minted in `Service.CreatePairingCode` (`service.go:~95-109`) via `generatePairingCode()` = 20 random bytes → base32 no-pad ≈160 bits (`pairingcode.go:14-65`).
  - **Storage**: Postgres table `pairing_codes` (`schema.sql:255-285`). Only the **sha256 hex hash** is stored (`code_hash`), never plaintext (`pairingcode.go:67-72`; insert `pairing_codes.sql:1-5`). `code_hash` has a global UNIQUE index so `/enroll` can resolve a code to its tenant before any tenant scope exists (`schema.sql:268-270`).
  - **TTL**: `pairingCodeTTL = 15 * time.Minute` (`pairingcode.go:19-20`); `expires_at` set at mint.
  - **Single-use + abuse guard**: `consumed_at` (one-shot), `attempts` capped at `pairingCodeMaxAttempts = 10` (`pairingcode.go:23-25`). Consume is conditional `WHERE consumed_at IS NULL` (`pairing_codes.sql:13-17`), enforced in a race-safe tx (`repo.go:324-332`).
  - Plaintext returned to operator exactly once in the create response (`handler.go:256-267`), shown once in the UI (section 5).

- **`/enroll` request/response** — public, unauthenticated, mounted on the root engine (NOT under `/api/v1`): `POST /enroll` (`handler.go:108-113` `RegisterPublic`; wired `server/server.go:159-160`).
  - Request body (`handler.go:300-308`, validated `service.go:111-120`): `{pairing_code, site_url (http/https), agent_public_key (base64 Ed25519), name, wp_version, php_version, tags}`.
  - Response `gen.EnrollResponse{site_id, tenant_id, control_plane_public_key}` (`handler.go:330-334`). The CP public key is handed to the agent here so it can later verify CP→agent command JWTs.

- **What gets sealed into the `sites` row on success** — `Service.Enroll` → `repo.Enroll` (`service.go:122-148`, `repo.go:252-336`). Tenant is derived ENTIRELY from the pairing code, never the caller (`repo.go:255-256, 307-308`). Two paths:
  - New site → `CreateSiteForEnroll` (`sites.sql:69-73`): sets `status='active'`, `agent_public_key`, `enrolled_at=now()`, `last_seen_at=now()`, `health_status='healthy'`, name/tags/versions.
  - Existing same-URL site → `AttachAgentToSite` (re-enroll, `sites.sql:75-87`): **rotates** `agent_public_key`, resets `status='active'`, `enrolled_at=now()`, `last_seen_at=now()`, `health_status='healthy'`. Idempotency keyed on `(tenant_id, url)` (`repo.go:291-322`).
  - Code consumed exactly once at the end of the same tx (`repo.go:324-332`).
  - Audit event `site.enrolled` recorded (`handler.go:329`, `audit.go:45`).

- **RLS for the pre-tenant path** — enroll runs under `InEnrollTx` which sets the `app.enroll='on'` GUC; policies `sites_enroll` / `pairing_codes_enroll` permit the by-hash + create/attach work (`schema.sql:103-117, 278-285`).

- **GAP (live):** enrollment is a one-shot HTTP exchange with no push/SSE notification to the operator's browser. The only client-side reaction is `usePairingCode().onSuccess` invalidating the sites list at **code-creation** time (`use-sites.ts:106-109`), which predates the agent actually enrolling. Nothing tells the dashboard when the agent later completes `/enroll`.

---

## 3. Agent handshake (Ed25519 / JWT)

Two distinct cryptographic directions exist; do not conflate them.

**(A) Agent → CP: Ed25519 signed-request scheme (NOT JWT).**
- Canonical message + verify: `apps/api/internal/agent/signature.go:46-85`. The agent signs `METHOD\nPATH\nTIMESTAMP\nNONCE\nhex(sha256(body))` with its Ed25519 secret key.
- Headers (agent must mirror exactly): `X-WPMgr-Agent-Key` (base64 pubkey), `X-WPMgr-Timestamp` (unix s), `X-WPMgr-Nonce` (jti), `X-WPMgr-Signature` (`signature.go:16-28`).
- Middleware: `agent/auth.go:73-144` (`Authenticate`). Checks header presence, nonce length 8..256, timestamp skew (default ±5 min, `auth.go:59-66, 90-104`), verifies signature, resolves site/tenant from the proven key (`ResolveByAgentKey`), and enforces single-use nonce (`RecordNonce`, false ⇒ `agent_replay`). Identity (`{SiteID, TenantID}`) is attached to ctx; **never** taken from a client header.
- Agent endpoints behind it: `POST /agent/v1/metadata`, `POST /agent/v1/heartbeat` (`agent/handler.go:354-357`), plus backup/autologin/diagnostics/errors/activity ingestion routes mounted on the same group (`server/server.go:165-189`).
- **HTTP error codes the CP returns** (all 401 Unauthorized via `a.fail`, distinguished by `code`):
  - `agent_unauthenticated` — missing/invalid headers, bad timestamp, body read fail, invalid signature (`auth.go:81-119`).
  - `agent_unknown` — key not enrolled (`repo.go:343-344`).
  - `agent_replay` — nonce reused (`auth.go:136-138`).
  - `/enroll` distinct codes: `pairing_code_invalid` (401), `pairing_code_consumed` (409), `pairing_code_expired` (401), validation `site_url_scheme` / `agent_public_key_invalid` (422) (`repo.go:258-274`, `service.go:133-139`). Agent maps these in `enrollErrorMessage` (`class-enrollment.php:175-187`).

**(B) CP → agent: Ed25519-signed JWT (EdDSA JWS) for command dispatch.**
- Minted by `apps/api/internal/agentcmd/jwt.go` with the CP private key. Header `{"alg":"EdDSA"}`, claims `{jti, exp, iat, iss, aud, cmd}`. `JWTTTL = 45s`, agent rejects exp > now+60s (`jwt.go:11-23, 39-42, 92-95`).
- Verified agent-side by `apps/agent/includes/class-connector.php:177-211` (`verify`): checks alg, exp window, jti anti-replay, `aud == own site_id`, `cmd == invoked command`.

**Agent local state persisted on success (PHP):**
- Enrollment client: `apps/agent/includes/class-enrollment.php`. `buildEnrollPayload` (`:78-89`) sends pairing_code + own pubkey + site_url/name/versions. `finishEnroll` (`:139-167`) parses `{site_id, tenant_id, control_plane_public_key}`, validates the CP key is 32 bytes, then persists.
- **Keystore** (`apps/agent/includes/class-keystore.php`): AES-256-GCM **encrypted-at-rest** material stored in wp-options:
  - `wpmgr_agent_cp_public_key` (`OPTION_CP_PUBLIC_KEY`, :38) — the CP Ed25519 pubkey, written on enroll (`:180`).
  - `wpmgr_agent_site_keypair` (`OPTION_SITE_KEYPAIR`, :41) — site's own Ed25519 keypair, generated on activation (`:205-208`).
  - `wpmgr_agent_age_identity` (`OPTION_AGE_IDENTITY`, :48) — private backup-decryption key; never leaves the agent.
  - Master key never in DB: priority `WPMGR_AGENT_KEY_FILE` const → HKDF-SHA256 from wp-config salts (preferred) → 0600 key file; the chosen source is pinned in `wpmgr_agent_master_key_source` (`:11-23, 38-57`).
- **Settings** (`apps/agent/includes/class-settings.php`): plaintext wp-options `wpmgr_agent_cp_url` (:33), `wpmgr_agent_site_id` (:36), `wpmgr_agent_tenant_id` (:39), `wpmgr_agent_activated_at` (:42), `wpmgr_agent_last_heartbeat` (:45), `wpmgr_agent_last_metadata` (:48). `setEnrollment` writes site/tenant ids (`:219-222`); `isEnrolled()` gates all reporting.

---

## 4. Sites repo + state

**`sites` table (`apps/api/db/schema.sql:33-73`):**
| column | type | notes |
|---|---|---|
| id | uuid PK | |
| tenant_id | uuid NOT NULL FK | RLS key |
| url | text | |
| name | text | |
| **status** | text DEFAULT `'pending'` | **free-text, NO CHECK constraint.** Observed values: `pending` (created via `CreateSite`, `repo.go:85-88`), `active` (set on enroll, `sites.sql:72,79`). |
| wp_version / php_version | text | |
| **agent_public_key** | text DEFAULT `''` | base64 Ed25519; UNIQUE where `<> ''` (`schema.sql:81-82`); rotated on re-enroll |
| **enrolled_at** | timestamptz NULL | set on enroll; NULL ⇒ "pending pairing" |
| **last_seen_at** | timestamptz NULL | bumped by heartbeat/metadata; freshness driver |
| **health_status** | text DEFAULT `'unknown'` | **free-text, NO CHECK.** Values: `unknown` → `healthy` → `unreachable` |
| server_info / multisite / active_theme / components(jsonb) / tags | | M2 metadata |
| age_recipient | text | backup pubkey |
| wp_timezone / wp_gmt_offset | text / real | scheduler |
| created_at / updated_at | timestamptz | |

- **Existing state fields relevant to the proposed lifecycle:** only `status` (pending/active), `health_status` (unknown/healthy/unreachable), `enrolled_at`, `last_seen_at`. **GAP: there is NO `connection_state`, `connected_at`, `disconnected_at`, `revoked_at`, `archived_at`, or any connection-state-machine column.** The migration that added these columns is `apps/api/migrations/20260527172114_m2_site_registry.sql:2` — all free-text, no enum/CHECK. (`revoked_at` exists in `schema.sql:178` but that is on the **api_keys** table, not sites.)

- **sqlc queries** (`apps/api/db/query/sites.sql`): CRUD (`CreateSite`, `GetSite`, `ListSites`, `DeleteSite`), enroll path (`GetSiteByURLForEnroll`, `CreateSiteForEnroll`, `AttachAgentToSite`), agent-auth (`GetSiteByAgentKey`), liveness (`UpdateSiteMetadata` and `TouchSiteSeen` both set `last_seen_at=now(), health_status='healthy'`, `:37-59`), health job (`ListEnrolledSitesAllTenants`, `MarkSiteUnreachable`, `ListEnrolledSitesForProbe`, `SetSiteHealthStatus`). Generated at `apps/api/internal/db/sqlc/sites.sql.go`.

- **Go repo/service** — repo interface + pgx impl: `apps/api/internal/site/repo.go` (`Repo` interface `:20-51`, `pgRepo` methods). Service: `apps/api/internal/site/service.go`. Domain model: `apps/api/internal/site/model.go:18-45`. Each op runs in an RLS-scoped tx (`InTenantTx` / `InEnrollTx` / `InAgentTx`, `repo.go:72-82`).

- **Health/state machine that DOES exist** — `apps/api/internal/site/health.go`: a River periodic job (`HealthCheckWorker`, `:83-104`) sweeps enrolled sites and flips `healthy → unreachable` when `last_seen_at` is older than `staleAfter` (≈2 missed heartbeat intervals, `:10-13, 33-59`). It also prunes `agent_nonces` older than the signature-skew window. This is the only automatic state transition today; it is one-directional (mark unreachable) — recovery back to `healthy` happens implicitly when the agent next pushes metadata/heartbeat (`sites.sql:48,56`).

- **Audit-log writer** — table `audit_log` (`schema.sql:193-207`): append-only, per-tenant **hash-chained** (`prev_hash`/`hash`). Go API `apps/api/internal/audit/audit.go`: `Recorder.Record(ctx, Event{TenantID, ActorType, ActorID, Action, TargetType, TargetID, Metadata})` (`:94-103, 152-196`), runs `InTenantTx`, chains to the tenant's last hash, inserts. Existing relevant actions: `site.enrolled`, `pairing_code.created`, `site.create`, `site.delete`, `site.tags.set` (`audit.go:41-47`). **GAP: no connect/disconnect/revoke/heartbeat audit actions exist yet.**

---

## 5. Dashboard sites list

**The sites list does NOT poll. Add-site is a code-display modal with no enrollment feedback — manual refresh is required, exactly as the brief states.**

- **Route**: `apps/web/src/routes/_authed/sites/index.tsx` (`SitesPage`). Consumes `useSites(appliedTag)` (`:41-42`). No interval/timer in the route.
- **Table component**: `apps/web/src/features/sites/sites-table.tsx`. Renders `HealthBadge` + `EnrollmentBadge` per row (`apps/web/src/features/sites/site-badges.tsx:9-41`). Health badge tones: healthy=success(pulsing), unreachable=destructive, unknown=muted (`site-badges.tsx:12-16`). Enrollment badge: `Enrolled` vs `Pending` from `site.enrolled` (`:34-41`).
- **Query hooks + keys**: `apps/web/src/features/sites/use-sites.ts`. Keys (`:25-30`): `["sites"]`, `["sites","list",{tag}]`, `["sites","detail",id]`. Hooks: `useSites`, `useSite`, `useDeleteSite`, `usePairingCode`, `useSetSiteTags`.
- **Polling cadence**: **NONE.** `useSites` (`:44-56`) and `useSite` (`:58-71`) declare no `refetchInterval`/`refetchOnWindowFocus`/`staleTime`. The list updates only on mount or explicit `invalidateQueries`. (Confirmed via repo-wide grep: `refetchInterval` appears in updates/backups/scans/uptime/errors/activity/security hooks but **never** in `use-sites.ts`.)
- **"Add site" modal**: `apps/web/src/features/sites/add-site-dialog.tsx`. Two steps: (1) optional name+tags form → `usePairingCode().mutateAsync` POSTs the pairing code (`:73-85`); (2) `PairingCodeDialog` shows the one-time plaintext code with a 1 s `setInterval` expiry **countdown** and static install instructions (`:174-297`). 
  - How the user gets the code: synchronously from the create-pairing-code response, shown once (`:84, 222-240`).
  - **It does NOT poll for enrollment and has no SSE.** `usePairingCode.onSuccess` invalidates `sitesKeys.lists()` (`use-sites.ts:106-109`) at code-creation time only. After the operator installs the plugin and the agent enrolls, the list will not reflect the new/active site until the user navigates away and back or hard-refreshes. **This is the exact gap the feature targets.**

---

## 6. Heartbeat path

**YES — the PHP agent already runs a 5-minute wp-cron heartbeat. The CP already has a `/agent/v1/heartbeat` receiver. So heartbeat is an EXTENSION, not a greenfield build.**

- **Cron registration**: `apps/agent/includes/class-scheduler.php`.
  - Hook `wpmgr_agent_heartbeat` (`HOOK_HEARTBEAT`, :29), scheduled on the custom `wpmgr_agent_5min` interval = **300 s** (`addSchedules` :144-147; `scheduleEvents` :167-168). Bound to `runHeartbeat` (`:121, 287-318`).
  - `runHeartbeat` no-ops until enrolled, then calls `Enrollment::sendHeartbeat()` (`:287-293`). It also acts as a **backstop** to schedule one-shot diagnostics/size pushes if stale (`:295-317`).
- **Heartbeat payload + endpoint**: `apps/agent/includes/class-enrollment.php:216-233` (`sendHeartbeat`). Signed POST to `/agent/v1/heartbeat` (`PATH_HEARTBEAT`, :37) with tiny body `{site_id, ts}`. On 2xx, persists `wpmgr_agent_last_heartbeat`.
- **CP receiver**: `apps/api/internal/agent/handler.go:393-404` (`heartbeat`) → `MetadataSink.Heartbeat` → `Service.Heartbeat` → `repo.TouchSeen` → `TouchSiteSeen` SQL sets `last_seen_at=now(), health_status='healthy'` (`sites.sql:53-59`). Returns 204.
- **Other agent→CP pushes already in place** (cadences):
  - Metadata: `wpmgr_agent_metadata`, **30 min** (`SCHEDULE_30MIN` :85,149-152,185-187), plus event-driven on plugin/theme changes (`:126-129`). Also bumps `last_seen_at` + `health_status='healthy'` (`UpdateSiteMetadata`, `sites.sql:37-51`).
  - Diagnostics: `wpmgr_agent_diagnostics_daily`, **daily** + up-to-4h jitter (`HOOK_DIAGNOSTICS` :43, :194-197) → `POST /agent/v1/diagnostics`.
  - Sizes: `wpmgr_agent_sizes_daily`, daily (`:53, 203-206`).
  - Activity-log ship: `wpmgr_agent_activity_ship`, **5 min** (`:63, 210-211`) → `POST /agent/v1/activity`.
  - PHP-error ship: `wpmgr_agent_errors_ship`, **5 min** (`:73, 217-218`).
  - One-shot safety: `wpmgr_agent_safety` ~30 min after activation self-deactivates the plugin if still unenrolled (`:35, 222-229, 383-403`).
- **Implication for the lifecycle:** heartbeat freshness already exists end-to-end (agent 5-min push → CP `last_seen_at` → River sweep marks `unreachable` when stale). A `connect/degraded/disconnected` state machine would build on these existing signals rather than introduce a new transport.

---

## 7. Open questions (decisions needed before implementation)

1. **In-process SSE bus vs multi-instance Cloud Run.** Both hubs are in-memory only (`backup/hub.go:30-33`, `update/hub.go:32-35`) with no Redis/cross-process fan-out. The CP runs on Cloud Run with potentially multiple instances. An enroll completion handled by instance A will NOT reach an "Add site" EventSource pinned to instance B. Does live enrollment require a shared bus (Redis pubsub / Postgres LISTEN-NOTIFY / River), or do we accept best-effort + the existing DB-reconcile-on-connect + polling fallback? The existing backup/update SSE tolerates this because each stream re-reads DB state on connect and falls back to polling — is that acceptable for enrollment too?

2. **What resource does the enrollment SSE key on?** Existing hubs key on a resource id the client already holds (snapshot_id, run_id). At "Add site" time the operator holds only the **pairing_code id** (or its plaintext), not yet a site_id (the site may not exist until the agent enrolls). Do we open the SSE keyed on pairing_code id, on tenant id (a tenant-wide "sites changed" channel), or on a client-minted correlation id? This is a genuinely new scoping model vs. the two existing per-resource hubs.

3. **No event IDs / replay anywhere.** There is no `id:`/`Last-Event-ID`/`?since=` support (section 1). If the operator's browser misses the single enroll event during a reconnect gap, nothing replays it (server only re-emits current state, and for enrollment "current state" may be "site doesn't exist yet"). Do we need at-least-once delivery (e.g. event log + cursor), or is on-connect DB reconcile + the create-pairing-code list invalidation sufficient?

4. **`status` and `health_status` are free-text with NO CHECK constraint** (`schema.sql:38,51`; `model.go` `Status string`). The proposed `connection_state` enum (enroll→connect→heartbeat→degraded→disconnected→revoked→archived→re-enroll) overlaps both existing columns: `status` already uses `pending`/`active`; `health_status` already uses `unknown`/`healthy`/`unreachable`. Is `connection_state` a NEW third column, or do we reconcile/migrate the two existing free-text columns into one machine? If new, how do the three columns relate (e.g. is `disconnected` a `status` value or a `connection_state` value)? This needs a data-model decision before any migration.

5. **No CP-side disconnect/revoke/archive exists.** The agent has LOCAL disconnect + revoke admin actions that wipe its own keystore (`class-admin.php:39-62, 451-459, 517+`), but the control plane has no endpoint or state transition for "operator revokes this site's agent" or "archive". The only CP-side site removal is hard `DELETE` (`sites.sql:19-21`). Does the lifecycle's `revoked`/`archived`/`disconnected` need new CP endpoints + audit actions + a soft-delete/state column, and how does revoking interact with the unique `agent_public_key` index (re-enroll rotates the key, but a revoked key is currently just left in place)?

6. **Heartbeat is push-only on a 5-min wp-cron with NO server→agent control.** The agent decides cadence (`class-scheduler.php:144-147`); the CP cannot ask for a faster beat or detect a missed beat faster than the River sweep's `staleAfter` threshold. For a responsive `degraded`/`disconnected` UX, do we (a) shorten the cron, (b) shorten the sweep threshold, or (c) add a CP→agent "beat now" command (the `agentcmd` JWT channel exists, `jwt.go`)? wp-cron only fires on site traffic, so a low-traffic site may silently appear `degraded` even when healthy — is that acceptable?

7. **Health recovery is implicit and untracked.** `MarkSiteUnreachable` is one-directional; recovery to `healthy` is a side effect of the next metadata/heartbeat write (`sites.sql:48,56`), and there is no `connected_at`/transition timestamp or audit trail of connect/disconnect events. The proposed lifecycle implies discrete transitions — do we need explicit transition timestamps + audit actions (`site.connected`, `site.disconnected`, `site.degraded`)?

8. **Enrollment idempotency rotates keys but the operator UI never learns the outcome.** Re-enrolling the same URL silently rotates the agent key and resets state to active (`repo.go:291-322`, `sites.sql:75-87`). With live enrollment, should the modal distinguish "new site enrolled" from "existing site re-enrolled / key rotated", and should re-enroll/rotation be an explicit lifecycle transition + audit event?
