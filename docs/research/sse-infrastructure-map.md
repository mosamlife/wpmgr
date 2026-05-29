# SSE Infrastructure Map (M3 update progress → M5.6 backup progress)

Purpose: map the existing M3 update-run SSE pipeline so we can add a parallel topic for M5.6 backup progress without re-inventing the wheel.

---

## 1. Backend (Go) — file & line index

| Concern | File | Lines |
|---|---|---|
| Hub type + Subscribe/Publish/SubscriberCount | `apps/api/internal/update/hub.go` | 1–99 |
| Event struct (SSE payload) | `apps/api/internal/update/hub.go` | 11–24 |
| HTTP handler (mounts SSE route, snapshot+stream loop) | `apps/api/internal/update/handler.go` | 22–205 |
| Route registration `GET /api/v1/updates/{runId}/events` | `apps/api/internal/update/handler.go` | 36–41 |
| SSE writer + heartbeat (`":\n\n"`, 15 s) | `apps/api/internal/update/handler.go` | 20, 153–205 |
| Worker → hub.Publish call sites | `apps/api/internal/update/worker.go` | 142, 238, 273–289 |
| Hub unit tests (publish, scope-by-runID, unsubscribe, non-blocking) | `apps/api/internal/update/hub_test.go` | 1–82 |
| Wiring: `updateHub := update.NewHub()` and pass to worker + handler | `apps/api/cmd/wpmgr/main.go` | 185, 188, 330 |
| Server route mount: `deps.UpdateH.Register(v1)` | `apps/api/internal/server/server.go` | 117–126 |
| ogen waiver for `text/event-stream` (skips SSE op codegen) | `apps/api/ogen.yaml` | 1–14 |

### 1.1 Hub API (`apps/api/internal/update/hub.go`)

In-process pub/sub, keyed by `runID uuid.UUID`. NO topic abstraction — the map is `map[uuid.UUID]map[*subscription]struct{}`.

```go
// Hub is an in-process pub/sub fan-out for update progress, keyed by run ID.
// River workers Publish transitions; SSE handlers Subscribe to a run.
// Delivery is best-effort: a slow subscriber whose buffer is full drops
// the event rather than blocking the worker.
type Hub struct {
    mu   sync.Mutex
    subs map[uuid.UUID]map[*subscription]struct{}
}
type subscription struct{ ch chan Event }

func NewHub() *Hub
func (h *Hub) Subscribe(runID uuid.UUID) (<-chan Event, func())   // ch buffer = 64
func (h *Hub) Publish(ev Event)                                   // non-blocking select; drops on full
func (h *Hub) SubscriberCount(runID uuid.UUID) int                // test aid
```

Key invariants:

- Buffer size = `64` per subscriber (hub.go:50).
- `Publish` is `select { case s.ch <- ev: default: /* drop */ }` (hub.go:86–90). The handler reconciles from the DB so dropped events only hurt smoothness.
- `unsub` removes the sub, closes its channel, and prunes the runID entry when empty (hub.go:58–69) — no leak on disconnect.
- `Event` is `struct{ RunID, TaskID, SiteID uuid.UUID; TargetType, TargetSlug, Status, FromVersion, ToVersion, Detail, RunStatus string }` (hub.go:11–24) — strongly typed to the update domain.

### 1.2 HTTP handler (`apps/api/internal/update/handler.go`)

```go
r.GET("/updates/:runId/events",
    authz.RequirePermission(authz.PermSiteRead), h.events)
```

The `events` handler at lines 123–194 does:

1. `domain.TenantIDFromContext` → 403 `tenant_required` if missing. Tenant scope comes from session/middleware; SSE inherits it.
2. Parse `runId` UUID → 400 `invalid_run_id`.
3. **Re-fetch the run (tenant-scoped)** via `h.svc.GetRun(ctx, tenantID, runID)` — this 404s cleanly *before* headers flush. After headers flush no JSON error is possible.
4. Assert `http.Flusher` → 500 `sse_unsupported`.
5. **Subscribe BEFORE writing the snapshot** so no transition is missed in the gap (handler.go:149–151).
6. Set headers `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`, `X-Accel-Buffering: no`.
7. Emit an initial snapshot: one `event: task` frame per task in the run.
8. If `run.Status == RunCompleted` return immediately.
9. Loop on `select { ctx.Done / ticker(15s) / ch }`:
   - `ctx.Done()` → return (client disconnected; `defer unsub()` cleans up).
   - ticker → write `":\n\n"` keep-alive comment + Flush.
   - event → `writeEvent`, Flush, and **return on `RunCompleted`**.

