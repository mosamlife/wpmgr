import { useState } from "react";

import { Button } from "@/components/ui/button";
import { PageError } from "@/components/feedback";
import {
  Table,
  TableBody,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import type { PhpError } from "@wpmgr/api";

import { ErrorRow } from "./error-row";
import { ErrorDetailDrawer } from "./error-detail-drawer";
import { usePHPErrors, useSilenceError } from "./use-errors";

// The PHP-error monitor table for one site. Columns:
//   Severity / file:line / message excerpt / count / last seen / actions
//
// A row click opens the detail dialog. The toolbar carries a "Show silenced"
// toggle; unsilenced is the default since silenced rows are deliberately
// hidden noise. The silence button on each row is a quick toggle that
// invalidates the list.

export function ErrorsTable({ siteId }: { siteId: string }) {
  const [showSilenced, setShowSilenced] = useState(false);
  const [active, setActive] = useState<PhpError | null>(null);

  const { data, isPending, isError, error, refetch } = usePHPErrors(siteId, {
    showSilenced,
  });
  const silence = useSilenceError(siteId);

  return (
    <section
      aria-labelledby="errors-heading"
      className="px-6 pt-6 pb-8 space-y-4"
    >
      <div className="flex items-center justify-between gap-3">
        <h2
          id="errors-heading"
          className="text-xs font-medium uppercase tracking-wide text-muted-foreground"
        >
          PHP errors
        </h2>
        <div className="flex items-center gap-2">
          <Button
            type="button"
            size="sm"
            variant={showSilenced ? "outline" : "ghost"}
            onClick={() => setShowSilenced((v) => !v)}
            aria-pressed={showSilenced}
          >
            {showSilenced ? "Hide silenced" : "Show silenced"}
          </Button>
          <Button
            type="button"
            size="sm"
            variant="ghost"
            onClick={() => void refetch()}
          >
            Reload
          </Button>
        </div>
      </div>

      {isPending ? (
        <p role="status" className="text-sm text-muted-foreground">
          Loading errors…
        </p>
      ) : isError ? (
        <PageError
          what="Could not load PHP errors."
          why={error instanceof Error ? error.message : "Unknown error"}
          onRetry={() => void refetch()}
          retryLabel="Reload errors"
        />
      ) : (data ?? []).length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No errors captured. The agent ships up to 50 newest rows per
          heartbeat.
        </p>
      ) : (
        <div className="rounded-lg border border-border bg-card">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-[110px]">Severity</TableHead>
                <TableHead>File:line</TableHead>
                <TableHead>Message</TableHead>
                <TableHead className="w-[80px] text-right">Count</TableHead>
                <TableHead className="w-[120px] text-right">
                  Last seen
                </TableHead>
                <TableHead className="w-[120px] text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(data ?? []).map((e) => (
                <ErrorRow
                  key={e.id}
                  error={e}
                  onOpen={() => setActive(e)}
                  onSilence={(silenced) =>
                    silence.mutate({ md5: e.md5, silenced })
                  }
                />
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      <ErrorDetailDrawer
        error={active}
        onClose={() => setActive(null)}
        onSilence={(silenced) => {
          if (active) {
            silence.mutate({ md5: active.md5, silenced });
            setActive(null);
          }
        }}
      />
    </section>
  );
}
