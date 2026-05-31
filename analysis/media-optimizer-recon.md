# Media Optimizer — Integration Recon (READ-ONLY)

Recon of existing WPMgr infrastructure for a "Media Optimizer" (cloud-encode JPEG/PNG → WebP/AVIF).
All file:line references are to the repo at `/Users/mosamgor/Desktop/Terminal/wpmgr`. Nothing was modified.

> **TL;DR of the architecture you must reuse**: River for the job queue; `blobstore.Store` for S3 with **presigned URLs only** (never live GetObject on GCS); the **Ed25519 signed-request scheme on `/agent/v1`** for agent→CP ingest (same as diagnostics/activity); the **CP→agent Ed25519 JWT** (`agentcmd`) for dispatching an "encode" command; the tenant-scoped **`/api/v1/sites/events` SSE bus** for live progress; a file-route tab under `routes/_authed/sites/$siteId.*`; the hash-chained **audit** recorder; single-file `<timestamp>_name.sql` migrations with `FORCE ROW LEVEL SECURITY` + `current_setting('app.tenant_id', true)`; and a **role-rank RBAC** (no named per-feature permissions).

---

## 1. River queue setup

**Where River is wired:** `apps/api/cmd/wpmgr/main.go`.
- `startRiver(ctx, pool.Pool, logger, riverDeps{...})` — `main.go:918` builds the client; called at `main.go:497`.
- `riverDeps` struct — `main.go:884` (one field per worker; nil = feature disabled).
- River's own schema is migrated separately via `migrateRiver(ctx, migPool.Pool)` — `main.go:104` (uses the owner/superuser DSN, same as app migrations).
- `riverClient` is created AFTER the workers are built; enqueuers that need the client are wired in a second phase (`main.go:528-547`). This two-phase wiring is load-bearing — copy it.

**Client config** — `main.go:1068`:
```go
client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
    Logger:       logger,
    Queues:       queues,        // map[string]river.QueueConfig
    Workers:      workers,       // river.NewWorkers()
    PeriodicJobs: periodics,
})
```
There is **no global `MaxAttempts`/`RetryPolicy` override** — River defaults apply (max 25 attempts, exponential backoff). Per-worker control is via `Timeout(*river.Job[T]) time.Duration` overrides only.

**Queues** (`main.go:935-1066`):
- `river.QueueDefault`: `{MaxWorkers: 5}`.
- Per-tenant update shards: `queues[q] = {MaxWorkers: perTenantParallelism}` (default 5) — `main.go:940`.
- `backup.SqlInspectLegacyQueue`: `{MaxWorkers: 1}` (CPU-heavy, OOM guard) — `main.go:987`.
- `scan.ScanRunQueue`: `{MaxWorkers: 4}` — `main.go:1057`.

**How jobs are typed** — implement `river.JobArgs` (a `Kind() string` method) and a worker embedding `river.WorkerDefaults[T]`. Cleanest template is the scan worker:
- `apps/api/internal/scan/worker.go:33` `type ScanRunArgs struct { TenantID, SiteID, RunID uuid.UUID }`
- `worker.go:40` `func (ScanRunArgs) Kind() string { return "scan_run" }`
- `worker.go:29` `const ScanRunQueue = "scan_run"`
- `worker.go:56` `type ScanRunWorker struct { river.WorkerDefaults[ScanRunArgs]; ... }`
- `worker.go:97` `func (w *ScanRunWorker) Timeout(*river.Job[ScanRunArgs]) time.Duration { return 90 * time.Second }`
- `worker.go:102` `func (w *ScanRunWorker) Work(ctx, job *river.Job[ScanRunArgs]) error` — **re-reads authoritative state from the DB on every attempt; returns `nil` early if the row is already terminal** (dup-delivery safe). Copy this idempotency contract.

**Per-job queue/options** — two equivalent patterns:
- Static on the args type: `apps/api/internal/update/worker.go:41` `func (a TaskArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{Queue: QueueForTenant(a.TenantID)} }`.
- At insert time: `apps/api/internal/backup/enqueuer.go:40` uses `&river.InsertOpts{Queue: ..., UniqueOpts: river.UniqueOpts{ByArgs:true, ByPeriod: 5*time.Minute}}` to dedupe.

**Enqueuing** — `client.Insert(ctx, args, opts)`. Template enqueuer: `apps/api/internal/backup/enqueuer.go`:
```go
type RiverEnqueuer struct{ client *river.Client[pgx.Tx] }
func (e *RiverEnqueuer) EnqueueBackup(ctx, tenantID, snapshotID uuid.UUID) error {
    _, err := e.client.Insert(ctx, BackupArgs{TenantID:tenantID, SnapshotID:snapshotID}, nil)
    ...
}
```

