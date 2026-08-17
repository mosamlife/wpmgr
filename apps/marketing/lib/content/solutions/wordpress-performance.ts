// Solution page: speed up WordPress / improve Core Web Vitals.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { SolutionPageData } from "@/lib/content/types";
import { signupHref } from "@/lib/site";

export const WORDPRESS_PERFORMANCE_SOLUTION: SolutionPageData = {
  slug: "wordpress-performance",
  title: "WordPress performance",
  heading: "Speed up WordPress and improve Core Web Vitals",
  metaTitle: "Speed Up WordPress and Improve Core Web Vitals",
  metaDescription:
    "WPMgr accelerates WordPress with full-page caching, Redis object cache, AVIF and WebP image conversion via the Media Optimizer, Real User Monitoring, and unused CSS removal. Open-source, self-hosted, no per-site fee.",
  layoutVariant: "default",
  hero: {
    eyebrow: "Performance suite",
    subhead:
      "Four independent acceleration layers that compound: full-page caching cuts server response time, the Media Optimizer converts your image library to AVIF and WebP, Redis object cache reduces database load, and Real User Monitoring proves the improvement with real visitor data.",
    primaryCta: { label: "Start optimising for free", href: signupHref("solution-hero"), variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "See Media Optimizer", href: "/features/media-optimizer", variant: "secondary", icon: "ArrowRight" },
  },
  outcomes: {
    heading: "Every acceleration layer works together",
    body: "Improving WordPress performance with isolated plugins leads to conflict and diminishing returns. WPMgr's performance suite is designed as a stack: full-page caching handles the majority of anonymous page loads before PHP runs; the Redis object cache reduces the database roundtrips that caching misses; the Media Optimizer eliminates image transfer overhead by converting the entire media library to modern formats; and unused CSS removal cuts the render-blocking stylesheet payload per page. Real User Monitoring closes the feedback loop by measuring LCP, CLS, and INP at p75 from real visitors, broken down by URL, device, and connection, so you can see exactly where time is being spent and verify that each optimisation is working.",
  },
  provingFeatures: [
    {
      featureSlug: "performance",
      icon: "Zap",
      title: "Full-page caching",
      summary: "Full-page cache, unused CSS removal, WOFF2 font subsetting, and WooCommerce-aware cache bypasses built into the agent.",
      href: "/features/performance",
    },
    {
      featureSlug: "object-cache",
      icon: "HardDrive",
      title: "Redis object cache",
      summary: "Per-site Redis object cache with hit ratio, memory pressure, and latency tracked in the fleet dashboard.",
      href: "/features/object-cache",
    },
    {
      featureSlug: "media-optimizer",
      icon: "ImageDown",
      title: "Media Optimizer",
      summary: "Convert your entire WordPress media library to AVIF and WebP. Originals are archived and the conversion is fully reversible.",
      href: "/features/media-optimizer",
    },
    {
      featureSlug: "real-user-monitoring",
      icon: "BarChart2",
      title: "Real User Monitoring",
      summary: "Core Web Vitals at p75 from real visitors, with 28-day trends, per-URL breakdown, and per-device segmentation.",
      href: "/features/real-user-monitoring",
    },
  ],
  stats: [
    { icon: "ImageDown", value: "AVIF + WebP", label: "Modern image formats with original archiving" },
    { icon: "BarChart2", value: "p75", label: "Core Web Vitals measured from real visitors" },
    { icon: "Zap", value: "Layer 1", label: "Full-page cache answers before PHP runs" },
  ],
  faq: [
    {
      q: "Does full-page caching work with WooCommerce?",
      a: "Yes. The WPMgr page cache includes WooCommerce-aware bypass rules that automatically exclude cart, checkout, and account pages, as well as logged-in users and users with items in their cart. You get the performance benefit on product listing and static pages without serving cached pages to active shoppers.",
    },
    {
      q: "How does the Media Optimizer handle existing images?",
      a: "The Media Optimizer processes your existing WordPress media library in a background job managed by the control plane. Each image is converted to AVIF and WebP by the media encoder service. The original file is archived, not deleted, so you can revert the conversion at any time from the dashboard.",
    },
    {
      q: "What Core Web Vitals does Real User Monitoring track?",
      a: "WPMgr tracks Largest Contentful Paint (LCP), Cumulative Layout Shift (CLS), and Interaction to Next Paint (INP) at the p75 percentile from real visitors using the Google web-vitals library. Data is broken down by URL, device type, and connection speed over a rolling 28-day window.",
    },
    {
      q: "Can I use Redis object cache without running Redis myself?",
      a: "You need a Redis instance reachable from your WordPress server. WPMgr handles the object cache plugin installation and configuration from the dashboard. Many managed hosting providers offer Redis as an add-on, and it is straightforward to run on a self-hosted server alongside WordPress.",
    },
    {
      q: "Do all four performance layers need to be enabled?",
      a: "No, each layer is independently configurable per site. You can run full-page caching on every site, the Media Optimizer on sites with large image libraries, and Redis only on sites that have a Redis instance available. Real User Monitoring is always on when the agent is connected and adds negligible overhead.",
    },
  ],
};
