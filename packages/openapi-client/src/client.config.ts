import type { CreateClientConfig } from "./generated/client.gen";

// Hey API runtime config hook. The generated client imports this and merges it
// over its defaults at module init. We default the baseUrl to "/api" so that
// requests flow through the Vite dev proxy (vite.config.ts proxies /api ->
// the backend). The consuming app can override via `client.setConfig(...)`.
//
// `credentials: "include"` makes the underlying fetch send (and accept) the
// HttpOnly `wpmgr_session` cookie on every request, which is how the backend
// authenticates the session (M1 — cookie auth, no X-Tenant-ID header).
export const createClientConfig: CreateClientConfig = (config) => ({
  ...config,
  baseUrl: "/api",
  credentials: "include",
});