**Re-enqueue / self-loop** (for a multi-step encode loop, mirror the scan pattern): a `Reenqueuer` interface (`scan/worker.go:44`) implemented by the enqueuer; wired post-start via `w.SetEnqueuer(e)` (`scan/worker.go:93`, `main.go:546`). The scan worker re-enqueues itself with a fresh job per partial iteration (`worker.go:487` `e.client.Insert(ctx, args, &river.InsertOpts{Queue: ScanRunQueue})`).

### Pattern a NEW `media_encode` worker follows
1. In `internal/media/worker.go`: `MediaEncodeArgs{TenantID, SiteID, JobID uuid.UUID}` + `Kind() string { return "media_encode" }` + `const MediaEncodeQueue = "media_encode"`.
2. `MediaEncodeWorker` embeds `river.WorkerDefaults[MediaEncodeArgs]`, overrides `Timeout` (encode I/O is slow — set ≥ HTTP timeout to the agent), and `Work()` re-reads state + returns nil if terminal.
3. `internal/media/enqueuer.go`: `RiverEnqueuer` with `EnqueueEncode(...)` calling `client.Insert`.
4. In `main.go`: add fields to `riverDeps`, `river.AddWorker(workers, d.mediaEncodeWorker)` and `queues[media.MediaEncodeQueue] = river.QueueConfig{MaxWorkers: N}` inside `startRiver`, then `mediaSvc.SetEnqueuer(media.NewRiverEnqueuer(riverClient))` after the client starts.

---

## 2. S3 / object storage client

**Package:** `apps/api/internal/blobstore/blobstore.go`. Built over `aws-sdk-go-v2` (ADR-010). Targets AWS S3, SeaweedFS/MinIO, **and GCS S3-compat**.

**`Store` methods** (all `apps/api/internal/blobstore/blobstore.go`):
- `New(cfg Config) (*Store, error)` — `:52`. No network I/O.
- `Bucket() string` — `:90`.
- `EnsureBucket(ctx) error` — `:108` (best-effort, non-fatal).
- `Put(ctx, key, body io.Reader, size int64) error` — `:131`.
- `Get(ctx, key) (io.ReadCloser, error)` — `:145`. **WARNING: live GetObject. On GCS this 403s** (`SignatureDoesNotMatch`).
- `GetViaPresign(ctx, key) (io.ReadCloser, error)` — `:172`. **Use this for CP-side reads** — mints a 60s presigned GET and fetches over plain HTTP. Returns `ErrNotFound` on 404.
- `Head(ctx, key) (exists bool, size int64, err error)` — `:198`.
- `Delete(ctx, key) error` — `:217` (idempotent).
- `List(ctx, prefix) ([]string, error)` — `:230` (paginated).
- `PresignPut(ctx, key, ttl) (string, error)` — `:253`. Mint for the agent to upload.
- `PresignGet(ctx, key, ttl) (string, error)` — `:267`. Mint for the agent to download.
- `ErrNotFound` — `:279`.

**The documented GCS gotcha** (verified at `blobstore.go:69-78`): `New()` sets
`o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired` and
`o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired`. Live `GetObject`/`PutObject` add `x-amz-sdk-checksum-*` headers GCS doesn't sign → 403. **Presigned URLs are unaffected** (no body checksum). Net rule for media: **the agent uploads encoded bytes via presigned PUT, downloads originals via presigned GET; the CP never streams image bytes itself**. If the CP must read a small control object, use `GetViaPresign`, never `Get`. Never log a presigned URL — it is a bearer credential (`blobstore.go:11`).

**How the backup engine streams + mints presigns** (the proven E2E pattern to copy):
- Object keying: content-addressed + tenant-namespaced. `apps/api/internal/backup/model.go:398` `func chunkS3Key(tenantID uuid.UUID, blake3 string) string { return "chunks/" + tenantID.String() + "/" + blake3 }`. Tenant-prefixing means a presign can never target another tenant's prefix (`model.go:396`).
- Mint PUT URLs: `apps/api/internal/backup/service.go:487-488` `key := chunkS3Key(tenantID, h); url, _ := presigner.PresignPut(ctx, key, s.presignTTL)`.
- Mint GET URLs: `service.go:389`.
- The agent requests these via the agent-authed callback `POST /agent/v1/backups/:snapshotId/presign` (`backup/agent_handler.go:34,45`) and submits a manifest via `.../manifest` (`agent_handler.go:78`). **This callback handshake is the exact shape the media flow should reuse** (see §3).

**Bucket config** — `apps/api/internal/config/config.go:105`:
```go
type S3Config struct { Endpoint, Region, Bucket, AccessKey, SecretKey string; ForcePathStyle bool }
func (s S3Config) Enabled() bool { return s.Bucket != "" }   // :116
```
Wired at `main.go:243-373` (built fresh in 2-3 places because `store` is block-scoped). `cfg.Backup.PresignTTL` bounds URL lifetime (`config.go:122`). A `blobstore.Registry` (`blobstore/registry.go`) resolves a per-snapshot destination store — for media you can start with the **default store** (`registry.DefaultStore()`, `registry.go:61`).

