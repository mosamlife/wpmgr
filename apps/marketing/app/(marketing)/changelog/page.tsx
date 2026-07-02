import type { Metadata } from "next";
import Link from "next/link";
import { buildMetadata, buildBreadcrumbLd } from "@/lib/seo";
import { JsonLd } from "@/lib/json-ld";
import { Container, Section } from "@/components/ui/primitives";
import { SITE_CONFIG } from "@/lib/site";

export const revalidate = 3600;

export const metadata: Metadata = buildMetadata({
  title: "Changelog: WPMgr Release Notes",
  description:
    "Every WPMgr release, newest first. See what shipped, when, and which features were added, changed, or fixed.",
  canonical: "/changelog/",
});

// ---------------------------------------------------------------------------
// Curated release entries (newest first, harvested from CHANGELOG.md).
// We show the ~20 most recent meaningful releases. The full history is
// available on GitHub Releases.
// ---------------------------------------------------------------------------

type ChangeTag = "Added" | "Changed" | "Fixed" | "Security";

type ChangeEntry = {
  version: string;
  date: string;
  summary: string;
  items: Array<{ tag: ChangeTag; text: string }>;
  featureLinks?: Array<{ label: string; href: string }>;
};

const TAG_COLOR: Record<ChangeTag, string> = {
  Added: "var(--success, oklch(55% 0.15 145))",
  Changed: "var(--info, oklch(55% 0.12 235))",
  Fixed: "var(--warning, oklch(65% 0.15 75))",
  Security: "var(--destructive)",
};

