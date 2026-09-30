import { PageError } from "@/components/feedback/page-error";
import { PageHeader } from "@/components/shared/page-header";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

import { verdictShareLabel } from "./content-copy";
import { useContentFleetReport } from "./use-content";

// Superadmin report: counts only. Tells the owner which builders to support
// first. Builder ids and versions come from the platform, rendered as text.

function pct(n: number, total: number): string {
  if (total <= 0) return "0%";
  return `${Math.round((n / total) * 100)}%`;
}

export function ContentFleetReport() {
  const q = useContentFleetReport();

  if (q.isPending) {
    return (
      <div role="status" aria-label="Loading report" className="space-y-2">
        <Skeleton className="h-9 w-full" />
        <Skeleton className="h-9 w-full" />
      </div>
    );
  }
  if (q.isError) {
    return (
      <PageError
        what="Could not load the page editor report"
        onRetry={() => void q.refetch()}
        isRetrying={q.isFetching}
      />
    );
  }

  const report = q.data;

  return (
    <div className="space-y-8">
      <PageHeader
        title="Page editors"
        subline={
          report.pages === 0
            ? "No pages have been checked yet."
            : `Pages checked: ${report.pages}`
        }
      />
      {report.pages === 0 ? null : (
        <>
          <section aria-labelledby="by-editor" className="space-y-2">
            <h2 id="by-editor" className="text-sm font-semibold">
              By page builder
            </h2>
            {report.by_builder.length === 0 ? (
              <p className="text-sm text-[var(--color-muted-foreground)]">
                No page builders detected.
              </p>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Builder</TableHead>
                    <TableHead>Version</TableHead>
                    <TableHead>Pages</TableHead>
                    <TableHead>Sites</TableHead>
                    <TableHead>Share</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {report.by_builder.map((b) => (
                    <TableRow key={`${b.integration_id}@${b.version ?? ""}`}>
                      <TableCell>{b.integration_id}</TableCell>
                      <TableCell>{b.version ?? "Unknown"}</TableCell>
                      <TableCell>{b.pages}</TableCell>
                      <TableCell>{b.sites}</TableCell>
                      <TableCell>{pct(b.pages, report.pages)}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </section>
          <section aria-labelledby="by-verdict" className="space-y-2">
            <h2 id="by-verdict" className="text-sm font-semibold">
              By kind of page
            </h2>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Kind</TableHead>
                  <TableHead>Pages</TableHead>
                  <TableHead>Sites</TableHead>
                  <TableHead>Share</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {report.by_verdict.map((v) => (
                  <TableRow key={`${v.verdict}/${v.route_number}`}>
                    <TableCell>{verdictShareLabel(v.verdict)}</TableCell>
                    <TableCell>{v.pages}</TableCell>
                    <TableCell>{v.sites}</TableCell>
                    <TableCell>{pct(v.pages, report.pages)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </section>
        </>
      )}
    </div>
  );
}