**Per-job temp object keying for media** — follow the namespace rule. Suggest:
`media/<tenantID>/<jobID>/<assetID>.webp` for outputs and (if you stage originals) `media/<tenantID>/<jobID>/src/<assetID>` — but note WPvivid/backup stores *content-addressed* (dedup). Media outputs are NOT dedup-able across sites, so a `<tenant>/<job>/<asset>` path is correct. GC unreferenced temp objects via a periodic River job mirroring `backup.GCWorker` (`backup/worker.go:444`) + `store.Delete`.

---

## 3. Agent ↔ control-plane auth (BOTH directions)

### (a) agent → CP : Ed25519 signed-request scheme  ← **USE THIS for media upload + sync-batch + job-status**

**CP side** (`apps/api/internal/agent/`):
- `auth.go:53` `type Authenticator` / `auth.go:73` `Authenticate() gin.HandlerFunc` — verifies signature, timestamp skew (default 5 min), nonce single-use; resolves site+tenant **from the verified key, never a header** (`auth.go:122`). Buffers body up to `maxAgentBody = 4 << 20` (4 MiB) to hash it (`auth.go:18,107`).
- `auth.go:22` `type Identity struct { SiteID, TenantID uuid.UUID }`; `auth.go:35` `IdentityFromContext(ctx) (Identity, bool)`. **Every handler reads identity from context** (e.g. `diagnostics_handler.go:38`).
- `signature.go:46` `CanonicalMessage(method, path, timestamp, nonce string, body []byte) []byte` = `METHOD\nPATH\nTIMESTAMP\nNONCE\nhex(sha256(body))`.
- `signature.go:63` `VerifySignature(pubKeyB64, sigB64, method, path, timestamp, nonce, body) bool`.
- Headers (`signature.go:17-28`): `X-WPMgr-Agent-Key`, `X-WPMgr-Timestamp`, `X-WPMgr-Nonce`, `X-WPMgr-Signature`.

**Agent (PHP) side** (`apps/agent/includes/`):
- `class-signer.php:96` `signHeaders($method, $path, $body, $now=null): array` → the four `X-WPMgr-*` headers. `class-signer.php:65` `canonicalMessage(...)` mirrors the Go format exactly.
- `class-enrollment.php:495` `private function signedPost($path, $body, $timeout=null)` — the canonical agent→CP POST helper. Signs `('POST', $path, $body)`, merges auth headers with `Content-Type: application/json`, `wp_remote_post($base.$path, ...)`. `controlPlaneUrl()` is the base (`class-enrollment.php:497`).
- `class-enrollment.php:290` `sendHeartbeat()` and `:334` `disconnect()` are examples that go through `signedPost`.

**Existing ingest handler structure** (copy `diagnostics_handler.go` verbatim as a skeleton):
```go
// apps/api/internal/agent/diagnostics_handler.go
type DiagnosticsHandler struct{ svc *diagnostics.Service }
func (h *DiagnosticsHandler) Register(r *gin.RouterGroup) { r.POST("/diagnostics", h.push) }   // :33
func (h *DiagnosticsHandler) push(c *gin.Context) {
    id, ok := IdentityFromContext(c.Request.Context())   // tenant+site from verified identity  :38
    body, _ := io.ReadAll(io.LimitReader(c.Request.Body, maxDiagnosticsBytes))  // 4 MiB cap   :43
    count, ierr := h.svc.IngestDiagnostics(ctx, id.TenantID, id.SiteID, body)   // :52
    ...
}
```
`activity_handler.go` is structurally identical.

**Registration** (`apps/api/internal/server/server.go:174-211`):
```go
agentGroup := engine.Group("/agent/v1")
agentGroup.Use(deps.AgentAuth.Authenticate())
deps.AgentH.Register(agentGroup)
if deps.DiagnosticsAgentH != nil { deps.DiagnosticsAgentH.Register(agentGroup) }  // :191
// ... add: if deps.MediaAgentH != nil { deps.MediaAgentH.Register(agentGroup) }
```
Add a `MediaAgentH *agent.MediaAgentHandler` field to `server.Deps` (around `server.go:84`) and wire it in `main.go`.

