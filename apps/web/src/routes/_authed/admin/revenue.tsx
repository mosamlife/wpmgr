import { createFileRoute, Link } from "@tanstack/react-router";
import { AlertTriangle, CircleCheck } from "lucide-react";

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { PageHeader } from "@/components/shared/page-header";
import { useMe } from "@/features/auth/use-auth";
import { relativeTime, cn } from "@/lib/utils";
import { useAdminRevenue } from "@/features/admin/use-admin-accounts";
import {
  formatCents,
  sortEventsNewestFirst,
  sortPastDueOldestFirst,
} from "@/features/admin/admin-accounts-format";
import { planLabel } from "@/features/billing/plan-catalog";
import type { BillingPlanId } from "@/features/billing/use-billing";

// M16 Phase C1 — the superadmin Revenue page. Deliberately anti-dashboard:
// raw counts (not churn %), and the past-due worklist is the actionable core.

export const Route = createFileRoute("/_authed/admin/revenue")({
  component: AdminRevenuePage,
});

function AdminRevenuePage() {
  const { data: me } = useMe();
  const hosted = me?.hosted === true;
  const { data, isPending, isError, error, refetch, isRefetching } =
    useAdminRevenue();

  if (isPending) {
    return (
      <section className="max-w-4xl space-y-6">
        <PageHeader title="Revenue" subline="Instance-wide billing health." />
        <div className="grid grid-cols-2 gap-4 sm:grid-cols-5">
          {Array.from({ length: 5 }).map((_, i) => (
            <Skeleton key={i} className="h-20 w-full rounded-xl" />
          ))}
        </div>
        <Skeleton className="h-64 w-full rounded-xl" />
      </section>
    );
  }

  if (isError || !data) {
    return (
      <section className="max-w-4xl space-y-6">
        <PageHeader title="Revenue" subline="Instance-wide billing health." />
        <PageError
          what="Could not load revenue data."
          why={error?.message}
          onRetry={() => void refetch()}
          isRetrying={isRefetching}
        />
      </section>
    );
  }

  const { tiles, plan_distribution, past_due, recent_events, last_webhook_at, webhook_stale } = data;
  const pastDueSorted = sortPastDueOldestFirst(past_due);
  const eventsSorted = sortEventsNewestFirst(recent_events).slice(0, 20);

  return (
    <section className="max-w-4xl space-y-6">
      <PageHeader title="Revenue" subline="Instance-wide billing health." />

      {!hosted ? (
        <div
          role="status"
          className="rounded-lg border border-border bg-muted/40 px-4 py-3 text-sm text-muted-foreground"
        >
          Billing is not configured on this installation. These figures will
          read zero.
        </div>
      ) : null}

      {/* Tiles */}
      <div className="grid grid-cols-2 gap-4 sm:grid-cols-5">
        <Tile label="MRR" value={formatCents(tiles.mrr_cents)} />
        <Tile
          label="Active subs"
          value={tiles.active_subs.toLocaleString()}
          subline={`${tiles.trialing.toLocaleString()} trialing`}
        />
        <Tile
          label="Past due"
          value={tiles.past_due_count.toLocaleString()}
          subline={`${formatCents(tiles.past_due_at_risk_cents)} at risk`}
          tone={tiles.past_due_count > 0 ? "critical" : undefined}
        />
        <Tile label="New this month" value={tiles.new_this_month.toLocaleString()} />
        <Tile label="Canceled this month" value={tiles.canceled_this_month.toLocaleString()} />
      </div>

      {/* Plan distribution */}
      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-sm font-medium">Plan distribution</CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          <Table>
            <caption className="sr-only">Accounts by plan</caption>
            <TableHeader>
              <TableRow>
                <TableHead>Plan</TableHead>
                <TableHead className="text-right">Accounts</TableHead>
                <TableHead className="text-right">MRR share</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {plan_distribution.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={3} className="py-6 text-center text-sm text-muted-foreground">
                    No accounts yet.
                  </TableCell>
                </TableRow>
              ) : (
                plan_distribution.map((row) => (
                  <TableRow key={row.plan}>
                    <TableCell>
                      <Link
                        to="/admin/accounts"
                        search={{ plan: [row.plan as BillingPlanId] }}
                        className="text-sm font-medium capitalize text-foreground hover:underline"
                      >
                        {planLabel(row.plan)}
                      </Link>
                    </TableCell>
                    <TableCell className="text-right tabular-nums text-sm">
                      {row.count.toLocaleString()}
                    </TableCell>
                    <TableCell className="text-right tabular-nums text-sm">
                      {row.comped_value_cents != null
                        ? `${formatCents(row.comped_value_cents)} comped value`
                        : formatCents(row.mrr_share_cents)}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      {/* Past due worklist */}
      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-sm font-medium">Past due</CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          {pastDueSorted.length === 0 ? (
            <div className="flex items-center gap-2 px-6 py-6 text-sm text-[var(--color-success-subtle-fg)]">
              <CircleCheck aria-hidden="true" className="size-4" />
              No accounts are past due.
            </div>
          ) : (
            <Table>
              <caption className="sr-only">Accounts past due, oldest first</caption>
              <TableHeader>
                <TableRow>
                  <TableHead>Org</TableHead>
                  <TableHead className="text-right">Amount</TableHead>
                  <TableHead className="text-right">Days past due</TableHead>
                  <TableHead>Grace until</TableHead>
                  <TableHead>Last failed</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {pastDueSorted.map((row) => (
                  <TableRow key={row.tenant_id}>
                    <TableCell>
                      <Link
                        to="/admin/accounts/$tenantId"
                        params={{ tenantId: row.tenant_id }}
                        className="flex flex-col gap-0.5"
                      >
                        <span className="text-sm font-medium text-foreground hover:underline">
                          {row.org_name}
                        </span>
                        <a
                          href={`mailto:${row.owner_email}`}
                          className="text-xs text-muted-foreground hover:underline"
                          onClick={(e) => e.stopPropagation()}
                        >
                          {row.owner_email}
                        </a>
                      </Link>
                    </TableCell>
                    <TableCell className="text-right tabular-nums text-sm">
                      {formatCents(row.amount_cents)}
                    </TableCell>
                    <TableCell className="text-right tabular-nums text-sm text-destructive">
                      {row.days_past_due}
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {row.grace_until ? new Date(row.grace_until).toLocaleDateString() : "–"}
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {row.last_failed_at
                        ? (relativeTime(row.last_failed_at) ?? row.last_failed_at)
                        : "–"}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Recent events */}
      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-sm font-medium">Recent billing events</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          {eventsSorted.length === 0 ? (
            <p className="text-sm text-muted-foreground">No recent billing events.</p>
          ) : (
            <ul className="divide-y divide-border">
              {eventsSorted.map((ev, i) => (
                <li key={`${ev.at}-${i}`} className="flex flex-wrap items-baseline gap-x-2 py-2 text-sm">
                  <time
                    dateTime={ev.at}
                    title={new Date(ev.at).toLocaleString()}
                    className="text-xs text-muted-foreground"
                  >
                    {relativeTime(ev.at) ?? ev.at}
                  </time>
                  <span className="font-medium text-foreground">{ev.org_name}</span>
                  <span className="text-muted-foreground">{ev.kind}</span>
                  <span className="ml-auto text-xs text-muted-foreground">{ev.source}</span>
                </li>
              ))}
            </ul>
          )}

          <div
            className={cn(
              "flex items-center gap-2 border-t border-border pt-3 text-xs",
              webhook_stale ? "text-warning-subtle-fg" : "text-muted-foreground",
            )}
          >
            {webhook_stale ? <AlertTriangle aria-hidden="true" className="size-3.5 shrink-0" /> : null}
            {last_webhook_at
              ? `Last webhook received ${relativeTime(last_webhook_at) ?? last_webhook_at}`
              : "No webhook has ever been received."}
          </div>
        </CardContent>
      </Card>
    </section>
  );
}

// ---------------------------------------------------------------------------
// Tile
// ---------------------------------------------------------------------------

function Tile({
  label,
  value,
  subline,
  tone,
}: {
  label: string;
  value: string;
  subline?: string;
  tone?: "critical";
}) {
  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="text-xs font-medium uppercase tracking-[0.02em] text-muted-foreground">
          {label}
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-0.5">
        <p
          className={cn(
            "text-2xl font-semibold tabular-nums",
            tone === "critical" && "text-destructive",
          )}
        >
          {value}
        </p>
        {subline ? <p className="text-xs text-muted-foreground">{subline}</p> : null}
      </CardContent>
    </Card>
  );
}
