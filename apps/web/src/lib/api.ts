// App-side wiring for the generated @wpmgr/api client.
//
// The generated client already defaults its baseUrl to "/api" (see
// packages/openapi-client/src/client.config.ts), which Vite proxies to the
// backend (vite.config.ts). We re-assert it here so the intent is explicit and
// lives next to the rest of the data layer. App code imports operations and
// types from "@wpmgr/api" — never from its ./generated internals.
import { client } from "@wpmgr/api";

export function configureApiClient(): void {
  // Empty baseUrl: operation paths already include their real prefixes
  // (/auth/*, /api/v1/*, /enroll, …) and are served from the same origin,
  // routed to the backend by nginx (prod) / the Vite proxy (dev).
  client.setConfig({
    baseUrl: "",
    credentials: "include",
  });
}

export type {
  Site,
  SiteList,
  SiteComponent,
  SiteComponents,
  PairingCode,
  PairingCodeCreate,
  ApiError,
} from "@wpmgr/api";