> **Answer to the prompt's question:** YES — the new agent→CP **"media encode" multipart upload + "sync-batch"/"job-status" calls MUST use the signed-request scheme on `/agent/v1`**, exactly like activity/diagnostics. Two important caveats:
> - **Multipart:** the existing `signedPost` (`class-enrollment.php:495`) hardcodes `Content-Type: application/json` and signs the raw body. The signature only hashes the **raw request body bytes** (`sha256(body)`), so multipart works **if** you add a new PHP helper that signs the raw multipart body and sends the correct `Content-Type: multipart/form-data; boundary=...`. **Do not reuse `signedPost` as-is for multipart** — clone it.
> - **In practice you likely don't need multipart at all:** mirror backups — the CP mints a presigned PUT, the agent PUTs the encoded bytes straight to S3, then POSTs a small JSON "manifest"/"sync-batch" of `{asset_id, s3_key, bytes, format}` back to `/agent/v1/media/...`. That keeps every agent→CP body small JSON (under the 4 MiB cap) and avoids streaming image bytes through the CP.

### (b) CP → agent : Ed25519 JWT (for dispatching the "encode" command)

**CP side** (`apps/api/internal/agentcmd/`):
- `jwt.go:62` `type Signer`; `jwt.go:67` `NewSigner(privB64)`; `jwt.go:109` `Mint(now, aud, cmd) (token, jti, err)`. JWT = `b64url(header).b64url(claims).b64url(sig)`, `alg:EdDSA`, claims `{jti,exp,iat,iss,aud,cmd}`, **exp ≤ now+45s** (`JWTTTL`, `jwt.go:42`; agent rejects >60s).
- `client.go:20` `const commandPathFormat = "/wp-json/wpmgr/v1/command/%s"` — the CP POSTs the signed command to the agent's REST route. `client.go:28-34` `Doer.DoOnce` (no retries; jti is single-use). `aud` = the target site's enrollment UUID, `cmd` = the command name (e.g. `"media_encode"`).
- The `Commander` interface lives per-feature; e.g. `update.Commander` (`update/worker.go:71`), `scan.AgentScanClient`. **Add a `Media(ctx, siteID, siteURL, req) (resp, error)` method to `agentcmd.Client`** and a feature-local interface (mirror `client.go:185` `Scan(...)`). When the signing key is empty, `disabledCommander` refuses (`main.go:1101-1133`) — add a `Media` stub there too or the build breaks.

**Agent (PHP) side** (`apps/agent/includes/class-connector.php`):
- `verify($jwt, $now=null)` — `:98`. `verifyCommand($jwt, $expectedCmd, $now=null)` — `:193`: checks `exp ≤ now+60s` (`MAX_FUTURE_EXP`, `:38`), `aud == own site UUID` (`:205` `hash_equals`), `cmd == expectedCmd` (`:209`), signature via `sodium_crypto_sign_verify_detached` against the stored CP public key (`:122`). The agent's command REST route must call `verifyCommand($jwt, 'media_encode')`.

---

## 4. SSE event bus

**Package:** `apps/api/internal/site/events/` + the envelope type in `apps/api/internal/site/connection.go`.