```go
// writeEvent serializes an Event as a single SSE "data:" frame.
func writeEvent(w gin.ResponseWriter, ev Event) {
    payload, _ := json.Marshal(ev)
    _, _ = w.Write([]byte("event: task\ndata: "))
    _, _ = w.Write(payload)
    _, _ = w.Write([]byte("\n\n"))
}
```

Note: `event: task` is the SSE event-type, but the frontend uses `onmessage` (not `addEventListener("task", ...)`), so the event-type is currently informational. Switching to typed listeners on the FE would be a one-line change.

### 1.3 Publish side (worker → hub) (`apps/api/internal/update/worker.go`)

```go
func (w *Worker) publish(task Task, runStatus string) {
    if w.hub == nil { return }   // hub-optional for tests
    w.hub.Publish(Event{RunID: ..., TaskID: ..., SiteID: ..., Status: ..., RunStatus: ...})
}
```

Called at exactly two points:

- `worker.go:142` — after `MarkTaskRunning` succeeds, publish with `RunRunning`.
- `worker.go:238` — inside `finish` (terminal state), publish with the now-possibly-`RunCompleted` runStatus.

### 1.4 Wiring (`apps/api/cmd/wpmgr/main.go`)

```go
updateHub := update.NewHub()                                         // line 185
updateRepo := update.NewRepo(pool)
updateWorker := update.NewWorker(updateRepo, sitesLookup, commander,
    prober, updateHub, auditRec, logger, cfg.Update.PerTenantParallelism)  // line 188
...
updateH := update.NewHandler(updateSvc, updateHub, auditRec)         // line 330
```

Single shared hub; same instance passed to the worker (publisher) and the HTTP handler (subscriber).

### 1.5 What is NOT implemented (M3 deferred or never needed)

| Concern | Status |
|---|---|
| `Last-Event-ID` / replay from seq | Not implemented. Reconciliation is via re-reading the DB snapshot on every (re)connect. Search `grep -n "Last-Event-ID"` across `update/` → zero hits. |
| Per-run subscriber cap | Not implemented. `Subscribe` always succeeds; no max. |
| Cross-tenant authz on the stream | Enforced ONCE at connect time via `svc.GetRun(ctx, tenantID, runID)` (handler.go:137). The hub itself has no notion of tenant; the hub key is the runID which is globally unique. A user from tenant B who somehow learned tenant A's runID would still be 404'd at the service layer before subscription. |
| Encoding | Strict SSE: `event: task\ndata: <json>\n\n`. Heartbeat is `:\n\n`. |
| Reconnect on the server side | Standard EventSource auto-reconnect; on a new connection the handler re-emits the full snapshot (idempotent). |

### 1.6 OpenAPI documentation

`packages/openapi/openapi.yaml`:

- Path entry `/api/v1/updates/{runId}/events` at lines 919–947 (operationId `streamUpdateRunEvents`), with a `text/event-stream` response whose schema references `UpdateEvent`.
- `UpdateEvent` component schema at lines 1888–1918.
- `apps/api/ogen.yaml` opts out via `ignore_not_implemented: ["unsupported content types"]` so codegen still works.

The schema is documented *for clients*; the Gin handler is hand-rolled and not generated.

---

## 2. Frontend (TS) — file & line index

| Concern | File | Lines |
|---|---|---|
| `EventSource` construction, lifecycle, fallback signal | `apps/web/src/features/updates/use-updates.ts` | 178–240 |
| Cache patch (TanStack `setQueryData`) | `apps/web/src/features/updates/use-updates.ts` | 118–164 |
| Polling fallback hook (`refetchInterval`) | `apps/web/src/features/updates/use-updates.ts` | 63–83 |
| Route-level UI integration | `apps/web/src/routes/_authed/updates/$runId.tsx` | 36–54 |
| Vite proxy that forwards `/api/*` to API origin | `apps/web/vite.config.ts` | 26–30 |

### 2.1 EventSource wiring (`apps/web/src/features/updates/use-updates.ts`)

```ts
export function useRunEventStream(runId, { enabled, onState }) {
  useEffect(() => {
    if (!enabled) return;
    if (typeof EventSource === "undefined") {
      onStateRef.current?.("polling"); return;     // SSR / unsupported
    }
    const url = `/api/v1/updates/${encodeURIComponent(runId)}/events`;
    const source = new EventSource(url, { withCredentials: true });

    source.onopen   = () => onStateRef.current?.("live");
    source.onmessage = (msg) => {
      const event = JSON.parse(msg.data) as UpdateEvent;
      patchEvent(queryClient, runId, event);
      if (event.run_status === "completed") {
        closed = true; source.close(); onStateRef.current?.("closed");
      }
    };
    source.onerror = () => {
      // EventSource auto-reconnects, but a hard failure (proxy/unsupported)
      // means we should stop relying on it and let polling take over.
      if (closed) return;
      closed = true; source.close(); onStateRef.current?.("polling");
    };

    return () => { closed = true; source.close(); };   // cleanup on unmount
  }, [enabled, runId, queryClient]);
}
```

