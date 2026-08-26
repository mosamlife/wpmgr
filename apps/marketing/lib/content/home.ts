// Home page content module. Seeded from apps/landing/src/data/content.ts.
// House rules enforced by scripts/check-copy.mjs: no em dashes, no en dashes,
// no competitor plugin names.

import type { Cta, Chip, Step, FaqItem, FeatureCluster } from "./types";
import { SITE_CONFIG, signupHref } from "@/lib/site";

// The AGENT plugin version, which is what the wordpress.org listing serves. One
// constant so the hero badge and the provenance fact row cannot drift apart and
// contradict the listing they link to.
//
// Must equal WPMGR_AGENT_VERSION in apps/agent/wpmgr-agent.php, exactly. CI
// enforces it. Bump it in the release commit that moves the agent version, not
// on every release: the agent version deliberately does not track the repo
// release version, so a control-plane-only release leaves this alone and the
// badge stays true.
const AGENT_VERSION = "0.61.146";

export const HOME_HERO = {
  badge: `v${AGENT_VERSION} / open source`,
  heading: "The open-source WordPress fleet manager you can run, read, and contribute to",
  subhead:
    "WPMgr is a self-hostable control plane for managing one WordPress site or a whole portfolio. Back up, restore, update, monitor uptime, optimize images with the Media Optimizer, clean the database, and lock down every site from a single dashboard, all on infrastructure you own, built from code you can read and improve.",
  bodyLines: [
    "Add a site by URL, paste a one-time code into the plugin, and watch it flip from Awaiting to Connected with no page refresh.",
    "Open source under AGPL with an MIT-licensed agent. Every message it exchanges is Ed25519-signed, so nothing happens to your sites you cannot verify.",
  ],
  trust: [
    { icon: "GitFork", title: "Fork and contribute", desc: "AGPL control plane, MIT agent, PRs welcome" },
    { icon: "ServerCog", title: "Your infrastructure", desc: "Fleet data never leaves your server" },
    { icon: "FileSearch", title: "Reviewed on WordPress.org", desc: "Listed in the plugin directory, install straight from wp-admin", href: "#provenance" },
  ],
  ctas: [
    { label: "Get started for free", href: signupHref("hero"), variant: "primary" as const, icon: "ArrowRight" },
  ] satisfies Cta[],
} as const;

export const HOME_OPS_STATUS = {
  heading: "Real-time fleet at a glance",
  subhead: "Live status, uptime signals, and active incidents for every site in the fleet.",
  sites: [
    { name: "shop.example.com", status: "up" as const, latency: "118ms", uptime: "99.98%" },
    { name: "blog.example.com", status: "up" as const, latency: "142ms", uptime: "100%" },
    { name: "staging.example.com", status: "degraded" as const, latency: "840ms", uptime: "97.1%" },
    { name: "legacy.example.com", status: "up" as const, latency: "201ms", uptime: "99.82%" },
    { name: "client-a.example.com", status: "up" as const, latency: "95ms", uptime: "100%" },
  ],
} as const;

