import type { CreateClientConfig } from "./generated/client.gen";

// Hey API runtime config hook. The generated client imports this and merges it
// over its defaults at module init.
//
// baseUrl is EMPTY so the generated operation paths — which already carry their
// real prefixes (/auth/*, /api/v1/*, /enroll, /agent/*, /healthz) — are requested
// verbatim against the SAME ORIGIN. The dashboard and API are served from one
// host (nginx in prod, the Vite dev proxy locally) which routes each prefix to
// the backend. getUrl uses `(baseUrl ?? "")`, so "" yields relative paths.
// (A "/api" baseUrl double-prefixed /api/v1/* -> /api/api/v1/* and mis-prefixed
// /auth/* -> /api/auth/*; it only went unnoticed because the e2e mocks used "**".)
//
// `credentials: "include"` sends/accepts the HttpOnly `wpmgr_session` cookie so
// the backend authenticates the session (M1 — cookie auth, no X-Tenant-ID).
export const createClientConfig: CreateClientConfig = (config) => ({
  ...config,
  baseUrl: "",
  credentials: "include",
});
