// File Manager feature page content.
// House rules: no em dashes, no en dashes, no competitor plugin names.
import type { FeaturePageData } from "@/lib/content/types";
import { SITE_CONFIG, signupHref } from "@/lib/site";

export const FILE_MANAGER_PAGE: FeaturePageData = {
  slug: "file-manager",
  title: "WordPress File Manager",
  metaTitle: "WordPress File Manager: Browse, Edit, and Upload Without SFTP",
  metaDescription:
    "Browse, edit, upload, download, and manage files on any managed WordPress site from the WPMgr dashboard. Version history, archive and extract, file search, executable-write prevention. Off by default, owner-gated, fully audited.",
  hero: {
    eyebrow: "File Manager",
    heading: "Manage WordPress site files from the dashboard, no SFTP needed",
    subhead:
      "Browse the full file tree, edit files inline, upload by drag-and-drop, zip and extract archives, search by name or content, and restore prior versions from an encrypted history panel. Off by default, restricted to owner and admin, and every action is written to the audit log.",
    primaryCta: { label: "Try it for free", href: signupHref("feature-hero"), variant: "primary", icon: "ArrowRight" },
    secondaryCta: { label: "Read the code", href: SITE_CONFIG.github, variant: "secondary", icon: "Github" },
  },
  problem: {
    heading: "Every file change on a managed site used to mean opening an SFTP client or a host control panel",
    body: "Fixing a config file, reviewing a plugin's output, or uploading a replacement asset all require switching tools. A file manager built into the fleet dashboard removes that context switch without relaxing security: write access is off by default, executable files are blocked from being written, protected WordPress directories are off-limits, and every action is written to the same tamper-evident audit log that covers every other WPMgr operation.",
  },
  steps: [
    {
      n: "1",
      icon: "FolderOpen",
      title: "Browse, preview, and download",
      desc: "Enable read access per site. Browse the full file tree, preview text files inline, and download any file. Binary files and large files download via presigned URL. A deny-list covering wp-config.php, .env, and key files requires owner confirmation before access.",
    },
    {
      n: "2",
      icon: "FilePen",
      title: "Edit, upload, rename, and delete",
      desc: "Enable the separate write toggle (off by default) to unlock editing files, uploading by drag-and-drop, creating folders, renaming, deleting with a typed confirmation, and changing permissions to safe modes. PHP files and any content containing executable markers are blocked from being written. wp-admin, wp-includes, and WordPress core are always protected.",
    },
    {
      n: "3",
      icon: "FileArchive",
      title: "Archive, extract, and search",
      desc: "Zip any file or folder and download the archive in one action. Extract a zip back into the site with zip-slip and zip-bomb protection enforced before a single byte is written. Search files by name or by content across the whole tree.",
    },
    {
      n: "4",
      icon: "History",
      title: "Browse and restore version history",
      desc: "Every edit and overwrite auto-saves an encrypted prior version. Open the version history panel on any file to see past revisions with timestamps and restore any of them in one click. No separate backup run required.",
    },
  ],
  subFeatures: [
    {
      icon: "FolderOpen",
      title: "Full file tree browser",
      desc: "Browse the entire WordPress file system from the dashboard. Preview text files inline, download any file, and get binary and large files via presigned URL.",
    },
    {
      icon: "ShieldCheck",
      title: "Executable-write prevention",
      desc: "The file manager refuses to write PHP files or any file whose content contains executable markers. wp-admin, wp-includes, and WordPress core directories are always protected.",
    },
    {
      icon: "FileArchive",
      title: "Archive and extract",
      desc: "Zip files or folders and download the archive. Extract a zip back into the site with zip-slip traversal and zip-bomb size protection enforced before any file is written.",
    },
    {
      icon: "Search",
      title: "File search",
      desc: "Search the file tree by file name or by file content across the whole site without opening an SSH session.",
    },
    {
      icon: "History",
      title: "Encrypted version history",
      desc: "Every edit auto-saves an encrypted prior version. Browse past revisions by timestamp and restore any version in one click from the history panel.",
    },
    {
      icon: "ClipboardList",
      title: "Filterable operator audit log",
      desc: "Every file read, write, delete, upload, extract, restore, and denial is written to the audit log. Filter the Audit page by the File manager action group or by site. A View activity link in the file manager jumps straight to that site's file trail.",
    },
  ],
  faq: [
    {
      q: "Is the file manager on by default?",
      a: "No. Read access must be explicitly enabled per site by an owner or admin. Write access has a separate toggle that is also off by default. No file manager capability is active until an operator turns it on.",
    },
    {
      q: "Who can use the file manager?",
      a: "Read and write access are restricted to owner and admin roles. Operator and viewer roles cannot access the file manager regardless of the per-site toggle state.",
    },
    {
      q: "How are sensitive files protected?",
      a: "A deny-list covering wp-config.php, .env, private key files, and similar sensitive paths requires explicit owner confirmation before read access is granted. On the write side, the file manager blocks writing PHP files or any file containing executable markers, and refuses requests targeting wp-admin, wp-includes, or WordPress core paths.",
    },
    {
      q: "What does the version history cover?",
      a: "Every time a file is edited or overwritten through the file manager, the prior version is encrypted and saved automatically. The history panel on each file lists every saved revision with its timestamp. Any revision can be restored in one click.",
    },
    {
      q: "Is zip extraction safe?",
      a: "Yes. Before any file is written, the extractor checks every entry for zip-slip path traversal (canonical-path containment check against the destination) and enforces a size ceiling to prevent zip-bomb decompression attacks.",
    },
    {
      q: "Where do file manager actions appear in the audit log?",
      a: "All file manager actions (reads, writes, deletes, uploads, extractions, version restores, and denials) are written to the operator audit log, which is the same tamper-evident hash-chained log that covers all other WPMgr operations. The Audit page can be filtered to show only File manager actions, and filtered further by site.",
    },
  ],
  siblingLinks: [
    { label: "Security suite", href: "/features/security" },
    { label: "Backups and restore", href: "/features/backups" },
    { label: "Team and access control", href: "/features/team-access" },
  ],
  solutionLinks: [
    { label: "Manage multiple WordPress sites", href: "/solutions/manage-multiple-sites" },
  ],
};