Solved gotchas worth preserving for backups:

- `withCredentials: true` — required so the same-origin session cookie is sent (Vite proxy makes `/api/*` same-origin in dev; in prod the API and SPA are co-served).
- `onStateRef` ref pattern (lines 185–191) — keeps the effect from tearing down on every parent re-render with a new `onState` closure.
- `closed` guard set in `onmessage`/`onerror`/cleanup prevents double-close races.
- Terminal-state detection from the event payload itself (`run_status === "completed"`) lets the FE close cleanly; no separate close frame needed.
- On first hard `onerror`, the hook flips to `"polling"`. Caller (the route component) reads that and toggles `useUpdateRun({ poll: true })`, which sets `refetchInterval: 2000` until status is terminal.
- `applyEvent` (lines 118–153) merges a delta into the cached run by `task_id`, *appending* if unseen — important if the snapshot frame races a delta after reconnect.

### 2.2 Route integration (`apps/web/src/routes/_authed/updates/$runId.tsx:41–54`)

```ts
const [streamState, setStreamState] = useState<RunStreamState>("connecting");
const poll = streamState === "polling";
const { data: run, ... } = useUpdateRun(runId, { poll });

useRunEventStream(runId, {
  enabled: run?.status !== "completed",
  onState: setStreamState,
});
```

`enabled: run?.status !== "completed"` keeps a re-mounted detail page from re-opening a stream for a finished run.

---

## 3. Proposal: add a parallel topic for M5.6 backup progress

### 3.1 Reuse vs. fresh hub

The existing `update.Hub` is **strongly typed to the update domain** (`Event{RunID, TaskID, TargetType, FromVersion, ToVersion, ...}`). Two options:

**A. Generic Hub keyed by `(topic string, id uuid.UUID)`** — would require refactoring `update.Hub` to carry an opaque payload (`[]byte` or `any`) and forces a type assertion on the subscribe side. Higher blast radius and ogen schemas would no longer enforce the payload shape per topic.

**B. (Recommended) A second, domain-specific hub in the `backup` package.** The Hub implementation is ~60 lines of straightforward pub/sub; cloning it is cheaper than genericizing it and keeps the BackupEvent schema independent. Backup events are coarser-grained (phases, not per-task transitions) so the payload shapes diverge anyway.

Go with **B**: copy the pattern, don't generalize prematurely. If a third domain wants SSE we can extract a generic primitive then.

### 3.2 Concrete changes

**New file `apps/api/internal/backup/hub.go`**: mirror `update/hub.go` 1-for-1. Type `BackupEvent` carries `SnapshotID, SiteID uuid.UUID, Phase string, PhaseDetail json.RawMessage, Status string, Error string, ProgressUpdatedAt time.Time`. Subscribe keyed by `snapshotID`. Same 64-buffer + drop-on-full + unsub cleanup.

**Edit `apps/api/internal/backup/service.go`** at `RecordProgress` (line 397): after `s.repo.UpdateSnapshotProgress(...)` succeeds, call `s.hub.Publish(BackupEvent{...})`. Also publish at the existing `MarkRunning` (line 352), `FailSnapshot` (line 357), and the completion path in `service.go` / `BackupWorker` (find equivalent "SetSnapshotCompleted"). Make `s.hub` optional (nil-check) so existing tests don't need touching.

**Edit `apps/api/internal/backup/handler.go`** at `Register` (line 33): add
```go
r.GET("/backups/:snapshotId/events",
    authz.RequirePermission(authz.PermSiteRead), h.events)
```
Implement `h.events` as a copy of `update/handler.go:123–194` with two changes: (a) snapshot-scope authz via `h.svc.GetSnapshot(ctx, tenantID, snapshotID)` (404s before flush), (b) emit a single initial snapshot frame from the current `Snapshot.Progress` + `Status`, then stream live `BackupEvent`s, terminating on `status in (completed, failed)`.

**Edit `apps/api/cmd/wpmgr/main.go`** around line 238: `backupHub := backup.NewHub()`, thread it into `backup.NewService(...)` (extend signature) and `backup.NewHandler(...)`.

**Edit `packages/openapi/openapi.yaml`**: add path `/api/v1/backups/{snapshotId}/events` mirroring lines 919–947, with response `text/event-stream` referencing a new `BackupEvent` component schema. `ogen.yaml` already waives the unsupported content type so no codegen tweak is needed.

