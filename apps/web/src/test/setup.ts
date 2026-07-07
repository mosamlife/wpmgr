// Vitest global test setup (wired via `test.setupFiles` in vite.config.ts).
//
// This file runs once per test FILE, before that file's tests execute. It
// exists purely to:
//   1. Register jest-dom's matchers (toBeInTheDocument, toHaveTextContent,
//      etc.) against Vitest's `expect` — every render test in this repo can
//      then use them with zero per-file boilerplate.
//   2. Unmount + detach every component rendered via `@testing-library/react`
//      after each test. RTL auto-registers this itself ONLY when `afterEach`
//      exists as a true global; this repo runs Vitest with `globals: false`
//      (every test file explicitly imports `describe`/`it`/`expect` from
//      "vitest" — see any existing *.test.ts), so we wire it explicitly here
//      instead of flipping the project over to Vitest's global mode.
//
// Do NOT add global mocks, MSW handlers, or fetch stubs here: this repo's
// pattern is to mock each feature's data hook (`vi.mock("./use-x")`) at the
// point of use — see `src/test/render.tsx` for the render helper and
// `vuln-panel.test.tsx` / `restore-dialog.test.tsx` for the pattern.
import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";
import "@testing-library/jest-dom/vitest";

afterEach(() => {
  cleanup();
});
