// Uptime monitoring feature page content.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { FeaturePageData } from "@/lib/content/types";
import { SITE_CONFIG, signupHref } from "@/lib/site";

export const UPTIME_MONITORING_PAGE: FeaturePageData = {
  slug: "uptime-monitoring",
  title: "WordPress Uptime Monitoring",
  metaTitle: "WordPress Uptime Monitoring for Your Whole Fleet",
  metaDescription:
    "WordPress uptime monitoring with a fleet status matrix, response-time trends, TLS expiry warnings, and down-and-recovery alerts. Self-hosted and free to run.",
  hero: {
    eyebrow: "Uptime and health",
    heading: "WordPress uptime monitoring for every site in the fleet",
    subhead:
      "A fleet status matrix shows which sites are up, degraded, or down in real time. Response-time trends, TLS expiry warnings, and instant alerts mean you know before your clients do.",
    primaryCta: { label: "Start monitoring for free", href: signupHref("feature-hero"), variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Read the code", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
  },
  problem: {
    heading: "Finding out a site is down from a client message is already too late",
    body: "Uptime tools that only ping a URL once a minute miss degraded responses, slow TLS handshakes, and partial outages that look fine from the outside. A fleet manager needs to distinguish a site that is genuinely unreachable from one that is just quiet, and alert the right person within seconds, not minutes.",
  },
  steps: [
    {
      n: "1",
      icon: "Activity",
      title: "Fleet status matrix at a glance",
      desc: "Every connected site is shown in a status matrix: up, degraded, or down. The matrix updates in real time over a server-sent events connection, so you see state changes as they happen.",
    },
    {
      n: "2",
      icon: "TrendingUp",
      title: "Response-time trends over 7, 30, and 90 days",
      desc: "Per-site response-time history is plotted at 7, 30, and 90 day horizons. Latency spikes and gradual degradation stand out before they become customer-visible outages.",
    },
    {
      n: "3",
      icon: "ShieldCheck",
      title: "TLS expiry and PHP fatal tracking",
      desc: "WPMgr warns when a TLS certificate is approaching expiry and tracks PHP fatal errors reported by the agent, so infrastructure health is visible alongside application health.",
    },
    {
      n: "4",
      icon: "Mail",
      title: "Down and recovery alerts",
      desc: "Configure email or webhook alerts for down and recovery events per site. Alert channels are scoped to a site or the whole fleet, and repeated alerts are suppressed while a site stays down.",
    },
  ],
  subFeatures: [
    {
      icon: "Activity",
      title: "Fleet status matrix",
      desc: "Up, degraded, and down states across every connected site in a single view, updated in real time over SSE, no manual refresh.",
    },
    {
      icon: "TrendingUp",
      title: "Response-time history",
      desc: "7, 30, and 90 day response-time trends per site. Spot gradual degradation before it becomes a complete outage.",
    },
    {
      icon: "ShieldCheck",
      title: "TLS expiry warnings",
      desc: "Certificate expiry dates are tracked and surfaced in the dashboard with configurable early-warning windows.",
    },
    {
      icon: "LayoutGrid",
      title: "Incident history",
      desc: "Every down and recovery event is recorded with a timestamp and duration, giving you a durable log to draw on for client reports.",
    },
    {
      icon: "Network",
      title: "Accurate idle vs unreachable detection",
      desc: "The sweeper verifies quiet sites directly rather than relying only on heartbeats, so the status badge accurately shows unreachable versus idle.",
    },
    {
      icon: "LayoutDashboard",
      title: "Screenshot-backed site cards",
      desc: "Each site in the grid view shows its live screenshot alongside uptime, latency, SSL expiry, and backup health, so you can see the visual state of the site alongside its health metrics.",
    },
  ],
  faq: [
    {
      q: "How does WPMgr detect a site that is down versus just quiet?",
      a: "WPMgr uses a sweeper that probes quiet sites directly, not just heartbeats from the agent. A site that has not sent a heartbeat for a while but responds to a probe is marked idle, not unreachable. The status badge reflects this distinction.",
    },
    {
      q: "Can I get alerted when a site goes down?",
      a: "Yes. You can configure email or webhook alerts for down and recovery events, scoped to individual sites or the whole fleet. Repeated alerts are suppressed while a site stays down to prevent notification spam.",
    },
    {
      q: "How far back does the response-time history go?",
      a: "WPMgr stores response-time data at 7, 30, and 90 day horizons. Per-site detail pages show all three views. The fleet uptime page shows the aggregated status matrix and incident history across all sites.",
    },
    {
      q: "Are TLS certificates monitored automatically?",
      a: "Yes. WPMgr tracks the expiry date for each site's TLS certificate and surfaces an expiry warning in the dashboard with a configurable early-warning window.",
    },
  ],
  siblingLinks: [
    { label: "Safe fleet updates", href: "/features/updates" },
    { label: "Real User Monitoring", href: "/features/real-user-monitoring" },
  ],
  solutionLinks: [
    { label: "Manage multiple WordPress sites", href: "/solutions/manage-multiple-sites" },
  ],
};
