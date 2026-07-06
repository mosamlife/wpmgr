import { useMemo } from "react";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ChevronDown, ChevronRight, Search, X } from "lucide-react";
import { z } from "zod";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { PageError } from "@/components/feedback";
import { PageHeader } from "@/components/shared/page-header";
import { useMe } from "@/features/auth/use-auth";
import { relativeTime, cn } from "@/lib/utils";
import {
  useAdminAccountsList,
} from "@/features/admin/use-admin-accounts";
import type { AdminAccountListItem } from "@/features/admin/use-admin-accounts";
import {
  ACCOUNT_PLAN_FILTER_OPTIONS,
  ACCOUNT_SORT_OPTIONS,
  ACCOUNT_STATUS_FILTER_OPTIONS,
  ACCOUNT_STATUS_LABEL,
  DEFAULT_ACCOUNTS_LIMIT,
  DEFAULT_ACCOUNTS_SORT,
  activeAccountsFilterCount,
  formatAccountMrr,
  formatCents,
  isIdle90d,
  type FilterableAccountStatus,
  type AdminAccountSort,
  type AdminAccountsFilters,
} from "@/features/admin/admin-accounts-format";
import {
  AccountStatusBadge,
  PlanBadge,
  SitesMeterChip,
  StorageMeterChip,
} from "@/features/admin/admin-account-ui";
import type { BillingPlanId } from "@/features/billing/use-billing";
import { planLabel } from "@/features/billing/plan-catalog";

// M16 Phase C1 — the superadmin Accounts list: the daily money-lens over
// every tenant on the instance. Route guard is inherited from the parent
// /admin layout (route.tsx) — no beforeLoad needed here.

// Status/plan literals are spelled out explicitly (rather than derived from
// ACCOUNT_STATUS_FILTER_OPTIONS/ACCOUNT_PLAN_FILTER_OPTIONS) because zod's
// z.enum() needs a literal tuple type, not a `readonly T[]` — this keeps the
// schema free of type-assertion gymnastics. Keep in sync with those arrays.
const accountsSearchSchema = z.object({
  q: z.string().optional(),
  status: z
    .array(z.enum(["active", "trialing", "past_due", "canceled", "suspended", "comped"]))
    .optional(),
  plan: z.array(z.enum(["free", "starter", "agency", "scale"])).optional(),
  near_limit: z.boolean().optional(),
  has_overrides: z.boolean().optional(),
  comped: z.boolean().optional(),
  idle: z.boolean().optional(),
  sort: z.enum(["needs_attention", "mrr_desc", "created_desc", "name_asc"]).optional(),
  limit: z.number().optional(),
  offset: z.number().optional(),
});

type AccountsSearch = z.infer<typeof accountsSearchSchema>;

export const Route = createFileRoute("/_authed/admin/accounts/")({
  validateSearch: accountsSearchSchema,
  component: AdminAccountsPage,
});

function filtersFromSearch(search: AccountsSearch): AdminAccountsFilters {
  return {
    search: search.q ?? "",
    status: search.status ?? [],
    plan: search.plan ?? [],
    nearLimit: search.near_limit ?? false,
    hasOverrides: search.has_overrides ?? false,
    comped: search.comped ?? false,
    idle90d: search.idle ?? false,
    sort: search.sort ?? DEFAULT_ACCOUNTS_SORT,
    limit: search.limit ?? DEFAULT_ACCOUNTS_LIMIT,
    offset: search.offset ?? 0,
  };
}

