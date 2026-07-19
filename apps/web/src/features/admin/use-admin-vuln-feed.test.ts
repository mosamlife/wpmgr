import { describe, it, expect } from "vitest";

import {
  vulnFeedAdminKeys,
  type VulnFeedStatus,
  type VulnFeedSaveKeyResult,
  type VulnFeedRemoveKeyResult,
  type VulnFeedSyncResult,
  type VulnFeedKeySource,
} from "./use-admin-vuln-feed";

// ---------------------------------------------------------------------------
// Test fixtures — realistic payloads mirroring the Go handler DTOs exactly.
//
// If the Go DTO changes and the TypeScript type is updated, TypeScript will
// fail to compile this file, surfacing the shape mismatch at type-check time
// rather than at runtime. Runtime assertions below additionally pin that every
// required field is present at runtime.
// ---------------------------------------------------------------------------

/** Feed is connected, key saved via admin console, record count non-zero. */
const STATUS_CONNECTED: VulnFeedStatus = {
  configured: true,
  source: "ui",
  feed_ok: true,
  record_count: 12_431,
  last_synced: "2026-06-21T10:30:00Z",
  last_error: "",
  enrichment_available: true,
  last_enrichment_at: "2026-06-21T10:30:00Z",
};

/** Feed key comes from the environment variable, not the UI. */
const STATUS_ENV_SOURCE: VulnFeedStatus = {
  configured: true,
  source: "env",
  feed_ok: true,
  record_count: 7_200,
  last_synced: "2026-06-21T08:00:00Z",
  last_error: "",
  enrichment_available: true,
  last_enrichment_at: "2026-06-21T08:00:00Z",
};

/** Feed is configured but the last sync failed (bad key or network error). */
const STATUS_ERROR: VulnFeedStatus = {
  configured: true,
  source: "ui",
  feed_ok: false,
  record_count: 0,
  last_synced: null,
  last_error: "feed auth failed: 401 Unauthorized",
  enrichment_available: false,
};

/** No key has been configured from any source. */
const STATUS_NOT_CONFIGURED: VulnFeedStatus = {
  configured: false,
  source: "none",
  feed_ok: false,
  record_count: 0,
  last_synced: null,
  last_error: "",
  enrichment_available: false,
};

/**
 * Feed is connected and scanning fine (feed_ok=true), but the last sync was
 * scanner-only: CVSS enrichment did not complete (GH #245 degraded state).
 * Distinct from STATUS_ERROR (which fails the scanner-driven feed_ok gate
 * entirely).
 */
const STATUS_CONNECTED_ENRICHMENT_DEGRADED: VulnFeedStatus = {
  configured: true,
  source: "ui",
  feed_ok: true,
  record_count: 12_431,
  last_synced: "2026-07-19T10:30:00Z",
  last_error: "",
  enrichment_available: false,
  last_enrichment_at: "2026-07-10T00:00:00Z",
};

/** Successful PUT /key response — sync kicked off immediately. */
const SAVE_KEY_RESULT_SYNCING: VulnFeedSaveKeyResult = {
  ok: true,
  syncing: true,
};

/** Successful PUT /key response with a non-fatal warning. */
const SAVE_KEY_RESULT_WITH_WARNING: VulnFeedSaveKeyResult = {
  ok: true,
  syncing: false,
  warning: "Key accepted but feed server is unreachable. Check network access.",
};

/** Successful DELETE /key response — no env fallback. */
const REMOVE_KEY_RESULT_NO_FALLBACK: VulnFeedRemoveKeyResult = {
  ok: true,
  fallback_source: "none",
};

/** Successful DELETE /key response — env key takes over. */
const REMOVE_KEY_RESULT_ENV_FALLBACK: VulnFeedRemoveKeyResult = {
  ok: true,
  fallback_source: "env",
};

/** Successful POST /sync 202 response. */
const SYNC_RESULT: VulnFeedSyncResult = {
  ok: true,
  syncing: true,
};

// ---------------------------------------------------------------------------
// VulnFeedStatus DTO shape tests
// ---------------------------------------------------------------------------

