/**
 * WPMgr RUM collector — Core Web Vitals + TTFB/FCP beacon sender.
 *
 * Reads per-site config from window.__WPMGR_RUM__ injected by the agent at
 * cache-write time, applies client-side sampling, then sends one
 * navigator.sendBeacon per finalised metric immediately inside the metric
 * callback.
 *
 * Why per-callback, not queued-flush: CLS and INP only finalise at
 * visibilitychange->hidden / pagehide. A queued-flush approach attaches its
 * own listener to the same events, creating a race: the flush can run and set
 * the flushed guard BEFORE web-vitals has pushed CLS/INP into the queue,
 * dropping those two metrics. Sending directly in the callback avoids the race
 * entirely — web-vitals fires the callback at exactly the right moment, and
 * navigator.sendBeacon is safe to call during visibilitychange/pagehide.
 *
 * Ingest contract (must match the Go control-plane POST /rum/ingest handler):
 *   key    string  plaintext public beacon key
 *   url    string  page URL with the query string AND hash stripped (origin +
 *                  pathname only — see stripQuery() below)
 *   metric string  lowercase one of: lcp | inp | cls | ttfb | fcp
 *   value  number  integer milliseconds (CLS: Math.round(value * 1000))
 *   device string  desktop | mobile | tablet
 *   conn   string  4g | 3g | 2g | slow-2g | offline | unknown
 *
 * Privacy note: the query string and hash are deliberately dropped before the
 * URL is sent. A query string can carry sensitive per-visit tokens (e.g. a
 * WooCommerce order-received key on an anonymous order-confirmation page); the
 * pathname alone still groups metrics per-page, which is all RUM needs.
 *
 * Transport: Blob("text/plain") keeps the request CORS-simple (no preflight).
 * Licensing note: web-vitals is Apache-2.0 (Google LLC). Bundled here under
 * the Apache-2.0 grant; the outer package remains MIT.
 */

import { onLCP, onINP, onCLS, onFCP, onTTFB, type Metric } from 'web-vitals';

/** Shape of the per-site config the PHP injector writes into window. */
interface WpmgrRumConfig {
  /** Plaintext public beacon key (derived by CP, stored only as hash server-side). */
  key: string;
  /** Full ingest endpoint URL, e.g. https://cp.example.com/rum/ingest */
  url: string;
  /** Client-side sample rate [0,1]. 1 = always send; 0 = never send. */
  rate: number;
}

declare global {
  interface Window {
    __WPMGR_RUM__?: WpmgrRumConfig;
  }
  interface Navigator {
    connection?: {
      effectiveType?: string;
    };
    /** Chromium-only User-Agent Client Hints API. */
    userAgentData?: {
      mobile?: boolean;
    };
  }
}

/**
 * Derive a coarse device class from the UA data API + viewport width.
 * Returns "desktop" | "mobile" | "tablet".
 */
function deviceType(): 'desktop' | 'mobile' | 'tablet' {
  try {
    // navigator.userAgentData is Chromium-only and chromium-hinted.
    if (navigator.userAgentData?.mobile) {
      // Mobile UA hint: distinguish phone vs tablet by viewport width.
      return window.innerWidth >= 768 ? 'tablet' : 'mobile';
    }
    // Fallback: coarse UA-string check for common tablet/phone keywords.
    const ua = navigator.userAgent ?? '';
    if (/tablet|ipad|playbook|silk/i.test(ua)) {
      return 'tablet';
    }
    if (/mobile|android|iphone|ipod|blackberry|iemobile|opera mini/i.test(ua)) {
      return 'mobile';
    }
    return 'desktop';
  } catch {
    return 'desktop';
  }
}

/**
 * Derive the effective connection type from the Network Information API.
 * Returns one of: "4g" | "3g" | "2g" | "slow-2g" | "offline" | "unknown".
 */
function connType(): string {
  try {
    if (!navigator.onLine) {
      return 'offline';
    }
    const et = navigator.connection?.effectiveType;
    if (et === '4g' || et === '3g' || et === '2g' || et === 'slow-2g') {
      return et;
    }
    return 'unknown';
  } catch {
    return 'unknown';
  }
}