**Envelope** — `apps/api/internal/site/connection.go:90`:
```go
type ConnectionEvent struct {
    ID       string         `json:"id"`        // app-minted ULID (monotonic per tenant) — the SSE replay cursor
    Type     string         `json:"type"`      // e.g. "site.heartbeat"; NEW: "media.optimize.progress"
    TenantID uuid.UUID      `json:"tenant_id"`
    SiteID   uuid.UUID      `json:"site_id"`
    TS       time.Time      `json:"ts"`
    Data     map[string]any `json:"data"`      // free-form payload
}
```
Existing type constants: `connection.go:77-86` (`site.created`, `site.heartbeat`, `site.state_changed`, …). Add `EventMediaOptimizeProgress = "media.optimize.progress"` etc. here (or in a media file — they're just strings).

**Bus** (`internal/site/events/`):
- `publisher.go:43` `func (p *Publisher) Publish(ctx, ev ConnectionEvent) error` — mints ULID if empty, persists to `site_events` under tenant RLS, fires `pg_notify('wpmgr_site_events', '<tenant_id>:<event_id>')` **in the same tx** (`publisher.go:65-79`). Channel name `notifyChannel = "wpmgr_site_events"` (`publisher.go:20`).
- `listener.go:37` `Listener.Run(ctx)` — one dedicated `LISTEN wpmgr_site_events` connection per process; on notify it loads the row under tenant scope and `Hub.Fanout`s it.
- `hub.go:45` `Hub.Subscribe(tenantID) (<-chan ConnectionEvent, func())`; `hub.go:73` `Fanout(ev)`.
- The production publisher is injected into `site.ConnectionService` (`connection_service.go:35`, called at `:54`/`:133`). **Note:** the SSE bus is currently only published to by the connection-state machine. For media you publish your own events directly through the same `events.Publisher` (`Publish` takes any `ConnectionEvent`) — inject `*events.Publisher` (which satisfies `site.EventPublisher`, `connection.go:103`) into the media service.

**SSE handler** — `internal/site/events/sse_handler.go`:
- `Register` — `:87` `r.GET("/sites/events", authz.RequirePermission(authz.PermSiteRead), h.stream)` → **`GET /api/v1/sites/events`** (single tenant-level stream).
- `stream` — `:95`: replays from `?since=`/`Last-Event-ID`, subscribes to the Hub, 15s keepalives, per-principal stream cap.
- **Per-site authorization is already enforced** (`:108-109`): `allowed := func(ev) bool { return p.CanAccessSite(ev.SiteID) }` is applied to both replay and live frames. A media event with the correct `SiteID` is automatically filtered to authorized principals — **just set `SiteID` correctly**.
- Frame format — `eventFrame` `:244`: `id: <ULID>\nevent: <Type>\ndata: <json>\n\n`.

**Frontend** (`apps/web/src/features/sites/`):
- `use-site-events.ts` — a **single module-level `EventSource`** to `/api/v1/sites/events` fanned out to a `Set` of handlers (`:106-256`). `useSiteEvents(handler)` subscribes (`:247`). It validates frames with Zod against `SITE_EVENT_TYPES` (`:33-42`) — **a media event type NOT in that array is dropped** (`siteEventSchema` uses `z.enum(SITE_EVENT_TYPES)`, `:62`). **To publish `media.*` you MUST add the new type strings to `SITE_EVENT_TYPES`** and attach an `addEventListener` (the loop at `:194` registers each type).
- `use-sites-live.ts` — `useSitesLiveSync()` patches the react-query cache from events (`:51` switch on `ev.type`). A media handler would add cases here or be its own hook (`useMediaLive(jobId)`) that filters by `ev.site_id`/`ev.data.job_id`.

> **Alternative considered & rejected for media:** backups use a SEPARATE per-snapshot SSE stream (`apps/web/src/features/backups/use-backup-stream.ts`, served by `backup/agent_handler.go`-adjacent `GET /backups/:snapshotId/events`, `backup/handler.go:77`). For a per-job media progress UI you can EITHER reuse the shared `/sites/events` bus (simplest; add `media.*` types) OR build a per-job stream like backups. The shared bus is the lower-effort path and already has auth + replay.

---

## 5. Site detail page + tabs

**Layout route:** `apps/web/src/routes/_authed/sites/$siteId.tsx`.
- Tab list — `$siteId.tsx:67-77`:
```ts
const TABS = [
  { to: "/sites/$siteId/health",   label: "Health" },
  { to: "/sites/$siteId/updates",  label: "Updates" },
  { to: "/sites/$siteId/backups",  label: "Backups" },
  { to: "/sites/$siteId/security", label: "Security" },
  { to: "/sites/$siteId/activity", label: "Activity" },
  { to: "/sites/$siteId/errors",   label: "Errors" },
  { to: "/sites/$siteId/settings", label: "Settings" },
] as const;
```
The nav renders `TABS.map(...)` as TanStack `<Link>`s with `activeProps` for the active style (`$siteId.tsx:443-456`); `<Outlet/>` renders the child (`:460`).

**Each tab is a real file-route** (dot-notation convention). Each child is a thin wrapper, e.g. `$siteId.activity.tsx` (full file):
```ts
export const Route = createFileRoute("/_authed/sites/$siteId/activity")({ component: ActivityTab });
function ActivityTab() { const { siteId } = Route.useParams(); return <ActivityTable siteId={siteId} />; }
```
Real logic lives in `apps/web/src/features/<feature>/`.

### Adding a NEW "Media" tab (exact steps)
1. Create `apps/web/src/routes/_authed/sites/$siteId.media.tsx` with `createFileRoute("/_authed/sites/$siteId/media")` rendering a `<MediaTab siteId={siteId}/>` (logic in `apps/web/src/features/media/`).
2. Add `{ to: "/sites/$siteId/media", label: "Media" }` to the `TABS` array in `$siteId.tsx:67`.
3. The route tree is generated (TanStack file-based routing) — `routeTree.gen.ts` regenerates on dev/build; no manual nav registration beyond the `TABS` entry.

---

## 6. Audit log

**Package:** `apps/api/internal/audit/audit.go`. Append-only, per-tenant **hash-chained** (each entry's `hash` chains to the tenant's previous `hash`; `canonical()` at `:132` is sha256 over `prevHash\ntenant\nactorType\nactorID\naction\ntargetType\ntargetID\nmetaJSON\ncreatedAt`).

- `Event` input — `audit.go:108`: `{TenantID uuid.UUID; ActorType, ActorID, Action, TargetType, TargetID string; Metadata map[string]any}`.
- `Entry` output — `audit.go:93` (adds `ID, PrevHash, Hash, CreatedAt`).
- `Recorder.Record(ctx, e Event) (Entry, error)` — `audit.go:165`. Runs in tenant RLS scope (`InTenantTx`), reads `GetLastAuditHash`, truncates `createdAt` to microseconds (so Verify re-hashes equal). **Best-effort:** callers ignore the error (`_, _ = ...`).
- Action constants live as `const` in `audit.go:31-89` (e.g. `ActionSiteCreate = "site.create"`). The recorder is constructed once in `main.go` (`auditRec := audit.NewRecorder(pool, clock)`, `main.go:~130`) and threaded into every service/worker.

**Call pattern** (from `scan/service.go:69`, `scan/worker.go:303`):
```go
_, _ = s.audit.Record(ctx, audit.Event{
    TenantID:   tenantID,
    ActorType:  audit.ActorUser,           // or ActorSystem for worker-driven
    ActorID:    actorID.String(),
    Action:     "media.optimize.started",  // add as a const in audit.go
    TargetType: "media_job",
    TargetID:   jobID.String(),
    Metadata:   map[string]any{"site_id": siteID, "assets": n},
})
```
> Note: the prompt mentions an `org.created` example — there is no `internal/org` audit constant of that exact name; the existing constants are e.g. `ActionSiteCreate`, `ActionTenantCreate` (`audit.go:41-43`). Add `ActionMediaOptimizeStarted`, `ActionMediaDeleteOriginalsConfirmed`, etc. as new consts. **Confirmations (e.g. delete-originals) should be recorded with `ActorUser` and the actor's id** so the chain attributes the destructive consent.

---

## 7. Migrations

**Directory:** `apps/api/migrations/`. Embedded via `//go:embed *.sql` (`migrations.go:12`).

**Runner** — `apps/api/internal/db/migrate.go:34-64`: reads `*.sql`, strips the `.sql` suffix to get the `version`, **`sort.Strings(versions)`** (pure lexical), applies any not in `schema_migrations`. There is **no `.up`/`.down` distinction and no down-migrations**.

### ⚠️ The prompt's assumptions vs reality (must-adapt)
| Prompt assumes | Reality | Action |
|---|---|---|
| `NNNN_media_optimizer.up.sql` with a `.up` suffix | **Single file** `<timestamp>_name.sql`. Naming is `YYYYMMDDhhmmss_mNN_name.sql`, e.g. `20260531100000_m22_sites_shared_read.sql` | Name it like `20260531110000_m23_media_optimizer.sql` (timestamp **after** the latest = `20260531100000`). No `.up`/`.down`. |
| `set_updated_at()` trigger | **No such function exists; no `BEFORE UPDATE` triggers anywhere.** `updated_at` is set by app code. | Do **not** add a trigger. Just `"updated_at" timestamptz NOT NULL DEFAULT now()` and set it in your repo's UPDATE statements. |
| RLS `current_setting('app.tenant_id')` | Actual: `current_setting('app.tenant_id', **true**)` (the `true` = missing_ok) wrapped in `nullif(..., '')::uuid` | Use the exact idiom below. |
| (unstated) permissive RLS | Actual: **`ENABLE` + `FORCE ROW LEVEL SECURITY`**, plus a separate `app.agent` policy for cross-tenant worker access | Add both policies. |

**Real template** (`apps/api/migrations/20260531010000_m15_scan.sql` is the canonical recent example). Every statement is `IF NOT EXISTS`-guarded inside `DO $$ ... $$` blocks (idempotent — runner has no down path):
```sql
CREATE TABLE IF NOT EXISTS "public"."media_jobs" (
    "id"         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    "tenant_id"  uuid NOT NULL,
    "site_id"    uuid NOT NULL,
    "status"     text NOT NULL DEFAULT 'queued',
    "created_at" timestamptz NOT NULL DEFAULT now(),
    "updated_at" timestamptz NOT NULL DEFAULT now(),   -- set by app code, NOT a trigger
    CONSTRAINT "media_jobs_tenant_id_fkey" FOREIGN KEY ("tenant_id")
        REFERENCES "public"."tenants"("id") ON DELETE CASCADE,
    CONSTRAINT "media_jobs_site_id_fkey" FOREIGN KEY ("site_id")
        REFERENCES "public"."sites"("id") ON DELETE CASCADE
);
ALTER TABLE "public"."media_jobs" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."media_jobs" FORCE ROW LEVEL SECURITY;
-- tenant isolation (operator/API path):
CREATE POLICY "media_jobs_tenant_isolation" ON "public"."media_jobs"
    USING      ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- cross-tenant worker/agent path (enumeration under app.agent GUC):
CREATE POLICY "media_jobs_agent" ON "public"."media_jobs"
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');
```
**Role/grants:** the `wpmgr_app` role and blanket grants are created ONCE in `20260527130000_auth_multitenancy.sql:18-126` (`CREATE ROLE wpmgr_app NOLOGIN NOSUPERUSER NOBYPASSRLS`, `GRANT … ON ALL TABLES`, plus `ALTER DEFAULT PRIVILEGES … GRANT … TO wpmgr_app`). **New tables inherit grants via the default-privileges clause — you usually do NOT need a per-table GRANT** (the scan migration `m15` adds none). Only `audit_log` has an extra `REVOKE UPDATE, DELETE, TRUNCATE` (`auth_multitenancy.sql:130`); replicate that only if a table must be append-only.

`atlas.sum` exists but the embedded runner ignores it (`migrations.go:10`). If you author via Atlas you must `atlas migrate hash`; if you hand-write the `.sql`, the embedded runner applies it regardless (the sum is only checked by the Atlas CLI path).

---

## 8. Tests

**Go integration (testcontainers — Docker required, skipped gracefully if absent):** `apps/api/tests/`.
- **Postgres testcontainer exists:** `rls_integration_test.go:30` `startPostgres(t)` runs `postgres:16-alpine` (`tcpostgres.Run`, `:34`), applies migrations as superuser (`:60`), then provisions the **non-superuser `wpmgr_app` login** and reconnects so RLS is actually enforced (`:68-83`). `connectAdmin` (`:101`) opens a superuser pool for out-of-band tamper tests. **Reuse `startPostgres(t)` for any media DB test.**
- **MinIO (S3) testcontainer exists:** `blobstore_integration_test.go:24` `minio.Run(ctx, "minio/minio:RELEASE.2024-01-16T16-07-38Z", ...)` — exercises real presigned PUT/GET. **Reuse for media S3 tests.**
- **River integration:** `river_integration_test.go:123` constructs a real `river.NewClient` against the PG container. `update_integration_test.go:277` builds a client with a custom `Queues`/`Workers` set.
- **NO real-WordPress container exists.** Nothing in `apps/`, `infra/`, or `scripts/` runs a `wordpress:`/`mariadb:`/`wp-cli` container. An E2E "real WP container" for media would have to be **built from scratch** (e.g. a `wordpress:php8.2-apache` + `mariadb` compose, install the agent plugin, enroll it). This is a non-trivial new piece of infra — flag it to the owner.

**Frontend:** `apps/web/`.
- **vitest** unit/component (`package.json:12` `"test": "vitest run"`, dep `vitest ^2.1.8`).
- **Playwright e2e** (`package.json:13` `"e2e": "playwright test"`, config `apps/web/playwright.config.ts`, specs in `apps/web/e2e/` e.g. `backups.spec.ts`, `health.spec.ts`). These run against the web app (mocked/fixture API), **not** a live agent.

**Agent (PHP):** `apps/agent/`.
- **PHPUnit 10.5 + Brain Monkey 2.6 + yoast/phpunit-polyfills** (`composer.json:15-20`, `"test": "phpunit"` `:40`). Tests in `apps/agent/tests/` (e.g. `SignerTest.php`, `ConnectorTest.php`, `BackupCommandTest.php`, `tests/Backup/`). **WP functions are mocked via Brain Monkey** — there is no live WordPress in agent tests. A media-encode command test would mock `wp_remote_post`, the keystore, etc., exactly like `BackupCommandTest.php`.

**What an integration test "real WP container" would need:** does NOT exist; must be built. Minimum: a WP+DB container, the agent plugin mounted/installed, an enrollment handshake against a test CP, real image fixtures, and assertions that encoded WebP/AVIF land in (MinIO) S3 via presigned PUT and the manifest callback completes. Given the cost, recommend starting with PHPUnit (agent encode unit tests) + Go testcontainer (CP queue/S3/DB) and deferring a true WP-container E2E.

---

## 9. Open questions / prompt-vs-reality deltas

1. **RBAC is role-rank, NOT named per-feature permissions.** `apps/api/internal/authz/role.go:42-89`: a fixed `Permission` enum (`site:read`, `site:write`, `member:manage`, `tenant:manage`, `site:autologin`) each mapped to a **minimum role** (`minRoleFor`, `:70`). `Allows(role, perm)` is pure rank comparison. **There is no `site.media.optimize` permission and no way to add one without editing this enum + matrix.** Roles: viewer < operator < admin < owner (`role.go:9-17`). **Decision needed:** gate media behind existing `PermSiteWrite` (operator+) for "start optimize" and a stronger gate (admin+? a new `PermSiteMediaDangerous`?) for "delete originals"? My recommendation: reuse `PermSiteWrite` for optimize, and either reuse `PermSiteWrite` + a typed confirmation, or add a new `PermMediaDeleteOriginals → RoleAdmin` const. Confirm with owner.

2. **Per-site authorization** for site-scoped collaborators is enforced by `authz.RequireSiteAccess("siteId")` on `/sites/:siteId/...` routes (`middleware.go:25`) and by `p.CanAccessSite` in SSE (`sse_handler.go:109`). **Every by-id route the media feature adds (e.g. `GET /media/jobs/:jobId`) MUST gate access** — but `RequireSiteAccess` keys off a `:siteId` path param. A `:jobId`-only route has no site param, so you must either (a) nest under `/sites/:siteId/media/...`, or (b) add a service-level `canReadMediaJob` that resolves the job's site and checks `p.CanAccessSite` (mirrors the per-site `canReadSite` pattern noted in project memory). Confirm the URL shape.

3. **API DTOs: newer features bypass the ogen-generated types.** The OpenAPI doc (`packages/openapi/openapi.yaml`) + ogen codegen (`internal/api/gen/`, generated via `//go:generate ogen`, `gen/generate.go:10`) is the *nominal* source of truth, **but Gin owns routing and recent features hand-roll local DTO structs + `c.JSON`** — e.g. `scan/handler.go:63-105` defines `runDTO`, `findingDTO`, etc. locally; openapi.yaml barely mentions scan. **Decide:** add media schemas to `openapi.yaml` and regenerate (heavier, gives the TS client typed models in `@wpmgr/api`), or hand-roll DTOs like scan (faster, but the frontend types are then hand-written). The frontend `Site` type IS generated (`@wpmgr/api`), so list/detail shapes that the existing UI consumes lean on codegen; a brand-new media surface can go either way. Confirm.

4. **SSE: frontend drops unknown event types.** `use-site-events.ts:33-42` hard-codes `SITE_EVENT_TYPES` and validates with `z.enum(...)` (`:62`). Publishing `media.*` on the shared bus requires editing this array + the `addEventListener` loop (`:194`). Alternatively build a dedicated per-job stream (like `use-backup-stream.ts`). **Which?** (Recommend: shared bus for low effort.)

5. **Agent→CP multipart is not a solved path.** `class-enrollment.php:495 signedPost` is JSON-only and the body cap is 4 MiB (`agent/auth.go:18`). The prompt's "multipart upload" should almost certainly become **presigned-PUT-to-S3 + small-JSON-callback** (the backup model), not a multipart POST through the CP. If true multipart to the CP is genuinely required, a new signing helper + a handler that reads the multipart body **before** signature verification re-reads it is needed (the auth middleware buffers the body to 4 MiB and restores it, `auth.go:107-113` — multipart image payloads would blow the 4 MiB cap). **Confirm the encode-output transport** (strongly recommend presigned PUT).

6. **Where does encoding actually run?** The prompt title says "cloud-encode." Reality: WPMgr's CP **never processes media bytes** (no decryption key, GCS GetObject 403s). The proven model is **the agent does the heavy lifting** (the agent dumps/encrypts/uploads for backups). So "cloud-encode" likely means *the agent encodes locally and uploads results to our S3*, OR *a separate CP-side encode service downloads via presigned GET, encodes, uploads via presigned PUT*. If the latter, that encode service is **new infra not present in this repo** (no image-processing library, no GPU/CPU encode worker). **Confirm the encode locus** — it changes whether §1's River worker calls an agent command (encode-on-site) or runs an in-CP encode step (new dependency: libvips/ffmpeg, new container).

---

## Top 5 must-adapt deltas (prompt → codebase)

1. **Migrations:** no `.up.sql`/`.down.sql` — it's a single `YYYYMMDDhhmmss_mNN_name.sql` applied lexically; **`set_updated_at()` does not exist** (no triggers; app sets `updated_at`); RLS uses `current_setting('app.tenant_id', true)` with `FORCE ROW LEVEL SECURITY` **plus** an `app.agent` policy.
2. **RBAC has no named `site.media.optimize` permission** — it's a fixed role-rank matrix (`authz/role.go`). Reuse `PermSiteWrite` or add a new const+mapping; you cannot just reference an arbitrary permission string.
3. **Agent→CP "multipart upload" should be presigned-PUT-to-S3 + JSON callback**, not multipart through the CP (the signed-request path is JSON-only with a 4 MiB body cap). Reuse the backup `/agent/v1/backups/:id/presign` + `/manifest` handshake shape.
4. **GCS live GetObject 403s** — the CP must never `Store.Get` image bytes; use presigned URLs / `GetViaPresign`. The agent transfers all bytes directly to/from S3.
5. **SSE `media.*` events won't reach the UI unmodified** — the frontend `EventSource` hard-codes `SITE_EVENT_TYPES` and Zod-`enum`-validates, dropping unknown types; you must register the new types (or build a per-job stream like backups). And newer CP features hand-roll DTOs rather than using the ogen-generated OpenAPI types — pick a lane.

---

*Confirmation: this file (`analysis/media-optimizer-recon.md`) was written. No source code was modified.*