export const HOME_FEATURES: {
  eyebrow: string;
  heading: string;
  subhead: string;
  clusters: FeatureCluster[];
} = {
  eyebrow: "The platform",
  heading: "Everything you need to run a fleet, all in the open release",
  subhead:
    "One dashboard, no add-on sprawl. Five capability areas, every line of code open to read and extend.",
  clusters: [
    {
      id: "platform-operate",
      icon: "ServerCog",
      name: "Operate",
      tagline: "Connect a site in under a minute, then run the whole fleet from one screen.",
      features: [
        {
          icon: "Network",
          title: "Fleet connection",
          summary: "Sites enroll with a one-time code and stay verifiably connected.",
          bullets: [
            "Live flip from Awaiting to Connected, no refresh",
            "One-click wp-admin login, no shared passwords",
            "Sweeper verifies quiet sites directly, not just heartbeats",
            "Accurate badge: unreachable vs idle, never a false disconnect",
          ],
        },
        {
          icon: "DatabaseBackup",
          title: "Backups and restore",
          summary: "Scheduled full and incremental backups with point-in-time restore.",
          bullets: [
            "Increments pack only files that changed",
            "Base plus increments in one expandable chain",
            "Restore to any snapshot, site stays online",
            "Fleet backup browser: protected, stale, or unprotected per site",
          ],
        },
        {
          icon: "RefreshCw",
          title: "Fleet updates",
          summary: "Preview version changes, then update with an automatic safety net.",
          bullets: [
            "Snapshot first, auto-revert on failed health check",
            "Bulk runs by group or tag",
            "Live per-site progress",
          ],
        },
        {
          icon: "Activity",
          title: "Monitoring and health",
          summary: "Uptime, response time, and fleet-wide status at a glance.",
          bullets: [
            "Fleet status matrix: up, degraded, or down across all sites",
            "7, 30, and 90 day response-time trends per site",
            "Down and recovery alerts by email or webhook",
            "TLS expiry warnings and PHP fatal tracking",
          ],
        },
        {
          icon: "LayoutGrid",
          title: "Sites grid and screenshots",
          summary:
            "A list or grid view of the fleet, each card led by a live screenshot of the site.",
          bullets: [
            "Screenshot captured server-side, refreshed weekly and on demand",
            "Capability icons: cache, object cache, HTTPS, backups, multisite",
            "Uptime, latency, SSL expiry, and backup health on the card",
            "Status and tags filters compose and persist in the URL",
          ],
        },
      ],
    },
    {
      id: "platform-accelerate",
      icon: "Gauge",
      name: "Accelerate",
      tagline: "Make every page faster, then prove it with real-visitor data.",
      features: [
        {
          icon: "Zap",
          title: "Performance and caching",
          summary: "Full-page caching and asset optimization, per site or fleet-wide.",
          bullets: [
            "Pre-gzipped pages served straight from disk",
            "Unused CSS removal, minify, defer, lazy-load",
            "WOFF2 font transcoding and subsetting",
            "WooCommerce cart-session caching",
          ],
          link: { href: "#performance" },
          visual: "cache-trend" as const,
        },
        {
          icon: "BarChart2",
          title: "Real User Monitoring",
          summary: "Core Web Vitals from real visitors at the p75 Google uses.",
          bullets: [
            "LCP, INP, CLS, FCP, TTFB distributions",
            "28-day trends with threshold lines drawn on",
            "Per-URL and per-device breakdowns",
            "Anonymous, off by default, no cookies",
          ],
          link: { href: "#rum" },
          visual: "rum-distribution" as const,
        },
        {
          icon: "ImageDown",
          title: "Media Optimizer",
          summary: "Re-encode the media library to AVIF and WebP, fully reversible.",
          bullets: [
            "Originals archived on the site",
            "Right format per browser, automatic fallback",
            "No image bytes touch the control plane",
          ],
          link: { href: "#media" },
          visual: "media-compare" as const,
        },
        {
          icon: "HardDrive",
          title: "Redis Object Cache",
          summary:
            "Per-site persistent object cache that accelerates logged-in users, admin, and every uncacheable database round-trip.",
          bullets: [
            "phpredis connection, TLS, ACL, and per-site key prefix",
            "Degrades safely to in-memory cache if Redis is unreachable",
            "Hit ratio, memory, and latency history in the dashboard",
            "Debug header verifies cache state per request",
          ],
        },
      ],
    },
    {
      id: "platform-clean",
      icon: "Eraser",
      name: "Clean up",
      tagline: "Database and media hygiene that previews first and reverses cleanly.",
      features: [
        {
          icon: "DatabaseZap",
          title: "Database Cleaner",
          summary:
            "Scan first, then clean revisions, transients, and orphans in batches.",
          bullets: [
            "Per-table inventory with owner labels",
            "Orphans classified against a signature corpus",
            "90-day health trend and a fleet-wide view",
          ],
        },
        {
          icon: "RotateCcw",
          title: "Database Snapshots",
          summary: "A quick local snapshot before a risky change, instant revert after.",
          bullets: [
            "Faster and lighter than a full backup",
            "No remote storage required",
          ],
        },
        {
          icon: "Replace",
          title: "Search and Replace",
          summary:
            "Serialization-safe find and replace across the whole database.",
          bullets: [
            "PHP-serialized data survives intact",
            "Preview matches before committing",
          ],
        },
        {
          icon: "ImageOff",
          title: "Unused Image Cleaner",
          summary:
            "Finds media nothing references, with proof of where used images appear.",
          bullets: [
            "Reversible quarantine before any delete",
            "Ambiguous references count as in-use",
            "Per-image usage report",
          ],
        },
      ],
    },
    {
      id: "platform-clients",
      icon: "Handshake",
      name: "Serve clients",
      tagline:
        "Group sites by customer and put your brand on everything they see.",
      features: [
        {
          icon: "Briefcase",
          title: "Client management",
          summary: "Group any number of sites under named client records.",
          bullets: [
            "Brand color, logo, contacts, and notes",
            "Bulk-assign sites from the fleet view",
            "Filter the fleet or jump to a client page",
          ],
        },
        {
          icon: "ScrollText",
          title: "White-label reports",
          summary: "Branded maintenance reports on a schedule or on demand.",
          bullets: [
            "Uptime, backups, updates, vitals, email health",
            "HTML email, print page, and vector-chart PDF",
            "Per-section toggles, custom intro and closing",
            "Powered-by footer removable on any plan",
          ],
        },
        {
          icon: "LayoutDashboard",
          title: "Client portal",
          summary:
            "A read-only branded portal where clients see their own sites.",
          bullets: [
            "Email invites on the same login page",
            "Uptime, backups, vitals, report downloads",
            "Access revoked instantly on removal",
          ],
        },
        {
          icon: "MailCheck",
          title: "Per-site email and log",
          summary:
            "Per-site outgoing email with a central, searchable delivery log.",
          bullets: [
            "SES, SendGrid, Mailgun, Postmark, any SMTP",
            "Named connections with automatic failover",
            "Webhook bounce and complaint suppression",
            "Fleet-wide deliverability view and digests",
          ],
        },
      ],
    },
    {
      id: "platform-protect",
      icon: "LockKeyhole",
      name: "Protect",
      tagline:
        "Hardening, access control, and account flows that cannot lock you out.",
      features: [
        {
          icon: "ShieldCheck",
          title: "Hardening, bans, integrity",
          summary:
            "Per-site hardening, bans, file integrity, vulnerability scanning, user 2FA, and password policy, all opt-in.",
          bullets: [
            "Disable file editor, restrict XML-RPC, REST API, and login IDs",
            "File hashes vs WordPress.org and baseline; vuln scan vs CVEs",
            "Site-user 2FA: authenticator app, email code, backup codes",
            "Password policy: strength, breach check, reuse, expiry",
          ],
        },
        {
          icon: "Users",
          title: "Team and access",
          summary:
            "Four roles, per-site sharing, SSO, and a tamper-evident audit log.",
          bullets: [
            "Owner to viewer, least privilege by default",
            "Share one site without exposing the fleet",
            "Email sign-in or company OIDC",
          ],
        },
        {
          icon: "Mail",
          title: "Email in minutes",
          summary:
            "Point the control plane at your own SMTP server in Settings.",
          bullets: [
            "Send a test message before saving",
            "Credentials encrypted at rest",
            "All transactional mail routes through it",
          ],
        },
        {
          icon: "KeyRound",
          title: "Account recovery",
          summary:
            "Self-serve password reset and change, no support ticket.",
          bullets: [
            "Works from any device, no admin involved",
            "A change signs out every other session",
          ],
        },
        {
          icon: "UserPlus",
          title: "Open sign up",
          summary:
            "Email-verified self-registration with a configurable gate.",
          bullets: [
            "Verification link before any access",
            "No manual provisioning",
            "Lock it down when onboarding is done",
          ],
        },
        {
          icon: "ShieldCheck",
          title: "Dashboard two-factor auth",
          summary:
            "Protect dashboard access with an authenticator app or a passkey.",
          bullets: [
            "TOTP and WebAuthn passkeys, set up in a guided wizard",
            "Remember a device for 30 days, revoke any device instantly",
            "Recovery codes for lost-device access, single-use and hashed",
            "Secret encrypted at rest, all events in the audit log",
          ],
        },
      ],
    },
  ],
};

