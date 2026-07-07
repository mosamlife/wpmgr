import { defineConfig, mergeConfig } from "vitest/config";

import viteConfig from "./vite.config";

// A SEPARATE config file (rather than a `test` key inlined into
// vite.config.ts) is deliberate: this workspace has two `vite` installs in
// its dependency graph (the app's own `vite@6.x` and the older `vite@5.x`
// that `@vitest/mocker` — a vitest@2.1.9 transitive dependency — pulls in).
// Vitest's usual "add `/// <reference types="vitest/config" />` to
// vite.config.ts" pattern relies on a `declare module "vite"` type
// augmentation that only merges cleanly when there's a single resolved
// `vite` module; with two, `defineConfig({ test: {...} })` in vite.config.ts
// fails to typecheck ("'test' does not exist in type 'UserConfigExport'")
// even though `vitest run` picks the field up fine at runtime. Vitest
// auto-discovers this file (higher priority than vite.config.ts's own `test`
// key) and `mergeConfig` here is intentionally loosely typed
// (`Record<string, any>` in both directions), so it composes with the app's
// vite config regardless of which `vite` each side resolved against.
export default mergeConfig(
  viteConfig,
  defineConfig({
    test: {
      // jsdom unlocks component-render tests (`@testing-library/react`) for
      // the domain feature panels/dialogs under `src/features/**` — see
      // `src/test/render.tsx` for the shared render helper and provider
      // stack. Every existing test in the suite is a pure-function /
      // shallow-render (`react-dom/server`) test and is unaffected by the
      // environment switch from the (implicit) `node` default.
      environment: "jsdom",
      setupFiles: ["./src/test/setup.ts"],
      // This repo runs Vitest WITHOUT global test APIs: every test file
      // explicitly imports `describe`/`it`/`expect`/`vi` from "vitest" (see
      // any existing `*.test.ts`). Keep that convention rather than
      // injecting globals.
      globals: false,
      // `e2e/**` holds Playwright specs (`*.spec.ts`), which Vitest's
      // default `include` glob (`**/*.{test,spec}.*`) would otherwise try to
      // collect and execute as Vitest tests — they call Playwright's
      // `test()`/`test.describe()` outside a Playwright runner and blow up
      // immediately. Playwright specs run only via `pnpm e2e`.
      exclude: ["**/node_modules/**", "**/dist/**", "e2e/**"],
    },
  }),
);
