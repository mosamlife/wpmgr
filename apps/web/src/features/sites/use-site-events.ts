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

// ---------------------------------------------------------------------------
// Module-level shared EventSource
// ---------------------------------------------------------------------------

const BASE_URL = "/api/v1/sites/events";
const BACKOFF_BASE_MS = 1000;
const BACKOFF_CAP_MS = 30000;

const handlers = new Set<SiteEventHandler>();
let source: EventSource | null = null;
let lastEventId: string | null = null;
let retryCount = 0;
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
let visibilityBound = false;

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
 * Test/diagnostic seam: returns whether the shared stream is currently open.
 * Not used in production UI; handy for e2e assertions.
 */
export function __isSiteStreamOpen(): boolean {
  return source !== null;
}
