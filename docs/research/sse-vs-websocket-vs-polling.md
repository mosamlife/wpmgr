# SSE vs WebSocket vs HTTP Polling — Head-to-Head for Backup Live Progress (CP → Browser)

Date: 2026-05-28. Decision-grade comparison for the WPMgr M5.6 backup-progress live UI. **Out of scope:** the agent → control-plane (CP) channel — that is always signed HTTP POST. This document is only about CP → browser.

Inputs: `docs/research/sse-infrastructure-map.md`, `docs/research/sse-tanstack-query-integration.md`, `docs/research/backup-live-progress-ux.md`, and the M3 production code in `apps/api/internal/update/{hub,handler}.go` + `apps/web/src/features/updates/use-updates.ts`.

---

## Use case recap (numeric)

- **Event source rate:** ~3 events/s during encrypt phase, ~0.3 events/s during archiving.
- **Payload size:** ~150 B JSON (`phase`, `chunks_done`, `chunks_total`, `bytes_done`, `current_artifact`).
- **Backup duration:** 5–30 min.
- **Concurrency:** typically 1 viewer; agency ceiling ~10 simultaneous backups, each with ~1 viewer.
- **Network path:** Cloudflare tunnel → nginx (Docker) → Go/Gin. We already proved M3 SSE survives this end-to-end.
- **Auth:** session cookie, same-origin via Vite proxy in dev and co-served in prod.

---

## The comparison table

Bandwidth assumes 150 B JSON payloads. SSE/WebSocket bandwidths use the actual event rate (avg ~1.5 events/s over a 30-min run). Polling rows show bandwidth for **one** snapshot fetch every N ms with a ~200 B response envelope and ~400 B of request+response headers (HTTP/2, cookie compressed by HPACK).

| Criterion | **SSE** | **WebSocket** | **Poll 250 ms** | **Poll 500 ms** | **Poll 1000 ms** |
|---|---|---|---|---|---|
| **1. Latency (server event → browser paint)** | 10–80 ms (TCP RTT + one flush) | 5–60 ms (no HTTP framing per msg) | 0–250 ms (avg 125) | 0–500 ms (avg 250) | 0–1000 ms (avg 500) |
| **2. Server bandwidth / backup / minute** | ~14 KB (≈90 events × 150 B + heartbeat) | ~12 KB (2-byte WS frame overhead) | ~144 KB (240 req × 600 B) | ~72 KB | ~36 KB |
| **3. Open server connections / backup** | 1 (long-lived HTTP) | 1 (long-lived TCP, Upgrade) | 0 between polls (1 in flight every 250 ms) | 0 between polls | 0 between polls |
| **4. CPU on CP / 10 concurrent backups** | **1×** (10 idle goroutines blocked on channel; near-zero) | 1× (same: 10 goroutines, gorilla/websocket) | ~4× (40 req/s, full Gin middleware chain each) | ~2× | ~1× (10 req/s, comparable to SSE) |
| **5. Implementation cost in our codebase** | **~250 LoC, ~4 h, 4 files** — 1-for-1 clone of `update/hub.go` + `update/handler.go` events route; `apps/web/src/features/backups/use-backup-stream.ts` mirrors `useRunEventStream`. Pattern is *already proven* in M3. | ~600 LoC, ~12 h, 6 files — add `gorilla/websocket` dep, hand-roll auth handshake (cookie-on-Upgrade), reconnect/backoff client, message framing convention. No prior pattern in our codebase. | ~120 LoC, ~3 h, 3 files — new `GET /backups/{id}/progress` endpoint returning current `Snapshot.Progress`; FE uses TanStack `refetchInterval`. | Same as 250 ms | Same as 250 ms |
| **6. Cloudflare tunnel** | Yes. `cloudflared` proxies `text/event-stream` transparently; no max-duration kill unless idle (heartbeat covers that). | Yes, but Cloudflare requires Free-tier WS to be on the same hostname as HTTP and counts toward concurrent-stream limits; documented quirks around Argo + WS upgrade headers. | Trivially yes. Standard HTTP. | Yes | Yes |
| **7. nginx proxy** | **Proven** for M3 (`X-Accel-Buffering: no` already set in `update/handler.go:156`). | Needs explicit `proxy_set_header Upgrade $http_upgrade; proxy_set_header Connection "upgrade"; proxy_read_timeout 3600s;` in `nginx.conf`. Not currently configured. | Trivially yes. | Yes | Yes |
| **8. Browser API maturity** | `EventSource` (native, ES2015, all evergreen browsers, no polyfill). | `WebSocket` (native, ES2011, ubiquitous). | `fetch` + TanStack Query (we already use it everywhere). |
| **9. Auth complexity** | Cookie via `withCredentials: true` — same-origin proxy makes this free. No header support. | EventSource limitation is moot here, but if we ever need cross-origin auth we'd need a one-shot ticket endpoint. | WS upgrade carries cookies on the initial HTTP request — works, **but** subprotocol-token convention (e.g. `Sec-WebSocket-Protocol: bearer.<jwt>`) is the only way to pass headers, since native `WebSocket()` cannot set them. Adds bespoke server code. | Standard cookie auth, identical to every other API call. |
| **10. Reconnect / replay** | Native auto-reconnect in `EventSource` (3 s default). `Last-Event-ID` supported by the standard but **not implemented** in M3 — handler re-emits a full DB snapshot on reconnect (idempotent, authoritative). Good enough. | Manual: client must implement backoff + resubscribe + de-dupe. No native replay token. | Trivial: next poll is the recovery. State always comes from a DB read. |
| **11. Failure modes** | Idle-timeout from any intermediary (mitigated by 15 s heartbeat `:\n\n`); proxy buffering (mitigated by `X-Accel-Buffering: no`); FE `onerror` falls back to polling. | TCP RST, Upgrade rejected by a strict proxy, ping/pong misconfig leads to silent half-open; needs explicit liveness probing. | Stampede on tab refocus (browsers fire queued timers); 5xx during a poll is just retried. |
| **12. Scaling (100 / 1k / 10k concurrent viewers)** | 100: trivial. 1k: ~1k goroutines (~8 MB stack), one Go process handles fine. 10k: needs a per-tenant subscriber cap and possibly a Redis fan-out, but we are *years* away from this. | Same as SSE; goroutine-per-conn model. Slightly higher memory per conn (write buffer). | 100 viewers × 1 req/s = 100 req/s — fine. 1k × 1 = 1k req/s — Gin handles, but DB read amplification is real (every request hits Postgres for the snapshot). 10k = bring out caching or just switch to SSE. |
| **13. Server-side complexity** | Pub/sub hub with per-subscriber buffered channel. We already have it. Worker calls `hub.Publish(ev)` non-blocking; handler ranges on `ch`. 100 LoC. | Same pub/sub + a writer goroutine per conn for ping/pong + framing. Add `gorilla/websocket` to `go.mod`. | None: each request reads `Snapshot.Progress` from Postgres and returns it. Pure REST. |
| **14. Smooth UI feel** | Excellent. ~3 events/s feeding a CSS `transition: width 250ms linear` on the progress bar gives genuinely continuous motion. | Equivalent to SSE in practice. | 250 ms: feels live. 500 ms: smooth with CSS easing. 1000 ms: noticeable steps unless CSS easing bridges the gap (per BackWPup/MainWP analysis in `backup-live-progress-ux.md`). |
| **15. Background tab throttling** | `EventSource` is **not** throttled (browsers preserve open streams). | WebSocket **not** throttled. | `setInterval` / `setTimeout` (and therefore TanStack `refetchInterval`) **is** throttled to ≥1 s in background tabs (Chrome 1 s, Firefox 1 s, Safari aggressively). Live progress freezes when you switch tabs; jumps on refocus. |