const RELEASES: ChangeEntry[] = [
  {
    version: "0.61.10",
    date: "2026-07-02",
    summary: "Bulk updates only touch sites that actually need them.",
    items: [
      {
        tag: "Fixed",
        text: "Multi-site bulk update now only creates a task for a site that actually has the selected update pending, instead of also creating a task, and then failing it, for every selected site that does not have that plugin, theme, or core update available. Sites the update does not apply to are now reported as skipped, not failed.",
      },
    ],
  },
  {
    version: "0.61.9",
    date: "2026-07-02",
    summary: "A failed update no longer strands a site in maintenance mode.",
    items: [
      {
        tag: "Fixed",
        text: "A failed plugin or theme update no longer leaves a site stuck showing WordPress's maintenance page to every visitor. The post-update health check now tolerates a brief, transient 503 from an in-progress database migration instead of treating it as a failure and rolling back an update that actually succeeded.",
      },
    ],
  },
  {
    version: "0.61.6",
    date: "2026-07-02",
    summary: "Reliable downtime alerts and a faster Sites list.",
    items: [
      {
        tag: "Fixed",
        text: "Downtime email alerts now fire reliably. A race in the alert state machine could let a sustained outage go unalerted; alert state now transitions atomically so every qualifying outage sends its alert.",
      },
      {
        tag: "Fixed",
        text: "The Notifications settings page now saves correctly, including the daily email digest toggle.",
      },
      {
        tag: "Fixed",
        text: "Fixed a slow first load of the Sites list after the dashboard had been idle for a while.",
      },
    ],
  },
  {
    version: "0.61.3",
    date: "2026-06-26",
    summary: "A batch of dashboard, backup, and update fixes.",
    items: [
      {
        tag: "Fixed",
        text: "Backups now correctly preserve plugin and theme vendor code that happens to live in a folder named cache or upgrade, while still excluding real runtime cache and update staging files.",
      },
      {
        tag: "Fixed",
        text: "The backup schedule form no longer rejects valid day-of-week, day-of-month, or hourly-interval selections, and update run history now shows accurate task counts and completion progress.",
      },
      {
        tag: "Fixed",
        text: "Closing any dialog no longer leaves the page unclickable, and bulk plugin and theme updates now default to only the items that actually have an update available.",
      },
    ],
    featureLinks: [{ label: "Backups", href: "/features/backups/" }],
  },
  {
    version: "0.61.0",
    date: "2026-06-23",
    summary: "File Manager for every managed site.",
    items: [
      {
        tag: "Added",
        text: "Browse, preview, edit, upload, download, zip, and restore prior versions of files on any managed WordPress site, right from the dashboard, no SFTP or cPanel required. A sensitive-file deny list and a separate write-access toggle keep it safe, and every action is written to the audit log. Off by default; only owners and admins can turn it on.",
      },
      {
        tag: "Added",
        text: "CloudPanel sites now get integrated cache purging: WPMgr clears its own page cache and CloudPanel's Varnish cache together, no separate plugin needed.",
      },
    ],
    featureLinks: [{ label: "File Manager", href: "/features/file-manager/" }],
  },
  {
    version: "0.57.7",
    date: "2026-06-21",
    summary: "New multipage marketing website.",
    items: [
      {
        tag: "Changed",
        text: "The public site at wpmgr.app is now a multipage, server-rendered site: a dedicated page for every feature, solution pages by audience and by job, pricing, this changelog, a resources area, and a self-hosted API reference generated from the OpenAPI spec. Faster, fully crawlable, and easier to extend.",
      },
    ],
  },
  {
    version: "0.57.0",
    date: "2026-06-21",
    summary: "Vulnerability feed configuration from the dashboard.",
    items: [
      {
        tag: "Added",
        text: "Instance administrators can configure the Wordfence Intelligence API key from a new admin page instead of an environment variable. The page shows live connection status, lets you save or remove the key, and provides a Sync now action. The key is encrypted at rest.",
      },
    ],
    featureLinks: [{ label: "Security suite", href: "/features/security/" }],
  },
  {
    version: "0.56.0",
    date: "2026-06-20",
    summary: "Vulnerability scanner across your fleet.",
    items: [
      {
        tag: "Added",
        text: "WPMgr now checks every managed site's plugins, themes, and WordPress core against the Wordfence Intelligence vulnerability feed. Each finding shows severity, affected version range, fixed version, and CVE references. One-click remediation updates the vulnerable item using the existing update flow. Findings appear per-site on the Security tab and fleet-wide on the Vulnerabilities page.",
      },
    ],
    featureLinks: [{ label: "Security suite", href: "/features/security/" }],
  },
  {
    version: "0.55.0",
    date: "2026-06-20",
    summary: "2FA enrollment flow for site users + redesigned Security tab.",
    items: [
      {
        tag: "Added",
        text: "After an operator requires 2FA for a user role, affected users now see a guided enrollment screen on next login: scan a QR code, confirm a code, save backup codes. Users can also start enrollment proactively from their WordPress profile.",
      },
      {
        tag: "Changed",
        text: "The per-site Security tab is now a card-based layout with a status overview strip and collapsible setting groups: Login and Two-Factor, Password policy, Hardening, File integrity, Bans, and Hide login.",
      },
    ],
    featureLinks: [
      { label: "Two-factor auth", href: "/features/two-factor-auth/" },
      { label: "Security suite", href: "/features/security/" },
    ],
  },
  {
    version: "0.54.0",
    date: "2026-06-20",
    summary: "2FA for WordPress site users, password policy, and hidden login.",
    items: [
      {
        tag: "Added",
        text: "Operators can require 2FA for chosen user roles, enforced at the WordPress login screen. Methods: authenticator app (TOTP), email one-time code, backup codes. A grace period lets users enroll before enforcement. The control plane and wp-config bypass can never be locked out.",
      },
      {
        tag: "Added",
        text: "Per-site password policy: minimum strength, known-compromised password blocking (privacy-preserving prefix query), reuse blocking, and optional expiry.",
      },
      {
        tag: "Added",
        text: "Hide login page: move wp-login.php to a secret address per site. All three controls are per-site, opt-in, and off by default.",
      },
    ],
    featureLinks: [{ label: "Two-factor auth", href: "/features/two-factor-auth/" }],
  },
  {
    version: "0.53.0",
    date: "2026-06-20",
    summary: "File integrity monitoring.",
    items: [
      {
        tag: "Added",
        text: "File integrity scanning over the full WordPress install or just wp-content. The control plane compares scanned file hashes against WordPress.org checksums for core, wp.org-hosted plugins and themes, and against a learned per-site baseline for everything else. Flagged files stay flagged until an operator reviews and accepts them.",
      },
    ],
    featureLinks: [{ label: "Security suite", href: "/features/security/" }],
  },
  {
    version: "0.52.0",
    date: "2026-06-20",
    summary: "Per-site WordPress hardening controls and IP ban list.",
    items: [
      {
        tag: "Added",
        text: "Security tab with hardening controls: file editor, XML-RPC, REST API restriction, login identifier restriction, force unique nicknames, block author enumeration, SSL with HSTS, directory browsing, PHP execution in uploads, and system file protection. All off by default.",
      },
      {
        tag: "Added",
        text: "Per-site ban list: blocked IP addresses, CIDR ranges, and user agents, stored on the control plane and enforced in the agent. Broad blocks and operator IPs are never banned.",
      },
    ],
    featureLinks: [{ label: "Security suite", href: "/features/security/" }],
  },
  {
    version: "0.51.5",
    date: "2026-06-20",
    summary: "Bulk and update-triggered backups now actually run.",
    items: [
      {
        tag: "Fixed",
        text: "The Sites bulk backup action, command bar backup commands, and the Updates tab Take backup first option previously showed feedback but never enqueued a backup. They now enqueue real backups, report per-site results, and the toast action link goes to the right place.",
      },
    ],
    featureLinks: [{ label: "Backups", href: "/features/backups/" }],
  },
  {
    version: "0.51.4",
    date: "2026-06-19",
    summary: "Uptime data now correct when using ClickHouse metrics backend.",
    items: [
      {
        tag: "Fixed",
        text: "Fleet uptime status and the uptime/SSL column in the Sites list read probe data directly from Postgres, so ClickHouse deployments saw every site as Unknown. Both now read through the metrics store so both backends display correct status, uptime percentage, latency, and TLS expiry.",
      },
    ],
  },
  {
    version: "0.51.3",
    date: "2026-06-19",
    summary: "Scheduled backups no longer re-fire every few minutes.",
    items: [
      {
        tag: "Fixed",
        text: "A schedule whose next-run time had slipped into the past was re-triggered on every scheduler tick, producing overlapping runs. Re-enabling or an overdue schedule now advances to the next future run slot. The scheduler claims and advances each due schedule atomically, and only one backup per site runs at a time.",
      },
    ],
    featureLinks: [{ label: "Backups", href: "/features/backups/" }],
  },
  {
    version: "0.50.0",
    date: "2026-06-16",
    summary: "Dashboard two-factor authentication (TOTP + WebAuthn).",
    items: [
      {
        tag: "Added",
        text: "Operators can protect their account with an authenticator app (TOTP) and/or a passkey or security key (WebAuthn/FIDO2). Setup is a guided flow with recovery codes. At login, a second step asks for the code or passkey; Remember this device can skip it for 30 days.",
      },
      {
        tag: "Security",
        text: "The TOTP secret is encrypted at rest, recovery codes are hashed and single-use, used codes are burned to prevent replay, a cloned authenticator is detected and rejected, and attempts are rate-limited. All two-factor events are in the audit log.",
      },
    ],
    featureLinks: [{ label: "Two-factor auth", href: "/features/two-factor-auth/" }],
  },
  {
    version: "0.49.0",
    date: "2026-06-16",
    summary: "Sites grid view with website screenshots.",
    items: [
      {
        tag: "Added",
        text: "The Sites dashboard has a list/grid toggle. The grid shows each site as a rich card with a real server-side screenshot, connection state, capability strip, pending updates, backup health, SSL expiry, uptime, versions, host, and tags.",
      },
      {
        tag: "Fixed",
        text: "Sites filters (Status and Tags) were previously inert. They are now real multi-select filters that compose with search and all other filters, with applied-count badges and a clear-all control.",
      },
      {
        tag: "Security",
        text: "Screenshot capture runs headless Chromium behind an in-process SSRF guard. QUIC, HTTP/3, and non-proxied WebRTC are disabled. The screenshot table is tenant-isolated with a restrictive row policy.",
      },
    ],
  },
  {
    version: "0.48.3",
    date: "2026-06-15",
    summary: "Activity log integrity report.",
    items: [
      {
        tag: "Added",
        text: "The Chain break badge is now a button that opens a report explaining why the tamper-evident audit chain failed to verify. The control plane classifies the break into missing events, a broken link, modified content, or a missing chain start.",
      },
    ],
    featureLinks: [{ label: "Team and access control", href: "/features/team-access/" }],
  },
  {
    version: "0.48.0",
    date: "2026-06-15",
    summary: "Fleet email and deliverability dashboard.",
    items: [
      {
        tag: "Added",
        text: "A new cross-site Email view: sent, failed, bounced, and complained totals with fleet bounce and complaint rates shown against provider thresholds. A per-site deliverability table lists every site with its provider, volume, rates, and sparkline, sorted riskiest first.",
      },
    ],
    featureLinks: [{ label: "Per-site email", href: "/features/email-deliverability/" }],
  },
  {
    version: "0.47.0",
    date: "2026-06-15",
    summary: "Fleet uptime, backup browser, and redesigned performance dashboard.",
    items: [
      {
        tag: "Added",
        text: "Fleet uptime overview: status tiles, dense status matrix (one cell per site), virtualized per-site table with 90-day uptime strip and response-time sparkline, and a cross-site incident feed.",
      },
      {
        tag: "Added",
        text: "Fleet backup browser: protected/stale/failed/unprotected tiles, virtualized per-site table with last-good-backup age, next scheduled run, size trend sparkline, and run-backup/restore actions.",
      },
      {
        tag: "Changed",
        text: "Performance dashboard redesigned as a true fleet view with headline figures, worst-offenders table with Core Web Vitals distribution bars, 28-day trend, and database-health rollup.",
      },
    ],
    featureLinks: [
      { label: "Backups", href: "/features/backups/" },
      { label: "Uptime monitoring", href: "/features/uptime-monitoring/" },
      { label: "Real User Monitoring", href: "/features/real-user-monitoring/" },
    ],
  },
  {
    version: "0.46.0",
    date: "2026-06-15",
    summary: "wp.org compliance: local backup paths and prepared DB queries.",
    items: [
      {
        tag: "Changed",
        text: "Local backups now store under the uploads directory with a deny-all .htaccess so archives are never directly downloadable.",
      },
      {
        tag: "Changed",
        text: "Database queries in the object-cache drop-in and media URL rewriter now use prepared placeholders.",
      },
      {
        tag: "Added",
        text: "The readme lists every outbound service the agent contacts, with links to terms and privacy for each.",
      },
    ],
  },
  {
    version: "0.45.0",
    date: "2026-06-13",
    summary: "Cron keep-alive and connection reliability improvements.",
    items: [
      {
        tag: "Added",
        text: "Cron keep-alive ensures the agent connection health check fires on schedule even on hosts that do not run WP-Cron reliably.",
      },
    ],
  },
];