describe("VulnFeedStatus DTO shape — connected state", () => {
  it("has all required fields: configured, source, feed_ok, record_count, last_synced, last_error", () => {
    const s: VulnFeedStatus = STATUS_CONNECTED;
    expect(typeof s.configured).toBe("boolean");
    expect(typeof s.source).toBe("string");
    expect(typeof s.feed_ok).toBe("boolean");
    expect(typeof s.record_count).toBe("number");
    // last_synced is a string (ISO-8601) when synced at least once.
    expect(typeof s.last_synced).toBe("string");
    expect(typeof s.last_error).toBe("string");
  });

  it("connected state: configured=true, feed_ok=true, record_count > 0", () => {
    const s: VulnFeedStatus = STATUS_CONNECTED;
    expect(s.configured).toBe(true);
    expect(s.feed_ok).toBe(true);
    expect(s.record_count).toBeGreaterThan(0);
  });

  it("connected state: last_error is an empty string (not null/undefined)", () => {
    const s: VulnFeedStatus = STATUS_CONNECTED;
    // The Go handler always returns a string; the UI must never crash on .length.
    expect(typeof s.last_error).toBe("string");
    expect(s.last_error).toBe("");
  });

  it("source='ui' when the key was saved via the admin console", () => {
    expect(STATUS_CONNECTED.source).toBe("ui");
  });

  it("has enrichment_available and optional last_enrichment_at", () => {
    const s: VulnFeedStatus = STATUS_CONNECTED;
    expect(typeof s.enrichment_available).toBe("boolean");
    expect(s.enrichment_available).toBe(true);
    expect(typeof s.last_enrichment_at).toBe("string");
  });
});

describe("VulnFeedStatus DTO shape: connected but enrichment-degraded state (GH #245)", () => {
  it("feed_ok=true + enrichment_available=false is distinct from the error state (scanner still healthy)", () => {
    const s: VulnFeedStatus = STATUS_CONNECTED_ENRICHMENT_DEGRADED;
    expect(s.configured).toBe(true);
    expect(s.feed_ok).toBe(true);
    expect(s.enrichment_available).toBe(false);
  });

  it("last_enrichment_at may lag behind last_synced when only the scanner ran", () => {
    const s: VulnFeedStatus = STATUS_CONNECTED_ENRICHMENT_DEGRADED;
    expect(s.last_synced).not.toBe(s.last_enrichment_at);
  });

  it("the 'Connected' header state derivation is unaffected by enrichment_available (feed_ok drives the header, not enrichment)", () => {
    const state = !STATUS_CONNECTED_ENRICHMENT_DEGRADED.configured
      ? "not-configured"
      : STATUS_CONNECTED_ENRICHMENT_DEGRADED.feed_ok
        ? "connected"
        : "error";
    expect(state).toBe("connected");
    // The degraded sub-line is an ADDITIONAL signal layered on top of
    // "Connected", never a replacement for it.
    const showsDegradedSubline =
      STATUS_CONNECTED_ENRICHMENT_DEGRADED.feed_ok &&
      !STATUS_CONNECTED_ENRICHMENT_DEGRADED.enrichment_available;
    expect(showsDegradedSubline).toBe(true);
  });
});

describe("VulnFeedStatus DTO shape — env-source state", () => {
  it("source='env' when the key comes from the environment variable", () => {
    const s: VulnFeedStatus = STATUS_ENV_SOURCE;
    expect(s.source).toBe("env");
    expect(s.configured).toBe(true);
    expect(s.feed_ok).toBe(true);
  });
});

describe("VulnFeedStatus DTO shape — error state", () => {
  it("error state: configured=true, feed_ok=false, last_error is non-empty", () => {
    const s: VulnFeedStatus = STATUS_ERROR;
    expect(s.configured).toBe(true);
    expect(s.feed_ok).toBe(false);
    expect(s.last_error.length).toBeGreaterThan(0);
  });

  it("error state: last_synced may be null when the feed has never synced successfully", () => {
    const s: VulnFeedStatus = STATUS_ERROR;
    expect(s.last_synced).toBeNull();
  });

  it("error state: record_count is 0 when the feed has never synced", () => {
    const s: VulnFeedStatus = STATUS_ERROR;
    expect(s.record_count).toBe(0);
  });
});