---

## Verdict — pick **SSE**

**One-liner:** SSE is the clear winner because we already have the entire pattern in production for M3 update progress, and the backup use case is structurally identical.

### Why not polling

- WPvivid/UpdraftPlus/BackWPup all poll. The reason isn't that polling is better — it's that they live inside WordPress + PHP-FPM, where every "long-lived connection" eats a FPM worker slot. We are **Go on goroutines**: an idle SSE connection costs ~8 KB of stack and zero CPU. The constraint that forced WP plugins into polling does not apply to us.
- Polling's worst weakness here is **criterion 15** (background-tab throttling). A user watching a 20-minute backup will switch tabs. With polling, they come back to a stale UI that "catches up" on refocus — exactly the broken-feeling UX we want to avoid. SSE keeps streaming.
- Even at 250 ms, polling burns ~4× the CPU of SSE for an *inferior* perceived latency (avg 125 ms vs 30 ms with SSE).
- Bandwidth at 250 ms (144 KB/min/backup) is 10× SSE for negligible win.

### Why not WebSocket

- WebSocket is symmetric (bidirectional), but our channel is one-way (server → browser). The user does not pause/resume/abort *over the live channel* — abort is a separate idempotent `DELETE` (or `POST /cancel`).
- No prior pattern in our codebase. ~3× implementation cost vs SSE (criterion 5) for zero capability gain.
- Adds nginx config changes (criterion 7) that we don't currently need.
- Auth handshake gymnastics (criterion 9) where SSE just rides the session cookie.

### Why SSE wins

1. **We already shipped it.** `apps/api/internal/update/{hub,handler}.go` + `apps/web/src/features/updates/use-updates.ts` is a working reference implementation. Cloning it is a half-day of work (criterion 5).
2. **Proven through our exact infra stack** (Cloudflare → nginx → Gin), with the proxy-buffering and heartbeat gotchas already solved.
3. **Browser-native `EventSource`** with auto-reconnect; the FE pattern (`onmessage` → `setQueryData`, `onerror` → flip to polling) is already a hook we can copy.
4. **Background-tab-safe** (criterion 15) — the single biggest UX differentiator over polling for a 20-minute backup.
5. **Cheap fan-out at agency scale** (10 concurrent backups = ~10 idle goroutines; we could go 100× without instrumenting anything).
6. **Polling fallback is built-in** to the existing hook (`useRunEventStream` flips to `polling: true` on hard error) — we get belt-and-braces resilience for free.

