// Performance feature page content. Seeded from apps/landing PERFORMANCE + PERFORMANCE_STEPS.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { FeaturePageData } from "@/lib/content/types";
import { SITE_CONFIG, signupHref } from "@/lib/site";

export const PERFORMANCE_PAGE: FeaturePageData = {
  slug: "performance",
  title: "WordPress Performance and Page Caching",
  metaTitle: "WordPress Caching and Speed Optimization",
  metaDescription:
    "Full-page caching, unused CSS removal, WOFF2 font transcoding, and WooCommerce-aware bypasses. WPMgr makes WordPress faster with a toggle, not a rebuild.",
  hero: {
    eyebrow: "Performance Suite",
    heading: "Speed up WordPress with full-page caching and asset optimization",
    subhead:
      "Turn on full-page caching and asset optimization and WPMgr serves your anonymous pages from disk and ships only the assets each page needs. Every layer is per site or across your whole portfolio, and a failed optimization always falls back to the original.",
    primaryCta: { label: "Get faster pages for free", href: signupHref("feature-hero"), variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Read the code", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
  },
  problem: {
    heading: "Most WordPress caching plugins are all-or-nothing and break WooCommerce checkout",
    body: "Full-page caching that does not understand logged-in users, cart sessions, or query variants either serves wrong content or disables itself entirely on dynamic pages. WPMgr caches what can be cached, bypasses what cannot, and degrades safely when a setting is wrong, so you never ship a broken checkout to fix a LCP score.",
  },
  steps: [
    {
      n: "1",
      icon: "Zap",
      title: "Cache pages to disk",
      desc: "Anonymous pages are stored as pre-gzipped HTML and served straight from disk on a hit, with variants for logged-in users, roles, mobile, and query strings, plus bypass rules so cart and checkout pages stay dynamic.",
    },
    {
      n: "2",
      icon: "Scissors",
      title: "Trim CSS and JS",
      desc: "Minify CSS and JS, delay scripts until interaction, and strip the CSS a page does not use. Remove Unused CSS runs on WPMgr's own engine and always serves full CSS when a result is not ready yet.",
    },
    {
      n: "3",
      icon: "ImageDown",
      title: "Lighten the front end",
      desc: "Lazy-load images with width, height, and srcset preserved, swap in fonts without blocking text, convert self-hosted fonts to WOFF2 for 50 to 65 percent smaller loads, and optionally subset each font to the latin-ext unicode range for a further 60 to 90 percent reduction.",
    },
    {
      n: "4",
      icon: "Gauge",
      title: "Manage it like a fleet",
      desc: "Save the config for one site, purge the cache across many at once, or apply a safe, balanced, or aggressive preset to a whole group in one run. Live status and stats stream to the dashboard with no refresh.",
    },
  ],
  subFeatures: [
    {
      icon: "Gauge",
      title: "Pages served from disk",
      desc: "Anonymous pages are stored as pre-gzipped HTML and served straight from disk. No PHP executes on a cache hit, cutting server time to near zero for anonymous visitors.",
    },
    {
      icon: "Scissors",
      title: "Remove Unused CSS",
      desc: "Unused CSS is stripped and inlined on a per-page basis using WPMgr's own pure-Go engine. No headless browser and no third-party service. Full CSS is always served on a cache miss.",
    },
    {
      icon: "Type",
      title: "WOFF2 font transcoding and subsetting",
      desc: "TTF, OTF, and WOFF fonts are transcoded to WOFF2 in the cloud for 50 to 65 percent smaller font loads. Optionally subset to latin-ext for a further reduction. Icon and variable fonts are detected and skipped automatically.",
    },
    {
      icon: "HardDrive",
      title: "WooCommerce cart-session caching",
      desc: "Cart and checkout pages are automatically bypassed from full-page caching. WooCommerce session cookies are recognized and routed around the cache so dynamic pages stay dynamic.",
    },
    {
      icon: "ToggleLeft",
      title: "Safe, balanced, and aggressive presets",
      desc: "Apply a preset to set a sensible starting configuration without guessing which toggles to flip. Presets range from safe (page cache only) to aggressive (all optimizations on). Override any setting individually.",
    },
    {
      icon: "BarChart2",
      title: "Fleet performance dashboard",
      desc: "Core Web Vitals across every site in one view: worst-offender ranking by LCP, INP, and CLS, a fleet-wide trend line, and per-site drill-down, so you can see which sites need attention without opening each one.",
    },
  ],
  faq: [
    {
      q: "Does the page cache break WooCommerce or logged-in user pages?",
      a: "No. WPMgr automatically bypasses cart, checkout, and account pages from the full-page cache. WooCommerce session cookies are detected and the cache is routed around for those requests. Logged-in user variant caching is configurable separately.",
    },
    {
      q: "How does Remove Unused CSS work?",
      a: "WPMgr uses its own pure-Go engine to compute unused CSS on a per-page basis. There is no headless browser and no third-party service involved. Interactive states are preserved, and a per-site safelist covers any CSS added by scripts at runtime. On a cache miss the full original CSS is served so rendering is never blocked.",
    },
    {
      q: "What formats does font transcoding support?",
      a: "TTF, OTF, and WOFF fonts are transcoded to WOFF2. Animated GIFs become animated WebP in the Media Optimizer. Icon fonts and variable fonts are detected and skipped automatically. The subsetting step is experimental and off by default.",
    },
    {
      q: "Can I apply a performance preset across the whole fleet at once?",
      a: "Yes. From the fleet updates view you can apply a safe, balanced, or aggressive preset to a tag group, a client record, or a manual selection of sites in a single run.",
    },
    {
      q: "Is full-page caching compatible with nginx?",
      a: "Yes. On Apache the server fast-path installs itself automatically via .htaccess. For nginx, WPMgr generates a ready-to-paste snippet for your site config that achieves the same disk-serving fast path.",
    },
  ],
  siblingLinks: [
    { label: "Redis Object Cache", href: "/features/object-cache" },
    { label: "Media Optimizer", href: "/features/media-optimizer" },
    { label: "Real User Monitoring", href: "/features/real-user-monitoring" },
  ],
  solutionLinks: [
    { label: "Speed up WordPress", href: "/solutions/wordpress-performance" },
  ],
};
