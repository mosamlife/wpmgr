// Backups feature page content.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { FeaturePageData } from "@/lib/content/types";
import { SITE_CONFIG } from "@/lib/site";

export const BACKUPS_PAGE: FeaturePageData = {
  slug: "backups",
  title: "WordPress Backup Plugin",
  metaTitle: "WordPress Backup Plugin with Incremental Backups | WPMgr",
  metaDescription:
    "WPMgr is a self-hosted WordPress backup plugin with incremental backups, point-in-time restore, fleet-wide backup health, and client-side encryption. Free and open source.",
  hero: {
    eyebrow: "Backups and restore",
    heading: "Incremental WordPress backups with point-in-time restore",
    subhead:
      "Schedule full and incremental backups for every site in your fleet. Restore to any snapshot with the site staying online, all without touching a shared password.",
    primaryCta: { label: "Start backing up for free", href: SITE_CONFIG.signup, variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Read the code", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
  },
  problem: {
    heading: "A full backup every night wastes time and storage",
    body: "Most WordPress backup plugins copy the entire site on every run. When something breaks at 2 am, that nightly archive is hours old, and restoring from a giant zip on a live server takes down the site for everyone. A smarter approach records only what changed, keeps the full history in an expandable chain, and lets you restore to any point without going offline.",
  },
  steps: [
    {
      n: "1",
      icon: "DatabaseBackup",
      title: "Schedule backups per site",
      desc: "Set a daily or weekly schedule per site, or run a manual backup on demand before a risky change. WPMgr queues the job and the lightweight agent plugin handles the export on the site's own server with no SSH and no shell access required.",
    },
    {
      n: "2",
      icon: "Cpu",
      title: "Increments pack only changed files",
      desc: "The first run is a full base snapshot. Every subsequent run packs only files that changed since the last base or increment. Database dumps use a pure-PHP streaming archiver that works on locked-down managed hosts without mysqldump.",
    },
    {
      n: "3",
      icon: "Upload",
      title: "Stored to the destination you choose",
      desc: "Completed archives are streamed to your configured backup destination: S3-compatible storage, SFTP, or local disk. Image bytes and file data move directly from the site to storage using short-lived presigned URLs, never through the control plane.",
    },
    {
      n: "4",
      icon: "RotateCcw",
      title: "Restore to any snapshot",
      desc: "Pick any base or increment in the chain and restore. The site stays online through the restore, the previous state is preserved as a restore point, and you can revert the revert if something still looks wrong.",
    },
  ],
  subFeatures: [
    {
      icon: "DatabaseBackup",
      title: "Incremental backup chains",
      desc: "Base plus increments in one expandable chain. Increments pack only files that changed, so storage costs stay proportional to what actually changed, not the full site size every time.",
    },
    {
      icon: "RotateCcw",
      title: "Point-in-time restore",
      desc: "Restore to any snapshot in the chain, full or incremental. The site stays reachable during restore. Disconnect and reconnect history is preserved so a full history is never lost.",
    },
    {
      icon: "LayoutGrid",
      title: "Fleet backup browser",
      desc: "See every site's backup status across the fleet: protected, stale, or unprotected. Sort and filter to find sites that have not run a successful backup in the last 24 hours.",
    },
    {
      icon: "FileLock2",
      title: "Client-side encrypted backups",
      desc: "Enable end-to-end encryption and the control plane stores only ciphertext. It never holds the decryption key, so even a compromised control plane cannot read your backup data.",
    },
    {
      icon: "ServerCog",
      title: "No shell access required",
      desc: "The agent uses a pure-PHP streaming dump and archiver. Backups work on managed and locked-down WordPress hosts without mysqldump, shell access, or FTP credentials.",
    },
    {
      icon: "RefreshCw",
      title: "Automatic pre-update snapshot",
      desc: "Fleet update runs take an automatic snapshot before applying changes. If a health check fails post-update, the site auto-reverts to the snapshot and reports the failure.",
    },
  ],
  faq: [
    {
      q: "How are incremental backups stored?",
      a: "WPMgr uses a content-addressed chunk store. A full base snapshot is taken on the first run and on a configurable cadence thereafter. Every subsequent run identifies changed files and packs only those into an incremental archive. The dashboard shows the full base-plus-increments chain so you can restore to any point.",
    },
    {
      q: "Can I restore to a specific point in time?",
      a: "Yes. Every base snapshot and every incremental archive is listed in the dashboard. Pick any one and restore. The site stays reachable during the restore, and the previous state is preserved as a revert point.",
    },
    {
      q: "Does it work on managed WordPress hosts that restrict shell access?",
      a: "Yes. The agent uses a pure-PHP streaming dump and archiver. There is no dependency on mysqldump, shell access, or cron-level permissions beyond what WordPress itself uses.",
    },
    {
      q: "Where are backups stored?",
      a: "You configure the destination: S3-compatible object storage, SFTP, or local disk on the control plane host. File data moves directly from the site to storage using short-lived presigned URLs and never passes through WPMgr's control plane.",
    },
    {
      q: "Can backups be encrypted?",
      a: "Yes. Client-side encryption is opt-in per site. When enabled the control plane stores only ciphertext and never holds the decryption key. The key stays on your infrastructure.",
    },
  ],
  siblingLinks: [
    { label: "Database Cleaner", href: "/features/database-cleaner" },
    { label: "Safe Fleet Updates", href: "/features/updates" },
  ],
  solutionLinks: [
    { label: "WordPress backup solutions", href: "/solutions/wordpress-backups" },
    { label: "Manage multiple WordPress sites", href: "/solutions/manage-multiple-sites" },
  ],
};
