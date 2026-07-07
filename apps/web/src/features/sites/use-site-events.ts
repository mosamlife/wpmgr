import { useEffect } from "react";
import { z } from "zod";

import type { ConnectedSite, ConnectionState } from "./connection-state";

// Phase 5.1 — the tenant-level Sites SSE stream.
//
// One stream, many consumers. The Add-site modal subscribes BEFORE a site_id
// exists (to watch for `site.state_changed → connected`), the sites table
// subscribes to patch rows live, and the site-detail header subscribes to keep
// the connection badge fresh. Opening one EventSource per consumer would
// multiply server connections and replay windows, so we run a single
// module-level EventSource and fan events out to a Set of handlers.
//
// Wire contract (Phase 3/4 SSE handler):
//   GET /api/v1/sites/events           (session-cookie auth, tenant-scoped)
//   id: <ULID>
//   event: site.created | site.enrolled | site.heartbeat | site.state_changed
//        | site.revoked | site.disconnected | site.archived | site.restored
//   data: { id, type, tenant_id, site_id, ts, data }
//   (15s `:\n\n` keepalive comments — EventSource ignores them natively)
//
// Reconnect: on a transient drop we reopen with `?since=<lastEventId>` so the
// ~5-min replay window backfills anything missed. Backoff is exponential
// (1s,2s,4s,8s … capped at 30s) and resets on a healthy `onopen`. A
// `visibilitychange → visible` forces an immediate reconnect because browsers
// freeze background EventSources and the native reconnect can lag.
//
// Mirrors the proven `useRunEventStream` / `useBackupStream` style:
// EventSource lives in an effect (never a queryFn), frames are validated with
// Zod, malformed frames are dropped, teardown is clean.

