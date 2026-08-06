// Solution page: WordPress tools for freelancers.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { SolutionPageData } from "@/lib/content/types";
import { SITE_CONFIG, signupHref } from "@/lib/site";

export const FREELANCERS_SOLUTION: SolutionPageData = {
  slug: "freelancers",
  title: "For freelancers",
  heading: "WordPress tools for freelancers",
  metaTitle: "WordPress Tools for Freelancers | WPMgr",
  metaDescription:
    "WPMgr is a self-hosted WordPress management tool built for freelancers who run sites for multiple clients. Automated backups, safe bulk updates, uptime monitoring, and security hardening from one open-source dashboard.",
  layoutVariant: "split",
  hero: {
    eyebrow: "Freelancer toolkit",
    subhead:
      "Manage all your client sites from a single dashboard without SSH access, shared passwords, or per-site logins. Automated backups run before every update, and you get alerts the moment a site goes down, before your client notices.",
    primaryCta: { label: "Start free", href: signupHref("solution-hero"), variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Read the code", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
  },
  outcomes: {
    heading: "Spend time building, not firefighting",
    body: "Freelancers managing more than a handful of WordPress sites spend a disproportionate share of every week on routine maintenance: logging into each site to run updates, manually checking that backups ran, and responding to client messages about a site that went down hours ago. WPMgr centralises all of that. Bulk updates with auto-snapshot and auto-revert on failure mean you can safely run updates across every site in minutes. Automated backups ensure there is always a clean snapshot before you make any change. Uptime monitoring with instant alerts means you know about an outage before your client does. And security hardening runs continuously in the background so you are not scrambling after an incident.",
  },
  provingFeatures: [
    {
      featureSlug: "backups",
      icon: "DatabaseBackup",
      title: "Automated backups",
      summary: "Scheduled incremental backups with fleet-wide health view. Restore any site to any snapshot in one click, without SSH.",
      href: "/features/backups",
    },
    {
      featureSlug: "updates",
      icon: "RefreshCw",
      title: "Safe fleet updates",
      summary: "Bulk update plugins, themes, and WordPress core across every site with auto-snapshot and auto-revert if something breaks.",
      href: "/features/updates",
    },
    {
      featureSlug: "uptime-monitoring",
      icon: "Activity",
      title: "Uptime monitoring",
      summary: "Fleet status matrix, response-time trends, TLS expiry warnings, and instant alerts so you know before your client does.",
      href: "/features/uptime-monitoring",
    },
    {
      featureSlug: "security",
      icon: "ShieldCheck",
      title: "Security hardening",
      summary: "Hardening rules, IP ban lists, file integrity monitoring, and vulnerability scanning across your entire client portfolio.",
      href: "/features/security",
    },
  ],
  stats: [
    { icon: "RefreshCw", value: "60s", label: "Add a new site in under a minute" },
    { icon: "Activity", value: "1 min", label: "Uptime check interval per site" },
    { icon: "DatabaseBackup", value: "0 SSH", label: "Backups and restores without shell access" },
  ],
  faq: [
    {
      q: "How do I add a client site?",
      a: "Go to the dashboard, click Add site, and enter the site URL. WPMgr generates a one-time enrollment code. Paste it into the WPMgr agent plugin on the site and the status flips from Awaiting to Connected in seconds, with no page refresh.",
    },
    {
      q: "What happens if a plugin update breaks a site?",
      a: "Before every update run, WPMgr automatically takes a backup snapshot. If the site's health check fails after the update, the agent rolls back the update automatically and marks the job as failed. You see exactly which plugin triggered the revert so you can investigate without client impact.",
    },
    {
      q: "Does monitoring work while I am asleep?",
      a: "Yes. Uptime checks run continuously and alerts fire as soon as a site returns an unexpected status code or stops responding. You choose how to receive alerts: dashboard notification, email, or both.",
    },
    {
      q: "Is there a per-site cost?",
      a: "The self-hosted open-source release has no per-site fee. You run the control plane on your own server and add as many sites as your infrastructure supports. The hosted cloud version is coming soon and will have a free tier.",
    },
  ],
};
