import { useState } from "react";
import { ExternalLink, RefreshCw, ShieldOff, ShieldCheck } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Table,
  TableBody,
  TableHead,
  TableHeader,
  TableRow,
  TableCell,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { VulnSeverityChip } from "@/components/status/vuln-severity-chip";
import { VulnEnrichmentBanner } from "./vuln-enrichment-banner";
import { toast } from "@/components/toast";
import { relativeTime } from "@/lib/utils";

import {
  useSiteVulnerabilities,
  useRescanVulns,
  useDismissVuln,
  useRestoreVuln,
  useRemediateVuln,
  safeExternalHref,
  type VulnFinding,
} from "./use-vuln";
import { useUpdateRun, useRunEventStream } from "@/features/updates/use-updates";

// Security Suite Phase 4 — per-site vulnerability panel.
//
// Renders inside a SecurityCard ("Vulnerabilities") in $siteId.security.tsx.
// The card body lists each finding: name, kind, installed -> fixed version,
// severity, CVSS, CVE link + MITRE notice (Gate 0), title.
//
// Per-finding actions:
//   "Update to X"  — calls remediate (CP maps to update.CreateRun). Tracks
//                    the resulting run via useUpdateRun + useRunEventStream
//                    (same flow as use-row-update.ts on the Updates page).
//   "Dismiss"      — POST /:id/dismiss (PermSecurityManage).
//   "Restore"      — POST /:id/restore for dismissed findings.
//
// FEED-NOT-CONFIGURED STATE: when feed_ok is false, render an informational
// state — never an empty "No vulnerabilities found" which would be misleading
// when the feed hasn't been set up.
//
// DEGRADED-ENRICHMENT BANNER (GH #245): when feed_ok is true but
// enrichment_available is false, the scanner is healthy but the last sync's
// CVSS enrichment pass did not complete. Some findings may be showing as
// Unknown severity when a real rating exists upstream. Distinct from the
// feed-not-configured state; rendered via VulnEnrichmentBanner.
//
// GATE 0 ATTRIBUTION: Defiant copyright/license footer + MITRE notice on CVE
// rows. These are legally required and must NOT be removed.

// ---------------------------------------------------------------------------
// Kind labels
// ---------------------------------------------------------------------------

const KIND_LABEL: Record<string, string> = {
  plugin: "Plugin",
  theme: "Theme",
  core: "WordPress core",
};

// ---------------------------------------------------------------------------
// Row remediation tracker
// Each VulnFindingRow owns its own remediation run tracking via the same
// `useUpdateRun` + `useRunEventStream` hooks used on the Updates page. This
// keeps per-row state isolated.
// ---------------------------------------------------------------------------

interface FindingRowProps {
  finding: VulnFinding;
  siteId: string;
  mitreNotice: string;
  canWrite: boolean;
}