const SITE_EVENT_TYPES = [
  "site.created",
  "site.enrolled",
  "site.heartbeat",
  "site.state_changed",
  "site.revoked",
  "site.disconnected",
  "site.archived",
  "site.restored",
  // Media Optimizer (ADR-043 §7). These are published on this SAME shared
  // tenant bus (filtered by site_id), NOT on a per-job stream. The names mirror
  // the Go constants in apps/api/internal/site/connection.go. Because the SSE
  // frame is validated with `z.enum(SITE_EVENT_TYPES)` below AND only the types
  // in this array get an `addEventListener` (see openSource), a media.* type
  // MISSING here would be silently dropped — every media event must be listed.
  "media.sync.started",
  "media.sync.completed",
  "media.optimize.started",
  "media.optimize.progress",
  "media.optimize.asset_done",
  "media.optimize.completed",
  "media.restore.started",
  "media.restore.asset_done",
  "media.restore.completed",
  "media.delete_originals.completed",
  "media.job.failed",
  "media.asset.deleted",
  // Performance Suite (Phase 7 / m36). Published on this SAME shared tenant bus
  // (filtered by site_id), mirroring the Go constants in
  // apps/api/internal/site/connection.go. As with media.*, any cache.*/rucss.*/
  // db.*/perf.* type MISSING from this array is silently dropped by the
  // z.enum(SITE_EVENT_TYPES) frame validation before it reaches usePerfEvents.
  "cache.enabled",
  "cache.disabled",
  "cache.purge.started",
  "cache.purge.completed",
  "cache.preload.started",
  "cache.preload.progress",
  "cache.preload.completed",
  "cache.stats.updated",
  "perf.config.updated",
  "db.clean.started",
  "db.clean.progress",
  "db.clean.completed",
  "db.clean.failed",
  // Database scan (Phase 2 — read-only preview before clean).
  // Synchronous on the agent; the CP emits exactly these three events.
  // Missing any one here would silently drop the frame before usePerfEvents
  // sees it — all three must be listed.
  "db.scan.started",
  "db.scan.completed",
  "db.scan.failed",
  "rucss.queued",
  "rucss.computing",
  "rucss.completed",
  "rucss.failed",
  // Orphan delete (P3.8). Async operation — started is emitted synchronously
  // before the agent command is dispatched; progress/completed/failed arrive
  // as the agent POSTs batched results back to the CP. All four must be listed
  // here or the z.enum frame validator silently drops them before they reach
  // usePerfEvents / OrphanReviewSection.
  "db.orphan.delete.started",
  "db.orphan.delete.progress",
  "db.orphan.delete.completed",
  "db.orphan.delete.failed",
  // Search-replace tool (#188). Synchronous command — two events only.
  "db.search.replace.completed",
  "db.search.replace.failed",
  // Font processing pipeline (ADR-052 Phase 2 / m55). Published on this SAME
  // shared tenant bus (filtered by site_id), mirroring the Go constants in
  // apps/api/internal/site/connection.go. As with rucss.*, any font.* type
  // MISSING from this array is silently dropped by the z.enum(SITE_EVENT_TYPES)
  // frame validation before it reaches usePerfEvents — every font event must be
  // listed. CP emits font.queued on batch enqueue, font.converting/ready/subset/
  // skipped/failed per font, and font.completed at page-build end.
  "font.queued",
  "font.converting",
  "font.ready",
  "font.subset",
  "font.skipped",
  "font.failed",
  "font.completed",
  // RUM rollup signal (Phase 3b). The CP emits this throttled aggregate frame
  // (at most once every few seconds) when the rollup worker has folded new
  // beacons into the hourly/daily rollup tables for the currently-open site.
  // The dashboard reacts by invalidating the rum / rumSummary queries — no
  // per-beacon streaming. Any new rum.* type NOT listed here is silently dropped
  // by z.enum(SITE_EVENT_TYPES) validation before it reaches usePerfEvents.
  "rum.rollup_updated",
  // Email Phase 4b live events. Published on this SAME shared tenant bus
  // (filtered by site_id, or site_id="" for fleet-wide suppressions). Any
  // email.* type MISSING from this array is silently dropped by the
  // z.enum(SITE_EVENT_TYPES) frame validation before it reaches
  // useEmailEvents — every email event must be listed.
  "email.log_ingested",
  "email.suppression_updated",
  "email.bounce",
  // m62 — org-config propagation result (tenant-wide fan-out, site_id=null)
  "email.config_propagated",
  // Object cache (m68 Phase 3 — status transitions + stats updates + lifecycle).
  // Published on this SAME shared tenant bus (filtered by site_id), mirroring
  // the Go constants in apps/api/internal/site/connection.go. Any objectcache.*
  // type MISSING from this array is silently dropped by z.enum(SITE_EVENT_TYPES)
  // frame validation before it reaches usePerfEvents — every object-cache event
  // must be listed.
  "objectcache.status_changed",
  "objectcache.stats_updated",
  "objectcache.flushed",
  "objectcache.config_applied",
  "objectcache.test_completed",
] as const;

export type SiteEventType = (typeof SITE_EVENT_TYPES)[number];

/**
 * The `data` payload for a `site.state_changed` event carries the transition
 * plus the full, post-transition site (including `connection_state`).
 */
const stateChangedDataSchema = z.object({
  from: z.string().optional(),
  to: z.string(),
  // The embedded site is the generated Site shape widened with connection
  // fields; we keep it permissive (passthrough) so a server-side field addition
  // never drops a frame. Consumers narrow via asConnectedSite where needed.
  site: z.looseObject({ id: z.string() }),
});

/** The SSE envelope, common to every named site event. */
const siteEventSchema = z.object({
  id: z.string(),
  type: z.enum(SITE_EVENT_TYPES),
  tenant_id: z.string().optional(),
  site_id: z.string(),
  ts: z.string(),
  data: z.unknown().optional(),
});

export type SiteEvent = {
  id: string;
  type: SiteEventType;
  tenant_id?: string;
  site_id: string;
  ts: string;
  data?: unknown;
};

/** Strongly-typed view of a `site.state_changed` event's data, when present. */
export interface StateChangedData {
  from?: ConnectionState;
  to: ConnectionState;
  site: ConnectedSite;
}

