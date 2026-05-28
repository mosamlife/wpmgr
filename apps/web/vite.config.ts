import { fileURLToPath, URL } from "node:url";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { tanstackRouter } from "@tanstack/router-plugin/vite";

// https://vite.dev/config/
export default defineConfig({
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
    // Proxy every backend surface to the API (mirrors the prod nginx routing).
    // The client uses same-origin relative paths (baseUrl ""), so each real API
    // prefix must be forwarded: /api/v1/*, /auth/*, /enroll, /agent/*, health.
    proxy: Object.fromEntries(
      ["/api", "/auth", "/enroll", "/agent", "/healthz", "/readyz"].map((p) => [
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