function formatDate(iso: string) {
  return new Date(iso).toLocaleDateString("en-US", {
    year: "numeric",
    month: "long",
    day: "numeric",
  });
}

export default function ChangelogPage() {
  const breadcrumb = buildBreadcrumbLd([
    { name: "Home", href: "/" },
    { name: "Changelog", href: "/changelog/" },
  ]);

  return (
    <>
      <JsonLd data={breadcrumb} />

      {/* Hero */}
      <section className="border-b border-[var(--border)] py-16 sm:py-20">
        <Container>
          <div className="max-w-2xl">
            <p className="mb-3 text-sm font-semibold uppercase tracking-[0.14em] text-[var(--eyebrow)]">
              Changelog
            </p>
            <h1 className="text-4xl font-semibold tracking-tight text-foreground sm:text-5xl">
              What shipped and when
            </h1>
            <p className="mt-5 text-lg leading-relaxed text-[var(--muted-foreground)]">
              Every WPMgr release, newest first. Each entry links to the relevant feature pages.
              For the full history and release artifacts, see{" "}
              <a
                href={`${SITE_CONFIG.github}/releases`}
                target="_blank"
                rel="noreferrer noopener"
                className="font-medium text-[var(--primary)] underline underline-offset-4 hover:opacity-80 transition-opacity"
              >
                GitHub Releases
              </a>
              .
            </p>
          </div>
        </Container>
      </section>

      {/* Release feed */}
      <Section>
        <Container className="max-w-4xl">
          <div className="relative">
            {/* Timeline spine */}
            <div
              aria-hidden
              className="absolute left-[15px] top-0 bottom-0 w-px bg-[var(--border)] sm:left-[19px]"
            />

            <ol className="space-y-12" aria-label="Release history">
              {RELEASES.map((release) => (
                <li key={release.version} className="relative pl-10 sm:pl-14">
                  {/* Dot */}
                  <div
                    aria-hidden
                    className="absolute left-0 top-1.5 h-[30px] w-[30px] sm:h-[38px] sm:w-[38px] flex items-center justify-center rounded-full border-2 border-[var(--border)] bg-[var(--background)] text-[10px] font-bold text-[var(--primary)]"
                  >
                    <span className="hidden sm:inline text-[9px]">v</span>
                    <span className="text-[8px] sm:text-[8px] leading-none">
                      {release.version.split(".").slice(0, 2).join(".")}
                    </span>
                  </div>

                  {/* Content */}
                  <div>
                    <div className="flex flex-wrap items-baseline gap-3">
                      <a
                        href={`${SITE_CONFIG.github}/releases/tag/v${release.version}`}
                        target="_blank"
                        rel="noreferrer noopener"
                        className="font-mono text-lg font-semibold text-foreground hover:text-[var(--primary)] transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] rounded-sm"
                      >
                        v{release.version}
                      </a>
                      <time
                        dateTime={release.date}
                        className="text-sm text-[var(--muted-foreground)]"
                      >
                        {formatDate(release.date)}
                      </time>
                    </div>

                    <p className="mt-1 font-medium text-foreground">{release.summary}</p>

                    <ul className="mt-4 space-y-3" aria-label="Changes in this release">
                      {release.items.map((item, i) => (
                        <li key={i} className="flex items-start gap-3">
                          <span
                            className="mt-0.5 inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-xs font-semibold"
                            style={{
                              background: `color-mix(in oklch, ${TAG_COLOR[item.tag]} 15%, transparent)`,
                              color: TAG_COLOR[item.tag],
                            }}
                          >
                            {item.tag}
                          </span>
                          <span className="text-sm leading-relaxed text-[var(--muted-foreground)]">
                            {item.text}
                          </span>
                        </li>
                      ))}
                    </ul>

                    {release.featureLinks && release.featureLinks.length > 0 && (
                      <div className="mt-4 flex flex-wrap gap-3">
                        {release.featureLinks.map((link) => (
                          <Link
                            key={link.href}
                            href={link.href}
                            className="inline-flex items-center gap-1.5 rounded-full border border-[var(--border)] bg-card px-3 py-1 text-xs font-medium text-[var(--muted-foreground)] transition-colors hover:bg-[var(--accent)] hover:text-[var(--accent-foreground)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
                          >
                            {link.label}
                          </Link>
                        ))}
                      </div>
                    )}
                  </div>
                </li>
              ))}
            </ol>
          </div>

          {/* Full history link */}
          <div className="mt-14 rounded-xl border border-[var(--border)] bg-[var(--muted)]/40 p-6 text-center">
            <p className="text-[var(--muted-foreground)]">
              This page shows the most recent releases. For the complete release history and all
              release artifacts:
            </p>
            <a
              href={`${SITE_CONFIG.github}/releases`}
              target="_blank"
              rel="noreferrer noopener"
              className="mt-3 inline-flex items-center gap-2 rounded-[var(--radius)] border border-[var(--border)] bg-card px-5 py-2.5 text-sm font-medium text-foreground shadow-sm transition-colors hover:bg-[var(--accent)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]"
            >
              View all releases on GitHub
            </a>
          </div>
        </Container>
      </Section>
    </>
  );
}