/**
 * Parse the `data` of a `site.state_changed` event. Returns null when the event
 * is a different type or the payload doesn't match (defensive — never throws).
 */
export function parseStateChanged(ev: SiteEvent): StateChangedData | null {
  if (ev.type !== "site.state_changed") return null;
  const result = stateChangedDataSchema.safeParse(ev.data);
  if (!result.success) return null;
  return {
    from: result.data.from as ConnectionState | undefined,
    to: result.data.to as ConnectionState,
    site: result.data.site as unknown as ConnectedSite,
  };
}

export type SiteEventHandler = (event: SiteEvent) => void;

/** Called whenever the shared stream successfully reconnects after a drop. */
export type ReconnectHandler = () => void;

// ---------------------------------------------------------------------------
// Module-level shared EventSource
// ---------------------------------------------------------------------------

const BASE_URL = "/api/v1/sites/events";
const BACKOFF_BASE_MS = 1000;
const BACKOFF_CAP_MS = 30000;

const handlers = new Set<SiteEventHandler>();
/** Subscribers that want a callback when the stream reconnects after a drop. */
const reconnectHandlers = new Set<ReconnectHandler>();
let source: EventSource | null = null;
let lastEventId: string | null = null;
let retryCount = 0;
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
let visibilityBound = false;
/**
 * True once the stream has made at least one successful open. When a
 * subsequent open fires after an error we treat it as a reconnect and notify
 * all `reconnectHandlers` so they can re-hydrate any server-state that may
 * have changed while the stream was down.
 */
let hasOpenedOnce = false;

function dispatch(event: SiteEvent): void {
  lastEventId = event.id;
  for (const handler of handlers) {
    try {
      handler(event);
    } catch {
      // A throwing consumer must not break the fan-out to the others.
    }
  }
}

function handleFrame(msg: MessageEvent<string>): void {
  // EventSource exposes the SSE `id:` line via `lastEventId`; trust it as the
  // replay cursor even when the JSON body's id is absent/garbled.
  if (msg.lastEventId) lastEventId = msg.lastEventId;
  let parsed: SiteEvent;
  try {
    const raw = JSON.parse(msg.data) as unknown;
    parsed = siteEventSchema.parse(raw);
  } catch {
    return; // keepalive / malformed frame — drop silently
  }
  dispatch(parsed);
}

function clearReconnectTimer(): void {
  if (reconnectTimer !== null) {
    clearTimeout(reconnectTimer);
    reconnectTimer = null;
  }
}

function closeSource(): void {
  if (source) {
    source.close();
    source = null;
  }
}

function buildUrl(): string {
  // Replay anything missed during the drop. The CP also accepts the
  // `Last-Event-ID` header, but EventSource sends that on its own native
  // reconnect; we add `?since=` for the explicit reconnects we drive here.
  if (lastEventId) {
    return `${BASE_URL}?since=${encodeURIComponent(lastEventId)}`;
  }
  return BASE_URL;
}

function scheduleReconnect(): void {
  clearReconnectTimer();
  const delay = Math.min(
    BACKOFF_BASE_MS * 2 ** retryCount,
    BACKOFF_CAP_MS,
  );
  retryCount += 1;
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    if (handlers.size > 0) openSource();
  }, delay);
}

function openSource(): void {
  if (typeof EventSource === "undefined") return;
  closeSource();
  clearReconnectTimer();

  const es = new EventSource(buildUrl(), { withCredentials: true });
  source = es;

  es.onopen = () => {
    retryCount = 0; // healthy connection — reset backoff
    if (hasOpenedOnce) {
      // A subsequent open after a previous drop: notify reconnect subscribers
      // so they can re-hydrate any state they may have missed while offline.
      for (const rh of reconnectHandlers) {
        try {
          rh();
        } catch {
          // A throwing reconnect handler must not break the others.
        }
      }
    }
    hasOpenedOnce = true;
  };

  // Named events: attach the same frame handler to every known type. We also
  // keep `onmessage` as a defensive fallback in case the wire ever emits a
  // default (unnamed) frame.
  for (const type of SITE_EVENT_TYPES) {
    es.addEventListener(type, handleFrame as EventListener);
  }
  es.onmessage = handleFrame;

  es.onerror = () => {
    // EventSource auto-reconnects, but a same-origin proxy hiccup can leave it
    // wedged. We take control: close and reconnect with our backoff + `?since=`
    // replay so we never silently miss the enroll event the modal is waiting
    // for. Only act while consumers remain.
    closeSource();
    if (handlers.size > 0) scheduleReconnect();
  };
}

