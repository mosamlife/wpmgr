// Solution page: manage multiple WordPress sites.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { SolutionPageData } from "@/lib/content/types";
import { signupHref } from "@/lib/site";

export const MANAGE_MULTIPLE_SITES_SOLUTION: SolutionPageData = {
  slug: "manage-multiple-sites",
  title: "Manage multiple sites",
  heading: "Manage multiple WordPress sites from one dashboard",
  metaTitle: "Manage Multiple WordPress Sites",
  metaDescription:
    "WPMgr is the open-source control plane for managing multiple WordPress sites: backups, safe bulk updates, uptime monitoring, and security in one dashboard.",
  layoutVariant: "default",
  hero: {
    eyebrow: "Fleet management",
    subhead:
      "Connect every WordPress site you manage to a single dashboard. Run bulk updates with auto-revert, monitor uptime across the fleet, trigger backups before every change, and give each team member exactly the access they need without sharing passwords.",
    primaryCta: { label: "Get started free", href: signupHref("solution-hero"), variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Or self-host it", href: "/self-host", variant: "secondary" },
  },
  outcomes: {
    heading: "One dashboard for everything that matters",
    body: "Managing multiple WordPress sites by logging into each one individually does not scale. Routine tasks like running updates, checking backup status, reviewing uptime, and applying security hardening multiply with every site you add. WPMgr is built around a fleet model: every site you connect appears in a single dashboard where you can act on all of them or any subset at once. Bulk update runs fire across the whole fleet with a pre-flight backup snapshot and auto-revert if a site's health check fails after the update. The uptime status matrix shows you every site's current state at a glance. Security hardening and vulnerability scanning run continuously. And the team access system means your collaborators see only the sites you assign to them without any shared credentials.",
  },
  provingFeatures: [
    {
      featureSlug: "backups",
      icon: "DatabaseBackup",
      title: "Fleet-wide backups",
      summary: "Scheduled incremental backups with a fleet-wide health browser. Restore any site to any snapshot in one click.",
      href: "/features/backups",
    },
    {
      featureSlug: "updates",
      icon: "RefreshCw",
      title: "Safe bulk updates",
      summary: "Update plugins, themes, and core across every site at once with auto-snapshot and auto-revert on failure.",
      href: "/features/updates",
    },
    {
      featureSlug: "uptime-monitoring",
      icon: "Activity",
      title: "Uptime and health monitoring",
      summary: "Fleet status matrix, response-time trends, TLS certificate expiry warnings, and instant outage alerts.",
      href: "/features/uptime-monitoring",
    },
    {
      featureSlug: "performance",
      icon: "Zap",
      title: "Performance and caching",
      summary: "Full-page caching, Redis object cache, Media Optimizer, and Real User Monitoring across the fleet.",
      href: "/features/performance",
    },
    {
      featureSlug: "security",
      icon: "ShieldCheck",
      title: "Security hardening",
      summary: "Fleet-wide hardening, file integrity monitoring, vulnerability scanning, and IP ban lists from a single interface.",
      href: "/features/security",
    },
    {
      featureSlug: "database-cleaner",
      icon: "DatabaseZap",
      title: "Database cleaner",
      summary: "Per-table scan, orphan classification, 90-day size trend, and fleet-wide database health across every site.",
      href: "/features/database-cleaner",
    },
    {
      featureSlug: "team-access",
      icon: "Users",
      title: "Team access and audit log",
      summary: "Four roles, per-site sharing, and a hash-chained audit log of every action across every connected site.",
      href: "/features/team-access",
    },
  ],
  stats: [
    { icon: "ServerCog", value: "60s", label: "Connect a new site in under a minute" },
    { icon: "ShieldCheck", value: "All in one", label: "Backups, updates, security, and monitoring" },
    { icon: "Scale", value: "AGPL + MIT", label: "Open-source, self-hostable, auditable" },
  ],
  faq: [
    {
      q: "Is there a limit on how many sites I can connect?",
      a: "The self-hosted open-source release has no hard site limit. You can connect as many WordPress sites as your server infrastructure supports. The hosted cloud version will introduce plan tiers, but the self-hosted path is always unlimited.",
    },
    {
      q: "Can I organise sites by client or team?",
      a: "Yes. Sites can be grouped by organisation (client/team account) and shared with specific team members using per-site permission roles. A team member assigned as a Maintainer on three sites sees only those three sites in their dashboard view.",
    },
    {
      q: "How do bulk updates work safely across the fleet?",
      a: "Before the update run begins, WPMgr automatically takes an incremental backup snapshot of every site in the update batch. After each update, the agent runs a health check. If the health check fails, the agent reverts the update automatically and marks the job as failed so you can investigate without client impact.",
    },
    {
      q: "What does the fleet status matrix show?",
      a: "The fleet status matrix displays every connected site with its current uptime status, last check timestamp, TLS certificate expiry, and any active incidents. Sites in a degraded or down state are visually highlighted so you can triage at a glance without clicking into each site.",
    },
    {
      q: "Do I need to install a separate plugin on each site?",
      a: "Yes, each WordPress site needs the WPMgr agent plugin installed. Installation is lightweight: add the plugin, enter a one-time enrollment code from the dashboard, and the site appears as Connected within seconds. There is no SSH access, no server configuration, and no additional dependencies.",
    },
  ],
};