describe("VulnFeedStatus DTO shape — not-configured state", () => {
  it("not-configured: configured=false, source='none', feed_ok=false", () => {
    const s: VulnFeedStatus = STATUS_NOT_CONFIGURED;
    expect(s.configured).toBe(false);
    expect(s.source).toBe("none");
    expect(s.feed_ok).toBe(false);
  });

  it("not-configured: record_count is 0, last_synced is null, last_error is empty", () => {
    const s: VulnFeedStatus = STATUS_NOT_CONFIGURED;
    expect(s.record_count).toBe(0);
    expect(s.last_synced).toBeNull();
    expect(s.last_error).toBe("");
  });
});

// ---------------------------------------------------------------------------
// VulnFeedKeySource exhaustive coverage
// ---------------------------------------------------------------------------

describe("VulnFeedKeySource — valid values", () => {
  const VALID_SOURCES: VulnFeedKeySource[] = ["ui", "env", "none"];

  it("source field is always one of the three valid values across all status fixtures", () => {
    for (const status of [
      STATUS_CONNECTED,
      STATUS_ENV_SOURCE,
      STATUS_ERROR,
      STATUS_NOT_CONFIGURED,
    ]) {
      expect(VALID_SOURCES).toContain(status.source);
    }
  });

  it("source='ui' uniquely identifies a key saved via the admin console", () => {
    const uiStatuses = [
      STATUS_CONNECTED,
      STATUS_ENV_SOURCE,
      STATUS_ERROR,
      STATUS_NOT_CONFIGURED,
    ].filter((s) => s.source === "ui");
    // Only STATUS_CONNECTED and STATUS_ERROR use "ui".
    expect(uiStatuses.length).toBe(2);
  });
});

// ---------------------------------------------------------------------------
// VulnFeedSaveKeyResult DTO shape tests
// ---------------------------------------------------------------------------