/**
 * Strip the query string and hash from the current page URL, keeping only
 * origin + pathname. Query strings can carry sensitive per-visit tokens (a
 * WooCommerce order-received key, a nonce, a session/cart identifier passed
 * as a URL param by some checkout flows); the pathname alone is sufficient
 * for RUM's per-page metric grouping and carries no such risk.
 *
 * Falls back to the raw href (best-effort) if the URL API is unavailable,
 * which should not happen in any browser new enough to run this collector.
 */
function stripQuery(href: string): string {
  try {
    const u = new URL(href);
    return u.origin + u.pathname;
  } catch {
    return href;
  }
}

/**
 * Convert a metric value to an integer following the ingest contract:
 * - LCP/INP/FCP/TTFB: Math.round(ms) — already in milliseconds from web-vitals.
 * - CLS: Math.round(value * 1000) — stored as milli-units so the int column
 *   carries the fractional shift score without losing precision.
 */
function metricValue(name: string, value: number): number {
  if (name === 'CLS') {
    return Math.round(value * 1000);
  }
  return Math.round(value);
}

/**
 * Build a send-callback bound to a fixed config+context snapshot.
 * Called once per metric type when web-vitals fires its final report.
 * Sends the beacon immediately — no queue, no flush race.
 */
function makeSender(
  config: WpmgrRumConfig,
  pageUrl: string,
  device: string,
  conn: string,
): (m: Metric) => void {
  return (m: Metric): void => {
    try {
      const body = JSON.stringify({
        key: config.key,
        url: pageUrl,
        metric: m.name.toLowerCase(),
        value: metricValue(m.name, m.value),
        device,
        conn,
      });
      // Blob with type "text/plain" keeps the request CORS-simple (no preflight).
      const blob = new Blob([body], { type: 'text/plain' });
      navigator.sendBeacon(config.url, blob);
    } catch {
      // Fire-and-forget: never throw, never block the page.
    }
  };
}

/**
 * Bootstrap the collector.
 *
 * Guards: requires sendBeacon, requires PerformanceObserver (feature-detect),
 * requires a valid config object. All failures are silent.
 *
 * Sampling is decided once at init. If sampled out, no listeners are registered
 * and nothing is ever sent for this page load.
 */
export function init(): void {
  try {
    if (!('sendBeacon' in navigator)) {
      return;
    }
    if (typeof window.PerformanceObserver === 'undefined') {
      return;
    }

    const config = window.__WPMGR_RUM__;
    if (!config || typeof config.key !== 'string' || config.key === '') {
      return;
    }
    if (typeof config.url !== 'string' || config.url === '') {
      return;
    }

    // Client-side sampling: skip this page load if outside the sample window.
    // The server re-applies an authoritative random sample; this just saves egress.
    const rate = typeof config.rate === 'number' ? config.rate : 1;
    if (Math.random() >= rate) {
      return;
    }

    // Snapshot context once — these don't change during the page load.
    // Query string + hash are stripped (see stripQuery() docblock) so no
    // per-visit token ever leaves the browser.
    const pageUrl = stripQuery(window.location.href);
    const device = deviceType();
    const conn = connType();

    // Build a single sender bound to this page's context.
    const send = makeSender(config, pageUrl, device, conn);

    // Register all five V1 metrics. web-vitals fires each callback exactly
    // once when the metric is final. CLS and INP finalise at page-hide; the
    // callback fires at that moment and sendBeacon is safe to call then.
    // Sending here (not in a separate flush listener) is the race-free pattern.
    //
    // Registration order: onFCP before onCLS (canonical web-vitals order).
    // onCLS internally calls onFCP(runOnce(armCLS)). armCLS registers the
    // visibilityWatcher.onHidden hook that fires CLS measurement on page-hide.
    // On a cached page where FCP has already occurred, both observers use
    // PerformanceObserver({buffered:true}) and receive the FCP entry in the
    // same async delivery task (~15 ms after observer creation). By the time
    // that task fires, both microtasks — our FCP send and armCLS — are queued
    // and drain together before any browser-dispatched visibilitychange can
    // interrupt them. Registering onFCP first follows the canonical order
    // shown in the web-vitals documentation and keeps the two related FCP
    // observers adjacent in the delivery task's microtask queue.
    onTTFB(send);
    onFCP(send);
    onLCP(send);
    onCLS(send);
    onINP(send);
  } catch {
    // Defensive top-level catch: this script must never throw.
  }
}
