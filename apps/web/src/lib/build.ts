// Build version is injected by vite at build time via the `__BUILD_VERSION__`
// define in vite.config.ts. The define reads from the BUILD_VERSION env var
// passed by Cloud Build (or the local default below for `vite build` outside
// the deploy pipeline). NEVER hardcode a version literal here — every deploy
// past v0.9.1-update-progress-fix carried this stale string forward because
// the literal was never updated.
declare const __BUILD_VERSION__: string;
export const BUILD_VERSION: string =
  typeof __BUILD_VERSION__ !== "undefined" ? __BUILD_VERSION__ : "dev";
