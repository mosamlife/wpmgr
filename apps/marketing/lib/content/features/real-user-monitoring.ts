// Real User Monitoring feature page content. Seeded from apps/landing RUM.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { FeaturePageData } from "@/lib/content/types";
import { SITE_CONFIG } from "@/lib/site";

export const RUM_PAGE: FeaturePageData = {
  slug: "real-user-monitoring",
  title: "WordPress Core Web Vitals Monitoring",
  metaTitle: "WordPress Core Web Vitals Monitoring with Real User Data | WPMgr",
  metaDescription:
    "Monitor WordPress Core Web Vitals with real visitor data at the p75 percentile Google uses. LCP, INP, CLS, FCP, and TTFB with 28-day trends, per-URL breakdowns, and privacy-first anonymous collection.",
  hero: {
    eyebrow: "Real User Monitoring",
    heading: "WordPress Core Web Vitals monitoring from real visitor data",
    subhead:
      "See how your pages actually perform in the field. All five Core Web Vitals at the p75 percentile Google uses for ranking, sourced from real browsers on your live site, anonymous by design.",
    primaryCta: { label: "Start monitoring Core Web Vitals free", href: SITE_CONFIG.signup, variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Read the code", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
  },
  problem: {
    heading: "Lab scores look good but real visitors still report slow pages",
    body: "Synthetic speed tests measure an empty browser in a lab. Real visitors bring extensions, slow connections, cached state, and different devices. Core Web Vitals scores from lab tools and real user data often diverge significantly. Google uses the p75 real-user percentile for search ranking, not your Lighthouse score, so only real-user data tells you whether you are actually passing.",
  },
  steps: [
    {
      n: "1",
      icon: "ToggleLeft",
      title: "Turn on RUM per site",
      desc: "Real User Monitoring is off by default. Turn it on per site from the dashboard. WPMgr injects a small, anonymous script into the page that collects Core Web Vitals from real visitor sessions using the web-vitals library.",
    },
    {
      n: "2",
      icon: "BarChart2",
      title: "See the p75 distribution for each metric",
      desc: "LCP, INP, CLS, FCP, and TTFB are collected for each page load and grouped into good, needs improvement, and poor buckets. The p75 value is displayed alongside a PageSpeed Insights-style distribution bar.",
    },
    {
      n: "3",
      icon: "TrendingUp",
      title: "Track 28-day trends with threshold lines",
      desc: "Every metric shows a 28-day p75 trend with the passing threshold drawn on. You can see the moment a deploy or a plugin update moved a metric across the threshold in either direction.",
    },
    {
      n: "4",
      icon: "Monitor",
      title: "Break down by URL and device",
      desc: "Narrow the view to a specific URL to find a slow page, or split desktop from mobile to understand where a metric is coming from. Per-URL and per-device breakdowns show in the same dashboard without any manual segmentation.",
    },
  ],
  subFeatures: [
    {
      icon: "BarChart2",
      title: "All five Core Web Vitals at p75",
      desc: "LCP, INP, CLS, FCP, and TTFB are all collected and displayed at the p75 percentile: the same threshold Google PageSpeed Insights and Search Console use for field data.",
    },
    {
      icon: "TrendingUp",
      title: "28-day p75 trend per metric",
      desc: "The passing threshold line is drawn on every trend chart so you can see the moment a change crossed it. Spot regressions before they become ranking signals.",
    },
    {
      icon: "Gauge",
      title: "Distribution bar",
      desc: "A PageSpeed Insights-style histogram bar built from the same good, needs improvement, and poor rating buckets Google uses, so you understand not just the p75 but the shape of the distribution.",
    },
    {
      icon: "Monitor",
      title: "Per-URL and per-device breakdowns",
      desc: "Narrow to a specific page or separate desktop from mobile without leaving the dashboard. Per-URL breakdowns make it practical to isolate a single slow template.",
    },
    {
      icon: "Activity",
      title: "Live updates over SSE",
      desc: "New beacons stream to the dashboard in real time over a server-sent events connection. No manual refresh needed to see the latest field data after a deploy.",
    },
    {
      icon: "EyeOff",
      title: "Privacy-first by design",
      desc: "Anonymous, off by default, no cookies, no cross-site identifiers. Page paths are stored with the query string stripped. Visitor IP addresses are never stored.",
    },
  ],
  faq: [
    {
      q: "Why does real user monitoring matter if I already use Lighthouse?",
      a: "Lighthouse measures an empty browser in a controlled lab. Google uses field data from real visitors at the p75 percentile for search ranking signals. Real User Monitoring shows you whether your actual visitors are experiencing good, needs improvement, or poor Core Web Vitals, which is the number that matters for ranking.",
    },
    {
      q: "Is RUM on by default?",
      a: "No. Real User Monitoring is off by default and must be turned on per site from the dashboard. The collection script is only injected when you enable it.",
    },
    {
      q: "Does RUM use cookies or track visitors?",
      a: "No. The collection is anonymous by design. No cookies are set, no cross-site identifiers are used, query strings are stripped from page paths, and visitor IP addresses are never stored.",
    },
    {
      q: "What is the p75 percentile?",
      a: "The 75th percentile means that 75 percent of your visitors experience a metric at or better than the reported value. Google uses p75 for Core Web Vitals assessment in Search Console and PageSpeed Insights, so WPMgr reports the same number to make comparison direct.",
    },
    {
      q: "Can I see Core Web Vitals for a specific page or device type?",
      a: "Yes. The per-URL breakdown lets you narrow to a specific page URL, and the per-device breakdown separates desktop from mobile. Both are available in the same dashboard view without switching tools.",
    },
  ],
  siblingLinks: [
    { label: "Performance and page caching", href: "/features/performance" },
    { label: "Media Optimizer", href: "/features/media-optimizer" },
  ],
  solutionLinks: [
    { label: "Speed up WordPress", href: "/solutions/wordpress-performance" },
  ],
};

// RUM demo data for the visual widget (illustrative sample data only)
export const RUM_DEMO = {
  metric: "LCP",
  p75: "2.1s",
  rating: "good" as const,
  distribution: [
    { label: "Good", pct: 68, tone: "good" as const },
    { label: "Needs improvement", pct: 22, tone: "needs-improvement" as const },
    { label: "Poor", pct: 10, tone: "poor" as const },
  ],
  trend: [2.6, 2.4, 2.3, 2.5, 2.2, 2.0, 2.1, 2.3, 2.0, 1.9, 2.1, 2.0, 2.1],
  threshold: 2.5,
  metrics: [
    { name: "LCP", p75: "2.1s", rating: "good" as const },
    { name: "INP", p75: "148ms", rating: "good" as const },
    { name: "CLS", p75: "0.05", rating: "good" as const },
    { name: "FCP", p75: "1.4s", rating: "good" as const },
    { name: "TTFB", p75: "310ms", rating: "needs-improvement" as const },
  ],
};