function AdminAccountsPage() {
  const { data: me } = useMe();
  const hosted = me?.hosted === true;

  const navigate = useNavigate({ from: Route.fullPath });
  const search = Route.useSearch();
  const filters = useMemo(() => filtersFromSearch(search), [search]);

  const { data, isPending, isError, error, refetch, isRefetching } =
    useAdminAccountsList(filters);

  function patchSearch(patch: Partial<AccountsSearch>) {
    void navigate({
      search: (prev: AccountsSearch) => ({ ...prev, ...patch, offset: 0 }),
      replace: true,
    });
  }

  function toggleStatus(value: FilterableAccountStatus) {
    const next = filters.status.includes(value)
      ? filters.status.filter((s) => s !== value)
      : [...filters.status, value];
    patchSearch({ status: next.length > 0 ? next : undefined });
  }

  function togglePlan(value: BillingPlanId) {
    const next = filters.plan.includes(value)
      ? filters.plan.filter((p) => p !== value)
      : [...filters.plan, value];
    patchSearch({ plan: next.length > 0 ? next : undefined });
  }

  const activeFilterCount = activeAccountsFilterCount(filters);

  function clearAllFilters() {
    void navigate({
      search: () => ({ sort: filters.sort === DEFAULT_ACCOUNTS_SORT ? undefined : filters.sort }),
      replace: true,
    });
  }

  const tiles = data?.tiles;
  const total = data?.total ?? 0;
  const pageStart = total === 0 ? 0 : filters.offset + 1;
  const pageEnd = Math.min(filters.offset + filters.limit, total);

  return (
    <section aria-labelledby="admin-accounts-heading" className="space-y-6">
      <PageHeader
        title="Accounts"
        subline="Billing accounts across every tenant on this instance."
      />

      {!hosted ? (
        <div
          role="status"
          className="rounded-lg border border-border bg-muted/40 px-4 py-3 text-sm text-muted-foreground"
        >
          Billing is not configured on this installation. Plan and MRR figures
          below will read zero for every account.
        </div>
      ) : null}

      {/* Tiles */}
      <div className="grid grid-cols-2 gap-4 sm:grid-cols-4">
        <Tile label="MRR" value={tiles ? formatCents(tiles.mrr_cents) : "–"} />
        <Tile label="Active subs" value={tiles ? tiles.active_subs.toLocaleString() : "–"} />
        <Tile
          label="Past due"
          value={tiles ? tiles.past_due_count.toLocaleString() : "–"}
          tone={tiles && tiles.past_due_count > 0 ? "critical" : undefined}
        />
        <Tile label="Accounts total" value={tiles ? tiles.accounts_total.toLocaleString() : "–"} />
      </div>

      {/* Filters */}
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative w-full max-w-xs">
          <Search
            aria-hidden="true"
            className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground"
          />
          <Input
            type="search"
            placeholder="Search org, slug, or owner email..."
            value={filters.search}
            onChange={(e) => patchSearch({ q: e.target.value || undefined })}
            aria-label="Search accounts"
            className="pl-8"
          />
        </div>

        <MultiSelectMenu
          label="Status"
          options={ACCOUNT_STATUS_FILTER_OPTIONS}
          renderLabel={(v) => ACCOUNT_STATUS_LABEL[v]}
          selected={filters.status}
          onToggle={toggleStatus}
          onClear={() => patchSearch({ status: undefined })}
        />
        <MultiSelectMenu
          label="Plan"
          options={ACCOUNT_PLAN_FILTER_OPTIONS}
          renderLabel={(v) => planLabel(v)}
          selected={filters.plan}
          onToggle={togglePlan}
          onClear={() => patchSearch({ plan: undefined })}
        />

        <ToggleChip
          label="Past due"
          active={filters.status.includes("past_due")}
          onClick={() => toggleStatus("past_due")}
        />
        <ToggleChip
          label="Near limit"
          active={filters.nearLimit}
          onClick={() => patchSearch({ near_limit: filters.nearLimit ? undefined : true })}
        />
        <ToggleChip
          label="Has overrides"
          active={filters.hasOverrides}
          onClick={() => patchSearch({ has_overrides: filters.hasOverrides ? undefined : true })}
        />
        <ToggleChip
          label="Comped"
          active={filters.comped}
          onClick={() => patchSearch({ comped: filters.comped ? undefined : true })}
        />
        <ToggleChip
          label="Idle 90d+"
          active={filters.idle90d}
          onClick={() => patchSearch({ idle: filters.idle90d ? undefined : true })}
        />

        {activeFilterCount > 0 ? (
          <button
            type="button"
            onClick={clearAllFilters}
            className="inline-flex h-9 items-center gap-1 rounded-md px-2 text-xs font-medium text-muted-foreground hover:text-foreground"
          >
            <X aria-hidden="true" className="size-3.5" />
            Clear filters ({activeFilterCount})
          </button>
        ) : null}

        <div className="ml-auto flex items-center gap-2">
          <label htmlFor="accounts-sort" className="text-xs text-muted-foreground">
            Sort
          </label>
          <Select
            id="accounts-sort"
            value={filters.sort}
            onChange={(e) => patchSearch({ sort: e.target.value as AdminAccountSort })}
            className="h-9 w-auto"
          >
            {ACCOUNT_SORT_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </Select>
        </div>
      </div>

      {isError ? (
        <PageError
          what="Could not load accounts."
          why={error?.message}
          onRetry={() => void refetch()}
          isRetrying={isRefetching}
        />
      ) : null}

      {/* Table */}
      <div className="rounded-xl border border-[var(--color-border)]">
        <Table>
          <caption className="sr-only">Billing accounts</caption>
          <TableHeader>
            <TableRow>
              <TableHead>Org</TableHead>
              <TableHead>Plan</TableHead>
              <TableHead>Status</TableHead>
              <TableHead className="text-right">MRR</TableHead>
              <TableHead>Sites</TableHead>
              <TableHead>Storage</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Last activity</TableHead>
              <TableHead className="sr-only">Open</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {isPending ? (
              <TableRow>
                <TableCell colSpan={9} className="py-10 text-center text-sm text-muted-foreground">
                  Loading accounts...
                </TableCell>
              </TableRow>
            ) : !data || data.items.length === 0 ? (
              <TableRow>
                <TableCell colSpan={9} className="py-10 text-center text-sm text-muted-foreground">
                  {activeFilterCount > 0 || filters.search
                    ? "No accounts match your filters."
                    : "No accounts yet."}
                </TableCell>
              </TableRow>
            ) : (
              data.items.map((account) => (
                <AccountRow key={account.tenant_id} account={account} />
              ))
            )}
          </TableBody>
        </Table>
      </div>

      {/* Pager */}
      <div className="flex items-center justify-between text-sm text-muted-foreground">
        <span className="tabular-nums">
          {total > 0 ? `${pageStart}-${pageEnd} of ${total}` : "0 of 0"}
        </span>
        <div className="flex items-center gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={filters.offset === 0}
            onClick={() => patchSearch({ offset: Math.max(0, filters.offset - filters.limit) })}
          >
            Previous
          </Button>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={pageEnd >= total}
            onClick={() => patchSearch({ offset: filters.offset + filters.limit })}
          >
            Next
          </Button>
        </div>
      </div>
    </section>
  );
}

// ---------------------------------------------------------------------------
// Tile
// ---------------------------------------------------------------------------

function Tile({
  label,
  value,
  tone,
}: {
  label: string;
  value: string;
  tone?: "critical";
}) {
  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="text-xs font-medium uppercase tracking-[0.02em] text-muted-foreground">
          {label}
        </CardTitle>
      </CardHeader>
      <CardContent>
        <p
          className={cn(
            "text-2xl font-semibold tabular-nums",
            tone === "critical" && "text-destructive",
          )}
        >
          {value}
        </p>
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// ToggleChip — a single-tap boolean filter chip
// ---------------------------------------------------------------------------

function ToggleChip({
  label,
  active,
  onClick,
}: {
  label: string;
  active: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={cn(
        "h-9 rounded-md border px-3 text-xs font-medium transition-colors",
        active
          ? "border-[var(--color-primary)]/50 bg-[var(--color-primary)]/5 text-foreground"
          : "border-border text-muted-foreground hover:text-foreground",
      )}
    >
      {label}
    </button>
  );
}

// ---------------------------------------------------------------------------
// MultiSelectMenu — generic checkbox dropdown for Status / Plan
// ---------------------------------------------------------------------------

function MultiSelectMenu<T extends string>({
  label,
  options,
  renderLabel,
  selected,
  onToggle,
  onClear,
}: {
  label: string;
  options: readonly T[];
  renderLabel: (value: T) => string;
  selected: readonly T[];
  onToggle: (value: T) => void;
  onClear: () => void;
}) {
  const count = selected.length;
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className={cn(
            "h-9 gap-1.5",
            count > 0 && "border-[var(--color-primary)]/50 bg-[var(--color-primary)]/5",
          )}
          aria-label={`Filter by ${label.toLowerCase()}`}
        >
          {label}
          {count > 0 ? (
            <span className="ml-0.5 rounded-sm bg-[var(--color-primary)]/10 px-1 text-xs font-medium tabular-nums text-[var(--color-primary)]">
              {count}
            </span>
          ) : null}
          <ChevronDown aria-hidden="true" className="size-3" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="min-w-[12rem]">
        {count > 0 ? (
          <>
            <DropdownMenuItem
              onSelect={(e) => {
                e.preventDefault();
                onClear();
              }}
              className="text-xs"
            >
              Show all
            </DropdownMenuItem>
            <DropdownMenuSeparator />
          </>
        ) : null}
        {options.map((opt) => (
          <DropdownMenuCheckboxItem
            key={opt}
            checked={selected.includes(opt)}
            onCheckedChange={() => onToggle(opt)}
            onSelect={(e) => e.preventDefault()}
            className="text-xs"
          >
            {renderLabel(opt)}
          </DropdownMenuCheckboxItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

// ---------------------------------------------------------------------------
// AccountRow
// ---------------------------------------------------------------------------

function AccountRow({ account }: { account: AdminAccountListItem }) {
  const idle = isIdle90d(account.last_activity);

  return (
    <TableRow>
      <TableCell>
        <Link
          to="/admin/accounts/$tenantId"
          params={{ tenantId: account.tenant_id }}
          className="flex flex-col gap-0.5 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
        >
          <span className="text-sm font-medium text-foreground hover:underline">
            {account.org_name}
          </span>
          <span className="font-mono text-xs text-muted-foreground">{account.org_slug}</span>
          {account.owner_email ? (
            <span className="text-xs text-muted-foreground">{account.owner_email}</span>
          ) : null}
        </Link>
      </TableCell>
      <TableCell>
        <PlanBadge
          plan={account.plan}
          comped={account.plan_status === "comped"}
          hasOverrides={account.has_overrides}
        />
      </TableCell>
      <TableCell>
        <AccountStatusBadge account={account} />
      </TableCell>
      <TableCell className="text-right tabular-nums text-sm">
        {formatAccountMrr(account)}
      </TableCell>
      <TableCell>
        <SitesMeterChip used={account.sites_used} cap={account.sites_cap} />
      </TableCell>
      <TableCell>
        <StorageMeterChip
          usedBytes={account.storage_used_bytes_approx}
          capBytes={account.storage_cap_bytes}
        />
      </TableCell>
      <TableCell className="text-xs text-muted-foreground">
        <time dateTime={account.created_at} title={new Date(account.created_at).toLocaleString()}>
          {relativeTime(account.created_at) ?? "–"}
        </time>
      </TableCell>
      <TableCell className={cn("text-xs", idle ? "text-warning-subtle-fg" : "text-muted-foreground")}>
        {account.last_activity ? (
          <time
            dateTime={account.last_activity}
            title={new Date(account.last_activity).toLocaleString()}
          >
            {relativeTime(account.last_activity) ?? "–"}
            {idle ? " · idle" : ""}
          </time>
        ) : (
          "Never"
        )}
      </TableCell>
      <TableCell>
        <Link
          to="/admin/accounts/$tenantId"
          params={{ tenantId: account.tenant_id }}
          aria-label={`Open ${account.org_name}`}
          className="flex items-center justify-end text-muted-foreground hover:text-foreground"
        >
          <ChevronRight aria-hidden="true" className="size-4" />
        </Link>
      </TableCell>
    </TableRow>
  );
}
