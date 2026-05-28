import { defineConfig } from "@hey-api/openapi-ts";

// Hey API codegen config (ADR-013). Reads the contract at ./openapi.yaml and
// emits a typed fetch client + SDK into src/generated/. The generated tree is
// committed; re-run with `pnpm --filter @wpmgr/api generate`.
//
// NOTE (M5): the canonical contract lives at ../openapi/openapi.yaml and is
// owned by the backend. The M5 uptime-monitoring endpoints were not yet present
// there, so this package vendors a local copy (./openapi.yaml) that adds the
// M5 monitoring paths/schemas. When the backend lands M5 in the canonical spec,
// re-point `input` back to "../openapi/openapi.yaml" and delete the local copy.
//
// The generator stays swappable: app code never imports from
// `./generated/*` directly — everything is re-exported through src/index.ts.
export default defineConfig({
  input: "../openapi/openapi.yaml",
  output: {
    path: "src/generated",
    postProcess: ["prettier"],
  },
  plugins: [
    {
      name: "@hey-api/client-fetch",
      // The fetch client runtime is generated locally (self-contained, no npm
      // runtime dep). The base URL is supplied by the consumer (apps/web) via
      // the runtime config below so requests flow through the Vite /api proxy.
      runtimeConfigPath: "./src/client.config.ts",
    },
    // Default SDK plugin emits flat standalone operation functions
    // (listSites, getSite, deleteSite, ...) that the data hooks import directly.
    "@hey-api/sdk",
    "@hey-api/schemas",
    {
      name: "@hey-api/typescript",
      enums: "javascript",
    },
  ],
});
