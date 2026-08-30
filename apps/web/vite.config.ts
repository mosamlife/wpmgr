import { fileURLToPath, URL } from "node:url";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { tanstackRouter } from "@tanstack/router-plugin/vite";

// The forwarded-path list lives in src/ so a test can read it and check it
// against the paths the Go server actually mounts. See
// src/lib/dev-proxy-paths.test.ts -- an untested proxy list is what let
// three root-mounted routes ship unreachable.
import { DEV_PROXY_PATHS } from "./src/lib/dev-proxy-paths";

// Build version is injected via the `BUILD_VERSION` env var at vite-build
// time (set by Cloud Build). `apps/web/src/lib/build.ts` reads it as
// `__BUILD_VERSION__`. Local `vite dev` and unsuffixed `vite build` runs fall
// back to "dev" / "local" so the dashboard never lies about what's deployed.
const buildVersion =
  process.env.BUILD_VERSION ??
  (process.env.NODE_ENV === "production" ? "local" : "dev");

// https://vite.dev/config/
export default defineConfig({
  define: {
    __BUILD_VERSION__: JSON.stringify(buildVersion),
  },
  plugins: [
    // File-based routing (ADR: TanStack Router, no SSR). Must come BEFORE the
    // react plugin. Generates src/routeTree.gen.ts from files in src/routes/.
    tanstackRouter({ target: "react", autoCodeSplitting: true }),
    react(),
    // Tailwind v4 via the first-party Vite plugin (no PostCSS config needed).
    tailwindcss(),
  ],
  resolve: {
    alias: {
      // Mirror tsconfig paths: @/* -> src/*
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  server: {
    port: 5173,
    // Proxy every backend surface to the API. The client uses same-origin
    // relative paths (baseUrl ""), so a surface that is not forwarded here is
    // answered by the SPA's index.html with a 200 and an HTML body -- which no
    // client reports as an error. DEV_PROXY_PATHS carries the list and its
    // test checks it against what the Go server mounts.
    proxy: Object.fromEntries(
      DEV_PROXY_PATHS.map((p) => [
        p,
        {
          target: process.env.VITE_API_BASE_URL ?? "http://localhost:8080",
          changeOrigin: true,
        },
      ]),
    ),
  },
  preview: {
    port: 5173,
  },
});