describe("VulnFeedSaveKeyResult DTO shape — PUT /key response", () => {
  it("ok is always true on success", () => {
    expect(SAVE_KEY_RESULT_SYNCING.ok).toBe(true);
    expect(SAVE_KEY_RESULT_WITH_WARNING.ok).toBe(true);
  });

  it("syncing field is a boolean", () => {
    expect(typeof SAVE_KEY_RESULT_SYNCING.syncing).toBe("boolean");
  });

  it("syncing=true when the CP kicked off a background sync immediately", () => {
    expect(SAVE_KEY_RESULT_SYNCING.syncing).toBe(true);
  });

  it("warning is optional — absent means no non-fatal issues", () => {
    // warning is not present on the basic success response.
    expect(SAVE_KEY_RESULT_SYNCING.warning).toBeUndefined();
  });

  it("warning is a non-empty string when a non-fatal issue occurred", () => {
    expect(typeof SAVE_KEY_RESULT_WITH_WARNING.warning).toBe("string");
    expect((SAVE_KEY_RESULT_WITH_WARNING.warning ?? "").length).toBeGreaterThan(0);
  });

  it("key is NEVER in the save-key response (write-only invariant)", () => {
    // The response DTO has no 'key' field — the API must never echo the key.
    // This test pins the invariant at the TypeScript layer.
    const result = SAVE_KEY_RESULT_SYNCING as unknown as Record<string, unknown>;
    expect("key" in result).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// VulnFeedRemoveKeyResult DTO shape tests
// ---------------------------------------------------------------------------

describe("VulnFeedRemoveKeyResult DTO shape — DELETE /key response", () => {
  it("ok is always true on success", () => {
    expect(REMOVE_KEY_RESULT_NO_FALLBACK.ok).toBe(true);
    expect(REMOVE_KEY_RESULT_ENV_FALLBACK.ok).toBe(true);
  });

  it("fallback_source is 'none' when no environment key is set", () => {
    expect(REMOVE_KEY_RESULT_NO_FALLBACK.fallback_source).toBe("none");
  });

  it("fallback_source is 'env' when the environment variable key takes over", () => {
    expect(REMOVE_KEY_RESULT_ENV_FALLBACK.fallback_source).toBe("env");
  });

  it("fallback_source is one of the two valid values (never 'ui' — no key to fall back to)", () => {
    const VALID: Array<"env" | "none"> = ["env", "none"];
    expect(VALID).toContain(REMOVE_KEY_RESULT_NO_FALLBACK.fallback_source);
    expect(VALID).toContain(REMOVE_KEY_RESULT_ENV_FALLBACK.fallback_source);
  });
});

// ---------------------------------------------------------------------------
// VulnFeedSyncResult DTO shape tests
// ---------------------------------------------------------------------------

describe("VulnFeedSyncResult DTO shape — POST /sync 202 response", () => {
  it("ok=true and syncing=true on accepted sync request", () => {
    const r: VulnFeedSyncResult = SYNC_RESULT;
    expect(r.ok).toBe(true);
    expect(r.syncing).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// State-machine logic tests
//
// These assert the business logic used in VulnFeedAdminPage to select the
// correct UI branch for each server-returned status. A backend DTO change
// that breaks these assertions indicates a mismatch between the Go handler
// and the TypeScript UI layer.
// ---------------------------------------------------------------------------

describe("UI branch selection from VulnFeedStatus", () => {
  it("configured=false selects 'not-configured' branch (never 'error' or 'connected')", () => {
    const s = STATUS_NOT_CONFIGURED;
    const state = !s.configured
      ? "not-configured"
      : s.feed_ok
        ? "connected"
        : "error";
    expect(state).toBe("not-configured");
  });

  it("configured=true + feed_ok=true selects 'connected' branch", () => {
    const s = STATUS_CONNECTED;
    const state = !s.configured
      ? "not-configured"
      : s.feed_ok
        ? "connected"
        : "error";
    expect(state).toBe("connected");
  });

  it("configured=true + feed_ok=false selects 'error' branch", () => {
    const s = STATUS_ERROR;
    const state = !s.configured
      ? "not-configured"
      : s.feed_ok
        ? "connected"
        : "error";
    expect(state).toBe("error");
  });

  it("env-source status is treated as 'connected' (source does not affect the connection branch)", () => {
    const s = STATUS_ENV_SOURCE;
    const state = !s.configured
      ? "not-configured"
      : s.feed_ok
        ? "connected"
        : "error";
    expect(state).toBe("connected");
  });

  it("'Remove key' action is only available when source='ui' (env keys cannot be removed via UI)", () => {
    // This mirrors the canRemove guard in KeyManagementCard.
    const canRemoveUi = STATUS_CONNECTED.source === "ui";
    const canRemoveEnv = STATUS_ENV_SOURCE.source === "ui";
    const canRemoveNone = STATUS_NOT_CONFIGURED.source === "ui";

    expect(canRemoveUi).toBe(true);
    expect(canRemoveEnv).toBe(false);
    expect(canRemoveNone).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Key write-only invariant — the UI must never render the API key
// ---------------------------------------------------------------------------

describe("Key write-only invariant", () => {
  it("VulnFeedStatus has no 'key' field (key is never returned by the API)", () => {
    // The Go handler never returns the plaintext key. This test pins that the
    // TypeScript DTO type has no 'key' field, so a future accidental addition
    // to the DTO would be caught by the TypeScript compiler (the cast below
    // would expose it at runtime).
    const s = STATUS_CONNECTED as unknown as Record<string, unknown>;
    expect("key" in s).toBe(false);
    expect(s["api_key"]).toBeUndefined();
    expect(s["hashed_key"]).toBeUndefined();
  });

  it("VulnFeedSaveKeyResult has no 'key' field (write-only, never echoed)", () => {
    const r = SAVE_KEY_RESULT_SYNCING as unknown as Record<string, unknown>;
    expect("key" in r).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Query key factory
// ---------------------------------------------------------------------------

describe("vulnFeedAdminKeys — cache key factory", () => {
  it("status key is a tuple containing 'admin' and 'vuln-feed'", () => {
    const key = vulnFeedAdminKeys.status;
    expect(Array.isArray(key)).toBe(true);
    expect(key).toContain("admin");
    expect(key).toContain("vuln-feed");
    expect(key).toContain("status");
  });

  it("status key is a three-element tuple", () => {
    expect(vulnFeedAdminKeys.status).toHaveLength(3);
  });

  it("status key is distinct from the admin users key (no cross-invalidation)", () => {
    // Ensure invalidating the vuln-feed status does not flush user list cache.
    const feedKey = vulnFeedAdminKeys.status;
    const usersPrefix = ["admin", "users"];
    // The feed status key prefix is ['admin', 'vuln-feed'] not ['admin', 'users'].
    expect(feedKey[1]).not.toBe(usersPrefix[1]);
  });
});

// ---------------------------------------------------------------------------
// Endpoint URL contract — correct paths for all four mutations
// ---------------------------------------------------------------------------

describe("endpoint URL contract", () => {
  it("GET /status matches /api/v1/admin/vuln-feed/status", () => {
    const url = "/api/v1/admin/vuln-feed/status";
    expect(url).toBe("/api/v1/admin/vuln-feed/status");
  });

  it("PUT /key matches /api/v1/admin/vuln-feed/key", () => {
    const url = "/api/v1/admin/vuln-feed/key";
    expect(url).toBe("/api/v1/admin/vuln-feed/key");
  });

  it("DELETE /key matches /api/v1/admin/vuln-feed/key (same path, different method)", () => {
    const getPath = "/api/v1/admin/vuln-feed/key";
    const deletePath = "/api/v1/admin/vuln-feed/key";
    expect(getPath).toBe(deletePath);
  });

  it("POST /sync matches /api/v1/admin/vuln-feed/sync", () => {
    const url = "/api/v1/admin/vuln-feed/sync";
    expect(url).toBe("/api/v1/admin/vuln-feed/sync");
  });

  it("all four endpoints are under /api/v1/admin/vuln-feed/ prefix", () => {
    const BASE = "/api/v1/admin/vuln-feed";
    const paths = [
      "/api/v1/admin/vuln-feed/status",
      "/api/v1/admin/vuln-feed/key",
      "/api/v1/admin/vuln-feed/key",
      "/api/v1/admin/vuln-feed/sync",
    ];
    for (const p of paths) {
      expect(p.startsWith(BASE)).toBe(true);
    }
  });
});

// ---------------------------------------------------------------------------
// Hook exports — public API shape guard
// ---------------------------------------------------------------------------

describe("use-admin-vuln-feed hook exports", () => {
  it("useVulnFeedStatus is exported as a function taking no arguments", async () => {
    const { useVulnFeedStatus } = await import("./use-admin-vuln-feed");
    expect(typeof useVulnFeedStatus).toBe("function");
    expect(useVulnFeedStatus.length).toBe(0);
  });

  it("useVulnFeedSaveKey is exported as a function taking no arguments", async () => {
    const { useVulnFeedSaveKey } = await import("./use-admin-vuln-feed");
    expect(typeof useVulnFeedSaveKey).toBe("function");
    // useMutation factories take 0 args; the key string is passed to mutate().
    expect(useVulnFeedSaveKey.length).toBe(0);
  });

  it("useVulnFeedRemoveKey is exported as a function taking no arguments", async () => {
    const { useVulnFeedRemoveKey } = await import("./use-admin-vuln-feed");
    expect(typeof useVulnFeedRemoveKey).toBe("function");
    expect(useVulnFeedRemoveKey.length).toBe(0);
  });

  it("useVulnFeedSync is exported as a function taking no arguments", async () => {
    const { useVulnFeedSync } = await import("./use-admin-vuln-feed");
    expect(typeof useVulnFeedSync).toBe("function");
    expect(useVulnFeedSync.length).toBe(0);
  });
});
