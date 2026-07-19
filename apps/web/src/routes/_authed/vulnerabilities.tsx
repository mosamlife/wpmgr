import { useState } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";
import {
  ExternalLink,
  ShieldAlert,
  ShieldOff,
  ShieldCheck,
} from "lucide-react";

import { PageHeader } from "@/components/shared/page-header";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { VulnSeverityChip } from "@/components/status/vuln-severity-chip";
import { VulnAttributionFooter } from "@/features/security/vuln-panel";
import { VulnEnrichmentBanner } from "@/features/security/vuln-enrichment-banner";
import {
  Table,
  TableBody,
  TableHead,
  TableHeader,
  TableRow,
  TableCell,
} from "@/components/ui/table";
import { cn } from "@/lib/utils";

import {
  useFleetVulnerabilities,
  safeExternalHref,
  type FleetVulnFinding,
  type VulnSeverity,
} from "@/features/security/use-vuln";

// Fleet Vulnerabilities — GET /api/v1/vulnerabilities
//
// Replaces the <PlannedFeature> stub (previously at this route) with a real
// fleet rollup page:
//   - 5-tile severity header (Critical / High / Unknown / Medium / Low counts) +
//     total_open, click-to-filter by severity.
//   - Prioritized table: site, component name, kind, installed -> fixed,
//     severity badge, CVSS, CVE link + Wordfence reference link-back.
//   - FEED-NOT-CONFIGURED STATE: feed_ok=false renders a clear info state.
//   - DEGRADED-ENRICHMENT BANNER: feed_ok=true but enrichment_available=false
//     (GH #245) renders an amber banner. The feed is scanning fine, but the
//     last sync's CVSS enrichment pass did not complete, so findings may be
//     showing as Unknown when a real rating exists upstream.
//   - GATE 0 ATTRIBUTION: Defiant + MITRE notices rendered in footer/rows.
//
// Sidebar entry already exists (sidebar.tsx Security > Vulnerabilities).

export const Route = createFileRoute("/_authed/vulnerabilities")({
  component: VulnerabilitiesPage,
});

// ---------------------------------------------------------------------------
// Severity filter chip (summary header)
// ---------------------------------------------------------------------------

const SEVERITY_COLORS: Record<
  VulnSeverity,
  { bg: string; icon: string; label: string }
> = {
  critical: {
    bg: "border-red-300 bg-red-50 dark:border-red-800 dark:bg-red-950",
    icon: "text-red-600 dark:text-red-400",
    label: "text-red-700 dark:text-red-300",
  },
  high: {
    bg: "border-orange-200 bg-orange-50 dark:border-orange-800 dark:bg-orange-950",
    icon: "text-orange-600 dark:text-orange-400",
    label: "text-orange-700 dark:text-orange-300",
  },
  medium: {
    bg: "border-amber-200 bg-amber-50 dark:border-amber-800 dark:bg-amber-950",
    icon: "text-amber-600 dark:text-amber-400",
    label: "text-amber-700 dark:text-amber-300",
  },
  low: {
    bg: "border-[var(--color-border)] bg-[var(--color-card)]",
    icon: "text-[var(--color-muted-foreground)]",
    label: "text-[var(--color-muted-foreground)]",
  },
  // Unknown (GH #245): a genuinely-unrated finding, not a confirmed low.
  // Neutral slate, deliberately distinct from Low's plain card surface, so
  // an unrated finding never reads as "checked and safe".
  unknown: {
    bg: "border-slate-300 bg-slate-100 dark:border-slate-700 dark:bg-slate-800",
    icon: "text-slate-600 dark:text-slate-300",
    label: "text-slate-700 dark:text-slate-200",
  },
};

const SEVERITY_WORD: Record<VulnSeverity, string> = {
  critical: "Critical",
  high: "High",
  medium: "Medium",
  low: "Low",
  unknown: "Unknown",
};

function SeverityTile({
  severity,
  count,
  active,
  onClick,
}: {
  severity: VulnSeverity;
  count: number;
  active: boolean;
  onClick: () => void;
}) {
  const c = SEVERITY_COLORS[severity];
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      aria-label={`Filter by ${SEVERITY_WORD[severity]}: ${count} finding${count !== 1 ? "s" : ""}`}
      className={cn(
        "flex cursor-pointer flex-col gap-2 rounded-xl border p-4 text-left transition-opacity",
        "hover:opacity-80 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:ring-offset-2",
        c.bg,
        active && "ring-2 ring-[var(--color-ring)] ring-offset-2",
      )}
    >
      <span className={cn("text-2xl font-bold tabular-nums", c.icon)}>
        {count}
      </span>
      <span className={cn("text-xs font-medium", c.label)}>
        {SEVERITY_WORD[severity]}
      </span>
    </button>
  );
}

