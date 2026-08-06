// Object cache feature page content.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { FeaturePageData } from "@/lib/content/types";
import { SITE_CONFIG, signupHref } from "@/lib/site";

export const OBJECT_CACHE_PAGE: FeaturePageData = {
  slug: "object-cache",
  title: "Redis Object Cache for WordPress",
  metaTitle: "Redis Object Cache WordPress: Per-Site Persistent Cache | WPMgr",
  metaDescription:
    "Per-site Redis object cache for WordPress with TLS, ACL, hit-ratio metrics, and a debug header that verifies cache state per request. Self-hosted, open source, and free.",
  hero: {
    eyebrow: "Redis Object Cache",
    heading: "Redis object cache for WordPress, managed per site from the dashboard",
    subhead:
      "Persistent object cache that accelerates logged-in users, admin screens, and every uncacheable database round-trip. Per-site key prefix, TLS, ACL, and a debug header that proves the cache is working.",
    primaryCta: { label: "Enable object caching for free", href: signupHref("feature-hero"), variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Read the code", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
  },
  problem: {
    heading: "Full-page caching misses logged-in users, admin, and uncacheable queries",
    body: "Full-page caching serves static pages fast but does nothing for a logged-in editor, a WooCommerce product page with live stock counts, or a complex WP_Query that runs on every admin screen load. An object cache stores the result of expensive database calls and serves them from memory on repeat requests, cutting database load and admin latency for the requests that need it most.",
  },
  steps: [
    {
      n: "1",
      icon: "HardDrive",
      title: "Connect to your Redis instance",
      desc: "Point WPMgr at your Redis host and port. Optionally set TLS, a password, or an ACL user. WPMgr generates a unique per-site key prefix so sites sharing a Redis instance cannot overwrite each other's data.",
    },
    {
      n: "2",
      icon: "Zap",
      title: "Deploy the drop-in",
      desc: "WPMgr pushes an object-cache drop-in to the site via the agent. WordPress picks it up automatically with no plugin activation or manual file editing. The drop-in degrades to in-memory cache if Redis becomes unreachable.",
    },
    {
      n: "3",
      icon: "BarChart2",
      title: "Verify with the debug header",
      desc: "Every cached response carries an x-wpmgr-object-cache header showing hit, miss, or bypass for that request. The dashboard surfaces the hit ratio, memory usage, and latency history so you can confirm the cache is serving traffic.",
    },
    {
      n: "4",
      icon: "Gauge",
      title: "Monitor hit ratio and memory over time",
      desc: "The dashboard plots hit ratio, memory consumption, and average latency over time. A low hit ratio or rising memory shows up before it affects site performance.",
    },
  ],
  subFeatures: [
    {
      icon: "HardDrive",
      title: "phpredis with TLS and ACL",
      desc: "The drop-in uses phpredis for the Redis connection and supports TLS, password auth, and ACL usernames for environments that require it.",
    },
    {
      icon: "KeyRound",
      title: "Per-site key prefix",
      desc: "WPMgr generates a unique key prefix per site so multiple sites sharing a Redis instance are fully isolated without needing separate Redis databases.",
    },
    {
      icon: "ShieldCheck",
      title: "Graceful Redis fallback",
      desc: "If Redis becomes unreachable the drop-in degrades to in-memory caching for that request. WordPress continues working and the dashboard marks the site's object cache as degraded.",
    },
    {
      icon: "BarChart2",
      title: "Hit ratio, memory, and latency dashboard",
      desc: "Per-site hit ratio, memory usage, and average operation latency are tracked over time and plotted in the dashboard. Trends show up before they affect performance.",
    },
    {
      icon: "Monitor",
      title: "Debug header per request",
      desc: "The x-wpmgr-object-cache response header shows the cache state for each request: hit, miss, bypass, or error. Enabled for admin users always; opt-in for all requests via a flag.",
    },
    {
      icon: "Network",
      title: "Fleet-wide object cache overview",
      desc: "The fleet performance dashboard shows which sites have object caching enabled, their hit ratios, and any sites currently degraded, so you can manage Redis health across the whole portfolio from one view.",
    },
  ],
  faq: [
    {
      q: "Does WPMgr include a Redis server?",
      a: "No. You provide the Redis instance, either self-hosted or a managed Redis service. WPMgr manages the WordPress-side drop-in and provides the dashboard metrics. A single Redis instance can serve multiple sites using per-site key prefixes.",
    },
    {
      q: "What happens if Redis goes down while the object cache is enabled?",
      a: "The drop-in degrades to in-memory caching for the duration of the request. WordPress continues working. The dashboard marks the site's object cache as degraded. When Redis becomes reachable again, the drop-in reconnects automatically.",
    },
    {
      q: "Does the object cache help with logged-in users?",
      a: "Yes. The object cache is most impactful for requests that cannot be served by the full-page cache: logged-in users, WooCommerce product pages with live stock, and complex WP_Query results. It stores the results of expensive database calls in memory for the duration of a request or across requests.",
    },
    {
      q: "How do I confirm the object cache is working?",
      a: "Every response from a WPMgr-cached page carries an x-wpmgr-object-cache header showing hit, miss, or bypass for that specific request. The dashboard also plots hit ratio, memory, and latency history over time.",
    },
  ],
  siblingLinks: [
    { label: "Performance and page caching", href: "/features/performance" },
    { label: "Real User Monitoring", href: "/features/real-user-monitoring" },
  ],
  solutionLinks: [
    { label: "Speed up WordPress", href: "/solutions/wordpress-performance" },
  ],
};
