import { useState } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";
import { AlertTriangle, ExternalLink, Mail } from "lucide-react";

import { Button } from "@/components/ui/button";
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
import { DefinitionList } from "@/components/shared/definition-list";
import { cn, relativeTime } from "@/lib/utils";
import { useAdminAccountDetail } from "@/features/admin/use-admin-accounts";
import {
  AccountStatusBadge,
  AdminMeterList,
  PlanBadge,
} from "@/features/admin/admin-account-ui";
import { formatCents } from "@/features/admin/admin-accounts-format";
import {
  CompAccountDialog,
  ExtendGraceDialog,
  ForceBillingStateDialog,
  OverridesEditorDialog,
  RestoreAccountDialog,
  RevokeCompDialog,
  SuspendAccountDialog,
} from "@/features/admin/admin-account-dialogs";

// M16 Phase C1 — the account detail page. Answers any support email from one
// screen. Route guard is inherited from the parent /admin layout.

export const Route = createFileRoute("/_authed/admin/accounts/$tenantId")({
  component: AdminAccountDetailPage,
});

type DialogKind =
  | "comp"
  | "revoke-comp"
  | "overrides"
  | "grace"
  | "force-state"
  | "suspend"
  | "restore"
  | null;

function AdminAccountDetailPage() {
  const { tenantId } = Route.useParams();
  const { data, isPending, isError, error, refetch, isRefetching } =
    useAdminAccountDetail(tenantId);
  const [openDialog, setOpenDialog] = useState<DialogKind>(null);

  if (isPending) {
    return (
      <section className="max-w-4xl space-y-6">
        <PageHeader
          title="Loading account..."
          backTo={{ to: "/admin/accounts", label: "Back to Accounts" }}
        />
        <Skeleton className="h-40 w-full rounded-xl" />
        <Skeleton className="h-64 w-full rounded-xl" />
      </section>
    );
  }

  if (isError || !data) {
    return (
      <section className="max-w-4xl space-y-6">
        <PageHeader
          title="Account"
          backTo={{ to: "/admin/accounts", label: "Back to Accounts" }}
        />
        <PageError
          what="Could not load this account."
          why={error?.message}
          onRetry={() => void refetch()}
          isRetrying={isRefetching}
        />
      </section>
    );
  }

  const { header, meters, entitlements, subscription, timeline, members, sites } = data;
  const isComped = header.plan_status === "comped";
  const hasActiveSubscription =
    subscription.provider_subscription_id != null &&
    header.plan_status !== "canceled" &&
    header.plan_status !== "none";
  const isSuspended = header.suspended_at != null;

  const timelineNewestFirst = [...timeline].sort(
    (a, b) => Date.parse(b.at) - Date.parse(a.at),
  );

  return (
    <section className="max-w-4xl space-y-6">
      <PageHeader
        title={header.org_name}
        copyable={header.tenant_id}
        backTo={{ to: "/admin/accounts", label: "Back to Accounts" }}
        badges={
          <>
            <PlanBadge plan={header.plan} comped={isComped} hasOverrides={false} />
            <AccountStatusBadge account={{ plan_status: header.plan_status, suspended: isSuspended }} />
          </>
        }
        subline={
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <span className="font-mono text-xs">{header.slug}</span>
            <span aria-hidden="true">&middot;</span>
            <a href={`mailto:${header.owner_email}`} className="inline-flex items-center gap-1 hover:underline">
              <Mail aria-hidden="true" className="size-3" />
              {header.owner_email}
            </a>
            <span aria-hidden="true">&middot;</span>
            <span>MRR {formatCents(header.mrr_cents)}</span>
            <span aria-hidden="true">&middot;</span>
            <span>
              Created{" "}
              <time dateTime={header.created_at} title={new Date(header.created_at).toLocaleString()}>
                {relativeTime(header.created_at) ?? header.created_at}
              </time>
            </span>
          </div>
        }
      />

      {/* Usage vs limits */}
      <Card>
        <CardHeader className="flex flex-row items-center justify-between pb-3">
          <CardTitle className="text-sm font-medium">Usage vs limits</CardTitle>
          <Button type="button" size="sm" variant="outline" onClick={() => setOpenDialog("overrides")}>
            Edit overrides
          </Button>
        </CardHeader>
        <CardContent className="space-y-5">
          <AdminMeterList meters={meters} />
          {entitlements.length > 0 ? (
            <div className="border-t border-border pt-4">
              <DefinitionList
                rows={entitlements.map((e) => ({
                  label: e.label,
                  value: e.value || "Not on this plan",
                }))}
              />
            </div>
          ) : null}
        </CardContent>
      </Card>

      {/* Subscription */}
      <Card>
        <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-2 pb-3">
          <CardTitle className="text-sm font-medium">Subscription</CardTitle>
          <div className="flex flex-wrap gap-2">
            {isComped ? (
              <Button type="button" size="sm" variant="outline" onClick={() => setOpenDialog("revoke-comp")}>
                Revoke comp
              </Button>
            ) : (
              <Button type="button" size="sm" variant="outline" onClick={() => setOpenDialog("comp")}>
                Comp account
              </Button>
            )}
            <Button type="button" size="sm" variant="outline" onClick={() => setOpenDialog("grace")}>
              Extend grace
            </Button>
            <Button type="button" size="sm" variant="outline" onClick={() => setOpenDialog("force-state")}>
              Force billing state
            </Button>
          </div>
        </CardHeader>
        <CardContent className="space-y-4">
          {subscription.webhook_stale ? (
            <p
              role="alert"
              className="flex items-center gap-2 rounded-md border border-[var(--color-warning)]/40 bg-warning-subtle px-3 py-2 text-sm text-warning-subtle-fg"
            >
              <AlertTriangle aria-hidden="true" className="size-4 shrink-0" />
              No billing webhook has landed recently — this data may be stale.
            </p>
          ) : null}

          <DefinitionList
            rows={[
              { label: "Provider", value: subscription.provider ?? undefined },
              {
                label: "Customer id",
                copyable: subscription.provider_customer_id ?? undefined,
              },
              {
                label: "Subscription id",
                copyable: subscription.provider_subscription_id ?? undefined,
              },
              {
                label: "Renews",
                value: subscription.current_period_end
                  ? new Date(subscription.current_period_end).toLocaleDateString()
                  : undefined,
              },
              {
                label: "Cancels at period end",
                value: subscription.cancel_at_period_end ? "Yes" : "No",
              },
              {
                label: "Grace until",
                value: subscription.grace_until
                  ? new Date(subscription.grace_until).toLocaleString()
                  : undefined,
              },
              {
                label: "Comp reason",
                value: subscription.comp_reason ?? undefined,
              },
              {
                label: "Last billing event",
                value: subscription.last_event_at
                  ? (relativeTime(subscription.last_event_at) ?? subscription.last_event_at)
                  : undefined,
              },
            ]}
          />

          {subscription.stripe_dashboard_base && subscription.provider_customer_id ? (
            <div className="flex flex-wrap gap-3 border-t border-border pt-3">
              <a
                href={`${subscription.stripe_dashboard_base}/customers/${subscription.provider_customer_id}`}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1 text-sm text-foreground underline underline-offset-2 hover:no-underline"
              >
                View customer in Stripe
                <ExternalLink aria-hidden="true" className="size-3" />
              </a>
              {subscription.provider_subscription_id ? (
                <a
                  href={`${subscription.stripe_dashboard_base}/subscriptions/${subscription.provider_subscription_id}`}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="inline-flex items-center gap-1 text-sm text-foreground underline underline-offset-2 hover:no-underline"
                >
                  View subscription in Stripe
                  <ExternalLink aria-hidden="true" className="size-3" />
                </a>
              ) : null}
            </div>
          ) : null}
        </CardContent>
      </Card>

      {/* Timeline */}
      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-sm font-medium">Timeline</CardTitle>
        </CardHeader>
        <CardContent>
          {timelineNewestFirst.length === 0 ? (
            <p className="text-sm text-muted-foreground">No events recorded yet.</p>
          ) : (
            <ul className="divide-y divide-border">
              {timelineNewestFirst.map((entry, i) => (
                <li key={`${entry.at}-${i}`} className="flex flex-col gap-0.5 py-2.5 text-sm">
                  <div className="flex flex-wrap items-baseline gap-x-2">
                    <time
                      dateTime={entry.at}
                      title={new Date(entry.at).toLocaleString()}
                      className="text-xs text-muted-foreground"
                    >
                      {relativeTime(entry.at) ?? entry.at}
                    </time>
                    <span className="font-medium text-foreground">{entry.label}</span>
                  </div>
                  <p className="text-xs text-muted-foreground">
                    {entry.actor ? `${entry.actor} · ` : ""}
                    {entry.source}
                    {entry.reason ? ` · ${entry.reason}` : ""}
                  </p>
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      </Card>

      {/* Members */}
      <Card>
        <CardHeader className="flex flex-row items-center justify-between pb-3">
          <CardTitle className="text-sm font-medium">Members</CardTitle>
          <Link to="/admin" className="text-xs text-primary underline-offset-2 hover:underline">
            Manage users
          </Link>
        </CardHeader>
        <CardContent className="p-0">
          <Table>
            <caption className="sr-only">Account members</caption>
            <TableHeader>
              <TableRow>
                <TableHead>Email</TableHead>
                <TableHead>Role</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>2FA</TableHead>
                <TableHead>Last login</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {members.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={5} className="py-6 text-center text-sm text-muted-foreground">
                    No members.
                  </TableCell>
                </TableRow>
              ) : (
                members.map((m) => (
                  <TableRow key={m.email}>
                    <TableCell className="font-mono text-xs">{m.email}</TableCell>
                    <TableCell className={cn("text-sm", m.role === "owner" && "font-semibold text-foreground")}>
                      {m.role}
                    </TableCell>
                    <TableCell className="text-sm">{m.status}</TableCell>
                    <TableCell className="text-sm">{m.email_verified ? "Verified" : "Unverified"}</TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {m.last_login_at
                        ? (relativeTime(m.last_login_at) ?? m.last_login_at)
                        : "Never"}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      {/* Sites */}
      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-sm font-medium">Sites</CardTitle>
        </CardHeader>
        <CardContent>
          {sites.length === 0 ? (
            <p className="text-sm text-muted-foreground">No sites on this account.</p>
          ) : (
            <ul className="divide-y divide-border text-sm">
              {sites.map((site) => (
                <li key={site.url} className="flex flex-wrap items-center gap-x-3 gap-y-1 py-2">
                  <a
                    href={site.url}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="font-mono text-xs text-muted-foreground hover:underline"
                  >
                    {site.url}
                  </a>
                  <SiteConnectionPill state={site.connection_state} />
                  <span className="ml-auto text-xs text-muted-foreground">
                    {relativeTime(site.created_at) ?? site.created_at}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      </Card>

      {/* Danger zone */}
      <Card className="border-destructive/30">
        <CardHeader className="pb-3">
          <CardTitle className="text-sm font-medium text-destructive">Danger zone</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-wrap items-center gap-3">
          {isSuspended ? (
            <>
              <p className="text-sm text-muted-foreground">
                This account is suspended. Restore it to re-enable access for every member.
              </p>
              <Button type="button" variant="outline" onClick={() => setOpenDialog("restore")}>
                Restore account
              </Button>
            </>
          ) : (
            <>
              <p className="text-sm text-muted-foreground">
                Suspending blocks every member of this org from operating their sites.
              </p>
              <Button type="button" variant="destructive" onClick={() => setOpenDialog("suspend")}>
                Suspend account
              </Button>
            </>
          )}
        </CardContent>
      </Card>

      {/* Dialogs */}
      <CompAccountDialog
        open={openDialog === "comp"}
        onClose={() => setOpenDialog(null)}
        tenantId={header.tenant_id}
        hasActiveSubscription={hasActiveSubscription}
      />
      <RevokeCompDialog
        open={openDialog === "revoke-comp"}
        onClose={() => setOpenDialog(null)}
        tenantId={header.tenant_id}
      />
      <OverridesEditorDialog
        open={openDialog === "overrides"}
        onClose={() => setOpenDialog(null)}
        tenantId={header.tenant_id}
        detail={data}
      />
      <ExtendGraceDialog
        open={openDialog === "grace"}
        onClose={() => setOpenDialog(null)}
        tenantId={header.tenant_id}
      />
      <ForceBillingStateDialog
        open={openDialog === "force-state"}
        onClose={() => setOpenDialog(null)}
        tenantId={header.tenant_id}
        currentPlan={header.plan}
      />
      <SuspendAccountDialog
        open={openDialog === "suspend"}
        onClose={() => setOpenDialog(null)}
        tenantId={header.tenant_id}
        orgSlug={header.slug}
      />
      <RestoreAccountDialog
        open={openDialog === "restore"}
        onClose={() => setOpenDialog(null)}
        tenantId={header.tenant_id}
        orgSlug={header.slug}
      />
    </section>
  );
}

// ---------------------------------------------------------------------------
// SiteConnectionPill — compact connection-state pill for the Sites card.
// Mirrors the small pill pattern in routes/_authed/admin/index.tsx
// (ExpandedSitesRow's connectionStatePill) rather than the fuller
// ConnectionStateBadge, since this compact list has no lastSeenAt/expiresAt
// context to drive that component's richer tail text.
// ---------------------------------------------------------------------------

function SiteConnectionPill({ state }: { state: string }) {
  if (state === "connected") {
    return (
      <span className="inline-flex items-center rounded-full bg-green-100 px-1.5 py-0.5 text-[10px] font-medium text-green-800 dark:bg-green-900/40 dark:text-green-300">
        {state}
      </span>
    );
  }
  if (state === "degraded") {
    return (
      <span className="inline-flex items-center rounded-full bg-yellow-100 px-1.5 py-0.5 text-[10px] font-medium text-yellow-800 dark:bg-yellow-900/40 dark:text-yellow-300">
        {state}
      </span>
    );
  }
  return (
    <span className="inline-flex items-center rounded-full bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
      {state}
    </span>
  );
}