// ---------------------------------------------------------------------------
// Kind labels
// ---------------------------------------------------------------------------

const KIND_LABEL: Record<string, string> = {
  plugin: "Plugin",
  theme: "Theme",
  core: "WordPress core",
};

// ---------------------------------------------------------------------------
// Fleet finding table row
// ---------------------------------------------------------------------------

function FleetFindingRow({
  item,
  mitreNotice,
}: {
  item: FleetVulnFinding;
  mitreNotice: string;
}) {
  const { finding } = item;
  const hasFix = Boolean(finding.fixed_version);

  // Validate feed-supplied URLs before they touch an href attribute.
  // safeExternalHref returns undefined for any non-http(s) scheme (javascript:,
  // data:, etc.), causing the anchor branch to be skipped entirely.
  const safeCveHref = safeExternalHref(finding.cve_link);
  const safeRefHref = safeExternalHref(finding.references[0]);

  return (
    <TableRow aria-label={`${item.site_name}: ${finding.name}`}>
      <TableCell>
        <div className="space-y-0.5">
          <Link
            to="/sites/$siteId/security"
            params={{ siteId: item.site_id }}
            className="block text-sm font-medium text-[var(--color-foreground)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] max-w-[180px] truncate"
            title={item.site_url}
          >
            {item.site_name}
          </Link>
          <p className="text-xs text-[var(--color-muted-foreground)] max-w-[180px] truncate">
            {item.site_url}
          </p>
        </div>
      </TableCell>
      <TableCell>
        <div className="space-y-0.5">
          <p className="text-sm font-medium text-[var(--color-foreground)] leading-tight">
            {finding.name}
          </p>
          <p className="text-xs text-[var(--color-muted-foreground)]">
            {KIND_LABEL[finding.kind] ?? finding.kind}
            {finding.slug && finding.kind !== "core" ? (
              <>
                {" "}
                &middot;{" "}
                <span className="font-mono">{finding.slug}</span>
              </>
            ) : null}
          </p>
        </div>
      </TableCell>
      <TableCell>
        <div className="flex items-center gap-1.5 text-xs tabular-nums">
          <span className="font-mono text-[var(--color-foreground)]">
            {finding.installed_version}
          </span>
          {hasFix ? (
            <>
              <span className="text-[var(--color-muted-foreground)]">
                {"→"}
              </span>
              <span className="font-mono text-green-700 dark:text-green-400">
                {finding.fixed_version}
              </span>
            </>
          ) : null}
        </div>
      </TableCell>
      <TableCell>
        <VulnSeverityChip severity={finding.severity} />
        {finding.cvss_score != null ? (
          <p className="mt-0.5 text-xs text-[var(--color-muted-foreground)] tabular-nums">
            CVSS {finding.cvss_score.toFixed(1)}
          </p>
        ) : null}
      </TableCell>
      <TableCell>
        {finding.cve ? (
          <div className="space-y-0.5">
            {/* safeCveHref is undefined when the feed-supplied URL is not http(s). */}
            {safeCveHref ? (
              <a
                href={safeCveHref}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1 text-xs font-mono text-[var(--color-primary)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
                aria-label={`View ${finding.cve} record (opens in new tab)`}
              >
                {finding.cve}
                <ExternalLink aria-hidden="true" className="size-3" />
              </a>
            ) : (
              <span className="text-xs font-mono text-[var(--color-foreground)]">
                {finding.cve}
              </span>
            )}
            {/* GATE 0: MITRE notice is legally required on any CVE row */}
            {mitreNotice ? (
              <p
                className="text-xs text-[var(--color-muted-foreground)] leading-tight max-w-[160px]"
                aria-label="MITRE copyright notice"
              >
                {mitreNotice}
              </p>
            ) : null}
          </div>
        ) : (
          <span className="text-xs text-[var(--color-muted-foreground)]">
            n/a
          </span>
        )}
      </TableCell>
      <TableCell>
        {/* Wordfence Intelligence link-back (Gate 0).
            safeRefHref is undefined when the feed-supplied URL is not http(s). */}
        {safeRefHref ? (
          <a
            href={safeRefHref}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1 text-xs text-[var(--color-muted-foreground)] hover:text-[var(--color-foreground)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
            aria-label={`View vulnerability details on Wordfence Intelligence (opens in new tab)`}
          >
            Details
            <ExternalLink aria-hidden="true" className="size-3" />
          </a>
        ) : null}
      </TableCell>
    </TableRow>
  );
}

// ---------------------------------------------------------------------------
// Empty states
// ---------------------------------------------------------------------------

function FeedNotConfiguredState() {
  return (
    <div className="flex flex-col items-center justify-center gap-3 rounded-xl border border-[var(--color-border)] bg-[var(--color-card)] px-6 py-16 text-center">
      <ShieldOff
        aria-hidden="true"
        className="size-8 text-[var(--color-muted-foreground)]"
      />
      <div className="space-y-1.5 max-w-md">
        <p className="text-base font-medium text-[var(--color-foreground)]">
          Vulnerability feed not configured yet
        </p>
        <p className="text-sm text-[var(--color-muted-foreground)]">
          An administrator needs to connect the Wordfence Intelligence feed from
          the Admin area, under Vulnerability feed. Vulnerability scanning begins
          automatically once the feed is connected.
        </p>
      </div>
    </div>
  );
}

function NoVulnerabilitiesState() {
  return (
    <div className="flex flex-col items-center justify-center gap-3 rounded-xl border border-[var(--color-border)] bg-[var(--color-card)] px-6 py-16 text-center">
      <ShieldCheck
        aria-hidden="true"
        className="size-8 text-green-600 dark:text-green-400"
      />
      <div className="space-y-1">
        <p className="text-base font-medium text-[var(--color-foreground)]">
          No known vulnerabilities across your fleet
        </p>
        <p className="text-sm text-[var(--color-muted-foreground)]">
          All installed plugins, themes, and WordPress versions are clear.
        </p>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Skeleton
// ---------------------------------------------------------------------------

function FleetVulnSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading vulnerability data"
      className="space-y-6"
    >
      <span className="sr-only">Loading vulnerability data</span>
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-5">
        {Array.from({ length: 5 }).map((_, i) => (
          <Skeleton key={i} className="h-20 w-full rounded-xl" />
        ))}
      </div>
      <div className="overflow-hidden rounded-xl border border-[var(--color-border)]">
        {Array.from({ length: 5 }).map((_, i) => (
          <div
            key={i}
            className="flex items-center gap-4 border-b border-[var(--color-border)] px-4 py-3 last:border-0"
          >
            <Skeleton className="h-4 flex-1" />
            <Skeleton className="h-4 w-32" />
            <Skeleton className="h-5 w-16 rounded" />
            <Skeleton className="h-4 w-24" />
          </div>
        ))}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Page component
// ---------------------------------------------------------------------------

function VulnerabilitiesPage() {
  const { data, isPending, isError, error, refetch } = useFleetVulnerabilities();
  const [activeSeverity, setActiveSeverity] = useState<VulnSeverity | null>(
    null,
  );

  if (isPending) {
    return (
      <div className="space-y-6 p-4 sm:p-6">
        <PageHeader
          title="Vulnerabilities"
          subline="Known security vulnerabilities across your sites"
        />
        <FleetVulnSkeleton />
      </div>
    );
  }

  if (isError) {
    return (
      <div className="space-y-6 p-4 sm:p-6">
        <PageHeader
          title="Vulnerabilities"
          subline="Known security vulnerabilities across your sites"
        />
        <PageError
          what="Could not load fleet vulnerability data."
          why={error instanceof Error ? error.message : "Unknown error"}
          onRetry={() => void refetch()}
          retryLabel="Reload"
        />
      </div>
    );
  }

  const attribution = data.attribution;

  // FEED-NOT-CONFIGURED STATE: when feed_ok is false the scanner hasn't been
  // set up. Do NOT render "No vulnerabilities found" — that would be misleading.
  if (!data.feed_ok) {
    return (
      <div className="space-y-6 p-4 sm:p-6">
        <PageHeader
          title="Vulnerabilities"
          subline="Known security vulnerabilities across your sites"
        />
        <FeedNotConfiguredState />
        <VulnAttributionFooter
          notice={attribution.defiant_notice}
          license={attribution.defiant_license}
        />
      </div>
    );
  }

  // Filter items by active severity tile (or show all when no filter).
  const allItems = data.items ?? [];
  const filteredItems = activeSeverity
    ? allItems.filter((ff) => ff.finding.severity === activeSeverity)
    : allItems;

  function toggleSeverity(sev: VulnSeverity) {
    setActiveSeverity((prev) => (prev === sev ? null : sev));
  }

  // Order matches the CP's own ORDER BY CASE in repo.go: unknown ranks above
  // Medium/Low (an unrated finding could turn out to be a CVSS 9.8) but
  // below Critical/High.
  const severityOrder: VulnSeverity[] = ["critical", "high", "unknown", "medium", "low"];
  const countBySeverity: Record<VulnSeverity, number> = {
    critical: data.critical,
    high: data.high,
    unknown: data.unknown,
    medium: data.medium,
    low: data.low,
  };

  return (
    <div className="space-y-6 p-4 sm:p-6">
      <PageHeader
        title="Vulnerabilities"
        subline={
          data.total_open > 0
            ? `${data.total_open} open finding${data.total_open !== 1 ? "s" : ""} across your sites`
            : "No known vulnerabilities across your fleet"
        }
      />

      {/* GH #245 degraded-enrichment banner. Distinct from the FeedNotConfiguredState
          early-return above: the feed IS configured and scanning (feed_ok=true),
          but CVSS enrichment did not complete on the last sync. */}
      {!data.enrichment_available ? <VulnEnrichmentBanner /> : null}

      {/* Severity summary header */}
      <div
        className="grid grid-cols-2 gap-3 sm:grid-cols-5"
        role="group"
        aria-label="Filter vulnerabilities by severity"
      >
        {severityOrder.map((sev) => (
          <SeverityTile
            key={sev}
            severity={sev}
            count={countBySeverity[sev]}
            active={activeSeverity === sev}
            onClick={() => toggleSeverity(sev)}
          />
        ))}
      </div>

      {/* Active severity filter indicator */}
      {activeSeverity ? (
        <p className="text-xs text-[var(--color-muted-foreground)]">
          Showing{" "}
          <span className="font-medium text-[var(--color-foreground)]">
            {SEVERITY_WORD[activeSeverity]}
          </span>{" "}
          findings ({filteredItems.length}).{" "}
          <button
            type="button"
            onClick={() => setActiveSeverity(null)}
            className="text-[var(--color-primary)] underline underline-offset-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
          >
            Clear filter
          </button>
        </p>
      ) : null}

      {/* Main content */}
      {allItems.length === 0 ? (
        <NoVulnerabilitiesState />
      ) : filteredItems.length === 0 ? (
        <div className="flex items-center justify-center rounded-xl border border-[var(--color-border)] bg-[var(--color-card)] px-6 py-12">
          <p className="text-sm text-[var(--color-muted-foreground)]">
            No{" "}
            {activeSeverity ? SEVERITY_WORD[activeSeverity].toLowerCase() : ""}{" "}
            findings found.
          </p>
        </div>
      ) : (
        <div className="overflow-hidden rounded-xl border border-[var(--color-border)] bg-[var(--color-card)]">
          <div className="flex items-center gap-2 border-b border-[var(--color-border)] px-4 py-3">
            <ShieldAlert
              aria-hidden="true"
              className="size-4 text-[var(--color-muted-foreground)]"
            />
            <p className="text-sm font-medium text-[var(--color-foreground)]">
              {filteredItems.length} finding{filteredItems.length !== 1 ? "s" : ""}
              {activeSeverity
                ? ` (${SEVERITY_WORD[activeSeverity]} only)`
                : " across all sites"}
            </p>
          </div>
          <div className="w-full overflow-x-auto">
            <Table className="min-w-[900px]">
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[200px]">Site</TableHead>
                  <TableHead>Component</TableHead>
                  <TableHead className="w-[180px]">Version</TableHead>
                  <TableHead className="w-[120px]">Severity</TableHead>
                  <TableHead className="w-[160px]">CVE</TableHead>
                  <TableHead className="w-[80px]">Source</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {filteredItems.map((item) => (
                  <FleetFindingRow
                    key={`${item.site_id}-${item.finding.id}`}
                    item={item}
                    mitreNotice={attribution.mitre_notice}
                  />
                ))}
              </TableBody>
            </Table>
          </div>
        </div>
      )}

      {/* GATE 0 ATTRIBUTION — legally required on all vuln views. Do NOT remove. */}
      <VulnAttributionFooter
        notice={attribution.defiant_notice}
        license={attribution.defiant_license}
      />
    </div>
  );
}
