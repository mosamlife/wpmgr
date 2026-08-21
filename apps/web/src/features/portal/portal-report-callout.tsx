// PortalReportCallout — highlights the latest completed report with period
// label + HTML/PDF download. Null state: renders nothing (per contract §2.7 —
// "no empty promises"). The download flow reuses fetchPortalReportDownload.

import { useState } from "react";
import { Link } from "@tanstack/react-router";
import { Download, FileText, Loader2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { toast } from "@/components/toast";
import { toError } from "@/features/auth/use-auth";
import { fetchPortalReportDownload } from "./use-portal";
import type { PortalSummaryLatestReport } from "./use-portal";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function formatPeriod(report: PortalSummaryLatestReport): string {
  const start = new Date(report.period_start).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
    year: "numeric",
  });
  const end = new Date(report.period_end).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
    year: "numeric",
  });
  return `${start} – ${end}`;
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

export interface PortalReportCalloutProps {
  // Null and undefined both mean "no report yet": the field is omitted on some
  // responses and an explicit JSON null on others. Both render as absence.
  latestReport: PortalSummaryLatestReport | null | undefined;
}

export function PortalReportCallout({ latestReport }: PortalReportCalloutProps) {
  const [downloading, setDownloading] = useState<"html" | "pdf" | null>(null);

  // Per contract §2.7: render nothing when there is no report yet.
  if (!latestReport) return null;

  async function handleDownload(format: "html" | "pdf") {
    if (downloading || !latestReport) return;
    setDownloading(format);
    try {
      const result = await fetchPortalReportDownload(latestReport.id, format);
      window.open(result.url, "_blank", "noopener,noreferrer");
    } catch (err) {
      toast.error(toError(err).message || "Could not get download link.");
    } finally {
      setDownloading(null);
    }
  }

  const periodLabel = formatPeriod(latestReport);

  return (
    <div className="mb-6 flex flex-col gap-3 rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] p-4 sm:flex-row sm:items-center">
      <div className="flex min-w-0 flex-1 items-center gap-3">
        <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-[var(--color-primary)]/10">
          <FileText
            aria-hidden="true"
            className="size-5 text-[var(--color-primary)]"
          />
        </div>
        <div className="min-w-0">
          <p className="text-sm font-semibold text-[var(--color-foreground)]">
            Latest report
          </p>
          <p className="truncate text-xs text-[var(--color-muted-foreground)]">
            {periodLabel}
          </p>
        </div>
      </div>

      <div className="flex shrink-0 flex-wrap items-center gap-2">
        <Button
          variant="default"
          size="sm"
          disabled={downloading !== null}
          onClick={() => void handleDownload("html")}
          aria-label="View HTML report"
        >
          {downloading === "html" ? (
            <Loader2 aria-hidden="true" className="mr-1.5 size-3.5 animate-spin" />
          ) : (
            <Download aria-hidden="true" className="mr-1.5 size-3.5" />
          )}
          View report
        </Button>
        <Button
          variant="outline"
          size="sm"
          disabled={downloading !== null}
          onClick={() => void handleDownload("pdf")}
          aria-label="Download PDF report"
        >
          {downloading === "pdf" ? (
            <Loader2 aria-hidden="true" className="mr-1.5 size-3.5 animate-spin" />
          ) : (
            <Download aria-hidden="true" className="mr-1.5 size-3.5" />
          )}
          PDF
        </Button>
        <Link
          to="/portal/reports"
          className="text-xs text-[var(--color-primary)] underline underline-offset-2 hover:opacity-80"
        >
          View all reports
        </Link>
      </div>
    </div>
  );
}