**New TS file `apps/web/src/features/backups/use-backup-stream.ts`** (or extend an existing `use-backups.ts` if present): port `useRunEventStream` 1-for-1. Hit `/api/v1/backups/${snapshotId}/events`, patch `backupKeys.detail(snapshotId)` in the TanStack cache, terminate on `status in (completed, failed)`, fall back to `useBackup({ poll: true })` (will need a `poll` option on the existing `useBackup` hook mirroring `useUpdateRun`).

**Confirm route namespacing**: `/api/v1/backups/{snapshotId}/events` slots cleanly alongside the existing `GET /api/v1/backups/{snapshotId}` (yaml lines 996–1015) and `POST /api/v1/backups/{snapshotId}/restore`. Use `snapshotId` (the existing parameter at yaml line 1235) for consistency.

### 3.3 Gotchas already solved by M3 to inherit verbatim

1. **Subscribe before snapshot** (`handler.go:149–151`) — prevents missing a transition in the gap between snapshot read and live subscribe.
2. **Re-fetch DB row before opening stream** for 404/403 mapping — *must* happen pre-flush.
3. **`X-Accel-Buffering: no`** header — disables nginx-style buffering of the stream.
4. **15 s heartbeat as a comment line** (`":\n\n"`) — keeps intermediaries from closing idle connections without triggering the FE `onmessage` parser.
5. **Best-effort delivery** (non-blocking `Publish` with drop on full buffer) — slow consumers cannot back up the worker.
6. **Defer `unsub()`** on every handler return path — guarantees the hub map shrinks when clients disconnect; covered by `TestHubUnsubscribeClosesChannelAndDropsRun` (hub_test.go:49–61).
7. **Terminal-status detection inside the payload** lets both server and client tear down without an explicit close frame.
8. **FE `closed` flag + `onStateRef`** — prevents tear-down races and avoids re-running the effect on closure identity changes.
9. **Same-origin Vite proxy + `withCredentials`** — cookie auth "just works" without an Authorization header (EventSource cannot set headers).
10. **Polling fallback path** on `onerror` — covers proxies that strip SSE.

### 3.4 Carry-forward debt (decide whether to fix in M5.6 or defer again)

- No `Last-Event-ID` replay. Acceptable for backups: re-reading the snapshot's progress JSONB on (re)connect is authoritative and cheap.
- No per-(run|snapshot) subscriber cap. Same gap exists for backups; a hostile user with viewer+ could fan out subscriptions. Same risk surface as M3 — defer unless M5.6 explicitly raises it.
- The `event:` line is unused by the FE. If you anticipate multiple event-types per stream (e.g. `phase` vs. `completed` vs. `error`), wire `addEventListener("phase", …)` on the FE this time instead of `onmessage`.

---

## Quick file pointer summary

- Backend hub: `/Users/mosamgor/Desktop/Terminal/wpmgr/apps/api/internal/update/hub.go`
- Backend SSE handler: `/Users/mosamgor/Desktop/Terminal/wpmgr/apps/api/internal/update/handler.go` (`events` at L123)
- Backend publish call sites: `/Users/mosamgor/Desktop/Terminal/wpmgr/apps/api/internal/update/worker.go` (L142, L238, L273–289)
- Backend wiring: `/Users/mosamgor/Desktop/Terminal/wpmgr/apps/api/cmd/wpmgr/main.go` (L185, L188, L330)
- Backend route mount: `/Users/mosamgor/Desktop/Terminal/wpmgr/apps/api/internal/server/server.go` (L124–126)
- OpenAPI path: `/Users/mosamgor/Desktop/Terminal/wpmgr/packages/openapi/openapi.yaml` (L919–947, schema L1888–1918)
- ogen waiver: `/Users/mosamgor/Desktop/Terminal/wpmgr/apps/api/ogen.yaml`
- FE hook: `/Users/mosamgor/Desktop/Terminal/wpmgr/apps/web/src/features/updates/use-updates.ts` (L178–240)
- FE route consumer: `/Users/mosamgor/Desktop/Terminal/wpmgr/apps/web/src/routes/_authed/updates/$runId.tsx` (L36–54)
- Backup service progress sink (publish point to add): `/Users/mosamgor/Desktop/Terminal/wpmgr/apps/api/internal/backup/service.go` (L397 `RecordProgress`, L352 `MarkRunning`, L357 `FailSnapshot`)
- Backup handler `Register` (to add `/events`): `/Users/mosamgor/Desktop/Terminal/wpmgr/apps/api/internal/backup/handler.go` (L33)
