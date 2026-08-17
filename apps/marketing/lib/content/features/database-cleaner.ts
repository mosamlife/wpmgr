// Database cleaner feature page content.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { FeaturePageData } from "@/lib/content/types";
import { SITE_CONFIG, signupHref } from "@/lib/site";

export const DATABASE_CLEANER_PAGE: FeaturePageData = {
  slug: "database-cleaner",
  title: "WordPress Database Cleaner",
  metaTitle: "WordPress Database Cleanup: Clean wp_options and Revisions",
  metaDescription:
    "WordPress database cleaner that scans first and cleans second. Remove revisions, transients, and bloated wp_options rows. 90-day trend, fully reversible.",
  hero: {
    eyebrow: "Database Cleaner",
    heading: "WordPress database cleanup that scans first and never guesses",
    subhead:
      "Scan the WordPress database, see a per-table inventory with owner labels, classify orphans against a signature corpus, then clean in batches. The 90-day health trend shows whether the database is shrinking or growing back.",
    primaryCta: { label: "Start cleaning for free", href: signupHref("feature-hero"), variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Read the code", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
  },
  problem: {
    heading: "A bloated wp_options table slows every page and most database cleaners delete data without showing what they removed",
    body: "WordPress accumulates revisions, expired transients, orphaned postmeta rows, and auto-loaded wp_options entries over time. Left unchecked these slow autoloaded queries and inflate the database size. Most cleanup tools run silently and give you no way to verify what was removed or roll back if something breaks. WPMgr scans first, shows you exactly what it found, and lets you clean in reviewed batches.",
  },
  steps: [
    {
      n: "1",
      icon: "DatabaseZap",
      title: "Scan and inventory the database",
      desc: "WPMgr runs a scan and produces a per-table row count with owner labels showing which plugin or theme registered each table. You see what is in the database before anything is removed.",
    },
    {
      n: "2",
      icon: "FileScan",
      title: "Classify orphans against the corpus",
      desc: "Orphaned rows are matched against a signature corpus of known plugin and theme data patterns. The corpus distinguishes safe-to-clean orphans from data that looks orphaned but is actually in use.",
    },
    {
      n: "3",
      icon: "DatabaseBackup",
      title: "Snapshot before you clean",
      desc: "Take a quick database snapshot before a clean run. The snapshot is faster and lighter than a full backup and stays on the site's own server so restoring does not require remote storage.",
    },
    {
      n: "4",
      icon: "Eraser",
      title: "Clean in batches, track the trend",
      desc: "Clean revisions, transients, orphaned postmeta, and wp_options bloat in configurable batches. The 90-day health trend shows database size over time so you can see whether cleaning is having the expected effect.",
    },
  ],
  subFeatures: [
    {
      icon: "DatabaseZap",
      title: "Per-table inventory with owner labels",
      desc: "Every table in the scan is labelled with the plugin or theme that created it. Orphaned tables are highlighted. You see the full picture before choosing what to clean.",
    },
    {
      icon: "FileScan",
      title: "Orphan classification corpus",
      desc: "Orphaned rows are classified against a curated signature corpus that identifies data patterns from known plugins and themes. This prevents false positives from flagging data that is still in use.",
    },
    {
      icon: "TrendingUp",
      title: "90-day health trend",
      desc: "Database size and table health are tracked over 90 days. The trend chart shows whether the database is shrinking after a clean run or accumulating bloat again.",
    },
    {
      icon: "LayoutGrid",
      title: "Fleet-wide database view",
      desc: "The fleet database health view shows which sites have clean databases, which have pending orphans flagged, and which have not been scanned recently, so nothing falls through the cracks.",
    },
    {
      icon: "RotateCcw",
      title: "Quick local snapshots",
      desc: "A database snapshot can be taken and restored without remote storage. It is faster and lighter than a full backup and stays on the site's server for immediate revert access.",
    },
    {
      icon: "Replace",
      title: "Serialization-safe search and replace",
      desc: "Find and replace across the whole database with PHP-serialized data intact. Preview matches before committing so you can verify what will change.",
    },
  ],
  faq: [
    {
      q: "Does the database cleaner delete data automatically?",
      a: "No. The cleaner scans first and presents results for review. Cleanup runs require a deliberate action. There is no silent auto-delete mode.",
    },
    {
      q: "What is the signature corpus?",
      a: "The corpus is a curated database of data patterns from known WordPress plugins and themes. When the scanner finds orphaned rows it checks them against the corpus to distinguish data that is genuinely safe to remove from data that only looks orphaned because of a foreign-key gap.",
    },
    {
      q: "Is a backup taken before cleaning?",
      a: "WPMgr encourages taking a quick database snapshot before any clean run. The snapshot is faster and lighter than a full backup and stays on the site's server. You can also run a full backup via the Backups feature first if you prefer.",
    },
    {
      q: "Can I clean the database across the whole fleet?",
      a: "Yes. The fleet database view shows which sites have pending orphans and their database sizes. You can initiate a clean run on multiple sites from the fleet view.",
    },
  ],
  siblingLinks: [
    { label: "Backups and restore", href: "/features/backups" },
    { label: "Performance and page caching", href: "/features/performance" },
  ],
  solutionLinks: [
    { label: "Manage multiple WordPress sites", href: "/solutions/manage-multiple-sites" },
  ],
};
