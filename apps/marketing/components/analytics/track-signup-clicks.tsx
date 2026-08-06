"use client";

// Records a signup_click event whenever a visitor follows any link to the
// dashboard's register page, tagged with the surface it came from.
//
// WHY A DELEGATED LISTENER RATHER THAN AN onClick ON EACH BUTTON. There are
// signup links in the header, the mobile nav, both page templates, the pricing
// tiers, the CTA band and inside content modules, and several are rendered from
// plain data objects with no component to hang a handler on. One listener on
// the document catches all of them, including any added later, which is the
// failure mode worth designing out: a new CTA that silently reports nothing.
//
// The dashboard lives on another origin and is not instrumented yet, so this
// click is the last event we can observe. It is therefore the conversion signal
// the marketing site actually has, and the ref parameter in the URL is what
// makes it attributable to a page rather than to the site as a whole.

import { useEffect } from "react";
import { usePathname } from "next/navigation";
import { analyticsEnabled } from "@/lib/analytics";
import { getPostHog } from "@/lib/posthog-client";
import { SITE_CONFIG } from "@/lib/site";

declare global {
  interface Window {
    gtag?: (command: string, event: string, params?: Record<string, unknown>) => void;
  }
}

export function TrackSignupClicks() {
  const pathname = usePathname();

  useEffect(() => {
    if (!analyticsEnabled.posthog && !analyticsEnabled.ga) return;

    function onClick(event: MouseEvent) {
      const target = event.target as HTMLElement | null;
      const anchor = target?.closest?.("a");
      if (!anchor) return;

      const href = anchor.getAttribute("href");
      if (!href || !href.startsWith(SITE_CONFIG.signup)) return;

      let source = "unknown";
      let plan: string | undefined;
      try {
        const url = new URL(href);
        source = url.searchParams.get("ref") ?? "unknown";
        plan = url.searchParams.get("plan") ?? undefined;
      } catch {
        // A malformed href is not worth failing a click over.
      }

      const payload = {
        source,
        ...(plan ? { plan } : {}),
        // The page the click happened on, which is the thing we actually want
        // to rank by. `source` says which BUTTON, this says which PAGE.
        page: pathname,
      };

      if (analyticsEnabled.posthog) {
        void getPostHog().then((ph) => ph?.capture("signup_click", payload));
      }
      if (analyticsEnabled.ga) window.gtag?.("event", "signup_click", payload);
    }

    // Capture phase, so the event is recorded even if something downstream
    // stops propagation.
    document.addEventListener("click", onClick, { capture: true });
    return () => document.removeEventListener("click", onClick, { capture: true });
  }, [pathname]);

  return null;
}