export const HOME_MEDIA = {
  eyebrow: "Flagship feature",
  heading: "Lighter images, no quality loss, fully reversible",
  subhead:
    "Turn on Media Optimization and WPMgr re-encodes your library to AVIF and WebP in the cloud. Every browser gets the format it supports, your originals stay safely archived on the site, and you can revert any image with one click.",
  bodyLines: [
    "WPMgr totals the bytes saved across every variant, including all the thumbnail sizes WordPress generates, plus a running count of images optimized, so the number on your dashboard reflects real files, not one hero image per upload.",
    "No image bytes ever pass through WPMgr's control plane. Source files move from your site to your storage, and optimized files move from the encoder to your storage, using short-lived presigned URLs while WPMgr keeps only metadata.",
    "The Unused Image Cleaner finds attachments that are not referenced anywhere, shows exactly where each image in use appears, and moves unwanted images to a reversible quarantine. Permanent deletion requires explicit confirmation. Ambiguous references are always treated as in-use, so the cleaner never flags a genuinely used image.",
  ],
  chips: [
    { icon: "ImageDown", value: "AVIF + WebP", label: "Modern formats served automatically, GIFs to animated WebP" },
    { icon: "Undo2", value: "100%", label: "Reversible: originals archived on the site" },
    { icon: "ShieldOff", value: "0 bytes", label: "Image data on the control plane, presigned URLs only" },
    { icon: "ToggleLeft", value: "Opt-in", label: "Disabled until you turn it on, per site" },
  ] satisfies Chip[],
  cta: { label: "See Media Optimization live", href: SITE_CONFIG.dashboard, icon: "ArrowRight" } satisfies Cta,
  demo: {
    caption: "A sample upload, full image plus its thumbnail set",
    originalLabel: "Original JPEG",
    originalBytes: 2_480_000,
    optimizedLabel: "Optimized AVIF",
    optimizedBytes: 712_000,
    library: [
      { label: "Optimized", pct: 72, tone: "success" as const },
      { label: "Pending", pct: 18, tone: "warning" as const },
      { label: "Unsupported", pct: 10, tone: "muted" as const },
    ],
  },
};