function onVisibilityChange(): void {
  if (document.visibilityState !== "visible") return;
  if (handlers.size === 0) return;
  // Tab regained focus — the background stream may be stale or frozen. Force a
  // fresh connection that replays from the last seen id.
  retryCount = 0;
  openSource();
}

function ensureStarted(): void {
  if (!visibilityBound) {
    document.addEventListener("visibilitychange", onVisibilityChange);
    visibilityBound = true;
  }
  if (!source && reconnectTimer === null) {
    retryCount = 0;
    openSource();
  }
}

function maybeStop(): void {
  if (handlers.size > 0) return;
  closeSource();
  clearReconnectTimer();
  if (visibilityBound) {
    document.removeEventListener("visibilitychange", onVisibilityChange);
    visibilityBound = false;
  }
  // Keep `lastEventId` so a later subscriber within the replay window can
  // resume rather than cold-start.
  // Reset the reconnect sentinel so the next subscriber gets a fresh lifecycle.
  hasOpenedOnce = false;
}

/**
 * Reset the shared Sites SSE stream for a tenant-context change (e.g. the
 * active-org switch in `useActivateOrg`). The CP resolves the stream's tenant
 * ONCE at connect time and holds that resolution for the life of the
 * connection (up to the ~15 min stream lifetime), so an already-open stream
 * keeps delivering the OLD tenant's events after `queryClient.clear()` — the
 * React Query cache half of a tenant switch isn't enough for the live (push)
 * half.
 *
 * This closes the current connection, cancels any pending reconnect timer,
 * and drops the replay cursor: `lastEventId` MUST be nulled so a fresh
 * connection never resumes against the old tenant's cursor position (a stale
 * cursor from tenant A must never be replayed against tenant B). If
 * subscribers are still mounted it reopens immediately so the very next
 * frames arrive under the new tenant with no gap; if nobody is subscribed
 * this is a no-op.
 */
export function resetSiteStream(): void {
  closeSource();
  clearReconnectTimer();
  lastEventId = null;
  hasOpenedOnce = false;
  retryCount = 0;
  if (handlers.size > 0) openSource();
}

/**
 * Subscribe to the shared, tenant-level Sites SSE stream. The first subscriber
 * opens the single EventSource; the last unsubscribe tears it down. The handler
 * identity is captured in a ref-free way: we register the latest closure on
 * every render via the effect dependency, so callers may pass inline handlers.
 */
export function useSiteEvents(handler: SiteEventHandler): void {
  useEffect(() => {
    handlers.add(handler);
    ensureStarted();
    return () => {
      handlers.delete(handler);
      maybeStop();
    };
  }, [handler]);
}

/**
 * Subscribe to the shared stream reconnect signal. The callback fires whenever
 * the EventSource successfully re-opens after a previous drop (e.g. after a
 * 900 s LB cut or an API deploy). Use it to re-hydrate any server-state that
 * may have changed while the stream was down and frames were being lost.
 *
 * The first open (cold start) does NOT fire the callback — only subsequent
 * re-opens do. On unmount the subscription is automatically removed.
 */
export function useSiteReconnect(handler: ReconnectHandler): void {
  useEffect(() => {
    reconnectHandlers.add(handler);
    return () => {
      reconnectHandlers.delete(handler);
    };
  }, [handler]);
}

/**
 * Test/diagnostic seam: returns whether the shared stream is currently open.
 * Not used in production UI; handy for e2e assertions.
 */
export function __isSiteStreamOpen(): boolean {
  return source !== null;
}
