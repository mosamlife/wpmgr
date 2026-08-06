// Lazily loaded PostHog singleton.
//
// WHY THIS EXISTS RATHER THAN A TOP-LEVEL `import posthog from "posthog-js"`.
// A static import puts the SDK in the initial client bundle whether or not it is
// ever used. Measured on this site: 244 KB uncompressed, the single largest
// chunk, loaded on every page, and loaded even in builds where no PostHog key
// is configured and the SDK does nothing at all. That is a poor trade anywhere,
// and a particularly bad one on a site whose argument is that it fixes other
// people's Core Web Vitals.
//
// A dynamic import moves it into its own chunk that is fetched only after the
// page is interactive, and only when a key exists. Forks and self-host builds
// never download it.

import type { PostHog } from "posthog-js";
import { POSTHOG_KEY, POSTHOG_HOST, analyticsEnabled } from "./analytics";

let pending: Promise<PostHog | null> | null = null;

/**
 * Resolves to an initialised PostHog instance, or null when analytics is not
 * configured. Safe to call repeatedly: the import and init happen once.
 */
export function getPostHog(): Promise<PostHog | null> {
  if (typeof window === "undefined" || !analyticsEnabled.posthog) {
    return Promise.resolve(null);
  }
  if (!pending) {
    pending = import("posthog-js")
      .then(({ default: posthog }) => {
        posthog.init(POSTHOG_KEY, {
          api_host: POSTHOG_HOST,
          // Pageviews are sent per route change by the provider. Automatic
          // capture would double-count every App Router navigation.
          capture_pageview: false,
          capture_pageleave: true,
          // Nobody signs in on the marketing site, so there is no identified
          // user to build a profile for. Anonymous events are cheaper on the
          // free plan and are all this site can honestly attribute.
          person_profiles: "never",
          // Respect the browser signal rather than making people find a setting.
          respect_dnt: true,
          // Masks text and inputs if session replay is ever switched on in the
          // project settings, so enabling it in the PostHog UI cannot start
          // recording form contents by surprise.
          session_recording: { maskAllInputs: true },
        });
        return posthog;
      })
      .catch(() => null);
  }
  return pending;
}