export const HOME_MEDIA_STEPS = {
  eyebrow: "Under the hood",
  heading: "How Media Optimization works, end to end",
  subhead:
    "Four steps, every one of them reversible. Auto-optimize runs on upload without slowing the editor.",
  steps: [
    {
      n: "1",
      icon: "Upload",
      title: "Upload or pick",
      desc: "Flip on auto-optimize and every new upload gets queued the moment WordPress finishes generating its sizes. Already have a full library? Select existing images and send them in batches. No reformatting your workflow.",
    },
    {
      n: "2",
      icon: "Cpu",
      title: "We re-encode safely",
      desc: "A dedicated cloud encoder reads each image by its real bytes, not a guessed file extension, and re-encodes the full image plus every thumbnail to AVIF or WebP. Animated GIFs become animated WebP. Your originals are renamed and archived on the site, never deleted.",
    },
    {
      n: "3",
      icon: "Globe",
      title: "Browsers get the modern format",
      desc: "A small .htaccess rule checks what each visitor's browser actually supports and serves the modern format only when it will display, falling back to the original everywhere else. Pages get lighter with no broken images and no front-end plugin bloat.",
    },
    {
      n: "4",
      icon: "RotateCcw",
      title: "Revert anytime",
      desc: "Changed your mind, or a single image looks off? Restore puts every archived original back, full image and all thumbnails, and rewrites the URLs in your content. Nothing about optimization is a one-way door.",
    },
  ] satisfies Step[],
};