function VulnFindingRow({ finding, siteId, mitreNotice, canWrite }: FindingRowProps) {
  const dismiss = useDismissVuln(siteId);
  const restore = useRestoreVuln(siteId);
  const remediate = useRemediateVuln(siteId);
  const [remRunId, setRemRunId] = useState<string | null>(null);

  // Track live progress of the update run triggered by remediation.
  // `enabled` gates the query so we do not fetch run details unnecessarily.
  const { data: remRun } = useUpdateRun(remRunId ?? "", {
    poll: true,
    enabled: Boolean(remRunId),
  });
  useRunEventStream(remRunId ?? "", { enabled: Boolean(remRunId) });

  const remRunStatus = remRun?.status;
  const isRemediating =
    Boolean(remRunId) && remRunStatus !== "completed";
  const remSucceeded = remRunStatus === "completed";

  function handleRemediate() {
    remediate.mutate(finding.id, {
      onSuccess: (data) => {
        setRemRunId(data.run_id);
        toast.success("Update queued.", {
          description: `Updating ${finding.name} to ${finding.fixed_version ?? "latest"}. Check Updates for progress.`,
        });
      },
      onError: (err: Error) => {
        toast.error("Could not queue update.", { description: err.message });
      },
    });
  }

  function handleDismiss() {
    dismiss.mutate(finding.id, {
      onSuccess: () => {
        toast.success("Finding dismissed.", {
          description: `${finding.name} will no longer appear in the open findings list.`,
        });
      },
      onError: (err: Error) => {
        toast.error("Could not dismiss finding.", { description: err.message });
      },
    });
  }

  function handleRestore() {
    restore.mutate(finding.id, {
      onSuccess: () => {
        toast.success("Finding restored.", {
          description: `${finding.name} is now visible in the open findings list.`,
        });
      },
      onError: (err: Error) => {
        toast.error("Could not restore finding.", { description: err.message });
      },
    });
  }

  const isDismissed = finding.status === "dismissed";
  const hasFix = Boolean(finding.fixed_version);

  // Validate feed-supplied URLs before they touch an href attribute.
  // safeExternalHref returns undefined for any non-http(s) scheme (javascript:,
  // data:, etc.), causing the anchor branch to be skipped entirely.
  const safeCveHref = safeExternalHref(finding.cve_link);
  const safeRefHref = safeExternalHref(finding.references[0]);
  const safeWordfenceLinkHref = safeExternalHref(finding.wordfence_link);

  return (
    <TableRow
      className={isDismissed ? "opacity-60" : undefined}
      aria-label={`Vulnerability: ${finding.name}`}
    >
      <TableCell>
        <VulnSeverityChip severity={finding.severity} />
      </TableCell>
      <TableCell>
        <div className="space-y-0.5">
          <p className="text-sm font-medium text-[var(--color-foreground)] leading-tight">
            {finding.name}
          </p>
          <p className="text-xs text-[var(--color-muted-foreground)]">
            {KIND_LABEL[finding.kind] ?? finding.kind}
            {finding.slug && finding.kind !== "core" ? (
              <> &middot; <span className="font-mono">{finding.slug}</span></>
            ) : null}
          </p>
        </div>
      </TableCell>
      <TableCell>
        <div className="space-y-0.5">
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
          {finding.cvss_score != null ? (
            <p className="text-xs text-[var(--color-muted-foreground)] tabular-nums">
              CVSS {finding.cvss_score.toFixed(1)}
            </p>
          ) : null}
        </div>
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
                className="text-xs text-[var(--color-muted-foreground)] leading-tight"
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
        <div className="flex flex-col items-start gap-0.5">
          {/* Wordfence Intelligence reference link-back (Gate 0).
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
          {/* Per-record Wordfence Intelligence link-back, required by the
              Defiant Wordfence Intelligence feed license: any copy of a
              vulnerability record must include a hyperlink to that record.
              safeWordfenceLinkHref is undefined when wordfence_link is absent
              (older CP response) or not an http(s) URL. */}
          {safeWordfenceLinkHref ? (
            <a
              href={safeWordfenceLinkHref}
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center gap-1 text-xs text-[var(--color-muted-foreground)] hover:text-[var(--color-foreground)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
              aria-label="View the Wordfence record (opens in new tab)"
            >
              View the Wordfence record
              <ExternalLink aria-hidden="true" className="size-3" />
            </a>
          ) : null}
        </div>
      </TableCell>
      {canWrite ? (
        <TableCell>
          <div className="flex items-center justify-end gap-1.5 flex-wrap">
            {!isDismissed && hasFix ? (
              <Button
                type="button"
                size="sm"
                variant="outline"
                disabled={remediate.isPending || isRemediating || remSucceeded}
                aria-busy={remediate.isPending || isRemediating}
                onClick={handleRemediate}
                className="h-7 px-2 text-xs whitespace-nowrap"
              >
                {remSucceeded
                  ? "Update queued"
                  : isRemediating
                    ? "Updating..."
                    : `Update to ${finding.fixed_version}`}
              </Button>
            ) : null}
            {!isDismissed ? (
              <Button
                type="button"
                size="sm"
                variant="ghost"
                disabled={dismiss.isPending}
                onClick={handleDismiss}
                className="h-7 px-2 text-xs"
                title="Dismiss this finding"
              >
                Dismiss
              </Button>
            ) : (
              <Button
                type="button"
                size="sm"
                variant="ghost"
                disabled={restore.isPending}
                onClick={handleRestore}
                className="h-7 px-2 text-xs"
                title="Restore this finding to the open list"
              >
                Restore
              </Button>
            )}
          </div>
        </TableCell>
      ) : null}
    </TableRow>
  );
}

// ---------------------------------------------------------------------------
// VulnPanel — main entry point used by the SecurityCard body
// ---------------------------------------------------------------------------

export interface VulnPanelProps {
  siteId: string;
  canWrite?: boolean;
}

export function VulnPanel({ siteId, canWrite = false }: VulnPanelProps) {
  const { data, isPending, isError, error, refetch } = useSiteVulnerabilities(siteId);
  const rescan = useRescanVulns(siteId);
  const [showDismissed, setShowDismissed] = useState(false);

  if (isPending) {
    return <VulnPanelSkeleton />;
  }

  if (isError) {
    return (
      <PageError
        what="Could not load vulnerability scan results."
        why={error instanceof Error ? error.message : "Unknown error"}
        onRetry={() => void refetch()}
        retryLabel="Reload"
      />
    );
  }

  // FEED-NOT-CONFIGURED STATE: when feed_ok is false, the Wordfence
  // Intelligence feed has not been set up for this instance. Do NOT render
  // "No vulnerabilities found" — that would be actively misleading.
  if (!data.feed_ok) {
    return <FeedNotConfiguredState />;
  }

  const allFindings = data.items ?? [];
  const openFindings = allFindings.filter((f) => f.status === "open");
  const dismissedFindings = allFindings.filter((f) => f.status === "dismissed");
  const visible = showDismissed ? allFindings : openFindings;

  const attribution = data.attribution;
  const feedSynced = data.feed_synced;

  function handleRescan() {
    rescan.mutate(undefined, {
      onSuccess: () => {
        toast.success("Rescan queued.", {
          description:
            "The vulnerability scanner will re-check your installed components against the feed.",
        });
      },
      onError: (err: Error) => {
        toast.error("Could not queue rescan.", { description: err.message });
      },
    });
  }

  return (
    <div className="space-y-4">
      {/* Toolbar */}
      <div className="flex items-center justify-between gap-3 flex-wrap">
        <p className="text-xs text-[var(--color-muted-foreground)]">
          {openFindings.length === 0
            ? "No known vulnerabilities"
            : `${openFindings.length} open finding${openFindings.length !== 1 ? "s" : ""}`}
          {dismissedFindings.length > 0
            ? ` (${dismissedFindings.length} dismissed)`
            : null}
          {feedSynced
            ? ` · Feed synced ${relativeTime(feedSynced)}`
            : null}
        </p>
        <div className="flex items-center gap-2">
          {dismissedFindings.length > 0 ? (
            <Button
              type="button"
              size="sm"
              variant="ghost"
              onClick={() => setShowDismissed((v) => !v)}
              aria-pressed={showDismissed}
              className="h-7 px-2 text-xs"
            >
              {showDismissed ? "Hide dismissed" : "Show dismissed"}
            </Button>
          ) : null}
          {canWrite ? (
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={rescan.isPending}
              aria-busy={rescan.isPending}
              onClick={handleRescan}
              className="h-7 px-2 text-xs"
            >
              <RefreshCw aria-hidden="true" className="size-3.5" />
              {rescan.isPending ? "Scanning..." : "Rescan now"}
            </Button>
          ) : null}
        </div>
      </div>

      {/* GH #245 degraded-enrichment banner. Distinct from the FeedNotConfiguredState
          early-return above: the feed IS configured and scanning (feed_ok=true),
          but CVSS enrichment did not complete on the last sync. */}
      {!data.enrichment_available ? <VulnEnrichmentBanner /> : null}

      {/* Zero-findings + feed OK state */}
      {allFindings.length === 0 ? (
        <NoVulnerabilitiesState feedSynced={feedSynced} />
      ) : visible.length === 0 ? (
        <div className="flex items-center justify-center rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] px-6 py-8">
          <p className="text-sm text-[var(--color-muted-foreground)]">
            All findings are dismissed.
          </p>
        </div>
      ) : (
        <div className="overflow-hidden rounded-lg border border-[var(--color-border)] bg-[var(--color-card)]">
          <div className="w-full overflow-x-auto">
            <Table className="min-w-[800px]">
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[100px]">Severity</TableHead>
                  <TableHead>Component</TableHead>
                  <TableHead className="w-[180px]">Version</TableHead>
                  <TableHead className="w-[160px]">CVE</TableHead>
                  <TableHead className="w-[80px]">Source</TableHead>
                  {canWrite ? (
                    <TableHead className="w-[220px] text-right">
                      Actions
                    </TableHead>
                  ) : null}
                </TableRow>
              </TableHeader>
              <TableBody>
                {visible.map((finding) => (
                  <VulnFindingRow
                    key={finding.id}
                    finding={finding}
                    siteId={siteId}
                    mitreNotice={attribution.mitre_notice}
                    canWrite={canWrite}
                  />
                ))}
              </TableBody>
            </Table>
          </div>
        </div>
      )}

      {/* GATE 0 ATTRIBUTION — Defiant copyright/license footer.
          Legally required on all vuln views. Do NOT remove. */}
      <VulnAttributionFooter
        notice={attribution.defiant_notice}
        license={attribution.defiant_license}
      />
    </div>
  );
}

// ---------------------------------------------------------------------------
// Attribution footer (Gate 0 — legally required)
// ---------------------------------------------------------------------------

export function VulnAttributionFooter({
  notice,
  license,
}: {
  notice: string;
  license: string;
}) {
  if (!notice && !license) return null;

  return (
    <div
      aria-label="Vulnerability data attribution"
      className="mt-2 border-t border-[var(--color-border)] pt-3 space-y-1"
    >
      {notice ? (
        <p className="text-xs text-[var(--color-muted-foreground)] leading-relaxed">
          {notice}
        </p>
      ) : null}
      {license ? (
        <p className="text-xs text-[var(--color-muted-foreground)] leading-relaxed">
          {license}
        </p>
      ) : null}
      <p className="text-xs text-[var(--color-muted-foreground)]">
        Vulnerability data provided by{" "}
        <a
          href="https://www.wordfence.com/wordfence-intelligence-terms-and-conditions/"
          target="_blank"
          rel="noopener noreferrer"
          className="underline underline-offset-2 hover:text-[var(--color-foreground)]"
          aria-label="Wordfence Intelligence terms and conditions (opens in new tab)"
        >
          Wordfence Intelligence
        </a>
        .
      </p>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Empty states
// ---------------------------------------------------------------------------

function FeedNotConfiguredState() {
  return (
    <div
      role="status"
      className="flex flex-col items-center justify-center gap-3 rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] px-6 py-10 text-center"
    >
      <ShieldOff
        aria-hidden="true"
        className="size-6 text-[var(--color-muted-foreground)]"
      />
      <div className="space-y-1">
        <p className="text-sm font-medium text-[var(--color-foreground)]">
          Vulnerability feed not configured yet
        </p>
        <p className="max-w-sm text-xs text-[var(--color-muted-foreground)]">
          An administrator needs to connect the Wordfence Intelligence feed from
          the Admin area, under Vulnerability feed. Vulnerability scanning begins
          automatically once the feed is connected.
        </p>
      </div>
    </div>
  );
}

function NoVulnerabilitiesState({ feedSynced }: { feedSynced?: string | null }) {
  return (
    <div
      role="status"
      className="flex flex-col items-center justify-center gap-3 rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] px-6 py-10 text-center"
    >
      <ShieldCheck
        aria-hidden="true"
        className="size-6 text-green-600 dark:text-green-400"
      />
      <div className="space-y-1">
        <p className="text-sm font-medium text-[var(--color-foreground)]">
          No known vulnerabilities
        </p>
        <p className="text-xs text-[var(--color-muted-foreground)]">
          Your installed plugins, themes, and WordPress version have no matches
          in the vulnerability database.
          {feedSynced
            ? ` Last checked ${relativeTime(feedSynced)}.`
            : null}
        </p>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Skeleton
// ---------------------------------------------------------------------------

function VulnPanelSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading vulnerability data"
      className="space-y-3"
    >
      <span className="sr-only">Loading vulnerability data</span>
      <div className="flex items-center justify-between">
        <Skeleton className="h-3 w-32" />
        <Skeleton className="h-7 w-24 rounded" />
      </div>
      <div className="overflow-hidden rounded-lg border border-[var(--color-border)]">
        <div className="flex items-center gap-4 border-b border-[var(--color-border)] px-3 py-2.5">
          <Skeleton className="h-3 w-16" />
          <Skeleton className="h-3 flex-1" />
          <Skeleton className="h-3 w-24" />
          <Skeleton className="h-3 w-20" />
        </div>
        {Array.from({ length: 3 }).map((_, i) => (
          <div
            key={i}
            className="flex items-center gap-4 border-b border-[var(--color-border)] px-3 py-3 last:border-0"
          >
            <Skeleton className="h-5 w-16 rounded" />
            <Skeleton className="h-4 flex-1" />
            <Skeleton className="h-3 w-24" />
            <Skeleton className="h-3 w-20" />
          </div>
        ))}
      </div>
    </div>
  );
}