### Proposed event shape for backups

Mirror M3's `Event` struct but for backups. Single SSE event-type `event: progress` (named, not `onmessage`) so we can add `event: phase` and `event: error` later without breaking the FE parser:

```go
type BackupEvent struct {
    SnapshotID      uuid.UUID `json:"snapshot_id"`
    SiteID          uuid.UUID `json:"site_id"`
    Phase           string    `json:"phase"`           // "initializing" | "db_dump" | "files" | "encrypting" | "uploading" | "finalizing"
    ChunksDone      int       `json:"chunks_done"`
    ChunksTotal     int       `json:"chunks_total"`
    BytesDone       int64     `json:"bytes_done"`
    BytesTotal      int64     `json:"bytes_total"`
    CurrentArtifact string    `json:"current_artifact,omitempty"`
    Status          string    `json:"status"`          // "running" | "completed" | "failed"
    Error           string    `json:"error,omitempty"`
    UpdatedAt       time.Time `json:"updated_at"`
}
```

---

## Concrete CP-side proposal

### Backend (`apps/api/internal/backup/`)

| Change | File | Action |
|---|---|---|
| New | `apps/api/internal/backup/hub.go` | 1-for-1 port of `update/hub.go`. Replace `Event` with `BackupEvent`, key by `snapshotID`. ~100 LoC. |
| New | `apps/api/internal/backup/hub_test.go` | Port `update/hub_test.go`. ~80 LoC. |
| Edit | `apps/api/internal/backup/service.go` | Add optional `*Hub` field. Call `s.hub.Publish(...)` from `RecordProgress` (~L397), `MarkRunning` (~L352), `FailSnapshot` (~L357), and the completion path. ~25 LoC. |
| Edit | `apps/api/internal/backup/handler.go` | In `Register`, add `r.GET("/backups/:snapshotId/events", authz.RequirePermission(authz.PermSiteRead), h.events)`. Implement `h.events` as a copy of `update/handler.go:123–194` adjusted for snapshot scope. Emit one initial snapshot frame from `Snapshot.Progress`. Terminate stream on `status in ("completed","failed")`. ~80 LoC. |
| Edit | `apps/api/cmd/wpmgr/main.go` (~L238) | `backupHub := backup.NewHub()`, thread into service constructor + handler constructor. ~3 LoC. |

### OpenAPI (`packages/openapi/openapi.yaml`)

| Change | Location | Action |
|---|---|---|
| New path | After existing `/api/v1/backups/{snapshotId}` (~L996) | `/api/v1/backups/{snapshotId}/events`, operationId `streamBackupEvents`, response `text/event-stream` referencing new `BackupEvent` component. Mirror lines 919–947 of the existing `streamUpdateRunEvents` op. |
| New schema | Components | `BackupEvent` mirroring the Go struct above. Mirror existing `UpdateEvent` at L1888–1918. |
| No change | `apps/api/ogen.yaml` | Already waives `text/event-stream` codegen via `ignore_not_implemented: ["unsupported content types"]`. |

### Frontend (`apps/web/src/features/backups/`)

| Change | File | Action |
|---|---|---|
| New | `apps/web/src/features/backups/use-backup-stream.ts` | Port `useRunEventStream` from `apps/web/src/features/updates/use-updates.ts:178–240`. URL `/api/v1/backups/${snapshotId}/events`. Patch `backupsKeys.detail(snapshotId)` cache with each event. Terminate on `status in ("completed","failed")`. ~80 LoC. |
| Edit | `apps/web/src/features/backups/use-backups.ts` | Add `poll?: boolean` option to `useBackup`, matching `useUpdateRun({ poll })`. `refetchInterval: 2000` while polling and snapshot not terminal. ~10 LoC. |
| Edit | `apps/web/src/features/backups/snapshot-progress-card.tsx` | Wire `useBackupStream(snapshotId, { enabled: status === "running" })`, render bar with `transition: width 350ms linear` for smooth fill, show phase label + current artifact. ~30 LoC. |

### Frontend route wiring

If a snapshot detail route is added (`apps/web/src/routes/_authed/backups/$snapshotId.tsx`), follow the M3 pattern at `apps/web/src/routes/_authed/updates/$runId.tsx:41–54` — local `streamState` + `poll = streamState === "polling"` + `enabled: status === "running"`.

### Total scope

- Backend: ~210 LoC across 4 files. ~3 h.
- OpenAPI: 1 path + 1 schema. ~30 min.
- Frontend: ~120 LoC across 3 files. ~2 h.

End-to-end shippable in well under a day, with zero new infrastructure decisions because every gotcha (proxy buffering, heartbeat, terminal-state detection, polling fallback, cookie auth) is already solved in M3.