export const HOME_STATS = {
  eyebrow: "Day one",
  heading: "What you get on day one",
  subhead: "Concrete capabilities, not a roadmap.",
  items: [
    { icon: "ImageDown", value: "AVIF + WebP", label: "Media Optimizer delivers modern formats to browsers that support them, originals as fallback" },
    { icon: "Undo2", value: "100%", label: "Reversible by design: media revert, backup restore, and db clean all roll back cleanly" },
    { icon: "DatabaseZap", value: "DB Cleaner", label: "Scan, classify orphans, trend 90 days of health, and act on the whole fleet at once" },
    { icon: "Activity", value: "7 / 30 / 90 days", label: "Fleet status matrix, response-time trends, and incident history across all sites" },
    { icon: "Users", value: "4 roles", label: "From owner to viewer, plus single-site sharing" },
    { icon: "ShieldCheck", value: "CVE scanning", label: "Vulnerability scanning, file integrity monitoring, hardening rules, and IP bans across the fleet" },
  ],
};

// Every fact below is copied from the live wordpress.org listing and is checkable
// against it in one click, which is the whole point of the section. If one of these
// drifts, the section argues against itself. Version comes from AGENT_VERSION above.
//
// "Tested up to" is the value apps/agent/readme.txt declares, not the value the
// listing page renders. WordPress.org rolls a declaration of 7.0 forward to the
// newest 7.0.x and renders that, so a point release of WordPress can restate the
// listing with no change here. Naming the minor is the only form of this fact
// that stays true without anybody watching it; it read 7.0.2 for exactly that
// reason and had gone stale by the time anyone looked.
export const HOME_PROVENANCE = {
  eyebrow: "Provenance",
  heading: "You do not have to take our word for it",
  subhead:
    "Two places you can check this project before you install it: the listing in the WordPress.org plugin directory, and the repository it is built in.",
  directory: {
    icon: "FileSearch",
    source: "WordPress.org plugin directory",
    title: "A human reviewed it before it went live",
    body: "Every plugin in the WordPress.org directory is read by a reviewer on the plugin review team before it is published. This is a gate the agent passed, not a page we wrote about ourselves.",
    link: {
      label: "View the listing on WordPress.org",
      href: "https://wordpress.org/plugins/fleet-agent-site-manager/",
    },
  },
  facts: [
    { label: "Plugin name", value: "Fleet Agent Site Manager" },
    { label: "Slug", value: "fleet-agent-site-manager", mono: true },
    { label: "Version", value: AGENT_VERSION, mono: true },
    { label: "Requires", value: "WordPress 6.2, PHP 8.1" },
    { label: "Tested up to", value: "WordPress 7.0" },
  ],
  repository: {
    icon: "Github",
    source: "GitHub repository",
    title: "What the directory ships is built here",
    body: "The agent, the control plane, and the full release history for both live in one public repository. Read a diff between any two releases and you can see exactly what changed on your sites.",
    link: {
      label: "View the source",
      href: SITE_CONFIG.github,
    },
  },
  checks: [
    {
      icon: "FileBadge",
      text: "The version in the directory is built from the release tagged in this repository. The directory build has the self-update path removed, because WordPress.org ships its own updates.",
    },
    {
      icon: "KeyRound",
      text: "Everything an agent sends the control plane is Ed25519-signed over the method, path, timestamp, nonce and body hash, and verified before it is trusted.",
    },
    {
      icon: "Scale",
      text: "The source in this repository is MIT. The copy distributed through the directory carries GPLv2 or later, which is the license the directory asks for.",
    },
  ],
};

