// Solution page: WordPress backup.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { SolutionPageData } from "@/lib/content/types";
import { SITE_CONFIG } from "@/lib/site";

export const WORDPRESS_BACKUPS_SOLUTION: SolutionPageData = {
  slug: "wordpress-backups",
  title: "WordPress backups",
  heading: "WordPress backup built for fleets",
  metaTitle: "WordPress Backup | WPMgr",
  metaDescription:
    "WPMgr provides self-hosted incremental WordPress backups with point-in-time restore, fleet-wide backup health, and database cleanup. Free, open-source, no per-site fee.",
  layoutVariant: "split",
  hero: {
    eyebrow: "Backups and restore",
    subhead:
      "Incremental backups on a schedule you control, stored where you choose, with point-in-time restore that keeps the site online. One dashboard shows backup health across every site so nothing falls through the cracks.",
    primaryCta: { label: "Start backing up for free", href: SITE_CONFIG.signup, variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "How backups work", href: "/features/backups", variant: "secondary", icon: "ArrowRight" },
  },
  outcomes: {
    heading: "A clean backup on record before every change",
    body: "A WordPress backup strategy that relies on nightly full-site archives creates two problems: storage bloat from redundant copies, and a restore point that is always hours behind the most recent change. WPMgr uses content-addressed incremental backups, recording only what changed since the last run. Before every plugin update or manual change, a pre-flight backup fires automatically so you always have a clean snapshot to fall back to. Restores run without taking the site offline. The fleet-wide backup health view flags sites with missing or stale backups before a client notices. And the database cleaner pairs with backups to keep the database lean before and after each snapshot.",
  },
  provingFeatures: [
    {
      featureSlug: "backups",
      icon: "DatabaseBackup",
      title: "Incremental backups and restore",
      summary: "Content-addressed incremental backups with fleet-wide health view, automatic pre-update snapshots, and point-in-time restore without downtime.",
      href: "/features/backups",
    },
    {
      featureSlug: "database-cleaner",
      icon: "DatabaseZap",
      title: "Database cleaner",
      summary: "Per-table scan with orphan classification, 90-day size trend, and fleet-wide view so your backups stay lean and restore faster.",
      href: "/features/database-cleaner",
    },
  ],
  stats: [
    { icon: "DatabaseBackup", value: "Incremental", label: "Only changed data is recorded each run" },
    { icon: "RotateCcw", value: "0 downtime", label: "Point-in-time restore keeps the site online" },
    { icon: "CheckCircle", value: "Pre-flight", label: "Automatic snapshot before every update" },
  ],
  faq: [
    {
      q: "How are incremental backups different from full backups?",
      a: "A full backup copies the entire site on every run, which means every archive is a complete duplicate of the previous one. WPMgr uses a content-addressed chunk store so only the blocks that changed since the last run are written. The result is faster backup jobs, far less storage, and a complete restore chain that can recover to any point.",
    },
    {
      q: "Where are backups stored?",
      a: "In the self-hosted release, backups are stored in the destination you configure: a local path on the server, an S3-compatible object store, or another remote location. The hosted cloud version will offer managed remote storage. You own the destination; WPMgr handles the scheduling and transfer.",
    },
    {
      q: "Can I restore a single database table without a full restore?",
      a: "Full point-in-time restore is the current focus. Per-table or per-file granular restore is on the roadmap as an incremental backup phase.",
    },
    {
      q: "How does the fleet-wide backup health view work?",
      a: "The backup health browser shows every site in your fleet with its last successful backup timestamp, the backup size trend, and a health indicator. Sites with no recent backup, a failed job, or a schedule gap are flagged so you can act before a client asks.",
    },
    {
      q: "Does the database cleaner affect backup size?",
      a: "Yes. Running the database cleaner before a backup removes accumulated post revisions, transients, orphaned meta records, and other bloat, which directly reduces the snapshot size and the time it takes to restore.",
    },
  ],
};
