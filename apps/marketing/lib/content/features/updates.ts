// Updates feature page content.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { FeaturePageData } from "@/lib/content/types";
import { SITE_CONFIG, signupHref } from "@/lib/site";

export const UPDATES_PAGE: FeaturePageData = {
  slug: "updates",
  title: "Safe Fleet Updates",
  metaTitle: "Bulk Update WordPress Plugins Safely",
  metaDescription:
    "Bulk update WordPress plugins, themes, and core across your fleet safely. WPMgr auto-reverts on a failed health check and logs a tamper-evident history.",
  hero: {
    eyebrow: "Fleet updates",
    heading: "Bulk update WordPress plugins safely with auto-revert",
    subhead:
      "Preview version changes, snapshot first, then update across the whole fleet. If a health check fails after the update, WPMgr reverts the site automatically.",
    primaryCta: { label: "Start managing updates for free", href: signupHref("feature-hero"), variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Read the code", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
  },
  problem: {
    heading: "Updating plugins one site at a time is slow, and a bad update can break a site silently",
    body: "Running updates manually across a fleet means hours of repetitive clicks and no consistent safety net. A plugin update that breaks a site often goes unnoticed until a client calls. The right approach snapshots every site before updating, checks health after, and reverts automatically when something breaks, without you staying online to babysit the run.",
  },
  steps: [
    {
      n: "1",
      icon: "RefreshCw",
      title: "See what needs updating across the fleet",
      desc: "The updates dashboard shows every available update across all connected sites in one view: plugin, theme, or core, with the current version, the available version, and how many sites are affected.",
    },
    {
      n: "2",
      icon: "DatabaseBackup",
      title: "Snapshot first, automatically",
      desc: "Before any update runs, WPMgr takes an automatic snapshot of the site. If the update fails or the health check reports an error, the site rolls back to this snapshot without any manual intervention.",
    },
    {
      n: "3",
      icon: "Zap",
      title: "Run bulk updates by group or tag",
      desc: "Select sites by tag, client, or individually, then kick off the update run. Live per-site progress streams to the dashboard with no refresh. Failed sites are flagged, auto-reverted, and reported.",
    },
    {
      n: "4",
      icon: "ScrollText",
      title: "Review the audit trail",
      desc: "Every update run, snapshot, and revert is recorded in the tamper-evident audit log. The log shows who initiated the run, which sites updated successfully, which auto-reverted, and why.",
    },
  ],
  subFeatures: [
    {
      icon: "RefreshCw",
      title: "Bulk runs by group or tag",
      desc: "Select a subset of the fleet by tag, client record, or manually, then run updates in one go. Live status updates for each site stream to the dashboard.",
    },
    {
      icon: "DatabaseBackup",
      title: "Auto-snapshot before every update",
      desc: "A snapshot is taken before any change lands. If the post-update health check fails, the site reverts automatically, and the failure reason is surfaced in the dashboard.",
    },
    {
      icon: "Activity",
      title: "Live per-site progress",
      desc: "Update state for each site streams in real time over a server-sent events connection. No manual refresh needed to see which sites are still running.",
    },
    {
      icon: "ScrollText",
      title: "Tamper-evident audit log",
      desc: "Every update, revert, and snapshot is written to the hash-chained audit log. The log shows who triggered the run, what changed, and the outcome per site.",
    },
    {
      icon: "ShieldCheck",
      title: "Pre-update version review",
      desc: "The dashboard shows the current version and the available version for every pending update before you apply it, so you can review changelogs and decide what to batch together.",
    },
    {
      icon: "GitFork",
      title: "Core, plugin, and theme updates",
      desc: "WPMgr handles WordPress core updates as well as plugin and theme updates, with the same snapshot-first, health-check-after safety model for each.",
    },
  ],
  faq: [
    {
      q: "What happens if an update breaks a site?",
      a: "WPMgr runs a health check after every update. If the check fails, the site is automatically reverted to the snapshot taken immediately before the update, and the failure reason is surfaced in the dashboard. No manual intervention is needed.",
    },
    {
      q: "Can I update a single site or do I have to run the whole fleet?",
      a: "Both. You can trigger an update for a single site from the site detail page, or run a bulk update across a selection, a tag group, or the whole fleet from the updates hub.",
    },
    {
      q: "Is the snapshot taken before or after the update?",
      a: "Before. WPMgr takes a snapshot of the current state before applying any update. The snapshot is preserved even after a successful update so you can roll back manually if you discover an issue later.",
    },
    {
      q: "Does update history show across the whole fleet?",
      a: "Yes. The tamper-evident audit log records every update run, auto-revert, and snapshot across all sites, with the operator who triggered the run, the timestamp, and the outcome per site.",
    },
  ],
  siblingLinks: [
    { label: "Backups and restore", href: "/features/backups" },
    { label: "Uptime and health monitoring", href: "/features/uptime-monitoring" },
  ],
  solutionLinks: [
    { label: "Manage multiple WordPress sites", href: "/solutions/manage-multiple-sites" },
  ],
};