export const HOME_ENROLL = {
  eyebrow: "Getting started",
  heading: "From zero to managed in under a minute",
  subhead:
    "No SSH, no FTP, no shared admin passwords. Three steps and the site is live on your dashboard.",
  body: "The agent is a lightweight PHP plugin that needs only PHP 8.1+ and WordPress 6.2+. It is listed in the WordPress.org plugin directory as Fleet Agent Site Manager, so you can install it from your own wp-admin. Backups use a pure-PHP streaming dump and archiver, so they work even on managed hosts that lock down shell access and the mysqldump binary.",
  steps: [
    {
      n: "1",
      icon: "PlusCircle",
      title: "Add the site by URL",
      desc: "Paste a site URL into the dashboard. WPMgr generates a one-time enrollment code and marks the site as Awaiting connection.",
    },
    {
      n: "2",
      icon: "KeyRound",
      title: "Push the one-time code",
      desc: "Search your Plugins screen for Fleet Agent Site Manager, install it, and paste the one-time code. The agent registers its signing key and the site flips from Awaiting to Connected with no page refresh.",
    },
    {
      n: "3",
      icon: "LayoutDashboard",
      title: "Manage everything",
      desc: "Back up, update, monitor uptime, optimize images, and lock the site down, all from the single dashboard. Disconnect cleanly anytime, and reconnecting later keeps the full history.",
    },
  ] satisfies Step[],
  cta: { label: "Or self-host it", href: "/self-host" } satisfies Cta,
};

export const HOME_FAQ: FaqItem[] = [
  {
    q: "Is WPMgr free?",
    a: "Yes. WPMgr is open source and free to self-host. The control plane and dashboard are AGPL-3.0, and the WordPress agent plugin is MIT-licensed. There is no paid tier or per-site fee to run it yourself.",
  },
  {
    q: "How do I contribute?",
    a: "Fork the repository on GitHub, pick a good-first-issue, and open a pull request. The control plane is Go with a generated OpenAPI layer, the frontend is React 19, and the agent is PHP. Each area has its own README with a dev-setup walkthrough.",
  },
  {
    q: "Do I have to self-host it?",
    a: "Self-hosting is the way WPMgr is built to run today, and it is what keeps your data on infrastructure you own. The bundled Docker Compose stack brings up the full system with a few commands, and prebuilt container images are published for production setups.",
  },
  {
    q: "Does it work on managed or locked-down WordPress hosts?",
    a: "Yes. The agent is a lightweight PHP plugin that needs PHP 8.1+ and WordPress 6.2+, nothing more. Backups use a pure-PHP streaming dump and archiver with no shell access and no mysqldump binary, so they work even on hosts that lock those down.",
  },
  {
    q: "Why is the plugin called Fleet Agent Site Manager on WordPress.org?",
    a: "That is its listing name in the WordPress.org plugin directory, where it was reviewed and published. The project is WPMgr and the plugin is the WPMgr Agent, which is the name you will see in wp-admin once it is installed from a self-hosted build. Search the plugin directory for Fleet Agent Site Manager and you have the right one.",
  },
  {
    q: "Can I install the agent with Composer?",
    a: "Yes. The directory listing is mirrored by both Composer repositories that serve WordPress.org, so require wpackagist-plugin/fleet-agent-site-manager if your project points at wpackagist.org, or wp-plugin/fleet-agent-site-manager if it points at repo.wp-packages.org, which is what current Bedrock ships. Composer installs the directory build, which updates through composer update rather than writing to your plugins folder on its own.",
  },
  {
    q: "Is my data private?",
    a: "Your data lives on the infrastructure you run WPMgr on, not ours. Backups can be client-side encrypted so the control plane only ever stores ciphertext and never holds a decryption key, image bytes move directly between your site and your storage, and the agent redacts emails, passwords, secrets, tokens, and salts before any diagnostics ever leave a site.",
  },
  {
    q: "How many sites can I manage?",
    a: "There is no built-in site limit. WPMgr is designed as fleet management for one site or many, with a real-time sites list, multi-tenant organizations, role-based access, and per-site sharing so you can hand a collaborator exactly one site without exposing the rest.",
  },
];

export const HOME_FINAL_CTA = {
  heading: "Run your whole fleet from a dashboard you own.",
  subhead:
    "Bring up the full stack with a few commands, enroll your first site with a one-time code, and run your whole fleet from a dashboard that lives on infrastructure you control. Or fork it and build what you need.",
  body: "Free, open source, no per-site fee. Read every line before you run it.",
  ctas: [
    { label: "Get started for free", href: signupHref("hero"), variant: "primary" as const, icon: "ArrowRight" },
  ] satisfies Cta[],
};
