"use client";

// PostHog pageviews for the marketing site. Loads nothing and sends nothing
// when NEXT_PUBLIC_POSTHOG_KEY is absent, which is the case for any fork or
// self-host build. See lib/analytics.ts for why that matters, and
// lib/posthog-client.ts for why the SDK is imported lazily.

import { Suspense, useEffect } from "react";
import { usePathname, useSearchParams } from "next/navigation";
import { getPostHog } from "@/lib/posthog-client";

function PageViews() {
  const pathname = usePathname();
  const searchParams = useSearchParams();

  useEffect(() => {
    if (!pathname) return;
    // App Router navigations do not reload the page, so a pageview has to be
    // sent per route change or an entire session reads as one page.
    const query = searchParams?.toString();
    const url = window.location.origin + pathname + (query ? `?${query}` : "");
    void getPostHog().then((ph) => ph?.capture("$pageview", { $current_url: url }));
  }, [pathname, searchParams]);

  return null;
}

export function PostHogPageViews() {
  return (
    // The Suspense boundary is required, not stylistic: useSearchParams in a
    // client component without one opts every page above it out of static
    // rendering. On a site that is almost entirely prerendered, that would
    // trade the whole prerender for a pageview counter.
    <Suspense fallback={null}>
      <PageViews />
    </Suspense>
  );
}
